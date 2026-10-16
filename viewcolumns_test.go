package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// Postgres expands a view into the statement that reads it, so a form the
// column check refuses in a statement is refused when a view, or a view it
// reads, holds it. Creating the view is not checked, so that dumps load.
func TestViewColumnCheckRefused(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 2), (2, 3)`)
	for _, q := range []string{
		`CREATE VIEW v1 AS SELECT t AS w FROM t`,
		`CREATE VIEW v2 AS SELECT (SELECT sum(t.n)) AS total FROM t`,
		`CREATE VIEW v3 AS SELECT total FROM v2`,
		`CREATE VIEW v4 AS SELECT id FROM t WHERE id IN (SELECT 1 FROM v1)`,
		`CREATE VIEW v5 AS SELECT t AS w, 1 / 0 AS z FROM t`,
		`CREATE VIEW ok AS SELECT id, n FROM t`,
	} {
		mustExec(t, db, q)
	}
	for _, q := range []string{
		`SELECT w FROM v1`,
		`SELECT total FROM v2`,
		`SELECT total FROM v3`,
		`SELECT id FROM v4`,
		`SELECT id FROM t WHERE EXISTS (SELECT 1 FROM v2)`,
		`SELECT t.id FROM t JOIN v3 ON true`,
		`UPDATE t SET n = 1 FROM v2 WHERE t.id = 1`,
		`DELETE FROM t USING v3 WHERE t.id = 1`,
		`SELECT * FROM v1`,
		// The refusal comes before the constants are folded, so the
		// view's division by zero does not stand in for it.
		`SELECT w FROM v5`,
		`WITH c AS (SELECT w FROM v1) SELECT * FROM c`,
		`INSERT INTO t SELECT 9, total FROM v2`,
		`UPDATE t SET n = (SELECT max(total) FROM v3)`,
		`CREATE TABLE x AS SELECT w FROM v1`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	if got, want := rowsOf(t, db, `SELECT n FROM ok ORDER BY id`), []string{"2", "3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A view whose query has a * gives its columns the names and the types the
// * expands to, so a check over them refuses what it refuses over a table.
func TestViewStarColumnsChecked(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	for _, q := range []string{
		`CREATE TABLE t (id int PRIMARY KEY, s text, n int)`,
		`CREATE VIEW tv AS SELECT * FROM t`,
		`CREATE VIEW pv AS SELECT * FROM (SELECT 'a'::text AS v) y`,
		`CREATE VIEW uv AS SELECT * FROM (SELECT 'a'::text AS v) y UNION ALL SELECT 'b'::text`,
		`CREATE VIEW vv AS SELECT * FROM tv`,
	} {
		mustExec(t, db, q)
	}
	for _, q := range []string{
		`SELECT abs(s) FROM tv WHERE false`,
		`SELECT abs(v) FROM pv WHERE false`,
		`SELECT abs(v) FROM uv WHERE false`,
		`SELECT abs(s) FROM vv WHERE false`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
}

// A view's column list renames its query's columns in order, so over a *
// whose columns detest cannot order, a list shorter than the query is
// refused rather than taken for the whole set.
func TestViewColumnListOverUnorderedStarRefused(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, s text, n int)`)
	mustExec(t, db, `CREATE VIEW jv(r) AS SELECT * FROM t JOIN (SELECT 1 AS k) z ON true`)
	mustExec(t, db, `CREATE VIEW fv(r, a, b, c) AS SELECT * FROM t JOIN (SELECT 1 AS k) z ON true`)
	if _, err := db.Exec(`SELECT s FROM jv`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("got %v, want ErrUnsupportedSQL", err)
	}
	if _, err := db.Exec(`SELECT r, a, b, c FROM fv`); err != nil {
		t.Error(err)
	}
}
