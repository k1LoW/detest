package detest

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

// Two handlers read-check-write a row while holding a detest.Mutex injected in
// place of a sync.Mutex. The handler finding the mutex held waits inside
// detest, and the lost update the exploration would otherwise find does not
// appear.
func TestInjectedMutexHeldAcrossYieldPoint(t *testing.T) {
	Explore(t, func(t *testing.T, m *Model) {
		db, store := m.DB("app", postgres.New())
		mu := m.Mutex("counter")
		m.Seed(func() {
			_, _ = db.Exec(`INSERT INTO "counters" ("id","n") VALUES ($1,$2)`, "c", int64(0))
		})
		handler := func(p *Proc) error {
			mu.Lock()
			defer mu.Unlock()
			var n int64
			if err := db.QueryRow(`SELECT "n" FROM "counters" WHERE "id" = $1`, "c").Scan(&n); err != nil {
				return err
			}
			_, err := db.Exec(`UPDATE "counters" SET "n"=$1 WHERE "id" = $2`, n+1, "c")
			return err
		}
		m.Manual("req_a", 1, handler)
		m.Manual("req_b", 1, handler)
		m.AtQuiescence(func(s *State) error {
			row, _ := s.Row(store, "counters", "c")
			if row.Int64("n") != 2 {
				return fmt.Errorf("lost update: n = %d", row.Int64("n"))
			}
			return nil
		})
	})
}

// The same handlers with a plain sync.Mutex. A handler blocked on it is not
// durably blocked, so the scheduler cannot tell it from a running one and
// waits; the stall watchdog reports the hang and points at the mutex. The
// exploration runs in a child process because the watchdog crashes it.
func TestUninjectedMutexStalls(t *testing.T) {
	if os.Getenv("DETEST_STALL_CHILD") == "1" {
		// Compiling the SQL parser takes seconds under -race; keep it out of
		// the one second the watchdog is given here.
		_ = CheckSQL(postgres.New(), `SELECT 1`)
		Explore(t, func(t *testing.T, m *Model) {
			db, _ := m.DB("app", postgres.New())
			var mu sync.Mutex
			m.Seed(func() {
				_, _ = db.Exec(`INSERT INTO "counters" ("id","n") VALUES ($1,$2)`, "c", int64(0))
			})
			handler := func(p *Proc) error {
				mu.Lock()
				defer mu.Unlock()
				_, err := db.Exec(`UPDATE "counters" SET "n"=$1 WHERE "id" = $2`, int64(1), "c")
				return err
			}
			m.Manual("req_a", 1, handler)
			m.Manual("req_b", 1, handler)
		})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUninjectedMutexStalls$")
	cmd.Env = append(os.Environ(), "DETEST_STALL_CHILD=1", "DETEST_STALL=1s")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the child to crash on the stall:\n%s", out)
	}
	for _, want := range []string{"no scheduling progress", "sync.(*Mutex).Lock", "TestUninjectedMutexStalls"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("stall report lacks %q:\n%s", want, out)
		}
	}
}

// One handler takes the mutex and then the row lock, the other takes the row
// lock and then the mutex. Postgres sees only one transaction waiting, so it
// detects no deadlock and both hang. detest reports the cycle.
func TestInjectedMutexAndRowLockInOppositeOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schedule string // empty explores every schedule
		want     string
	}{
		{name: "explore", want: "cycle of waits"},
		// row_first takes the row lock and waits for the mutex; mutex_first,
		// holding the mutex, then closes the cycle by waiting for the row.
		{name: "closed by the row lock wait", schedule: "1,1,0,0,1", want: "database cannot detect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.schedule != "" {
				t.Setenv("DETEST_SCHEDULE", tc.schedule)
			}
			Explore(t, func(t *testing.T, m *Model) {
				db, _ := m.DB("app", postgres.New())
				mu := m.Mutex("cache")
				m.Seed(func() {
					_, _ = db.Exec(`INSERT INTO "jobs" ("id","status") VALUES ($1,$2)`, "j1", "RUNNING")
				})
				m.Manual("mutex_first", 1, func(p *Proc) error {
					mu.Lock()
					defer mu.Unlock()
					_, err := db.Exec(`UPDATE "jobs" SET "status"=$1 WHERE "id" = $2`, "CANCELED", "j1")
					return err
				})
				m.Manual("row_first", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					if _, err := tx.Exec(`UPDATE "jobs" SET "status"=$1 WHERE "id" = $2`, "SUCCESS", "j1"); err != nil {
						_ = tx.Rollback()
						return err
					}
					mu.Lock()
					mu.Unlock()
					return tx.Commit()
				})
				m.ExpectViolation(tc.want)
			})
		})
	}
}

