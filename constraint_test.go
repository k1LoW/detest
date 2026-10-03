package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

func TestNotNullAndCheckConstraints(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `
CREATE TABLE items (
  id text PRIMARY KEY,
  name text NOT NULL,
  stock int NOT NULL DEFAULT 0 CHECK (stock >= 0),
  price int,
  note text,
  CONSTRAINT price_positive CHECK (price > 0)
);
INSERT INTO items (id, name, stock, price) VALUES ('a', 'apple', 1, 100);
`)
	for _, tc := range []struct {
		query      string
		is         error
		code       string
		column     string
		constraint string
	}{
		{`INSERT INTO items (id) VALUES ('b')`, ErrNotNullViolation, "23502", "name", ""},
		{`UPDATE items SET name = NULL`, ErrNotNullViolation, "23502", "name", ""},
		{`UPDATE items SET stock = stock - 2`, ErrCheckViolation, "23514", "", "items_stock_check"},
		{`INSERT INTO items (id, name, price) VALUES ('b', 'banana', 0)`, ErrCheckViolation, "23514", "", "price_positive"},
	} {
		_, err := db.Exec(tc.query)
		var se *DBError
		if !errors.As(err, &se) || !errors.Is(err, tc.is) || se.Code != tc.code || se.Column != tc.column || se.Constraint != tc.constraint {
			t.Errorf("%s: got %#v", tc.query, err)
		}
	}
	// A CHECK whose expression is NULL passes, as in Postgres.
	mustExec(t, db, `INSERT INTO items (id, name) VALUES ('c', 'cherry')`)

	// ALTER TABLE adds and drops them.
	mustExec(t, db, `ALTER TABLE items ALTER COLUMN note SET NOT NULL`)
	if _, err := db.Exec(`INSERT INTO items (id, name) VALUES ('d', 'date')`); !errors.Is(err, ErrNotNullViolation) {
		t.Errorf("SET NOT NULL: %v", err)
	}
	mustExec(t, db, `ALTER TABLE items ALTER COLUMN note DROP NOT NULL`)
	mustExec(t, db, `ALTER TABLE items ADD CONSTRAINT short_name CHECK (length(name) < 10)`)
	if _, err := db.Exec(`INSERT INTO items (id, name) VALUES ('e', 'elderberry!')`); !errors.Is(err, ErrCheckViolation) {
		t.Errorf("ADD CONSTRAINT CHECK: %v", err)
	}
	mustExec(t, db, `ALTER TABLE items DROP CONSTRAINT short_name`)
	mustExec(t, db, `INSERT INTO items (id, name) VALUES ('e', 'elderberry!')`)

	// A renamed column keeps its constraints.
	mustExec(t, db, `ALTER TABLE items RENAME COLUMN stock TO qty`)
	if _, err := db.Exec(`UPDATE items SET qty = -1 WHERE id = 'a'`); !errors.Is(err, ErrCheckViolation) {
		t.Errorf("CHECK after RENAME COLUMN: %v", err)
	}
	if _, err := db.Exec(`UPDATE items SET qty = NULL WHERE id = 'a'`); !errors.Is(err, ErrNotNullViolation) {
		t.Errorf("NOT NULL after RENAME COLUMN: %v", err)
	}
	// Dropping a column drops the checks on it.
	mustExec(t, db, `ALTER TABLE items DROP COLUMN price`)
	mustExec(t, db, `INSERT INTO items (id, name) VALUES ('f', 'fig')`)
}

