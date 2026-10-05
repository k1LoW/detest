package detest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/k1LoW/detest/postgres"
)

func TestParseInterval(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want time.Duration
	}{
		{"1 day", 24 * time.Hour},
		{"2 days 3 hours", 51 * time.Hour},
		{"1.5 days", 36 * time.Hour},
		{"1 week", 7 * 24 * time.Hour},
		{"1.5 weeks", 252 * time.Hour},
		{"90 minutes", 90 * time.Minute},
		{"10min", 10 * time.Minute},
		{"1h", time.Hour},
		{"2 hrs", 2 * time.Hour},
		{"3 mins 2 secs", 3*time.Minute + 2*time.Second},
		{"30", 30 * time.Second},
		{"1 hour 30", time.Hour + 30*time.Second},
		{"1:30", 90 * time.Minute},
		{"1 day 2:03:04.5", 26*time.Hour + 3*time.Minute + 4500*time.Millisecond},
		{"-1 day", -24 * time.Hour},
		{"1 day ago", -24 * time.Hour},
		{"@ 1 day", 24 * time.Hour},
		{"1 day -2 hours", 22 * time.Hour},
		{"+3 hours", 3 * time.Hour},
		{"1.5 hours", 90 * time.Minute},
		{"0.000001 seconds", time.Microsecond},
		{"1 D", 24 * time.Hour},
	} {
		got, err := parseInterval(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("%q: got %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{"1 day 1 day", "1 2", ""} {
		if _, err := parseInterval(in); err == nil || !err.malformed {
			t.Errorf("%q: got %v, want malformed", in, err)
		}
	}
	for _, in := range []string{"1 month", "2 years", "1 decade", "P1D", "1.0000005 seconds", "1 dayz", "1e3 seconds", "1 hour 2:00"} {
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
	} {
		var args []any
		if strings.Contains(tt.q, "$1") {
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
		`SELECT id FROM ev WHERE at > now() - interval '1 month'`,
		`SELECT interval 'P1D'`,
		`SELECT on_day - on_day FROM ev`,
		`SELECT CURRENT_DATE - CURRENT_DATE`,
		`SELECT CURRENT_DATE - 1`,
		`SELECT extract(hour FROM on_day) FROM ev`,
		`SELECT extract(day FROM interval '27 hours')`,
		`SELECT date_trunc('day', interval '1 day')`,
		`SELECT extract(julian FROM at) FROM ev`,
		`SELECT extract(timezone FROM at) FROM ev`,
		`SELECT date_trunc('day', at, 'Asia/Tokyo') FROM ev`,
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
