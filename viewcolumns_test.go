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
