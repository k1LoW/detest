package detest

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

// keys lists a table's rows as "id:col" for the given column, sorted.
func keys(store *DB, table, col string) string {
	var out []string
	for _, r := range store.Peek(table) {
		out = append(out, fmt.Sprintf("%s:%v", r.Str("id"), r[col]))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func TestForeignKeyUpdateActions(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `
CREATE TABLE orgs (id text PRIMARY KEY);
CREATE TABLE teams (id text PRIMARY KEY, org_id text REFERENCES orgs (id) ON UPDATE CASCADE);
CREATE TABLE members (id text PRIMARY KEY, org_id text, team_org text,
  FOREIGN KEY (team_org) REFERENCES orgs (id) ON UPDATE SET NULL);
CREATE TABLE labels (id text PRIMARY KEY, org_id text DEFAULT 'o0' REFERENCES orgs (id) ON UPDATE SET DEFAULT ON DELETE SET DEFAULT);
INSERT INTO orgs (id) VALUES ('o0'), ('o1');
INSERT INTO teams (id, org_id) VALUES ('t1', 'o1');
INSERT INTO members (id, team_org) VALUES ('m1', 'o1');
INSERT INTO labels (id, org_id) VALUES ('l1', 'o1');
`)
	mustExec(t, db, `UPDATE orgs SET id = 'o9' WHERE id = 'o1'`)
	if got := keys(store, "teams", "org_id"); got != "t1:o9" {
		t.Errorf("ON UPDATE CASCADE: %s", got)
	}
	if got := keys(store, "members", "team_org"); got != "m1:<nil>" {
		t.Errorf("ON UPDATE SET NULL: %s", got)
	}
	if got := keys(store, "labels", "org_id"); got != "l1:o0" {
		t.Errorf("ON UPDATE SET DEFAULT: %s", got)
	}

	mustExec(t, db, `UPDATE labels SET org_id = 'o9'`)
	mustExec(t, db, `DELETE FROM teams`)
	mustExec(t, db, `DELETE FROM orgs WHERE id = 'o9'`)
	if got := keys(store, "labels", "org_id"); got != "l1:o0" {
		t.Errorf("ON DELETE SET DEFAULT: %s", got)
	}
	// The default must itself reference a parent.
	if _, err := db.Exec(`DELETE FROM orgs WHERE id = 'o0'`); !errors.Is(err, ErrForeignKeyViolation) {
		t.Errorf("SET DEFAULT to a missing parent: %v", err)
	}
}

// Cascades follow the foreign keys of the rows they rewrite.
func TestForeignKeyCascadeChain(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `
CREATE TABLE a (id text PRIMARY KEY);
CREATE TABLE b (id text PRIMARY KEY, a_id text REFERENCES a ON UPDATE CASCADE, UNIQUE (a_id));
CREATE TABLE c (id text PRIMARY KEY, b_a text REFERENCES b (a_id) ON UPDATE CASCADE);
INSERT INTO a VALUES ('x');
INSERT INTO b VALUES ('b1', 'x');
INSERT INTO c VALUES ('c1', 'x');
`)
	mustExec(t, db, `UPDATE a SET id = 'y'`)
	if got := keys(store, "b", "a_id") + " " + keys(store, "c", "b_a"); got != "b1:y c1:y" {
		t.Errorf("cascade chain: %s", got)
	}
}

func TestForeignKeyMatchFull(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `
CREATE TABLE p (a int, b int, PRIMARY KEY (a, b));
CREATE TABLE full_ref (id text PRIMARY KEY, a int, b int, FOREIGN KEY (a, b) REFERENCES p MATCH FULL);
CREATE TABLE simple_ref (id text PRIMARY KEY, a int, b int, FOREIGN KEY (a, b) REFERENCES p);
`)
	if _, err := db.Exec(`INSERT INTO full_ref VALUES ('f1', 1, NULL)`); !errors.Is(err, ErrForeignKeyViolation) {
		t.Errorf("MATCH FULL with a partly NULL key: %v", err)
	}
	mustExec(t, db, `INSERT INTO full_ref VALUES ('f2', NULL, NULL)`)
	mustExec(t, db, `INSERT INTO simple_ref VALUES ('s1', 1, NULL)`)
}

func TestForeignKeyDeferred(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `
CREATE TABLE orgs (id text PRIMARY KEY);
CREATE TABLE teams (id text PRIMARY KEY, org_id text REFERENCES orgs DEFERRABLE INITIALLY DEFERRED);
CREATE TABLE seats (id text PRIMARY KEY, org_id text, CONSTRAINT seats_org_fkey FOREIGN KEY (org_id) REFERENCES orgs DEFERRABLE);
`)
	inTx := func(stmts ...string) error {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range stmts {
			if _, err := tx.Exec(q); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		return tx.Commit()
	}

	// The child before its parent, both in the transaction.
	if err := inTx(`INSERT INTO teams VALUES ('t1', 'o1')`, `INSERT INTO orgs VALUES ('o1')`); err != nil {
		t.Fatalf("child first: %v", err)
	}
	// No parent by the commit: the commit fails and rolls back.
	err := inTx(`INSERT INTO teams VALUES ('t2', 'nope')`)
	var se *DBError
	if !errors.As(err, &se) || se.Code != "23503" {
		t.Fatalf("commit without the parent: %v", err)
	}
	if got := keys(store, "teams", "org_id"); got != "t1:o1" {
		t.Fatalf("the failed commit left %s", got)
	}
	// The parent deleted before its child, NO ACTION deferred.
	if err := inTx(`DELETE FROM orgs WHERE id = 'o1'`, `DELETE FROM teams WHERE id = 't1'`); err != nil {
		t.Fatalf("parent first: %v", err)
	}
	// SET CONSTRAINTS ... IMMEDIATE checks what was deferred so far.
	err = inTx(`INSERT INTO teams VALUES ('t3', 'nope')`, `SET CONSTRAINTS ALL IMMEDIATE`)
	if !errors.Is(err, ErrForeignKeyViolation) {
		t.Fatalf("SET CONSTRAINTS ALL IMMEDIATE: %v", err)
	}
	// A DEFERRABLE INITIALLY IMMEDIATE constraint checks at once, unless deferred.
	if _, err := db.Exec(`INSERT INTO seats VALUES ('s1', 'o2')`); !errors.Is(err, ErrForeignKeyViolation) {
		t.Fatalf("initially immediate: %v", err)
	}
	if err := inTx(`SET CONSTRAINTS seats_org_fkey DEFERRED`, `INSERT INTO seats VALUES ('s1', 'o2')`, `INSERT INTO orgs VALUES ('o2')`); err != nil {
		t.Fatalf("deferred by name: %v", err)
	}
	// Outside a transaction block the statement is the transaction.
	if _, err := db.Exec(`INSERT INTO teams VALUES ('t4', 'nope')`); !errors.Is(err, ErrForeignKeyViolation) {
		t.Fatalf("autocommit: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM teams`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("teams left: %d, %v", n, err)
	}
}