func TestInjectedMutexLockedTwice(t *testing.T) {
	Explore(t, func(t *testing.T, m *Model) {
		mu := m.Mutex("m")
		m.Manual("relock", 1, func(p *Proc) error {
			mu.Lock()
			mu.Lock()
			return nil
		})
		m.ExpectViolation("held by relock")
	})
}

// Two readers each read a row twice under the read lock while a writer updates
// it under the write lock. The readers never see the row change in between,
// and holding the read lock together does not block them.
func TestInjectedRWMutexExcludesWriter(t *testing.T) {
	Explore(t, func(t *testing.T, m *Model) {
		db, _ := m.DB("app", postgres.New())
		rw := m.RWMutex("counter")
		var torn []string
		m.Seed(func() {
			torn = nil
			_, _ = db.Exec(`INSERT INTO "counters" ("id","n") VALUES ($1,$2)`, "c", int64(0))
		})
		read := func() (int64, error) {
			var n int64
			err := db.QueryRow(`SELECT "n" FROM "counters" WHERE "id" = $1`, "c").Scan(&n)
			return n, err
		}
		reader := func(p *Proc) error {
			rw.RLock()
			defer rw.RUnlock()
			a, err := read()
			if err != nil {
				return err
			}
			b, err := read()
			if err != nil {
				return err
			}
			if a != b {
				torn = append(torn, fmt.Sprintf("%s read %d then %d", p.Name(), a, b))
			}
			return nil
		}
		m.Manual("reader_a", 1, reader)
		m.Manual("reader_b", 1, reader)
		m.Manual("writer", 1, func(p *Proc) error {
			rw.Lock()
			defer rw.Unlock()
			_, err := db.Exec(`UPDATE "counters" SET "n"=$1 WHERE "id" = $2`, int64(1), "c")
			return err
		})
		m.AtQuiescence(func(s *State) error {
			if len(torn) > 0 {
				return fmt.Errorf("torn read: %s", strings.Join(torn, ", "))
			}
			return nil
		})
	})
}

// A reader takes the read lock again while a writer waits for the write lock.
// As with sync.RWMutex, the waiting writer keeps the second RLock out, and the
// writer waits for the first one: a deadlock.
func TestInjectedRWMutexRecursiveReadLock(t *testing.T) {
	Explore(t, func(t *testing.T, m *Model) {
		rw := m.RWMutex("cache")
		m.Manual("reader", 1, func(p *Proc) error {
			rw.RLock()
			defer rw.RUnlock()
			p.Step("calls a helper that takes the read lock again")
			rw.RLock()
			defer rw.RUnlock()
			return nil
		})
		m.Manual("writer", 1, func(p *Proc) error {
			rw.Lock()
			defer rw.Unlock()
			return nil
		})
		m.ExpectViolation("held by a waiting writer")
	})
}

func TestInjectedRWMutexUpgrade(t *testing.T) {
	Explore(t, func(t *testing.T, m *Model) {
		rw := m.RWMutex("m")
		m.Manual("upgrade", 1, func(p *Proc) error {
			rw.RLock()
			rw.Lock()
			return nil
		})
		m.ExpectViolation("cycle of waits")
	})
}

var (
	_ sync.Locker = (*Mutex)(nil)
	_ sync.Locker = (*RWMutex)(nil)
)
