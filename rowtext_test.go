package detest

import (
	"errors"
	"testing"

	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/postgres"
)

// A row read back or written as text is Postgres's record output, with its
// fields quoted where the server quotes them.
func TestRowAsText(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE u (n int, s text, b bool, nm numeric, f float8, at timestamptz, d date, by bytea)`)
	mustExec(t, db, `INSERT INTO u VALUES (1, 'a b', true, 1.5, 1.5, '2026-01-01 00:00:00+00', '2026-01-01', '\x01')`)
	for q, want := range map[string]string{
		`SELECT ROW(1, 2)`:                                 `(1,2)`,
		`SELECT ROW(1, 2)::text`:                           `(1,2)`,
		`SELECT ROW(n, 2) FROM u`:                          `(1,2)`,
		`SELECT x.v FROM (SELECT ROW(1, 2) AS v) x`:        `(1,2)`,
		`SELECT ROW(n, s, NULL) FROM u`:                    `(1,"a b",)`,
		`SELECT ROW(NULL)`:                                 `()`,
		`SELECT ROW('')`:                                   `("")`,
		`SELECT ROW('"', '\', '(', ')', ',', 'a''b', '{')`: `("""","\\","(",")",",",a'b,{)`,
		`SELECT ROW(ROW(1, 'a b'), 2)`:                     `("(1,""a b"")",2)`,
		`SELECT ROW(b, NOT b) FROM u`:                      `(t,f)`,
		`SELECT ROW(1, 2)::varchar(3)`:                     `(1,`,
		`SELECT ROW(n, s) || 'x' FROM u`:                   `(1,"a b")x`,
		`SELECT concat(ROW(n, s), 'x') FROM u`:             `(1,"a b")x`,
	} {
		var got string
		if err := db.QueryRow(q).Scan(&got); err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %s, want %s", q, got, want)
		}
	}
	// The text of a numeric, a float, a time or a bytea is written by the
	// server's own rules, which a field written by Go's would not match.
	for _, q := range []string{
		`SELECT ROW(nm) FROM u`,
		`SELECT ROW(f) FROM u`,
		`SELECT ROW(n, at) FROM u`,
		`SELECT ROW(d) FROM u`,
		`SELECT ROW(by) FROM u`,
		`SELECT ROW(ROW(nm)) FROM u`,
		`SELECT ROW(nm)::text FROM u`,
		`SELECT ROW(f) || 'x' FROM u`,
		`SELECT concat(ROW(at)) FROM u`,
		`SELECT ROW(1.5)`,
		`SELECT n FROM u WHERE ROW(f)::text = '(1.5)'`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
}

// MySQL fails a row where it wants a single value with 1241.
func TestRowAsTextMySQL(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysql.New())
	for _, q := range []string{
		`SELECT ROW(1, 2)`,
		`SELECT (1, 2)`,
		`SELECT CAST(ROW(1, 2) AS CHAR)`,
		`SELECT CONCAT((1, 2), 'x')`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
}
