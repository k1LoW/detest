package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/lib/pq"

	"github.com/k1LoW/detest/db/postgres"
)

func enumDB(t *testing.T) *sql.DB {
	t.Helper()
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy')`)
	mustExec(t, db, `CREATE TABLE p (id bigint PRIMARY KEY, m mood)`)
	mustExec(t, db, `INSERT INTO p VALUES (1, 'happy'), (2, 'sad'), (3, 'ok')`)
	return db
}

// A parameter compared with an enum, or bound as an array of them, is read
// by the enum's input, as Postgres infers the parameter's type from the
// column, and a value that is no label fails as that input does.
func TestEnumParameters(t *testing.T) {
	db := enumDB(t)
	for _, c := range []struct {
		q    string
		args []any
		want []int64
	}{
		{`SELECT id FROM p WHERE m = $1 ORDER BY id`, []any{"ok"}, []int64{3}},
		{`SELECT id FROM p WHERE m < $1 ORDER BY id`, []any{"happy"}, []int64{2, 3}},
		{`SELECT id FROM p WHERE m = ANY($1) ORDER BY id`, []any{pq.Array([]string{"sad", "ok"})}, []int64{2, 3}},
		{`SELECT id FROM p WHERE m IN ($1, $2) ORDER BY m`, []any{"happy", "sad"}, []int64{2, 1}},
	} {
		if got := queryIDs(t, db, c.q, c.args...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %v: got %v, want %v", c.q, c.args, got, c.want)
		}
	}
	for _, c := range []struct {
		q    string
		args []any
	}{
		{`SELECT id FROM p WHERE m = $1`, []any{"Happy"}},
		{`SELECT id FROM p WHERE m = ANY($1)`, []any{pq.Array([]string{"sad", "glad"})}},
		{`INSERT INTO p VALUES (4, $1)`, []any{"glad"}},
	} {
		if _, err := db.Exec(c.q, c.args...); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s %v: want invalid input for the enum, got %v", c.q, c.args, err)
		}
	}
	var m string
	if err := db.QueryRow(`SELECT m FROM p WHERE id = 1`).Scan(&m); err != nil || m != "happy" {
		t.Errorf("an enum reads back as its label: %q, %v", m, err)
	}
}

// A default and a domain over an enum are of the enum.
func TestEnumDefaultAndDomain(t *testing.T) {
	db := enumDB(t)
	mustExec(t, db, `CREATE DOMAIN feeling AS mood`)
	mustExec(t, db, `CREATE TABLE q (id bigint PRIMARY KEY, m mood NOT NULL DEFAULT 'ok', f feeling)`)
	mustExec(t, db, `INSERT INTO q (id, f) VALUES (1, 'happy'), (2, 'sad')`)
	mustExec(t, db, `INSERT INTO q (id, m, f) VALUES (3, 'sad', 'ok')`)
	if got := queryIDs(t, db, `SELECT id FROM q ORDER BY m, id`); !reflect.DeepEqual(got, []int64{3, 1, 2}) {
		t.Errorf("by the default's label: %v", got)
	}
	if got := queryIDs(t, db, `SELECT id FROM q ORDER BY f`); !reflect.DeepEqual(got, []int64{2, 3, 1}) {
		t.Errorf("by the domain's labels: %v", got)
	}
	if _, err := db.Exec(`INSERT INTO q (id, f) VALUES (4, 'glad')`); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("a domain over an enum takes its labels only: %v", err)
	}
}

