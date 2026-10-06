package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

// The time zone cases set the session's TimeZone with SET TIME ZONE, as the
// container's server runs in UTC, and compare how a timestamptz converts to
// and from a timestamp and a date there. The seeds give every time its own
// offset, since they run in a session of their own. Results are booleans and
// integers, as the server renders times as text.
var tzCases = []difftest.Case{
	{
		Name:   "a session in Asia/Tokyo",
		Schema: []string{`CREATE TABLE ev (id int PRIMARY KEY, at timestamptz NOT NULL, ts timestamp NOT NULL, on_day date NOT NULL)`},
		Seed: []string{`INSERT INTO ev VALUES
			(1, '2024-05-19 15:30:00+00', '2024-05-20 00:30:00', '2024-05-20'),
			(2, '2024-05-20 14:59:59+00', '2024-05-20 23:59:59', '2024-05-20'),
			(3, '2024-05-20 15:00:00+00', '2024-05-21 00:00:00', '2024-05-21'),
			(4, '2024-01-31 15:00:00+00', '2024-02-01 00:00:00', '2024-02-01')`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `SET TIME ZONE 'Asia/Tokyo'`),
			difftest.Q(0, `SELECT id, at = ts, at::timestamp = ts, at::date = on_day, ts::timestamptz = at, on_day::timestamptz <= at FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, extract(hour FROM at)::int, extract(day FROM at)::int, extract(dow FROM at)::int, extract(epoch FROM at)::bigint, extract(epoch FROM ts)::bigint FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE date_trunc('day', at) = on_day ORDER BY id`),
			difftest.Q(0, `SELECT id, date_trunc('month', at) = date_trunc('month', ts), date_trunc('day', on_day) = on_day::timestamptz FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE at >= '2024-05-20' AND at < '2024-05-21' ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE at >= on_day AND at < on_day + interval '1 day' ORDER BY id`),
			difftest.Q(0, `SELECT id, at + interval '1 month' = (ts + interval '1 month')::timestamptz, (at + interval '1 day')::date = (on_day + interval '1 day')::date FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, COALESCE(ts, at) = at, greatest(ts, at) = at, CASE WHEN id > 0 THEN ts ELSE at END = at FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT CURRENT_DATE = now()::date, LOCALTIMESTAMP = now()::timestamp`),
			difftest.Q(0, `SELECT id, nullif(ts, at) IS NULL, nullif(at, ts) IS NULL FROM ev ORDER BY id`),
			difftest.S(0, `INSERT INTO ev VALUES (5, '2024-05-20 00:30', '2024-05-20 00:30+05', '2024-05-20 00:30')`),
			difftest.Q(0, `SELECT at = ts, at = '2024-05-19 15:30+00'::timestamptz, on_day = '2024-05-20'::date FROM ev WHERE id = 5`),
			difftest.S(0, `UPDATE ev SET ts = at, on_day = at WHERE id = 3`),
			difftest.Q(0, `SELECT ts = '2024-05-21 00:00'::timestamp, on_day = '2024-05-21'::date FROM ev WHERE id = 3`),
			difftest.S(0, `RESET TIME ZONE`),
			difftest.Q(0, `SELECT id, extract(hour FROM at)::int, at = ts, at::date = on_day FROM ev ORDER BY id`),
		},
	},
	{
		Name:   "a session in America/New_York across daylight saving changes",
		Schema: []string{`CREATE TABLE ev (id int PRIMARY KEY, at timestamptz NOT NULL)`},
		Seed: []string{`INSERT INTO ev VALUES
			(1, '2024-03-09 12:00+00'), (2, '2024-11-02 12:00+00'),
			(3, '2024-03-10 06:30+00'), (4, '2024-03-09 07:30+00'), (5, '2024-11-03 05:30+00')`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `SET TIME ZONE 'America/New_York'`),
			difftest.Q(0, `SELECT id, extract(epoch FROM at + interval '1 day')::bigint - extract(epoch FROM at)::bigint, extract(hour FROM at + interval '1 day')::int, extract(hour FROM at + interval '24 hours')::int FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, at + interval '1 day' - at, at + interval '1 month' - at FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, extract(epoch FROM date_trunc('day', at))::bigint, extract(hour FROM at)::int FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT '2024-02-10 02:30'::timestamptz + interval '1 month 1 day' = '2024-03-11 07:30+00'::timestamptz, '2024-02-10 02:30'::timestamptz + interval '1 month' + interval '1 day' = '2024-03-11 07:30+00'::timestamptz`),
			difftest.Q(0, `SELECT '2024-03-10 02:30'::timestamptz = '2024-03-10 07:30+00'::timestamptz, '2024-11-03 01:30'::timestamptz = '2024-11-03 06:30+00'::timestamptz, timestamp '2024-03-10 02:30' = '2024-03-10 07:30+00'::timestamptz, timestamp '2024-11-03 01:30' = '2024-11-03 06:30+00'::timestamptz`),
			// Down to the hour, date_trunc keeps the instant's own offset,
			// so the first and the second 01:30 of a fold stay apart.
			difftest.Q(0, `SELECT extract(epoch FROM date_trunc('minute', timestamptz '2024-11-03 05:30:30+00'))::bigint, extract(epoch FROM date_trunc('hour', timestamptz '2024-11-03 05:30:30+00'))::bigint, extract(epoch FROM date_trunc('hour', timestamptz '2024-11-03 06:30:30+00'))::bigint, extract(epoch FROM date_trunc('day', timestamptz '2024-11-03 06:30:30+00'))::bigint`),
			difftest.Q(0, `SELECT id, extract(epoch FROM date_trunc('microseconds', at))::bigint = extract(epoch FROM at)::bigint FROM ev ORDER BY id`),
			difftest.S(0, `SET TIME ZONE 'Australia/Sydney'`),
			difftest.Q(0, `SELECT '2024-10-06 02:30'::timestamptz = '2024-10-05 16:30+00'::timestamptz, '2024-04-07 02:30'::timestamptz = '2024-04-06 16:30+00'::timestamptz`),
			difftest.S(0, `SET TIME ZONE 9.5`),
			difftest.Q(0, `SELECT id, extract(hour FROM at)::int, extract(minute FROM at)::int FROM ev ORDER BY id`),
			difftest.S(0, `SET TIME ZONE -3`),
			difftest.Q(0, `SELECT id, extract(hour FROM at)::int, at::date = (at - interval '3 hours')::date FROM ev ORDER BY id`),
		},
	},
	{
		// SET TIME ZONE is the session's: it holds for the connection's
		// later transactions once its own commits, SET LOCAL ends with the
		// transaction, and ROLLBACK, also to a savepoint, undoes either.
		Name:   "SET TIME ZONE across transactions and sessions",
		Schema: []string{`CREATE TABLE ev (id int PRIMARY KEY, at timestamptz NOT NULL)`},
		Seed:   []string{`INSERT INTO ev VALUES (1, '2024-05-19 15:30+00')`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, `SET TIME ZONE 'Asia/Tokyo'`),
			difftest.Q(0, `SELECT extract(hour FROM at)::int FROM ev`),
			difftest.Q(1, `SELECT extract(hour FROM at)::int FROM ev`),
			difftest.S(1, `BEGIN`),
			difftest.S(1, `SET LOCAL TIME ZONE 'Asia/Tokyo'`),
			difftest.Q(1, `SELECT extract(hour FROM at)::int FROM ev`),
			difftest.S(1, `COMMIT`),
			difftest.Q(1, `SELECT extract(hour FROM at)::int FROM ev`),
			difftest.S(1, `BEGIN`),
			difftest.S(1, `SET TIME ZONE 'Asia/Tokyo'`),
			difftest.S(1, `ROLLBACK`),
			difftest.Q(1, `SELECT extract(hour FROM at)::int FROM ev`),
			difftest.S(1, `BEGIN`),
			difftest.S(1, `SAVEPOINT a`),
			difftest.S(1, `SET TIME ZONE 'Asia/Tokyo'`),
			difftest.Q(1, `SELECT extract(hour FROM at)::int FROM ev`),
			difftest.S(1, `ROLLBACK TO SAVEPOINT a`),
			difftest.Q(1, `SELECT extract(hour FROM at)::int FROM ev`),
			difftest.S(1, `COMMIT`),
			difftest.S(1, `BEGIN`),
			difftest.S(1, `SET TIME ZONE 'Asia/Tokyo'`),
			difftest.S(1, `SET LOCAL TIME ZONE 'America/New_York'`),
			difftest.Q(1, `SELECT extract(hour FROM at)::int FROM ev`),
			difftest.S(1, `COMMIT`),
			difftest.Q(1, `SELECT extract(hour FROM at)::int FROM ev`),
			difftest.S(0, `RESET ALL`),
			difftest.Q(0, `SELECT extract(hour FROM at)::int FROM ev`),
			difftest.S(0, `SET TIME ZONE 'Asia/Tokyo'`),
			difftest.S(0, `SET TIME ZONE LOCAL`),
			difftest.Q(0, `SELECT extract(hour FROM at)::int FROM ev`),
		},
	},
}

func TestDiffTimeZone(t *testing.T) {
	for _, c := range tzCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
