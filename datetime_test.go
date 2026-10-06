package detest

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/k1LoW/detest/postgres"
)

func iv(months, days int64, d time.Duration) pgInterval {
	return pgInterval{months, days, d.Microseconds()}
}

func TestParseInterval(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want pgInterval
	}{
		{"1 day", iv(0, 1, 0)},
		{"2 days 3 hours", iv(0, 2, 3*time.Hour)},
		{"1.5 days", iv(0, 1, 12*time.Hour)},
		{"1 week", iv(0, 7, 0)},
		{"1.5 weeks", iv(0, 10, 12*time.Hour)},
		{"90 minutes", iv(0, 0, 90*time.Minute)},
		{"10min", iv(0, 0, 10*time.Minute)},
		{"1h", iv(0, 0, time.Hour)},
		{"2 hrs", iv(0, 0, 2*time.Hour)},
		{"3 mins 2 secs", iv(0, 0, 3*time.Minute+2*time.Second)},
		{"30", iv(0, 0, 30*time.Second)},
		{"1 hour 30", iv(0, 0, time.Hour+30*time.Second)},
		{"1:30", iv(0, 0, 90*time.Minute)},
		{"1 day 2:03:04.5", iv(0, 1, 2*time.Hour+3*time.Minute+4500*time.Millisecond)},
		{"-1 day", iv(0, -1, 0)},
		{"1 day ago", iv(0, -1, 0)},
		{"@ 1 day", iv(0, 1, 0)},
		{"1 day -2 hours", iv(0, 1, -2*time.Hour)},
		{"+3 hours", iv(0, 0, 3*time.Hour)},
		{"1.5 hours", iv(0, 0, 90*time.Minute)},
		{"0.000001 seconds", iv(0, 0, time.Microsecond)},
		{"1 D", iv(0, 1, 0)},
		{"+ 1 day", iv(0, 1, 0)},
		{"- 2 hours", iv(0, 0, -2*time.Hour)},
		{". seconds", iv(0, 0, 0)},
		{"1:2.5", iv(0, 0, time.Minute+2500*time.Millisecond)},
		{"1 week 2:00", iv(0, 7, 2*time.Hour)},
		{"1. seconds", iv(0, 0, time.Second)},
		{"1 month", iv(1, 0, 0)},
		{"2 years", iv(24, 0, 0)},
		{"1 decade", iv(120, 0, 0)},
		{"1 year 2 months 3 days 04:05:06.5", iv(14, 3, 4*time.Hour+5*time.Minute+6500*time.Millisecond)},
		{"1.5 months", iv(1, 15, 0)},
		{"1.25 months", iv(1, 7, 12*time.Hour)},
		{"1.5 years", iv(18, 0, 0)},
		{"1 week 2 days", iv(0, 9, 0)},
		{"-1 year 2 mons", iv(-10, 0, 0)},
		// Each unit's fraction is carried down before the next is added.
		{"1 year -0.5 months", iv(12, -15, 0)},
		{"1 month -0.5 days", iv(1, 0, -12*time.Hour)},
		{"1.5 months -0.5 days", iv(1, 15, -12*time.Hour)},
	} {
		got, err := parseInterval(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("%q: got %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{"1 day 1 day", "1 2", "", "nonsense", "pizza", "1 dayz", "--1:00", "day", "1.2.3 seconds", "1.2.3", "1e3 seconds", "1.5e2 min", "- seconds", "1 hour 2:00", "2:00 1 minute", "1:00 2:00", "1:2:3:4", "1.5:00", "1 quarter", "1 month 1 mon"} {
		if _, err := parseInterval(in); err == nil || !err.malformed {
			t.Errorf("%q: got %v, want malformed", in, err)
		}
	}
	for _, in := range []string{"1.3 years", "P1D", "PT1H", "infinity", "1.0000005 seconds"} {
		if _, err := parseInterval(in); err == nil || err.malformed {
			t.Errorf("%q: got %v, want unread", in, err)
		}
	}
}

func datetimeDB(t *testing.T) *Sim {
	t.Helper()
	return newSim(t)
}

func TestDatetimeFunctions(t *testing.T) {
	s := datetimeDB(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE ev (id int PRIMARY KEY, at timestamptz, on_day date)`)
	mustExec(t, db, `INSERT INTO ev VALUES
		(1, '2024-05-15 13:45:12.345678+00', '2024-05-15'),
		(2, '2024-05-19 00:00:00+00', $1),
		(3, $2, '2024-05-20')`,
		time.Date(2024, 5, 19, 23, 30, 0, 0, time.FixedZone("x", 9*3600)), time.Date(2024, 5, 20, 1, 0, 0, 0, time.UTC))

	ints := func(q string, args ...any) []int64 { return queryIDs(t, db, q, args...) }
	eq := func(got, want []int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	for _, tt := range []struct {
		q    string
		want []int64
	}{
		{`SELECT id FROM ev WHERE at >= $1::timestamptz - interval '1 day' ORDER BY id`, []int64{2, 3}},
		{`SELECT id FROM ev WHERE at < $1::timestamptz - interval '4 days 10:14' ORDER BY id`, []int64{1}},
		{`SELECT id FROM ev WHERE on_day = '2024-05-19'::date ORDER BY id`, []int64{2}},
		{`SELECT id FROM ev WHERE at::date = on_day ORDER BY id`, []int64{1, 2, 3}},
		{`SELECT id FROM ev WHERE at >= date '2024-05-19' ORDER BY id`, []int64{2, 3}},
		{`SELECT id FROM ev WHERE date_trunc('day', at) = on_day ORDER BY id`, []int64{1, 2, 3}},
		{`SELECT id FROM ev WHERE date_trunc('week', at) = date '2024-05-13' ORDER BY id`, []int64{1, 2}},
		{`SELECT extract(day FROM at) FROM ev ORDER BY id`, []int64{15, 19, 20}},
		{`SELECT extract(dow FROM on_day) FROM ev ORDER BY id`, []int64{3, 0, 1}},
		{`SELECT extract(epoch FROM at - on_day)::bigint FROM ev ORDER BY id`, []int64{49512, 0, 3600}},
		{`SELECT date_part('hour', at)::int FROM ev ORDER BY id`, []int64{13, 0, 1}},
		{`SELECT count(*) FROM ev WHERE CURRENT_DATE = now()::date`, []int64{3}},
		// A time bound for a date is the date it holds, not its date in UTC.
		{`SELECT id FROM ev WHERE on_day = $1::date ORDER BY id`, []int64{3}},
	} {
		var args []any
		switch {
		case strings.Contains(tt.q, "$1::date"):
			args = []any{time.Date(2024, 5, 20, 0, 30, 0, 0, time.FixedZone("JST", 9*3600))}
		case strings.Contains(tt.q, "$1"):
			args = []any{time.Date(2024, 5, 20, 0, 0, 0, 0, time.UTC)}
		}
		if got := ints(tt.q, args...); !eq(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
	var ms, sec float64
	if err := db.QueryRow(`SELECT extract(milliseconds FROM at), extract(second FROM at) FROM ev WHERE id = 1`).Scan(&ms, &sec); err != nil || ms != 12345.678 || sec != 12.345678 {
		t.Errorf("extract of a fraction: got %v, %v, %v", ms, sec, err)
	}
}

// A time bound for a date or a timestamp is the value the driver sends once
// Postgres has typed the parameter: pgx takes the time's own date and clock
// (discardTimeZone), whatever its zone. A timestamptz keeps the instant.
func TestTimeParametersTypedAsDateOrTimestamp(t *testing.T) {
	s := datetimeDB(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE ev (id int PRIMARY KEY, ts timestamp, tz timestamptz, on_day date)`)
	jst := time.FixedZone("JST", 9*3600)
	at := time.Date(2024, 5, 20, 0, 30, 0, 0, jst) // 2024-05-19 15:30 UTC
	mustExec(t, db, `INSERT INTO ev VALUES (1, $1, $1, $1)`, at)
	for _, tt := range []struct {
		q    string
		want []int64
	}{
		{`SELECT extract(day FROM ts)::int FROM ev`, []int64{20}},
		{`SELECT extract(hour FROM ts)::int FROM ev`, []int64{0}},
		{`SELECT extract(day FROM tz)::int FROM ev`, []int64{19}},
		{`SELECT extract(day FROM on_day)::int FROM ev`, []int64{20}},
		{`SELECT id FROM ev WHERE ts = $1`, []int64{1}},
		{`SELECT id FROM ev WHERE $1 = ts`, []int64{1}},
		{`SELECT id FROM ev WHERE tz = $1`, []int64{1}},
		{`SELECT id FROM ev WHERE on_day = $1`, []int64{1}},
		{`SELECT id FROM ev WHERE on_day IN ($1)`, []int64{1}},
		{`SELECT id FROM ev WHERE ts = $1::timestamp`, []int64{1}},
		{`SELECT id FROM ev WHERE date_trunc('day', ts) = $1::date`, []int64{1}},
		// The arguments of COALESCE take the type of the typed one.
		{`SELECT id FROM ev WHERE COALESCE($1, on_day) = on_day`, []int64{1}},
		{`SELECT id FROM ev WHERE CASE WHEN true THEN $1 ELSE ts END = ts`, []int64{1}},
		// A date and a timestamp together are a timestamp, so $1 keeps its clock.
		{`SELECT id FROM ev WHERE COALESCE(on_day, ts) <> $1 OR CASE WHEN false THEN on_day ELSE ts END = $1`, []int64{1}},
		{`SELECT extract(hour FROM COALESCE(on_day, ts))::int FROM ev`, []int64{0}},
		// A date moved by an interval is a timestamp, so $1 keeps its clock.
		{`SELECT id FROM ev WHERE on_day + interval '30 minutes' = $1`, []int64{1}},
		{`SELECT id FROM ev WHERE interval '30 minutes' + on_day = $1`, []int64{1}},
	} {
		if got := queryIDs(t, db, tt.q, at); !slices.Equal(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
}

// A timestamptz is kept in UTC, so writing one to a date or a timestamp
// takes its date and clock there, as Postgres does in a session in UTC,
// whatever the zone of the time it came from.
func TestTimestamptzWrittenToDateOrTimestamp(t *testing.T) {
	s := datetimeDB(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE ev (id int PRIMARY KEY, ts timestamp, on_day date)`)
	at := time.Date(2024, 5, 20, 0, 30, 0, 0, time.FixedZone("JST", 9*3600)) // 2024-05-19 15:30 UTC
	mustExec(t, db, `INSERT INTO ev VALUES (1, $1::timestamptz, $1::timestamptz)`, at)
	mustExec(t, db, `INSERT INTO ev VALUES (2, now(), now())`)
	for _, tt := range []struct {
		q    string
		want []int64
	}{
		{`SELECT extract(day FROM on_day)::int FROM ev WHERE id = 1`, []int64{19}},
		{`SELECT extract(hour FROM ts)::int FROM ev WHERE id = 1`, []int64{15}},
		{`SELECT id FROM ev WHERE on_day = CURRENT_DATE`, []int64{2}},
		{`SELECT id FROM ev WHERE ts::date = on_day ORDER BY id`, []int64{1, 2}},
	} {
		if got := queryIDs(t, db, tt.q); !slices.Equal(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
	// The cast of a literal or a parameter is read before any row.
	for _, tt := range []struct {
		q    string
		args []any
	}{
		{`SELECT id FROM ev WHERE false AND 'nonsense'::interval > interval '0'`, nil},
		{`SELECT id FROM ev WHERE id = 9 AND $1::interval > interval '0'`, []any{"1 dayz"}},
	} {
		if _, err := db.Exec(tt.q, tt.args...); !errors.Is(err, ErrInvalidDatetimeFormat) {
			t.Errorf("%s: got %v, want 22007", tt.q, err)
		}
	}
}

func TestDatetimeErrorsAndRefusals(t *testing.T) {
	s := datetimeDB(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE ev (id int PRIMARY KEY, at timestamptz, on_day date)`)
	mustExec(t, db, `INSERT INTO ev VALUES (1, '2024-05-15 13:45:12+00', '2024-05-15')`)
	for _, tt := range []struct {
		q    string
		want error
	}{
		{`SELECT interval '1 day 1 day'`, ErrInvalidDatetimeFormat},
		{`SELECT $1::interval`, ErrInvalidDatetimeFormat},
		{`SELECT date_trunc('fortnight', at) FROM ev`, ErrInvalidParameterValue},
		{`SELECT extract(fortnight FROM at) FROM ev`, ErrInvalidParameterValue},
		{`SELECT extract(fortnight FROM on_day) FROM ev`, ErrInvalidParameterValue},
		{`SELECT extract(fortnight FROM interval '1 day')`, ErrInvalidParameterValue},
		{`SELECT date_trunc('fortnight', interval '1 day')`, ErrInvalidParameterValue},
		// mm is extract's minute, and no unit of date_trunc's.
		{`SELECT date_trunc('mm', at) FROM ev`, ErrInvalidParameterValue},
		{`SELECT interval 'nonsense'`, ErrInvalidDatetimeFormat},
	} {
		var args []any
		if tt.q == `SELECT $1::interval` {
			args = []any{"1 2"}
		}
		if _, err := db.Exec(tt.q, args...); !errors.Is(err, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, err, tt.want)
		}
	}
	for _, q := range []string{
		`SELECT interval '1.3 years'`,
		`SELECT interval 'P1D'`,
		`SELECT on_day - on_day FROM ev`,
		`SELECT CURRENT_DATE - CURRENT_DATE`,
		`SELECT CURRENT_DATE - 1`,
		`SELECT extract(hour FROM on_day) FROM ev`,
		// An interval has no calendar position.
		`SELECT extract(dow FROM interval '27 hours')`,
		`SELECT date_trunc('week', interval '1 day')`,
		`SELECT extract(julian FROM at) FROM ev`,
		`SELECT extract(timezone FROM at) FROM ev`,
		`SELECT date_trunc('day', at, 'Asia/Tokyo') FROM ev`,
		// Postgres cannot pick the overload for a source of no type (42725).
		`SELECT date_trunc('day', $1)`,
		`SELECT extract(year FROM $1)`,
		`SELECT date_part('year', $1)`,
		`SELECT date_trunc('day', '2024-05-20')`,
		// A parameter or a literal beside a date is a date, and the
		// difference of two dates is days.
		`SELECT $1 - on_day FROM ev`,
		`SELECT on_day - '2024-05-01' FROM ev`,
		// A parameter given two date and time types.
		`SELECT at = $1::timestamptz FROM ev WHERE on_day = $1`,
		`SELECT extract(j FROM at) FROM ev`,
		`SET TIME ZONE 'Asia/Tokyo'`,
		`SET timezone = 'America/New_York'`,
		`SELECT CURRENT_TIME`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	for _, q := range []string{`SET TIME ZONE 'UTC'`, `SET TIME ZONE 'Etc/UTC'`, `SET TIME ZONE LOCAL`, `SET TIME ZONE DEFAULT`, `RESET timezone`, `SET TIME ZONE 0`, `SET LOCAL timezone = 'GMT'`} {
		if err := CheckSQL(postgres.New(), q); err != nil {
			t.Errorf("%s: got %v, want nil", q, err)
		}
	}
}

// Each expected value is what Postgres 18 returns for the query.
func TestIntervals(t *testing.T) {
	s := datetimeDB(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE plan (id int PRIMARY KEY, every interval NOT NULL)`)
	mustExec(t, db, `INSERT INTO plan VALUES (1, '1 month'), (2, '30 days'), (3, $1), (4, '1 year')`, "720:00:00")

	for _, tt := range []struct{ q, want string }{
		{`SELECT (interval '1 month' = interval '30 days')::text`, "true"},
		{`SELECT (interval '1 day' = interval '24 hours')::text`, "true"},
		{`SELECT (interval '1 year' > interval '360 days')::text`, "false"},
		{`SELECT (interval '1 year' < interval '366 days')::text`, "true"},
		{`SELECT interval '1 year 2 months 3 days 04:05:06.5'`, "1 year 2 mons 3 days 04:05:06.5"},
		{`SELECT interval '-1 day 2 hours'`, "-1 days +02:00:00"},
		{`SELECT interval '1.5 months'`, "1 mon 15 days"},
		{`SELECT interval '27 hours'`, "27:00:00"},
		{`SELECT interval '-1 year -2 mons'`, "-1 years -2 mons"},
		{`SELECT interval '0'`, "00:00:00"},
		{`SELECT interval '-00:00:01.25'`, "-00:00:01.25"},
		{`SELECT ((timestamp '2024-01-31' + interval '1 month') = timestamp '2024-02-29')::text`, "true"},
		{`SELECT ((timestamp '2024-03-31' - interval '1 month') = timestamp '2024-02-29')::text`, "true"},
		{`SELECT ((timestamp '2024-02-29' + interval '1 year') = timestamp '2025-02-28')::text`, "true"},
		{`SELECT timestamp '2024-05-20 10:00' - timestamp '2024-05-18 08:30'`, "2 days 01:30:00"},
		{`SELECT timestamp '2024-05-18' - timestamp '2024-05-20 10:00'`, "-2 days -10:00:00"},
		{`SELECT interval '1 month' * 1.5`, "1 mon 15 days"},
		{`SELECT interval '1 day' * 0.5`, "12:00:00"},
		{`SELECT 2 * interval '1 hour 30 minutes'`, "03:00:00"},
		{`SELECT interval '1 year' + interval '3 days' - interval '1 hour'`, "1 year 3 days -01:00:00"},
		{`SELECT -interval '1 month 1 day'`, "-1 mons -1 days"},
		{`SELECT extract(day FROM interval '27 hours')::text`, "0"},
		{`SELECT extract(hour FROM interval '27 hours')::text`, "27"},
		{`SELECT extract(week FROM interval '15 days')::text`, "2"},
		{`SELECT extract(month FROM interval '14 months')::text`, "2"},
		{`SELECT extract(year FROM interval '14 months')::text`, "1"},
		{`SELECT extract(quarter FROM interval '7 months')::text`, "3"},
		{`SELECT extract(epoch FROM interval '1 month 1 day')::bigint::text`, "2678400"},
		{`SELECT extract(epoch FROM interval '1 year')::bigint::text`, "31557600"},
		{`SELECT date_trunc('day', interval '3 days 04:05')`, "3 days"},
		{`SELECT date_trunc('year', interval '14 months 3 days')`, "1 year"},
		{`SELECT date_trunc('hour', interval '27 hours 30 minutes')`, "27:00:00"},
		{`SELECT date_trunc('quarter', interval '7 months 3 days')`, "6 mons"},
		{`SELECT date_trunc('decade', interval '25 years')`, "20 years"},
		// A column compares by the span, as the index of an interval does.
		{`SELECT count(*)::text FROM plan WHERE every = interval '720 hours'`, "3"},
		{`SELECT every FROM plan WHERE id = 3`, "720:00:00"},
	} {
		var got string
		if err := db.QueryRow(tt.q).Scan(&got); err != nil {
			t.Errorf("%s: %v", tt.q, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.q, got, tt.want)
		}
	}
	if got := queryIDs(t, db, `SELECT id FROM plan WHERE every < interval '1 year' ORDER BY id`); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("every < 1 year: got %v", got)
	}
	if got := queryIDs(t, db, `SELECT id FROM plan WHERE every <= interval '1 year' ORDER BY every, id`); !slices.Equal(got, []int64{1, 2, 3, 4}) {
		t.Errorf("every <= 1 year: got %v", got)
	}

	// The unique key of an interval is its span too.
	mustExec(t, db, `CREATE TABLE once (every interval PRIMARY KEY)`)
	mustExec(t, db, `INSERT INTO once VALUES ('1 day')`)
	if _, err := db.Exec(`INSERT INTO once VALUES ('24 hours')`); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("a duplicate span: got %v, want ErrUniqueViolation", err)
	}
	if _, err := db.Exec(`INSERT INTO plan VALUES (5, 'nonsense')`); !errors.Is(err, ErrInvalidDatetimeFormat) {
		t.Errorf("malformed interval text: got %v, want ErrInvalidDatetimeFormat", err)
	}
	if _, err := db.Exec(`INSERT INTO plan VALUES (6, $1)`, time.Hour); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a time.Duration: got %v, want ErrUnsupportedSQL", err)
	}
}
