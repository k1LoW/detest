package gormpool_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	gormmysql "gorm.io/driver/mysql"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"

	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/postgres"
	"github.com/k1LoW/detest/db/seam/gormpool"
)

func open(t *testing.T, s *detest.Sim, schema ...string) (*gorm.DB, *detest.DB) {
	t.Helper()
	return openOn(t, s, false, schema...)
}

func openOn(t *testing.T, s *detest.Sim, onMySQL bool, schema ...string) (*gorm.DB, *detest.DB) {
	t.Helper()
	srv := postgres.New()
	if onMySQL {
		srv = mysql.New(mysql.Collation("utf8mb4_bin"))
	}
	sqlDB, store := s.DB("app", srv)
	for _, q := range schema {
		if _, err := sqlDB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	dialector := gormpostgres.New(gormpostgres.Config{Conn: gormpool.New(sqlDB)})
	if onMySQL {
		dialector = gormmysql.New(gormmysql.Config{Conn: gormpool.New(sqlDB), SkipInitializeWithVersion: true})
	}
	gdb, err := gorm.Open(dialector, &gorm.Config{DisableAutomaticPing: true, Logger: glogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	return gdb, store
}

const counters = `CREATE TABLE counters (id varchar(16) PRIMARY KEY, n int NOT NULL)`

// failures collects the errors processes return, which detest only records
// in the trace, so that a test requires every explored schedule to succeed
// rather than one of them.
type failures struct {
	mu   sync.Mutex
	errs []error
}

func (f *failures) check(err error) error {
	// A process cut short by the end of its run, such as one crashed by
	// MaxCrashes, unwinds with this error, and nothing it does counts.
	if err != nil && !strings.Contains(err.Error(), "detest: the run ended") {
		f.mu.Lock()
		f.errs = append(f.errs, err)
		f.mu.Unlock()
	}
	return err
}

func (f *failures) wrap(fn func(p *detest.Proc) error) func(p *detest.Proc) error {
	return func(p *detest.Proc) error { return f.check(fn(p)) }
}

func (f *failures) report(t *testing.T) {
	t.Helper()
	if len(f.errs) > 0 {
		t.Errorf("%d runs failed, the first with: %v", len(f.errs), f.errs[0])
	}
}

// Goroutines sharing a GORM transaction take turns on its connection, and
// both orders of two updates of one row are explored. On the *sql.DB alone
// the order is the runtime's, and the exploration stops.
func TestGoroutinesSharingTxExplored(t *testing.T) {
	fails := &failures{}
	defer fails.report(t)
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		gdb, store := open(t, s, counters)
		s.Seed(func() { store.SeedRow("counters", detest.Row{"id": "c", "n": int64(0)}) })
		s.Manual("pod", 1, fails.wrap(func(p *detest.Proc) error {
			return gdb.WithContext(p.Context()).Transaction(func(tx *gorm.DB) error {
				var wg sync.WaitGroup
				errs := make([]error, 2)
				for i, n := range []int{1, 2} {
					wg.Go(func() { errs[i] = tx.Exec(`UPDATE counters SET n = ? WHERE id = 'c'`, n).Error })
				}
				wg.Wait()
				return errors.Join(errs...)
			})
		}))
		for _, n := range []int{1, 2} {
			s.Sometimes(fmt.Sprintf("n is %d", n), func(st *detest.State) bool {
				r, ok := st.Row(store, "counters", "c")
				return ok && r.Int("n") == n
			})
		}
	})
}

// A violation found through goroutines sharing a GORM transaction replays
// the same way, the order they took the connection in included.
func TestGoroutinesSharingTxReplay(t *testing.T) {
	fails := &failures{}
	defer fails.report(t)
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		gdb, store := open(t, s, counters)
		s.Seed(func() { store.SeedRow("counters", detest.Row{"id": "c", "n": int64(0)}) })
		s.Manual("pod", 1, fails.wrap(func(p *detest.Proc) error {
			return gdb.WithContext(p.Context()).Transaction(func(tx *gorm.DB) error {
				var wg sync.WaitGroup
				errs := make([]error, 3)
				for i, n := range []int{1, 2, 3} {
					wg.Go(func() { errs[i] = tx.Exec(`UPDATE counters SET n = ? WHERE id = 'c'`, n).Error })
				}
				wg.Wait()
				return errors.Join(errs...)
			})
		}))
		s.Always(func(st *detest.State) error {
			if r, ok := st.Row(store, "counters", "c"); ok && r.Int("n") == 2 {
				return errors.New("n is 2")
			}
			return nil
		})
		s.ExpectViolation("n is 2")
	}, detest.Workers(8))
}