// The transaction API writes an enum by its label, and it and State read
// one as its label.
func TestEnumTransactionAPI(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy')`)
		mustExec(t, db, `CREATE TABLE p (id text PRIMARY KEY, m mood)`)
		s.Seed(func() { store.SeedRow("p", Row{"id": "s", "m": "sad"}) })
		s.Manual("writer", 1, func(p *Proc) error {
			return store.Tx(p, func(tx *Tx) error {
				if err := tx.Insert("p", Row{"id": "x", "m": "glad"}); !errors.Is(err, ErrInvalidTextRepresentation) {
					return fmt.Errorf("a value that is no label: %w", err)
				}
				return nil
			})
		})
		s.Manual("reader", 1, func(p *Proc) error {
			return store.Tx(p, func(tx *Tx) error {
				if err := tx.Insert("p", Row{"id": "y", "m": "happy"}); err != nil {
					return err
				}
				if r, ok := tx.Get("p", "y"); !ok || r["m"] != "happy" {
					return fmt.Errorf("Tx.Get of an enum: %#v, %v", r, ok)
				}
				// A seeded label is the enum's value, which CAS compares
				// with and SQL orders by its labels.
				if ok, err := tx.CAS("p", "s", "m", "sad", "ok"); err != nil || !ok {
					return fmt.Errorf("CAS of a seeded enum: %v, %w", ok, err)
				}
				return nil
			})
		})
		s.AtQuiescence(func(st *State) error {
			r, ok := st.Row(store, "p", "y")
			if !ok {
				return fmt.Errorf("State lacks the row the reader inserted")
			}
			if v, _ := r.Get("m"); v != "happy" {
				return fmt.Errorf("State of an enum: %#v", v)
			}
			if r, _ := st.Row(store, "p", "s"); r.Str("m") != "ok" {
				return fmt.Errorf("the seeded row after CAS: %q", r.Str("m"))
			}
			return nil
		})
	})
}

// A column of an enum added with a default gives the rows already there a
// value of the enum, and a CHECK over one compares by the enum's input.
func TestEnumAddedColumnAndCheck(t *testing.T) {
	db := enumDB(t)
	mustExec(t, db, `ALTER TABLE p ADD COLUMN n mood NOT NULL DEFAULT 'ok'`)
	mustExec(t, db, `INSERT INTO p (id, m) VALUES (4, 'sad')`)
	if got := queryIDs(t, db, `SELECT id FROM p WHERE n = 'ok' ORDER BY n, id`); !reflect.DeepEqual(got, []int64{1, 2, 3, 4}) {
		t.Errorf("rows of the added column: %v", got)
	}
	if got := queryIDs(t, db, `SELECT id FROM p WHERE n < 'happy' ORDER BY id`); !reflect.DeepEqual(got, []int64{1, 2, 3, 4}) {
		t.Errorf("rows of the added column ordered: %v", got)
	}
	mustExec(t, db, `CREATE TABLE c (id bigint PRIMARY KEY, m mood CHECK (m > 'sad'))`)
	if _, err := db.Exec(`INSERT INTO c VALUES (1, 'sad')`); !errors.Is(err, ErrCheckViolation) {
		t.Errorf("a CHECK over an enum: %v", err)
	}
	mustExec(t, db, `INSERT INTO c VALUES (2, 'ok')`)
}

// A CREATE TABLE AS column of an enum is of the enum, whether the query or
// its rows tell it.
func TestEnumCreateTableAs(t *testing.T) {
	db := enumDB(t)
	mustExec(t, db, `CREATE TABLE q AS SELECT m FROM p WHERE false`)
	mustExec(t, db, `CREATE TABLE r AS SELECT * FROM (SELECT m FROM p) s`)
	for _, table := range []string{"q", "r"} {
		if _, err := db.Exec(fmt.Sprintf(`INSERT INTO %s VALUES ('bogus')`, table)); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%s: a value that is no label: %v", table, err)
		}
		if table == "q" {
			mustExec(t, db, `INSERT INTO q VALUES ('happy'), ('sad')`) // r has p's rows
		}
		var first string
		if err := db.QueryRow(fmt.Sprintf(`SELECT m FROM %s ORDER BY m LIMIT 1`, table)).Scan(&first); err != nil || first != "sad" {
			t.Errorf("%s: ordered by the labels: %q %v", table, first, err)
		}
	}
}