// Two buyers of the last item: the CHECK makes the second fail, as in
// Postgres, so the code under test takes its error path instead of selling
// stock it does not have.
func TestCheckConstraintUnderConcurrency(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("shop", postgres.New())
		mustExec(t, db, `CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL CHECK (n >= 0))`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO stock VALUES ('apple', 1)`) })
		sold := 0
		s.Seed(func() { sold = 0 })
		for _, name := range []string{"alice", "bob"} {
			s.Manual(name, 1, func(p *Proc) error {
				_, err := db.ExecContext(p.Context(), `UPDATE stock SET n = n - 1 WHERE sku = 'apple'`)
				if errors.Is(err, ErrCheckViolation) {
					return nil // sold out
				}
				if err == nil {
					sold++
				}
				return err
			})
		}
		s.AtQuiescence(func(st *State) error {
			row, _ := st.Row(store, "stock", "apple")
			if sold != 1 || row.Int64("n") != 0 {
				return fmt.Errorf("sold %d, stock %d", sold, row.Int64("n"))
			}
			return nil
		})
	})
}

// Comparisons with NULL are NULL, and NOT, AND, OR and IN follow SQL's
// three-valued logic, so a WHERE never picks a row through a negated
// unknown.
func TestThreeValuedLogic(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `
CREATE TABLE items (id text PRIMARY KEY, price int);
INSERT INTO items VALUES ('a', 100), ('b', NULL);
`)
	for _, tc := range []struct {
		where string
		want  string
	}{
		{`NOT (price > 0)`, ""},
		{`price <> 100`, ""},
		{`NOT (price = 100) OR price IS NULL`, "b"},
		{`price > 0 OR price IS NULL`, "a b"},
		{`NOT (price > 0 AND id = 'b')`, "a"},
		{`id NOT IN ('a', NULL)`, ""},
		{`price NOT IN (1, 2)`, "a"},
	} {
		rows, err := db.Query(`SELECT id FROM items WHERE ` + tc.where + ` ORDER BY id`) //nolint:gosec // the test's own fixed predicates
		if err != nil {
			t.Fatalf("%s: %v", tc.where, err)
		}
		var got []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		_ = rows.Close()
		if g := fmt.Sprint(got); g != "["+tc.want+"]" {
			t.Errorf("WHERE %s: %s, want [%s]", tc.where, g, tc.want)
		}
	}
}

// pg_dump writes CHECK (status IN (...)) as = ANY over an array.
func TestCheckInDumpFormIsEnforced(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE orders (
    id integer NOT NULL,
    status character varying NOT NULL,
    CONSTRAINT orders_status_check CHECK (((status)::text = ANY ((ARRAY['open'::character varying, 'closed'::character varying])::text[])))
)`)
	mustExec(t, db, `INSERT INTO orders (id, status) VALUES (1, 'open')`)
	_, err := db.Exec(`INSERT INTO orders (id, status) VALUES (2, 'lost')`)
	var se *DBError
	if !errors.As(err, &se) || !errors.Is(err, ErrCheckViolation) || se.Constraint != "orders_status_check" {
		t.Errorf("got %#v", err)
	}
}