// A goroutine sharing a GORM transaction waits for a row another process
// holds, rather than being refused, and the cycle it closes with that
// process is detected as a deadlock.
func TestGoroutineSharingTxWaitsAndDeadlocks(t *testing.T) {
	for _, onMySQL := range []bool{false, true} {
		t.Run(map[bool]string{false: "postgres", true: "mysql"}[onMySQL], func(t *testing.T) {
			testWaitsAndDeadlocks(t, onMySQL)
		})
	}
}

func testWaitsAndDeadlocks(t *testing.T, onMySQL bool) {
	fails := &failures{}
	defer fails.report(t)
	var mu sync.Mutex
	deadlocks, refused := 0, 0
	count := func(err error) error {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case errors.Is(err, detest.ErrDeadlock):
			deadlocks++
			return nil
		case errors.As(err, new(*detest.ErrUnsupportedSQL)):
			refused++
			return nil
		}
		return fails.check(err)
	}
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		gdb, store := openOn(t, s, onMySQL, counters)
		s.Seed(func() {
			store.SeedRow("counters", detest.Row{"id": "a", "n": int64(0)})
			store.SeedRow("counters", detest.Row{"id": "b", "n": int64(0)})
		})
		update := func(tx *gorm.DB, id string) error {
			return tx.Exec(`UPDATE counters SET n = n + 1 WHERE id = ?`, id).Error
		}
		s.Manual("x", 1, func(p *detest.Proc) error {
			return count(gdb.WithContext(p.Context()).Transaction(func(tx *gorm.DB) error {
				var err error
				var wg sync.WaitGroup
				wg.Go(func() {
					if err = update(tx, "a"); err == nil {
						err = update(tx, "b")
					}
				})
				wg.Wait()
				return err
			}))
		})
		s.Manual("y", 1, func(p *detest.Proc) error {
			return count(gdb.WithContext(p.Context()).Transaction(func(tx *gorm.DB) error {
				if err := update(tx, "b"); err != nil {
					return err
				}
				return update(tx, "a")
			}))
		})
	})
	if deadlocks == 0 {
		t.Error("no run detected the deadlock through the goroutine")
	}
	if refused > 0 {
		t.Errorf("%d runs refused a statement", refused)
	}
}

// An outbox poller locks a batch with SKIP LOCKED and deletes each row from
// a goroutine of its own, committing once they are done. Two pollers never
// send a row twice.
func TestOutboxPollers(t *testing.T) { testOutboxPollers(t) }

// A poller may crash while a goroutine of it holds the transaction's
// connection, and its batch is rolled back and taken by the other.
func TestOutboxPollersCrash(t *testing.T) { testOutboxPollers(t, detest.MaxCrashes(1)) }

func testOutboxPollers(t *testing.T, opts ...detest.Option) {
	fails := &failures{}
	defer fails.report(t)
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		gdb, store := open(t, s,
			`CREATE TABLE outbox (id varchar(16) PRIMARY KEY)`,
			`CREATE TABLE sent (id varchar(16) PRIMARY KEY)`)
		s.Seed(func() {
			for _, id := range []string{"m1", "m2", "m3"} {
				store.SeedRow("outbox", detest.Row{"id": id})
			}
		})
		s.Manual("poller", 1, fails.wrap(func(p *detest.Proc) error {
			return gdb.WithContext(p.Context()).Transaction(func(tx *gorm.DB) error {
				var ids []string
				if err := tx.Raw(`SELECT id FROM outbox ORDER BY id LIMIT 2 FOR UPDATE SKIP LOCKED`).Scan(&ids).Error; err != nil {
					return err
				}
				var wg sync.WaitGroup
				errs := make([]error, len(ids))
				for i, id := range ids {
					wg.Go(func() {
						if errs[i] = tx.Exec(`INSERT INTO sent (id) VALUES (?)`, id).Error; errs[i] == nil {
							errs[i] = tx.Exec(`DELETE FROM outbox WHERE id = ?`, id).Error
						}
					})
				}
				wg.Wait()
				return errors.Join(errs...)
			})
		}), detest.Instances(2))
		s.AtQuiescence(func(st *detest.State) error {
			if n := len(st.Rows(store, "outbox")) + len(st.Rows(store, "sent")); n != 3 {
				return fmt.Errorf("%d rows in outbox and sent, want 3", n)
			}
			return nil
		})
	}, opts...)
}

