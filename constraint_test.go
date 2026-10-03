package detest

import (
	"errors"
	"fmt"
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
		var se *SQLError
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
