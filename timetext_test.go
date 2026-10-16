package detest

import (
	"errors"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// Postgres writes a numeric with the scale it was computed with, so extract's
// text is 1.000000, and a time of day with its seconds, 10:00:00. detest
// keeps the numeric as a float and the time as the text it was written in,
// so forms that would show either are refused.
func TestExtractAndTimeOfDayText(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE shift (id int PRIMARY KEY, starts time, ends timetz, opens time DEFAULT '09:00')`)
	mustExec(t, db, `CREATE TABLE plain (id int PRIMARY KEY, starts time)`)
	mustExec(t, db, `INSERT INTO plain VALUES (1, NULL)`)
	mustExec(t, db, `INSERT INTO plain (id) VALUES (2)`)
	// A cast of extract to text keeps the scale of its unit.
	for q, want := range map[string]string{
		`SELECT EXTRACT(SECOND FROM interval '1 second')::text`:                  "1.000000",
		`SELECT EXTRACT(SECOND FROM timestamp '2020-01-02 10:00:01.5')::text`:    "1.500000",
		`SELECT EXTRACT(MILLISECONDS FROM interval '1 second')::text`:            "1000.000",
		`SELECT EXTRACT(MICROSECONDS FROM interval '1 second')::text`:            "1000000",
		`SELECT EXTRACT(HOUR FROM timestamp '2020-01-02 10:00:01')::text`:        "10",
		`SELECT EXTRACT(DAY FROM date '2020-01-02')::text`:                       "2",
		`SELECT EXTRACT(SECOND FROM interval '1 second')::varchar(3)`:            "1.0",
		`SELECT EXTRACT(SECOND FROM timestamptz '2020-01-02 10:00:01+00')::text`: "1.000000",
	} {
		var got string
		if err := db.QueryRow(q).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q, %v, want %q", q, got, err, want)
		}
	}
	for _, q := range []string{
		// epoch has no scale from a date and six places from a timestamp.
		`SELECT EXTRACT(EPOCH FROM date '2020-01-02')::text`,
		`SELECT EXTRACT(EPOCH FROM timestamp '2020-01-02')::text`,
		`SELECT (EXTRACT(SECOND FROM interval '1 second') + 1)::text`,
		`SELECT x.s::text FROM (SELECT EXTRACT(SECOND FROM interval '1 second') AS s) x`,
		`SELECT ROW(EXTRACT(SECOND FROM interval '1 second'))::text`,
		`SELECT ROW(EXTRACT(SECOND FROM interval '1 second'))`,
		`SELECT concat(EXTRACT(SECOND FROM interval '1 second'))`,
		`SELECT concat('a', 1.0)`,
		`SELECT '10:00'::time::text`,
		`SELECT '10:00'::time`,
		`SELECT time '10:00'`,
		`SELECT '10:00+02'::timetz`,
		`SELECT now()::time`,
		`SELECT id FROM plain WHERE starts < '9:00'::time`,
		`INSERT INTO shift (id, starts) VALUES (1, '10:00')`,
		`INSERT INTO shift (id, ends) VALUES (2, '10:00+02')`,
		`INSERT INTO shift (id) VALUES (3)`,
		`UPDATE plain SET starts = '10:00' WHERE id = 1`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// extract's numeric divides as a numeric, not as an integer.
	var half, whole bool
	if err := db.QueryRow(`SELECT EXTRACT(DAY FROM date '2024-05-15') / 2 = 7.5, EXTRACT(DAY FROM date '2024-05-15') / 4 * 4 = 15`).Scan(&half, &whole); err != nil || !half || !whole {
		t.Errorf("extract divided: got %v, %v, %v", half, whole, err)
	}
}
