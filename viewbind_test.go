package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/postgres"
)

// A view whose query left a name unbound when it was created, as an
// output name in GROUP BY or a * over a function with ORDINALITY, may read
// another column once a table it reads gains or renames a column of that
// name, so a statement reading it is refused from then on. The DDL itself
// runs, so that migrations load.
func TestViewLeftUnboundRefusedAfterColumnChange(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	for _, q := range []string{
		`CREATE TABLE t (id int PRIMARY KEY, s text, n int)`,
		`CREATE VIEW gv AS SELECT n AS k, count(*) AS c FROM t GROUP BY k`,
		`CREATE VIEW ov AS SELECT * FROM t JOIN generate_series(1, 2) WITH ORDINALITY g ON true`,
		`CREATE VIEW wv AS SELECT k FROM gv`,
		`ALTER TABLE t ADD COLUMN x int`,
	} {
		mustExec(t, db, q)
	}
	if _, err := db.Exec(`SELECT * FROM gv`); err != nil {
		t.Errorf("SELECT * FROM gv: %v", err)
	}
	mustExec(t, db, `ALTER TABLE t ADD COLUMN k int`)
	for _, q := range []string{
		`SELECT * FROM gv`,
		`SELECT k FROM wv`,
		`SELECT id FROM ov`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	// Creating the view again binds it again.
	mustExec(t, db, `CREATE OR REPLACE VIEW gv AS SELECT n AS k, count(*) AS c FROM t GROUP BY 1`)
	if _, err := db.Exec(`SELECT k FROM wv`); err != nil {
		t.Errorf("SELECT k FROM wv: %v", err)
	}
}

// MySQL keeps a view's query by the names it reads, so a view fails once
// a column it reads is renamed. detest has no check of MySQL's column
// names, so it refuses such a view rather than read the column as NULL.
func TestMySQLViewRefusedAfterColumnRename(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysql.New())
	for _, q := range []string{
		`CREATE TABLE t (id INT PRIMARY KEY, s TEXT, n INT)`,
		`CREATE VIEW tv AS SELECT * FROM t`,
		`CREATE VIEW nv AS SELECT id, n FROM t`,
		`CREATE VIEW wv AS SELECT s FROM tv`,
		`CREATE TABLE u (id INT PRIMARY KEY, s TEXT)`,
		`CREATE VIEW jv AS SELECT u.s FROM t JOIN u ON t.id = u.id`,
		`ALTER TABLE t RENAME COLUMN s TO s2`,
	} {
		mustExec(t, db, q)
	}
	for _, q := range []string{`SELECT id FROM tv`, `SELECT s FROM wv`} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	for _, q := range []string{`SELECT n FROM nv`, `SELECT s FROM jv`} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// A view reads the relation its name resolved to when it was created, so a
// rename of that relation is followed, and a relation of the name created
// later earlier in the search path does not take it over.
func TestViewKeepsRelationOnSearchPath(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New(postgres.SearchPath("billing", "public")))
	for _, q := range []string{
		`CREATE SCHEMA billing`,
		`CREATE TABLE public.t (id int PRIMARY KEY)`,
		`CREATE TABLE public.u (id int PRIMARY KEY)`,
		`CREATE VIEW public.tv AS SELECT id FROM t`,
		`CREATE VIEW public.uv AS SELECT id FROM u`,
		`ALTER TABLE public.t RENAME TO t2`,
		`CREATE TABLE billing.u (id int PRIMARY KEY)`,
		`INSERT INTO public.t2 VALUES (1)`,
		`INSERT INTO public.u VALUES (2)`,
		`INSERT INTO billing.u VALUES (3)`,
	} {
		mustExec(t, db, q)
	}
	if got, want := rowsOf(t, db, `SELECT id FROM tv`), []string{"1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tv: got %v, want %v", got, want)
	}
	if got, want := rowsOf(t, db, `SELECT id FROM uv`), []string{"2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("uv: got %v, want %v", got, want)
	}
}

// Replacing a view may add columns at its end, which a * left unbound in a
// view reading it would take up, so such a view is refused from then on.
func TestViewLeftUnboundRefusedAfterReplace(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	for _, q := range []string{
		`CREATE VIEW v(a, b) AS SELECT * FROM (SELECT 1 AS x, 2 AS x) d`,
		`CREATE VIEW w AS SELECT * FROM v`,
		`CREATE VIEW nv AS SELECT 1 AS a`,
		`CREATE VIEW cv AS SELECT * FROM nv`,
		`CREATE OR REPLACE VIEW v AS SELECT 1 AS a, 2 AS b, 3 AS c`,
		`CREATE OR REPLACE VIEW nv AS SELECT 1 AS a, 2 AS c`,
	} {
		mustExec(t, db, q)
	}
	if _, err := db.Exec(`SELECT * FROM w`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("SELECT * FROM w: got %v, want ErrUnsupportedSQL", err)
	}
	if _, err := db.Exec(`SELECT c FROM cv`); err == nil || errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("SELECT c FROM cv: got %v, want an undefined column", err)
	}
}