// The process that began the transaction runs a statement on it while its
// goroutine runs one, which stalls on the *sql.DB alone, as both park
// inside database/sql's locks.
func TestOwnerAlongsideGoroutine(t *testing.T) {
	fails := &failures{}
	defer fails.report(t)
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		gdb, store := open(t, s, `CREATE TABLE marks (id text PRIMARY KEY)`)
		s.Manual("pod", 1, fails.wrap(func(p *detest.Proc) error {
			return gdb.WithContext(p.Context()).Transaction(func(tx *gorm.DB) error {
				var err error
				var wg sync.WaitGroup
				wg.Go(func() { err = tx.Exec(`INSERT INTO marks (id) VALUES ('child')`).Error })
				ownErr := tx.Exec(`INSERT INTO marks (id) VALUES ('owner')`).Error
				wg.Wait()
				return errors.Join(err, ownErr)
			})
		}))
		s.AtQuiescence(func(st *detest.State) error {
			if n := len(st.Rows(store, "marks")); n != 2 {
				return fmt.Errorf("%d marks, want 2", n)
			}
			return nil
		})
	})
}

type mark struct{ ID string }

// GORM's own use of the pool works as on the *sql.DB: a create in GORM's
// default transaction, a nested transaction rolled back to its savepoint,
// and the *sql.DB GORM hands out.
func TestGORMOnPool(t *testing.T) {
	fails := &failures{}
	defer fails.report(t)
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		gdb, store := open(t, s, `CREATE TABLE marks (id text PRIMARY KEY)`)
		if _, err := gdb.DB(); err != nil {
			t.Fatal(err)
		}
		s.Manual("pod", 1, fails.wrap(func(p *detest.Proc) error {
			g := gdb.WithContext(p.Context())
			if err := g.Create(&mark{ID: "a"}).Error; err != nil {
				return err
			}
			return g.Transaction(func(tx *gorm.DB) error {
				if _, err := tx.DB(); err != nil {
					return err
				}
				if err := tx.Create(&mark{ID: "b"}).Error; err != nil {
					return err
				}
				_ = tx.Transaction(func(tx *gorm.DB) error {
					if err := tx.Create(&mark{ID: "c"}).Error; err != nil {
						return err
					}
					return errors.New("roll back to the savepoint")
				})
				return nil
			})
		}))
		s.AtQuiescence(func(st *detest.State) error {
			var ids []string
			for _, r := range st.Rows(store, "marks") {
				ids = append(ids, r.Str("id"))
			}
			if fmt.Sprint(ids) != "[a b]" {
				return fmt.Errorf("marks %v, want [a b]", ids)
			}
			return nil
		})
	})
}

// A query keeps the transaction's connection until its rows are closed. A
// goroutine whose statement got in while the rows were open would park in
// the driver holding database/sql's lock of the connection, and reading the
// rows would then stall the run.
func TestRowsHoldConnection(t *testing.T) {
	fails := &failures{}
	defer fails.report(t)
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		gdb, store := open(t, s, `CREATE TABLE marks (id text PRIMARY KEY)`)
		s.Seed(func() {
			store.SeedRow("marks", detest.Row{"id": "a"})
			store.SeedRow("marks", detest.Row{"id": "b"})
		})
		s.Manual("pod", 1, fails.wrap(func(p *detest.Proc) error {
			return gdb.WithContext(p.Context()).Transaction(func(tx *gorm.DB) error {
				rows, err := tx.Raw(`SELECT id FROM marks ORDER BY id`).Rows()
				if err != nil {
					return err
				}
				var childErr error
				var wg sync.WaitGroup
				wg.Go(func() { childErr = tx.Exec(`INSERT INTO marks (id) VALUES ('child')`).Error })
				// A statement outside the transaction lets the goroutine run
				// while the rows are open.
				if err := gdb.WithContext(p.Context()).Exec(`SELECT 1`).Error; err != nil {
					return err
				}
				var ids []string
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						return err
					}
					ids = append(ids, id)
				}
				if err := rows.Close(); err != nil {
					return err
				}
				wg.Wait()
				if fmt.Sprint(ids) != "[a b]" {
					return fmt.Errorf("read %v, want [a b]", ids)
				}
				return childErr
			})
		}))
		s.AtQuiescence(func(st *detest.State) error {
			if n := len(st.Rows(store, "marks")); n != 3 {
				return fmt.Errorf("%d marks, want 3", n)
			}
			return nil
		})
	})
}
