package detest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

func TestForeignKeys(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `
CREATE TABLE orgs (id text PRIMARY KEY, name text);
CREATE TABLE teams (id text PRIMARY KEY, org_id text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE);
CREATE TABLE members (id text PRIMARY KEY, team_id text, CONSTRAINT members_team_fkey FOREIGN KEY (team_id) REFERENCES teams (id) ON DELETE CASCADE);
CREATE TABLE invites (id text PRIMARY KEY, team_id text REFERENCES teams ON DELETE SET NULL);
CREATE TABLE audits (id text PRIMARY KEY, org_id text);
ALTER TABLE audits ADD CONSTRAINT audits_org_fkey FOREIGN KEY (org_id) REFERENCES orgs (id);
INSERT INTO orgs (id, name) VALUES ('o1', 'one'), ('o2', 'two');
INSERT INTO teams (id, org_id) VALUES ('t1', 'o1');
INSERT INTO members (id, team_id) VALUES ('m1', 't1'), ('m2', NULL);
INSERT INTO invites (id, team_id) VALUES ('i1', 't1');
INSERT INTO audits (id, org_id) VALUES ('a1', 'o2');
`)
	var se *SQLError
	if _, err := db.Exec(`INSERT INTO teams (id, org_id) VALUES ('t2', 'nope')`); !errors.As(err, &se) || se.Code != "23503" || se.Constraint != "teams_org_id_fkey" {
		t.Fatalf("insert without a parent: %#v", err)
	}
	if _, err := db.Exec(`UPDATE members SET team_id = 'nope' WHERE id = 'm2'`); !errors.Is(err, ErrForeignKeyViolation) {
		t.Fatalf("update to a missing parent: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM orgs WHERE id = 'o2'`); !errors.As(err, &se) || se.Constraint != "audits_org_fkey" {
		t.Fatalf("NO ACTION must refuse the delete: %#v", err)
	}
	if _, err := db.Exec(`UPDATE orgs SET id = 'o9' WHERE id = 'o2'`); !errors.Is(err, ErrForeignKeyViolation) {
		t.Fatalf("updating a referenced key: %v", err)
	}
	mustExec(t, db, `UPDATE orgs SET name = 'ONE' WHERE id = 'o1'`) // not a key: no check
	mustExec(t, db, `DELETE FROM orgs WHERE id = 'o1'`)
	ids := func(table string) (out []string) {
		for _, r := range store.Peek(table) {
			out = append(out, fmt.Sprintf("%s:%v", r.Str("id"), r["team_id"]))
		}
		return out
	}
	if got := fmt.Sprint(ids("teams"), ids("members"), ids("invites")); got != "[] [m2:<nil>] [i1:<nil>]" {
		t.Fatalf("cascade and set null: %s", got)
	}
}

// A child inserted while its parent is deleted: the insert's FOR KEY SHARE on
// the parent makes the delete wait, so no schedule leaves an orphan.
func TestForeignKeyConcurrentDelete(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `
CREATE TABLE orgs (id text PRIMARY KEY);
CREATE TABLE teams (id text PRIMARY KEY, org_id text NOT NULL REFERENCES orgs (id));
`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO orgs (id) VALUES ('o1')`) })
		s.Manual("add_team", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`INSERT INTO teams (id, org_id) VALUES ('t1', 'o1')`); err != nil {
				return nil // the org was already gone
			}
			p.Step("does more work in the transaction")
			return tx.Commit()
		})
		s.Manual("delete_org", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `DELETE FROM orgs WHERE id = 'o1'`)
			if errors.Is(err, ErrForeignKeyViolation) {
				return nil // the team was added first
			}
			return err
		})
		s.AtQuiescence(func(st *State) error {
			orgs, teams := len(st.Rows(store, "orgs")), len(st.Rows(store, "teams"))
			if teams > 0 && orgs == 0 {
				return fmt.Errorf("orphan team")
			}
			return nil
		})
	})
}

func TestLockConflicts(t *testing.T) {
	for _, tc := range []struct {
		held, want lockMode
		conflict   bool
	}{
		{lockKeyShare, lockNoKeyUpdate, false},
		{lockKeyShare, lockShare, false},
		{lockKeyShare, lockUpdate, true},
		{lockShare, lockShare, false},
		{lockShare, lockNoKeyUpdate, true},
		{lockNoKeyUpdate, lockNoKeyUpdate, true},
		{lockNoKeyUpdate, lockKeyShare, false},
		{lockUpdate, lockKeyShare, true},
	} {
		if got := tc.held.conflicts(tc.want); got != tc.conflict {
			t.Errorf("%s held, %s wanted: conflict %v, want %v", tc.held, tc.want, got, tc.conflict)
		}
	}
}
