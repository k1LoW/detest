package detest

import (
	"errors"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// Statements Postgres rejects fail with its SQLSTATE, so code that branches
// on the code sees what it would in production.
func TestStatementErrorsCarrySQLSTATE(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `
CREATE TABLE items (id text PRIMARY KEY, name text);
CREATE VIEW item_names AS SELECT name FROM items;
`)
	for _, tc := range []struct {
		query string
		args  []any
		code  string
		is    error
	}{
		{`SELEC 1`, nil, "42601", ErrSyntaxError},
		{`INSERT INTO items (id, name) VALUES ('a')`, nil, "42601", ErrSyntaxError},
		{`INSERT INTO items (id) VALUES ('a', 'b')`, nil, "42601", ErrSyntaxError},
		{`SELECT 1 UNION SELECT 1, 2`, nil, "42601", ErrSyntaxError},
		{`SELECT $2::text`, []any{"x"}, "42P02", ErrUndefinedParameter},
		{`INSERT INTO items (id, name) VALUES ('a', 'b') ON CONFLICT (name) DO NOTHING`, nil, "42P10", ErrInvalidColumnReference},
		{`CREATE TABLE items (id text PRIMARY KEY)`, nil, "42P07", ErrDuplicateTable},
		{`CREATE VIEW item_names AS SELECT id FROM items`, nil, "42P07", ErrDuplicateTable},
		{`REFRESH MATERIALIZED VIEW items`, nil, "42809", ErrWrongObjectType},
		{`ALTER TABLE items ADD PRIMARY KEY (name)`, nil, "42P16", ErrInvalidTableDefinition},
		{`SAVEPOINT sp`, nil, "25P01", ErrNoActiveTransaction},
		{`DROP TABLE nope`, nil, "42P01", ErrUndefinedTable},
		{`ALTER TABLE nope ADD COLUMN x text`, nil, "42P01", ErrUndefinedTable},
		{`ALTER TABLE nope RENAME TO other`, nil, "42P01", ErrUndefinedTable},
	} {
		_, err := db.Exec(tc.query, tc.args...)
		var se *DBError
		if !errors.As(err, &se) || se.Code != tc.code || !errors.Is(err, tc.is) {
			t.Errorf("%s: got %v, want SQLSTATE %s", tc.query, err, tc.code)
		}
	}
}

// A syntax error aborts the transaction, as any failed statement does.
func TestSyntaxErrorAbortsTheTransaction(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE items (id text PRIMARY KEY)`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SELEC 1`); !errors.Is(err, ErrSyntaxError) {
		t.Fatalf("got %v, want a syntax error", err)
	}
	if _, err := tx.Exec(`INSERT INTO items (id) VALUES ('a')`); !errors.Is(err, ErrAborted) {
		t.Fatalf("got %v, want the transaction aborted", err)
	}
	if _, err := tx.Exec(`ROLLBACK TO SAVEPOINT nope`); !errors.Is(err, ErrInvalidSavepoint) {
		t.Fatalf("got %v, want an invalid savepoint", err)
	}
}
