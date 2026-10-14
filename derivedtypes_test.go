package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// A column of a *, a set operation or generate_series in FROM has the type
// Postgres gives it, so a literal beside it runs where Postgres runs it.
func TestDerivedColumnTypes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, name text, n int)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'a', 5), (2, NULL, NULL)`)
	mustExec(t, db, `CREATE VIEW tv AS SELECT id, name FROM t UNION ALL SELECT 3, NULL`)
	for _, tt := range []struct {
		q    string
		want []string
	}{
		{`SELECT coalesce(s.name, 'x') FROM (SELECT * FROM t) s ORDER BY s.id`, []string{"a", "x"}},
		{`SELECT coalesce(s.name, 'x') FROM (SELECT t.* FROM t) s ORDER BY s.id`, []string{"a", "x"}},
		{`WITH c AS (SELECT * FROM t) SELECT coalesce(c.name, 'x') FROM c ORDER BY c.id`, []string{"a", "x"}},
		{`SELECT coalesce(name, 'x') FROM tv ORDER BY id`, []string{"a", "x", "x"}},
		{`SELECT CASE WHEN s.id = 1 THEN s.name ELSE 'x' END FROM (SELECT * FROM t) s ORDER BY s.id`, []string{"a", "x"}},
		{`SELECT greatest(s.n, 1) FROM (SELECT * FROM t) s ORDER BY s.id`, []string{"5", "1"}},
		{`SELECT coalesce(u.name, 'x') FROM (SELECT name FROM t UNION SELECT name FROM t) u ORDER BY 1`, []string{"a", "x"}},
		{`SELECT coalesce(u.name, 'x') FROM (SELECT name FROM t UNION ALL SELECT NULL) u ORDER BY 1`, []string{"a", "x", "x"}},
		{`SELECT coalesce(u.n, 0) FROM (SELECT NULL AS n UNION ALL (SELECT NULL UNION ALL SELECT n FROM t)) u ORDER BY 1`, []string{"0", "0", "0", "5"}},
		{`SELECT coalesce(u.name, 'x') FROM (SELECT 'b' AS name UNION ALL SELECT name FROM t) u ORDER BY 1`, []string{"a", "b", "x"}},
		{`SELECT coalesce(g, 0) FROM generate_series(1, 2) AS g ORDER BY 1`, []string{"1", "2"}},
		{`SELECT coalesce(k, 0) FROM generate_series(1, 2) AS g(k) ORDER BY 1`, []string{"1", "2"}},
		{`SELECT * FROM generate_series(1, 2) g ORDER BY 1 DESC`, []string{"2", "1"}},
	} {
		if got := rowsOf(t, db, tt.q); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
	// A * gives its items' columns, so a name none of them has is no
	// column, and one two of them have is refused, as of a select list
	// that names two columns alike.
	if _, err := db.Query(`SELECT s.nope FROM (SELECT * FROM t) s`); !errors.Is(err, ErrUndefinedColumn) {
		t.Errorf("a column a * does not give: got %v", err)
	}
	if _, err := db.Query(`SELECT s.id FROM (SELECT * FROM t a JOIN t b ON a.id = b.id) s`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a column a * gives twice: got %v", err)
	}
	// Queries of a set operation that disagree, or one of a type the check
	// does not know, leave the column unresolved, as it may be an interval.
	// Postgres resolves a pair of NULLs, or of CASE or COALESCE over them,
	// to text, which an integer does not join, and converts a string
	// literal beside an interval, which detest does not.
	for _, q := range []string{
		`SELECT coalesce(u.v, 'x') FROM (SELECT id AS v FROM t UNION SELECT n::int8 FROM t) u`,
		`SELECT coalesce(u.v, 0) FROM (SELECT NULL AS v UNION ALL SELECT NULL UNION ALL SELECT 1) u`,
		`SELECT coalesce(u.v, 0) FROM (SELECT coalesce(NULL, NULL) AS v UNION ALL SELECT 1) u`,
		`SELECT coalesce(u.v, interval '0 days') FROM (SELECT NULL::interval AS v UNION ALL SELECT 'bogus') u`,
		`SELECT coalesce(u.v, 0) FROM (SELECT n AS v FROM t UNION ALL SELECT '5') u`,
		`SELECT coalesce(g, 0) FROM generate_series($1, 2) AS g`,
	} {
		if _, err := db.Query(q, 1); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
}
