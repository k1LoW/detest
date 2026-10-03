package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

// claimJob declares two workers that each claim the next pending job with
// query and mark it running, and the invariant that no job is claimed twice.
func claimJob(t *testing.T, s *Sim, query string) {
	t.Helper()
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE queues (id text PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE jobs (id text PRIMARY KEY, queue text NOT NULL, status text NOT NULL)`)
	s.Seed(func() {
		mustExec(t, db, `INSERT INTO queues VALUES ('q')`)
		mustExec(t, db, `INSERT INTO jobs VALUES ('j1', 'q', 'pending')`)
	})
	var claims map[string]int
	s.Seed(func() { claims = map[string]int{} })
	claim := func(p *Proc) error {
		tx, err := db.BeginTx(p.Context(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		var id string
		if err := tx.QueryRow(query).Scan(&id); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE jobs SET status = 'running' WHERE id = $1`, id); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		claims[id]++
		return nil
	}
	s.Manual("x", 1, claim)
	s.Manual("y", 1, claim)
	s.AtQuiescence(func(*State) error {
		if claims["j1"] > 1 {
			return fmt.Errorf("j1 claimed %d times", claims["j1"])
		}
		return nil
	})
}

// A locking read on a join locks the rows of the tables it reads.
func TestForUpdateWithJoinLocks(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		claimJob(t, s, `SELECT j.id FROM jobs j JOIN queues q ON q.id = j.queue WHERE j.status = 'pending' ORDER BY j.id LIMIT 1 FOR UPDATE OF j`)
	})
}

// After waiting for a row, a locking read evaluates its predicate again on
// the row's new version and leaves out a row that no longer matches.
func TestForUpdateRechecksAfterWait(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		claimJob(t, s, `SELECT id FROM jobs WHERE status = 'pending' ORDER BY id LIMIT 1 FOR UPDATE`)
	})
}

func TestForUpdateTargets(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE a (id text PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE b (id text PRIMARY KEY)`)
	if _, err := db.Exec(`SELECT * FROM a LEFT JOIN b ON b.id = a.id FOR UPDATE`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("nullable side: got %v", err)
	}
	mustExec(t, db, `SELECT * FROM a LEFT JOIN b ON b.id = a.id FOR UPDATE OF a`)
	if _, err := db.Exec(`SELECT * FROM a FOR UPDATE OF c`); !errors.Is(err, ErrUndefinedTable) {
		t.Errorf("unknown OF: got %v", err)
	}
	if _, err := db.Exec(`SELECT * FROM a FOR UPDATE OF a FOR SHARE OF a`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("two locking clauses: got %v", err)
	}
}
