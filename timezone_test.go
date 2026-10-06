package detest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
	"github.com/k1LoW/detest/postgres"
)

// A server in Asia/Tokyo reads a timestamptz's date, fields and days there,
// and converts a timestamp, a date and text without a zone to a timestamptz
// there, as SET TIME ZONE does for a session (internal/difftest compares
// that with Postgres).
func TestServerTimeZone(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.TimeZone("Asia/Tokyo")))
	mustExec(t, db, `CREATE TABLE ev (id int PRIMARY KEY, at timestamptz, ts timestamp, on_day date)`)
	jst := time.FixedZone("JST", 9*3600)
	at := time.Date(2024, 5, 20, 0, 30, 0, 0, jst) // 2024-05-19 15:30 UTC
	mustExec(t, db, `INSERT INTO ev VALUES (1, '2024-05-19 15:30+00', '2024-05-20 00:30', '2024-05-20')`)
	mustExec(t, db, `INSERT INTO ev VALUES (2, '2024-05-20 00:30', '2024-05-20 00:30', '2024-05-20')`)
	mustExec(t, db, `INSERT INTO ev VALUES (3, $1, $2, $3)`, at, at, at)
	mustExec(t, db, `INSERT INTO ev VALUES (4, $1, $2, $3)`, at.UTC(), at.UTC(), at.UTC())
	for _, tt := range []struct {
		q    string
		args []any
		want []int64
	}{
		{`SELECT id FROM ev WHERE at = '2024-05-19 15:30+00'::timestamptz ORDER BY id`, nil, []int64{1, 2, 3, 4}},
		{`SELECT extract(hour FROM at)::int FROM ev ORDER BY id`, nil, []int64{0, 0, 0, 0}},
		{`SELECT id FROM ev WHERE at::date = on_day ORDER BY id`, nil, []int64{1, 2, 3}},
		{`SELECT id FROM ev WHERE at = ts ORDER BY id`, nil, []int64{1, 2, 3}},
		{`SELECT id FROM ev WHERE date_trunc('day', at) = on_day ORDER BY id`, nil, []int64{1, 2, 3}},
		{`SELECT id FROM ev WHERE at >= '2024-05-20' ORDER BY id`, nil, []int64{1, 2, 3, 4}},
		{`SELECT id FROM ev WHERE at < '2024-05-20' ORDER BY id`, nil, nil},
		// A parameter compared with a timestamptz is the instant it holds,
		// in an array too, and one compared with a timestamp its own clock.
		{`SELECT id FROM ev WHERE at = $1 ORDER BY id`, []any{at}, []int64{1, 2, 3, 4}},
		{`SELECT id FROM ev WHERE at = ANY($1) ORDER BY id`, []any{[]time.Time{at.UTC()}}, []int64{1, 2, 3, 4}},
		{`SELECT id FROM ev WHERE ts = $1 ORDER BY id`, []any{at}, []int64{1, 2, 3}},
		{`SELECT id FROM ev WHERE ts = ANY($1) ORDER BY id`, []any{[]time.Time{at}}, []int64{1, 2, 3}},
		// NULLIF compares a timestamp with a timestamptz as = does.
		{`SELECT id FROM ev WHERE nullif(ts, at) IS NULL ORDER BY id`, nil, []int64{1, 2, 3}},
		{`SELECT count(*) FROM ev WHERE LOCALTIMESTAMP = now()::timestamp AND CURRENT_DATE = now()::date`, nil, []int64{4}},
	} {
		if got := queryIDs(t, db, tt.q, tt.args...); !slices.Equal(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
	// A timestamptz reaches the code under test as the instant, and a
	// timestamp as its clock.
	var tz, ts time.Time
	if err := db.QueryRow(`SELECT at, ts FROM ev WHERE id = 1`).Scan(&tz, &ts); err != nil {
		t.Fatal(err)
	}
	if !tz.Equal(at) || ts != time.Date(2024, 5, 20, 0, 30, 0, 0, time.UTC) {
		t.Errorf("got %v, %v", tz, ts)
	}
	// LAG and LEAD give their value and default one type, a timestamptz
	// here, whichever of the two a row returns.
	for _, q := range []string{
		`SELECT lag(ts, 0, at) OVER (ORDER BY id) FROM ev WHERE id = 1`,
		`SELECT lead(ts, 1, at) OVER (ORDER BY id) FROM ev WHERE id = 1`,
	} {
		var got time.Time
		if err := db.QueryRow(q).Scan(&got); err != nil || !got.Equal(at) {
			t.Errorf("%s: got %v, %v, want %v", q, got, err, at)
		}
	}
	// A set operation does not convert a timestamp beside a timestamptz.
	if _, err := db.Exec(`SELECT ts FROM ev UNION SELECT at FROM ev`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("UNION of a timestamp and a timestamptz: got %v, want ErrUnsupportedSQL", err)
	}
	// SET TIME ZONE overrides the server's for the session, and LOCAL,
	// DEFAULT and RESET return to the server's, not to UTC.
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hour := func() int {
		var h int
		if err := conn.QueryRowContext(t.Context(), `SELECT extract(hour FROM at)::int FROM ev WHERE id = 1`).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, step := range []struct {
		set  string
		want int
	}{
		{`SET TIME ZONE 'UTC'`, 15},
		{`SET TIME ZONE LOCAL`, 0},
		{`SET TIME ZONE 'America/New_York'`, 11},
		{`RESET timezone`, 0},
		{`SET TIME ZONE 5.5`, 21},
		{`SET TIME ZONE DEFAULT`, 0},
	} {
		if _, err := conn.ExecContext(t.Context(), step.set); err != nil {
			t.Fatalf("%s: %v", step.set, err)
		}
		if got := hour(); got != step.want {
			t.Errorf("after %s: got hour %d, want %d", step.set, got, step.want)
		}
	}
}

// A zone the server is given that time.LoadLocation does not read, or that
// Postgres does not take as TimeZone, fails the declaration.
func TestServerTimeZoneRefused(t *testing.T) {
	for _, name := range []string{"Nowhere/City", "Local", "", "UTC+9", "+09:00", "JST-9"} {
		kind := sqlir.ImplOf(postgres.New(postgres.TimeZone(name)))
		if err := kind.Check(kind.Isolation()); err == nil {
			t.Errorf("%q: got nil, want an error", name)
		}
	}
	for _, name := range []string{"UTC", "utc", "Etc/UTC", "Asia/Tokyo", "America/New_York"} {
		kind := sqlir.ImplOf(postgres.New(postgres.TimeZone(name)))
		if err := kind.Check(kind.Isolation()); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
}

// SET TIME ZONE to a POSIX zone, whose offset Postgres takes with the sign
// reversed, to an abbreviation or an interval is refused rather than read
// another way.
func TestSetTimeZoneRefusals(t *testing.T) {
	for _, q := range []string{
		`SET TIME ZONE 'UTC+9'`,
		`SET TIME ZONE '+09:00'`,
		`SET TIME ZONE 'JST'`,
		`SET TIME ZONE 'Local'`,
		`SET TIME ZONE 'Nowhere/City'`,
		`SET TIME ZONE INTERVAL '+09:00' HOUR TO MINUTE`,
		`SET timezone FROM CURRENT`,
	} {
		if err := CheckSQL(postgres.New(), q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	for _, q := range []string{`SET TIME ZONE 'Asia/Tokyo'`, `SET timezone = 'America/New_York'`, `SET TIME ZONE 9`, `SET TIME ZONE -3.5`, `SET LOCAL TIME ZONE 'Europe/London'`, `SET TIME ZONE 'utc'`} {
		if err := CheckSQL(postgres.New(), q); err != nil {
			t.Errorf("%s: got %v, want nil", q, err)
		}
	}
}

// The transaction API and State read a timestamptz as a time.Time, and the
// transaction API writes one to a timestamptz column as the instant it is.
func TestTimestamptzThroughTheTransactionAPI(t *testing.T) {
	at := time.Date(2024, 5, 20, 0, 30, 0, 0, time.FixedZone("JST", 9*3600))
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New(postgres.TimeZone("Asia/Tokyo")))
		mustExec(t, db, `CREATE TABLE ev (id text PRIMARY KEY, at timestamptz, done bool NOT NULL DEFAULT false)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO ev (id, at) VALUES ('a', $1)`, at) })
		s.Manual("api", 1, func(p *Proc) error {
			return store.Tx(p, func(tx *Tx) error {
				r, ok := tx.Get("ev", "a")
				if v, isTime := r["at"].(time.Time); !ok || !isTime || !v.Equal(at) {
					t.Errorf("Get: got %#v", r["at"])
				}
				if n := len(tx.Select("ev", func(r Row) bool { v, _ := r["at"].(time.Time); return v.Equal(at) })); n != 1 {
					t.Errorf("Select: got %d rows", n)
				}
				if err := tx.Insert("ev", Row{"id": "b", "at": at.UTC(), "done": false}); err != nil {
					return err
				}
				if ok, err := tx.CAS("ev", "a", "at", at, at.Add(time.Hour)); !ok || err != nil {
					t.Errorf("CAS: %v, %v", ok, err)
				}
				return nil
			})
		})
		s.AtQuiescence(func(st *State) error {
			for _, r := range st.Rows(store, "ev") {
				v, _ := r.Get("at")
				if _, isTime := v.(time.Time); !isTime {
					t.Errorf("RowView.Get: got %#v", v)
				}
				if _, isTime := r.Clone()["at"].(time.Time); !isTime {
					t.Errorf("RowView.Clone: got %#v", r.Clone()["at"])
				}
			}
			return nil
		})
	})
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE ev (id int PRIMARY KEY, at timestamptz)`)
	mustExec(t, db, `INSERT INTO ev VALUES (1, $1)`, at)
	if got := queryIDs(t, db, `SELECT id FROM ev WHERE at = $1`, at); !slices.Equal(got, []int64{1}) {
		t.Errorf("got %v", got)
	}
}
