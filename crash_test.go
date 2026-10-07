package detest

import (
	"fmt"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// jobModel declares a worker that claims a job in one transaction and
// finishes it in another, and the invariant that no job is left claimed.
func jobModel(t *testing.T, s *Sim) {
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE jobs (id text PRIMARY KEY, status text NOT NULL)`)
	s.Seed(func() { mustExec(t, db, `INSERT INTO jobs VALUES ('j1', 'pending')`) })
	s.Manual("worker", 1, func(p *Proc) error {
		if _, err := db.ExecContext(p.Context(), `UPDATE jobs SET status = 'running' WHERE id = 'j1'`); err != nil {
			return err
		}
		_, err := db.ExecContext(p.Context(), `UPDATE jobs SET status = 'done' WHERE id = 'j1'`)
		return err
	})
	s.AtQuiescence(func(st *State) error {
		if row, _ := st.Row(store, "jobs", "j1"); row.Str("status") == "running" {
			return fmt.Errorf("job j1 stuck running")
		}
		return nil
	})
}

// A process that dies between claiming work and finishing it leaves the
// claim behind; nothing in this model recovers it.
func TestCrashLeavesClaimedWork(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		jobModel(t, s)
		s.ExpectViolation("stuck running")
	}, MaxCrashes(1))
}

// Without MaxCrashes nothing crashes.
func TestNoCrashByDefault(t *testing.T) {
	Explore(t, jobModel)
}

// A crash rolls back the transaction the process had open.
func TestCrashRollsBackTheTransaction(t *testing.T) {
	seen := map[int]bool{}
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE marks (id text PRIMARY KEY)`)
		s.Manual("writer", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`INSERT INTO marks VALUES ('m')`); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.AtQuiescence(func(st *State) error {
			seen[len(st.Rows(store, "marks"))] = true
			return nil
		})
	}, MaxCrashes(1))
	if !seen[0] || !seen[1] {
		t.Fatalf("rows at quiescence: %v, want runs with and without the commit", seen)
	}
}

// The mutex a crashed process held is freed with it: another process can
// take it, rather than waiting forever.
func TestCrashFreesTheMutex(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		mu := s.Mutex("mu")
		for _, name := range []string{"a", "b"} {
			s.Manual(name, 1, func(p *Proc) error {
				mu.Lock()
				p.Step("works holding the mutex")
				mu.Unlock()
				return nil
			})
		}
	}, MaxCrashes(1))
}

// The message a crashed consumer was handling is redelivered, not lost.
func TestCrashRedeliversTheMessage(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE handled (id text PRIMARY KEY)`)
		q := s.Queue("events")
		s.Seed(func() { q.SeedMsg(Msg{"id": "e1"}) })
		s.OnMessage("consumer", q, func(p *Proc, msg Msg) error {
			_, err := db.ExecContext(p.Context(), `INSERT INTO handled VALUES ($1) ON CONFLICT DO NOTHING`, msg.Str("id"))
			return err
		})
		s.AtQuiescence(func(st *State) error {
			if len(st.Rows(store, "handled")) == 0 {
				return fmt.Errorf("message e1 lost")
			}
			return nil
		})
	}, MaxCrashes(1))
}

// A crash at a commit's yield point rolls the transaction back and frees its
// row locks, so another worker can take the row with SKIP LOCKED.
func TestCrashAtCommitReleasesTheLocks(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE jobs (id text PRIMARY KEY, status text NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO jobs VALUES ('j1', 'pending')`) })
		s.Manual("worker", 2, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var id string
			if err := tx.QueryRow(`SELECT id FROM jobs WHERE status = 'pending' FOR UPDATE SKIP LOCKED`).Scan(&id); err != nil {
				return nil // nothing left
			}
			if _, err := tx.Exec(`UPDATE jobs SET status = 'done' WHERE id = $1`, id); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(store, "jobs", "j1"); row.Str("status") != "done" {
				return fmt.Errorf("job j1 left %s", row.Str("status"))
			}
			return nil
		})
	}, MaxCrashes(1))
}