// The schema statements on an enum whose effect detest does not record are
// refused rather than taken for another.
func TestEnumSchemaChanges(t *testing.T) {
	db := enumDB(t)
	for _, q := range []string{
		`DROP TYPE mood CASCADE`,
		`DROP TYPE mood`,
		`ALTER TYPE mood RENAME TO feeling`,
		`CREATE TYPE other.mood AS ENUM ('a')`,
		`CREATE DOMAIN other.mood AS text`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: want unsupported, got %v", q, err)
		}
	}
	if _, err := db.Exec(`ALTER TYPE mood ADD VALUE 'ok'`); err == nil {
		t.Error("a label added twice must fail")
	}
	mustExec(t, db, `ALTER TYPE mood ADD VALUE IF NOT EXISTS 'ok'`)
	// A name in another schema is another type, which detest does not keep.
	mustExec(t, db, `DROP TYPE IF EXISTS other.mood`)
	if _, err := db.Exec(`INSERT INTO p VALUES (9, 'glad')`); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("the enum after a DROP of another schema's type of its name: %v", err)
	}
	mustExec(t, db, `ALTER TYPE mood ADD VALUE 'meh' AFTER 'sad'`)
	if got := queryIDs(t, db, `SELECT id FROM p WHERE m > 'meh' ORDER BY m`); !reflect.DeepEqual(got, []int64{3, 1}) {
		t.Errorf("after the added label: %v", got)
	}
	mustExec(t, db, `CREATE TYPE spare AS ENUM ('a'); DROP TYPE spare; CREATE TYPE spare AS ENUM ('b')`)
	mustExec(t, db, `CREATE TABLE keyless (n int, m spare)`)
	mustExec(t, db, `ALTER TYPE spare RENAME VALUE 'b' TO 'c'`)
	mustExec(t, db, `INSERT INTO keyless VALUES (1, 'c')`)
	if _, err := db.Exec(`ALTER TYPE spare RENAME VALUE 'c' TO 'd'`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a label renamed under a row keyed by its values: %v", err)
	}
	mustExec(t, db, `CREATE TYPE fresh AS ENUM ('x'); CREATE TABLE f (id int PRIMARY KEY, m fresh)`)
	if _, err := db.Exec(`BEGIN; INSERT INTO f VALUES (1, 'x'); ALTER TYPE fresh RENAME VALUE 'x' TO 'y'; COMMIT`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a label renamed under a row its own transaction wrote: %v", err)
	}
	// A label a kept expression names as text is not renamed under it.
	mustExec(t, db, `CREATE TYPE tone AS ENUM ('low', 'high'); CREATE TABLE tt (id int PRIMARY KEY, x tone DEFAULT 'low')`)
	if _, err := db.Exec(`ALTER TYPE tone RENAME VALUE 'low' TO 'soft'`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a label a default names, renamed: %v", err)
	}
	mustExec(t, db, `ALTER TYPE tone RENAME VALUE 'high' TO 'loud'`)
	// A type of another kind shares the bare name an enum is known by.
	for _, q := range []string{
		`CREATE TYPE other2.shade AS (r int); CREATE TYPE shade AS ENUM ('a')`,
		`CREATE TYPE hue AS ENUM ('a'); CREATE TYPE other2.hue AS (r int)`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: want unsupported, got %v", q, err)
		}
	}
	// Any name detest could take for another type is refused: a built-in
	// type, a quoted name casts reach lowercased, a relation's row type,
	// and a type another schema has or had, also through a rename.
	for _, q := range []string{
		`CREATE TYPE bit AS ENUM ('zero', 'one')`,
		`CREATE TYPE pg_mood AS ENUM ('a')`,
		`CREATE TYPE "Upbeat" AS ENUM ('a')`,
		`CREATE TYPE p AS ENUM ('a')`,
		`CREATE TYPE later AS ENUM ('a'); CREATE TABLE later (id int)`,
		`CREATE TYPE mood2 AS ENUM ('a'); ALTER TABLE p RENAME TO mood2`,
		`CREATE TYPE other3.tint AS (r int); DROP TYPE IF EXISTS public.tint; CREATE TYPE tint AS ENUM ('a')`,
		`CREATE TYPE other3.old AS (r int); ALTER TYPE other3.old RENAME TO fresh2; CREATE TYPE fresh2 AS ENUM ('a')`,
		`CREATE DOMAIN feeling2 AS text; ALTER DOMAIN feeling2 RENAME TO state2; CREATE TYPE feeling2 AS ENUM ('ok')`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: want unsupported, got %v", q, err)
		}
	}
	// SET SCHEMA moves the enum, which an ALTER of the new name then finds.
	mustExec(t, db, `CREATE SCHEMA other; ALTER TYPE public.spare SET SCHEMA other; ALTER TYPE other.spare ADD VALUE 'e'`)
	mustExec(t, db, `INSERT INTO keyless VALUES (2, 'e')`)
	mustExec(t, db, `CREATE TABLE d (id int PRIMARY KEY, m mood DEFAULT md5('x')::mood)`)
	if _, err := db.Exec(`INSERT INTO d (id) VALUES (1)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a default detest cannot compute on an enum column: %v", err)
	}
}
