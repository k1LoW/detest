package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

// The date and time cases compare interval text, date columns, CURRENT_DATE,
// date_trunc and extract with Postgres, whose TimeZone is UTC as detest
// takes it. Steps take no parameters and detest does not cast text to a
// timestamp, so the times are column values; a numeric result is compared
// or cast to an integer, as the server renders it as text. now() and
// CURRENT_DATE read the fake clock of the bubble under detest, so they are
// compared with each other only.
var (
	evSchema = []string{`CREATE TABLE ev (id int PRIMARY KEY, at timestamptz NOT NULL, on_day date NOT NULL)`}
	evSeed   = []string{`INSERT INTO ev VALUES
		(1, '2024-05-15 13:45:12.345678+00', '2024-05-15'),
		(2, '2024-05-19 00:00:00+00', '2024-05-19'),
		(3, '2024-05-20 01:00:00+00', '2024-05-20'),
		(4, '2021-01-03 23:59:59.999999+00', '2021-01-03'),
		(5, '2000-12-31 12:00:00+00', '2000-12-31')`}
)

var datetimeCases = []difftest.Case{
	{
		Name:   "interval text",
		Schema: evSchema, Seed: evSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT interval '1.5 days' = interval '36 hours', interval '90 min' = interval '1:30', interval '1 week' = interval '7 days', interval '1 hour 30' = interval '3630 seconds'`),
			difftest.Q(0, `SELECT interval '1 day ago' = interval '-1 day', interval '@ 2 hours' = interval '2h', interval '1 day -2 hours' = interval '22 hours', interval '10min' = interval '600'`),
			difftest.Q(0, `SELECT interval '1 day 2:03:04.5' = interval '93784.5 seconds', interval '0.000001 seconds' > interval '0', interval '2 hrs 3 mins 4 secs' = interval '7384 s'`),
			difftest.Q(0, `SELECT id FROM ev WHERE at >= (SELECT max(at) FROM ev) - interval '1 day 1 hour' ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE at + interval '4 days 10:14' < (SELECT max(at) FROM ev) ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE at - interval '1 week' > (SELECT min(at) FROM ev) ORDER BY id`),
			difftest.Q(0, `SELECT interval '1 day 1 day'`),
			difftest.Q(0, `SELECT '1 2'::interval`),
			difftest.Q(0, `SELECT ''::interval`),
			difftest.Q(0, `SELECT interval 'nonsense'`),
			difftest.Q(0, `SELECT interval '1 dayz'`),
			difftest.Q(0, `SELECT interval '--1:00'`),
			difftest.Q(0, `SELECT interval '1.2.3 seconds'`),
			difftest.Q(0, `SELECT interval '1e3 seconds'`),
			difftest.Q(0, `SELECT interval '+ 1 day' = interval '1 day', interval '- 2 hours' = interval '-2 hours', interval '. seconds' = interval '0', interval '1. seconds' = interval '1 second'`),
		},
	},
	{
		Name:   "date columns, casts to date and CURRENT_DATE",
		Schema: evSchema, Seed: evSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM ev WHERE at::date = on_day ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE on_day = date '2024-05-19' ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE on_day = '2024-05-20 10:00'::date ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE at >= date '2024-05-19' ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE on_day + interval '1 day' > at ORDER BY id`),
			difftest.Q(0, `SELECT CURRENT_DATE = now()::date, CURRENT_DATE <= now(), CURRENT_DATE + interval '1 day' > now()`),
			difftest.S(0, `UPDATE ev SET on_day = '2024-06-01' WHERE id = 1`),
			difftest.Q(0, `SELECT id FROM ev WHERE on_day > date '2024-05-31' ORDER BY id`),
		},
	},
	{
		Name:   "date_trunc",
		Schema: evSchema, Seed: evSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id FROM ev WHERE date_trunc('day', at) = on_day ORDER BY id`),
			difftest.Q(0, `SELECT id, date_trunc('week', at) = date '2024-05-13', date_trunc('month', at) = date '2024-05-01', date_trunc('quarter', at) = date '2024-04-01', date_trunc('year', at) = date '2024-01-01' FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, date_trunc('decade', at) = date '2020-01-01', date_trunc('century', at) = date '2001-01-01', date_trunc('millennium', at) = date '2001-01-01' FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, date_trunc('hour', at) < at, date_trunc('second', at) = date_trunc('milliseconds', at), date_trunc('microseconds', at) = at FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, date_trunc('days', at) = date_trunc('d', at), date_trunc('Mon', at) = date_trunc('month', at), date_trunc('y', at) = date_trunc('YEAR', at) FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, date_trunc('week', on_day) = date_trunc('week', at) FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT date_trunc('fortnight', at) FROM ev`),
			difftest.Q(0, `SELECT count(*) FROM ev WHERE date_trunc(NULL, at) IS NULL`),
		},
	},
	{
		Name:   "extract and date_part",
		Schema: evSchema, Seed: evSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id, extract(year FROM at)::int, extract(month FROM at)::int, extract(day FROM at)::int, extract(hour FROM at)::int, extract(minute FROM at)::int FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, extract(dow FROM at)::int, extract(isodow FROM at)::int, extract(doy FROM at)::int, extract(week FROM at)::int, extract(isoyear FROM at)::int, extract(quarter FROM at)::int FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, extract(decade FROM at)::int, extract(century FROM at)::int, extract(millennium FROM at)::int FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, extract(microseconds FROM at)::bigint, extract(second FROM at) = 12.345678, extract(milliseconds FROM at) = 12345.678 FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, extract(epoch FROM at)::bigint, extract(epoch FROM at - on_day)::bigint, extract(epoch FROM on_day)::bigint FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, extract(day FROM on_day)::int, extract(dow FROM on_day)::int, extract(epoch FROM interval '1 day 2 hours')::int FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id, date_part('hour', at)::int, date_part('days', at)::int, date_part('epoch', on_day)::bigint, date_part('hour', on_day)::int FROM ev ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE extract(epoch FROM (SELECT max(at) FROM ev) - at) < 86400 * 7 ORDER BY id`),
			difftest.Q(0, `SELECT id FROM ev WHERE extract(year FROM at) = 2024 AND extract(dow FROM at) IN (0, 6) ORDER BY id`),
			difftest.Q(0, `SELECT extract(fortnight FROM at) FROM ev`),
			difftest.Q(0, `SELECT extract(fortnight FROM on_day) FROM ev`),
			difftest.Q(0, `SELECT extract(fortnight FROM interval '1 day')`),
			difftest.Q(0, `SELECT date_trunc('fortnight', interval '1 day')`),
			difftest.Q(0, `SELECT count(*) FROM ev WHERE extract(year FROM NULL::timestamptz) IS NULL`),
		},
	},
	{
		Name:   "SET TIME ZONE to UTC",
		Schema: evSchema, Seed: evSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `SET TIME ZONE 'UTC'`),
			difftest.Q(0, `SELECT id, extract(hour FROM at)::int, date_trunc('day', at) = on_day FROM ev ORDER BY id`),
			difftest.S(0, `SET TIME ZONE 'Etc/UTC'`),
			difftest.S(0, `SET TIME ZONE LOCAL`),
			difftest.Q(0, `SELECT id, at::date = on_day FROM ev ORDER BY id`),
		},
	},
	{
		// The recheck after the wait computes the interval against the row's
		// new version.
		Name:   "an update over an interval waits and rechecks",
		Schema: evSchema, Seed: evSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE ev SET at = at + interval '10 days' WHERE id = 1`),
			difftest.S(1, `UPDATE ev SET on_day = on_day + interval '1 day' WHERE at < (SELECT max(at) FROM ev) - interval '2 days'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id, on_day = at::date FROM ev ORDER BY id`),
		},
	},
}

func TestDiffDatetime(t *testing.T) {
	for _, c := range datetimeCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
