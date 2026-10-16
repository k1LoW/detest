package detest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/postgres"
)

// On MySQL, a write whose subquery reads its own table through a view, or
// whose joined view or derived table holds a subquery reading it, is
// refused, since MySQL fails it or runs it by whether it merges the view or
// the derived table. The table is left as it was. Postgres runs them all.
func TestTargetReadRefused(t *testing.T) {
	stmts := []string{
		`UPDATE t SET n = (SELECT max(n) FROM tv)`,
		`DELETE FROM t WHERE id IN (SELECT id FROM tv WHERE id = 3)`,
		`INSERT INTO t VALUES ((SELECT max(id) FROM tv) + 1, 0)`,
		`UPDATE t SET n = (SELECT max(n) FROM ta)`,
		`UPDATE t SET n = (SELECT max(n) FROM tvv)`,
		`UPDATE t SET n = (SELECT max(n) FROM us)`,
		`UPDATE t SET n = (SELECT max(n) FROM u WHERE EXISTS (SELECT 1 FROM tv))`,
		`UPDATE t JOIN us ON us.id = t.id SET t.n = 5`,
		`UPDATE t JOIN (SELECT id FROM u WHERE id IN (SELECT id FROM t)) x ON x.id = t.id SET t.n = 5`,
		`UPDATE t JOIN (SELECT id FROM us) x ON x.id = t.id SET t.n = 5`,
	}
	s := newSim(t)
	db, _ := s.DB("app", mysql.New())
	pg, _ := s.DB("pg", postgres.New())
	for _, q := range []string{
		`CREATE TABLE t (id int PRIMARY KEY, n int)`,
		`CREATE TABLE u (id int PRIMARY KEY, n int)`,
		`CREATE VIEW tv AS SELECT * FROM t`,
		`CREATE VIEW ta AS SELECT max(n) AS n FROM t`,
		`CREATE VIEW tvv AS SELECT * FROM tv`,
		`CREATE VIEW us AS SELECT * FROM u WHERE id IN (SELECT id FROM t)`,
		`INSERT INTO t VALUES (1, 2), (2, 1), (3, 3)`,
		`INSERT INTO u VALUES (1, 1), (3, 3)`,
	} {
		mustExec(t, db, q)
		mustExec(t, pg, q)
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("mysql %s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	if got, want := rowsOf(t, db, `SELECT n FROM t ORDER BY id`), []string{"2", "1", "3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the refused writes: got %v, want %v", got, want)
	}
	for _, q := range stmts {
		if strings.HasPrefix(q, "UPDATE t JOIN") {
			continue // Postgres has no UPDATE ... JOIN
		}
		mustExec(t, pg, q)
	}
}

// A schema-qualified name of the target, on either side, is the same table
// to MySQL, which fails the write with 1093.
func TestTargetReadQualified(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysql.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 2)`)
	for _, q := range []string{
		`DELETE FROM app.t WHERE id IN (SELECT id FROM t)`,
		`DELETE FROM t WHERE id IN (SELECT id FROM app.t)`,
	} {
		var e *DBError
		if _, err := db.Exec(q); !errors.As(err, &e) || e.Number != 1093 || !errors.Is(err, ErrTargetTableRead) {
			t.Errorf("%s: got %v, want 1093", q, err)
		}
	}
}