// A generated column is computed from the row on every write, and the
// constraints on it see the computed value.
func TestGeneratedColumns(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, a int, b int GENERATED ALWAYS AS (a * 2) STORED, CONSTRAINT b_small CHECK (b < 100))`)
	b := func(id int) int64 {
		t.Helper()
		var v int64
		if err := db.QueryRow(`SELECT b FROM t WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	mustExec(t, db, `INSERT INTO t (id, a) VALUES (1, 1)`)
	mustExec(t, db, `INSERT INTO t VALUES (2, 2, DEFAULT)`)
	if b(1) != 2 || b(2) != 4 {
		t.Errorf("insert: b = %d, %d", b(1), b(2))
	}
	mustExec(t, db, `UPDATE t SET a = 5 WHERE id = 1`)
	mustExec(t, db, `INSERT INTO t (id, a) VALUES (2, 7) ON CONFLICT (id) DO UPDATE SET a = excluded.a`)
	if b(1) != 10 || b(2) != 14 {
		t.Errorf("update: b = %d, %d", b(1), b(2))
	}
	if _, err := db.Exec(`UPDATE t SET a = 60 WHERE id = 1`); !errors.Is(err, ErrCheckViolation) {
		t.Errorf("CHECK on the generated column: %v", err)
	}
	mustExec(t, db, `ALTER TABLE t RENAME COLUMN a TO c`)
	mustExec(t, db, `UPDATE t SET c = 8 WHERE id = 1`)
	if b(1) != 16 {
		t.Errorf("after RENAME COLUMN: b = %d", b(1))
	}
	// A new column that takes the old name does not become the source.
	mustExec(t, db, `ALTER TABLE t ADD COLUMN a int`)
	mustExec(t, db, `ALTER TABLE t RENAME COLUMN a TO e`)
	mustExec(t, db, `UPDATE t SET c = 9, e = 1 WHERE id = 1`)
	if b(1) != 18 {
		t.Errorf("after reusing the old name: b = %d", b(1))
	}
	// Without a column list, N values fill the first N columns.
	mustExec(t, db, `INSERT INTO t VALUES (4, 3)`)
	if b(4) != 6 {
		t.Errorf("VALUES for the first columns: b = %d", b(4))
	}
	// RETURNING * returns every column, the computed one included.
	for _, q := range []string{`INSERT INTO t VALUES (5, 1) RETURNING *`, `INSERT INTO t (id, c) VALUES (6, 1) RETURNING *`} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, _ := rows.Columns()
		_ = rows.Close()
		if !slices.Contains(cols, "b") {
			t.Errorf("%s: columns %v", q, cols)
		}
	}
	for _, q := range []string{
		`INSERT INTO t (id, c, b) VALUES (3, 1, 2)`,
		`INSERT INTO t VALUES (3, 1, 2)`,
		`UPDATE t SET b = 1`,
		`INSERT INTO t (id, c) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET b = 1`,
		`ALTER TABLE t ADD COLUMN d int GENERATED ALWAYS AS (c + 1) STORED`,
		`INSERT INTO t (id, c) VALUES (99, 1) ON CONFLICT (id) DO UPDATE SET b = 1`, // no conflict
		`ALTER TABLE t ALTER COLUMN b SET DEFAULT 1`,
		`CREATE TABLE z (a int, b int GENERATED ALWAYS AS (coalesce(missing, 0)) STORED)`,
		`ALTER TABLE t ADD COLUMN z int GENERATED ALWAYS AS (missing + 1) STORED`,
		`ALTER TABLE t ALTER COLUMN b DROP DEFAULT`,
		`ALTER TABLE t DROP COLUMN c`,
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}

	// Refused before the source query runs.
	mustExec(t, db, `CREATE SEQUENCE s`)
	if _, err := db.Exec(`INSERT INTO t (id, b) SELECT nextval('s'), 1`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("INSERT ... SELECT: got %v", err)
	}
	if _, err := db.Exec(`UPDATE t SET b = 1 WHERE nextval('s') > 0`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("UPDATE: got %v", err)
	}
	var n int64
	if err := db.QueryRow(`SELECT nextval('s')`).Scan(&n); err != nil || n != 1 {
		t.Errorf("nextval after a refused INSERT and UPDATE: %d, %v", n, err)
	}

	// The same statement may not add a generated column and drop what it reads.
	mustExec(t, db, `CREATE TABLE v (id int PRIMARY KEY, a int)`)
	if _, err := db.Exec(`ALTER TABLE v ADD COLUMN b int GENERATED ALWAYS AS (a * 2) STORED, DROP COLUMN a`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("ADD and DROP in one ALTER: got %v", err)
	}

	// A generation expression detest cannot evaluate loads, but writing a
	// row that needs it is refused rather than storing a made-up value.
	mustExec(t, db, `CREATE TABLE w (id int PRIMARY KEY, a text, b text GENERATED ALWAYS AS (reverse(a)) STORED)`)
	if _, err := db.Exec(`INSERT INTO w (id, a) VALUES (1, 'ab')`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("unevaluable generated column: got %v", err)
	}
	// A value that happens to be the placeholder's text is still a value.
	mustExec(t, db, `CREATE TABLE w3 (id int PRIMARY KEY, a text, b text GENERATED ALWAYS AS (a) STORED)`)
	mustExec(t, db, `INSERT INTO w3 (id, a) VALUES (1, $1)`, Unknown)
	// A generated column may not refer to another, as in Postgres.
	if _, err := db.Exec(`CREATE TABLE w4 (a int, b int GENERATED ALWAYS AS (a + 1) STORED, c int GENERATED ALWAYS AS (b + 1) STORED)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("generated column on a generated column: got %v", err)
	}
	mustExec(t, db, `CREATE TABLE w2 (id int PRIMARY KEY, a text[], b text GENERATED ALWAYS AS (a[1]) STORED)`)
	if _, err := db.Exec(`INSERT INTO w2 (id) VALUES (1)`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("unconvertible generated column: got %v", err)
	}

	// A row the transaction inserted counts as a row there already.
	mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY)`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO u VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`ALTER TABLE u ADD COLUMN d int GENERATED ALWAYS AS (id + 1) STORED`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("ADD COLUMN after an insert in the transaction: got %v", err)
	}
}

// A strict function of NULL is NULL, so NULLs do not collide in a unique
// generated column, as in Postgres.
func TestStrictFunctionsOfNull(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE users (id int PRIMARY KEY, email text, email_l text GENERATED ALWAYS AS (lower(email)) STORED UNIQUE)`)
	mustExec(t, db, `INSERT INTO users (id) VALUES (1), (2)`)
	for _, q := range []string{`SELECT lower(NULL)`, `SELECT length(NULL)`, `SELECT 'a' || NULL`, `SELECT abs(NULL)`} {
		var v sql.NullString
		if err := db.QueryRow(q).Scan(&v); err != nil || v.Valid {
			t.Errorf("%s: %v %v, want NULL", q, v, err)
		}
	}
	// A call no signature takes is refused, NULL or not.
	for _, q := range []string{`SELECT power(NULL)`, `SELECT abs(NULL, NULL)`} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v", q, err)
		}
	}
	for q, want := range map[string]float64{`SELECT round(2.345, 2)`: 2.35, `SELECT round(1.5)`: 2, `SELECT round(-2.345, 1)`: -2.3,
		`SELECT round(-81.865, 2)`: -81.87, `SELECT round(1.005, 2)`: 1.01, `SELECT round(1250, -2)`: 1300,
		`SELECT round(1.5, 1000000000)`: 1.5, `SELECT round(1.5, -1000000000)`: 0} {
		var v float64
		if err := db.QueryRow(q).Scan(&v); err != nil || v != want {
			t.Errorf("%s: %v %v, want %v", q, v, err, want)
		}
	}
}
