package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// Postgres checks a scalar subquery's width and a locking query's shape when
// it analyzes the statement, so a form it refuses fails even where no row
// reaches it.
func TestSubqueryShapeBeforeAnyRow(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 0)`)
	for _, q := range []string{
		`SELECT coalesce(0, (SELECT 1 AS a, 2 AS b)::int)`,
		`SELECT CASE WHEN true THEN 0 ELSE (SELECT 1 AS a, 2 AS b)::int END`,
		`SELECT (SELECT * FROM t) FROM t WHERE false`,
		`SELECT coalesce(n, (SELECT sum(n) FROM t FOR UPDATE)) FROM t`,
		`SELECT n FROM t WHERE false AND n = (SELECT DISTINCT n FROM t FOR UPDATE)`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// A row compared with a subquery takes as many columns.
	if got := rowsOf(t, db, `SELECT id FROM t WHERE (id, n) = (SELECT id, n FROM t WHERE id = 1)`); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("a row compared with a subquery: got %v", got)
	}
}
