package detest

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
	"github.com/k1LoW/detest/mysql"
	"github.com/k1LoW/detest/postgres"
)

// A statement waiting for a row lock whose context ends is refused, as the
// server goes on running it once the row is free, and the run goes on.
func TestStatementWaitingForLockCanceledByDeadline(t *testing.T) {
	for _, srv := range []Server{postgres.New(), mysql.New()} {
		t.Run(fmt.Sprintf("%T", srv), func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, store := s.DB("app", srv)
				if _, err := db.Exec(`CREATE TABLE counters (id bigint PRIMARY KEY, n bigint NOT NULL)`); err != nil {
					t.Fatal(err)
				}
				var waitErr error
				s.Seed(func() {
					waitErr = nil
					store.SeedRow("counters", Row{"id": int64(1), "n": int64(0)})
				})
				s.Manual("holder", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					if _, err := tx.Exec(`UPDATE counters SET n = 1 WHERE id = 1`); err != nil {
						_ = tx.Rollback()
						return err
					}
					time.Sleep(time.Minute)
					return tx.Commit()
				})
				s.Manual("waiter", 1, func(p *Proc) error {
					ctx, cancel := context.WithTimeout(p.Context(), time.Second)
					defer cancel()
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if _, err := tx.ExecContext(ctx, `UPDATE counters SET n = 2 WHERE id = 1`); err != nil {
						waitErr = err
						return err
					}
					return tx.Commit()
				})
				s.Sometimes("the waiting statement was refused when its deadline passed", func(*State) bool {
					_, ok := errors.AsType[*ErrUnsupportedSQL](waitErr)
					return ok
				})
				s.AtQuiescence(func(st *State) error {
					row, _ := st.Row(store, "counters", "1")
					if waitErr != nil && row.Int64("n") != 1 {
						return fmt.Errorf("n = %d after the waiter was canceled", row.Int64("n"))
					}
					return nil
				})
			})
		})
	}
}

// A goroutine's statement parked at its yield point is refused when a
// sibling cancels the group's context, as the server goes on running it, its
// transaction rolls back, and the run goes on.
func TestStatementAtYieldCanceledBySibling(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		var workErr error
		s.Seed(func() {
			workErr = nil
			store.SeedRow("counters", Row{"id": "c", "n": int64(0)})
		})
		s.Manual("pod", 1, func(p *Proc) error {
			ctx, cancel := context.WithCancel(p.Context())
			defer cancel()
			work := func(fail bool) error {
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				if fail {
					return errors.New("boom")
				}
				for range 3 {
					if _, err := tx.ExecContext(ctx, `UPDATE "counters" SET "n" = "n" + 1 WHERE "id" = 'c'`); err != nil {
						return err
					}
				}
				return tx.Commit()
			}
			var wg sync.WaitGroup
			for _, fail := range []bool{false, true} {
				wg.Go(func() {
					err := work(fail)
					if !fail {
						workErr = err
					}
					if err != nil {
						cancel()
					}
				})
			}
			wg.Wait()
			return nil
		})
		s.Sometimes("the parked statement was refused when its context ended", func(*State) bool {
			_, ok := errors.AsType[*ErrUnsupportedSQL](workErr)
			return ok
		})
		s.AtQuiescence(func(st *State) error {
			row, _ := st.Row(store, "counters", "c")
			if n := row.Int64("n"); n != 0 && n != 3 {
				return fmt.Errorf("n = %d, a canceled transaction committed in part", n)
			}
			return nil
		})
	})
}

// Goroutines that run statements on their process's *sql.Tx, as in an
// errgroup inside a transaction, run them in that transaction, which their
// process then commits.
func TestGoroutinesShareSQLTx(t *testing.T) {
	for _, srv := range []Server{postgres.New(), mysql.New()} {
		t.Run(fmt.Sprintf("%T", srv), func(t *testing.T) { testGoroutinesShareSQLTx(t, srv) })
	}
}

func testGoroutinesShareSQLTx(t *testing.T, srv Server) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", srv)
		if _, err := db.Exec(`CREATE TABLE marks (id bigint PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		s.Manual("pod", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var wg sync.WaitGroup
			errs := make([]error, 2)
			insert := "INSERT INTO marks (id) VALUES ($1)"
			if sqlir.ImplOf(srv).InnoDB() {
				insert = "INSERT INTO marks (id) VALUES (?)"
			}
			for i, id := range []int64{1, 2} {
				wg.Go(func() {
					_, errs[i] = tx.ExecContext(p.Context(), insert, id)
				})
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.AtQuiescence(func(st *State) error {
			for _, id := range []string{"1", "2"} {
				if _, ok := st.Row(store, "marks", id); !ok {
					return fmt.Errorf("mark %s missing", id)
				}
			}
			return nil
		})
	}, nil, nil, 0)
	if res.Violated || res.Fatal != nil {
		t.Fatalf("unexpected result: %s", res.report())
	}
	if res.Shared == 0 {
		t.Error("no statement was counted as run on a shared transaction")
	}
	if !strings.Contains(res.report(), "sharing a transaction") {
		t.Errorf("the report does not state the bound:\n%s", res.report())
	}
}

// A goroutine's statement on its process's transaction that would wait for
// a lock another process holds is refused, as it cannot park while
// database/sql holds the transaction's locks.
func TestGoroutineOnSharedSQLTxRefusedToWait(t *testing.T) {
	for _, srv := range []Server{postgres.New(), mysql.New()} {
		t.Run(fmt.Sprintf("%T", srv), func(t *testing.T) { testGoroutineOnSharedSQLTxRefusedToWait(t, srv) })
	}
}

func testGoroutineOnSharedSQLTxRefusedToWait(t *testing.T, srv Server) {
	var mu sync.Mutex
	var refused bool
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", srv)
		if _, err := db.Exec(`CREATE TABLE counters (id bigint PRIMARY KEY, n bigint NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Seed(func() { store.SeedRow("counters", Row{"id": int64(1), "n": int64(0)}) })
		s.Manual("holder", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE counters SET n = 1 WHERE id = 1`); err != nil {
				_ = tx.Rollback()
				return err
			}
			p.Step("hold the row")
			return tx.Commit()
		})
		s.Manual("pod", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var gerr error
			var wg sync.WaitGroup
			wg.Go(func() {
				_, gerr = tx.Exec(`UPDATE counters SET n = 2 WHERE id = 1`)
			})
			wg.Wait()
			if _, ok := errors.AsType[*ErrUnsupportedSQL](gerr); ok {
				mu.Lock()
				refused = true
				mu.Unlock()
			}
			if gerr != nil {
				return gerr
			}
			return tx.Commit()
		})
	})
	if !refused {
		t.Error("no run refused the goroutine's wait as unsupported")
	}
}

// A transaction whose context ends while its process sleeps between two
// statements is rolled back without a step, whichever of the process and
// database/sql's own goroutine gets to roll it back first, so every replay
// takes the same path.
func TestIdleTransactionCanceledBySibling(t *testing.T) {
	for range 20 {
		Explore(t, func(t *testing.T, s *Sim) {
			db, store := s.DB("app", postgres.New())
			s.Seed(func() { store.SeedRow("counters", Row{"id": "c", "n": int64(0)}) })
			s.Manual("pod", 1, func(p *Proc) error {
				ctx, cancel := context.WithCancel(p.Context())
				defer cancel()
				var wg sync.WaitGroup
				wg.Go(func() {
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						return
					}
					defer func() { _ = tx.Rollback() }()
					if _, err := tx.ExecContext(ctx, `UPDATE "counters" SET "n" = 1 WHERE "id" = 'c'`); err != nil {
						return
					}
					time.Sleep(time.Second)
					_, _ = tx.ExecContext(ctx, `UPDATE "counters" SET "n" = 2 WHERE "id" = 'c'`)
				})
				wg.Go(func() {
					p.Step("give up")
					cancel()
				})
				wg.Wait()
				_, err := db.Exec(`UPDATE "counters" SET "n" = 3 WHERE "id" = 'c'`)
				return err
			})
			s.AtQuiescence(func(st *State) error {
				row, _ := st.Row(store, "counters", "c")
				if n := row.Int64("n"); n != 3 {
					return fmt.Errorf("n = %d", n)
				}
				return nil
			})
		})
	}
}

// The stall report names database/sql's own locks when a goroutine waits for
// one, rather than a mutex of the application.
func TestOnSQLLock(t *testing.T) {
	for _, tc := range []struct {
		stack string
		want  bool
	}{
		{"goroutine 7 [sync.Mutex.Lock, synctest bubble 1]:\nsync.(*Mutex).Lock(...)\ndatabase/sql.withLock({0x1, 0x2}, 0x3)\ndatabase/sql.(*DB).execDC(...)", true},
		{"goroutine 8 [sync.RWMutex.Lock, synctest bubble 1]:\nsync.(*RWMutex).Lock(0x1)\ndatabase/sql.(*Tx).rollback(0x1, 0x1)\ndatabase/sql.(*Tx).awaitDone(0x1)", true},
		{"goroutine 9 [sync.Mutex.Lock, synctest bubble 1]:\nsync.(*Mutex).Lock(...)\nexample.com/app.(*Cache).Get(...)", false},
	} {
		if got := onSQLLock(tc.stack); got != tc.want {
			t.Errorf("onSQLLock(%q) = %v, want %v", tc.stack, got, tc.want)
		}
	}
}

// Goroutines sharing a transaction that update the same row in one step
// leave the committed value to the order database/sql hands them the
// connection in, which a replay does not repeat, so the exploration stops.
func TestGoroutinesSharingSQLTxOnOneRowStop(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE counters (id bigint PRIMARY KEY, n bigint NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Seed(func() { store.SeedRow("counters", Row{"id": int64(1), "n": int64(0)}) })
		s.Manual("pod", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var wg sync.WaitGroup
			for _, n := range []int64{1, 2} {
				wg.Go(func() { _, _ = tx.Exec(`UPDATE counters SET n = $1 WHERE id = 1`, n) })
			}
			wg.Wait()
			return tx.Commit()
		})
	}, nil, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "depends on their order") {
		t.Fatalf("want the exploration stopped for order-dependent statements, got:\n%s", res.report())
	}
}

// An outbox poller locks rows with SKIP LOCKED and has a goroutine per row
// delete it by its key, which commute, so the exploration goes on.
func TestOutboxGoroutinesDeleteSharedSQLTxRows(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE outbox (id bigint PRIMARY KEY, payload text NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Seed(func() {
			for i := range int64(3) {
				store.SeedRow("outbox", Row{"id": i + 1, "payload": "m"})
			}
		})
		poll := func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.Query(`SELECT id FROM outbox ORDER BY id LIMIT 2 FOR UPDATE SKIP LOCKED`)
			if err != nil {
				return err
			}
			var ids []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
			}
			if err := rows.Close(); err != nil {
				return err
			}
			var wg sync.WaitGroup
			errs := make([]error, len(ids))
			for i, id := range ids {
				wg.Go(func() { _, errs[i] = tx.Exec(`DELETE FROM outbox WHERE id = $1`, id) })
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				return err
			}
			return tx.Commit()
		}
		s.Manual("poller", 2, poll)
		s.AtQuiescence(func(st *State) error {
			if n := len(st.Rows(store, "outbox")); n > 1 {
				return fmt.Errorf("%d rows left after two polls of two", n)
			}
			return nil
		})
	})
}

// Goroutines sharing a transaction whose statements take values from a
// sequence or a generator in one step get them in the order database/sql
// hands them the connection, so the exploration stops, even when each
// inserts a row of its own.
func TestGoroutinesSharingSQLTxGeneratedValuesStop(t *testing.T) {
	for name, stmt := range map[string]string{
		"default": `INSERT INTO marks (id) VALUES ($1)`,
		"select":  `SELECT nextval('s') + $1`,
	} {
		t.Run(name, func(t *testing.T) {
			res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", postgres.New())
				for _, ddl := range []string{
					`CREATE SEQUENCE s`,
					`CREATE TABLE marks (id bigint PRIMARY KEY, n bigint NOT NULL DEFAULT nextval('s') + 1)`,
				} {
					if _, err := db.Exec(ddl); err != nil {
						t.Fatal(err)
					}
				}
				s.Manual("pod", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					var wg sync.WaitGroup
					for _, id := range []int64{1, 2} {
						wg.Go(func() {
							rows, err := tx.Query(stmt, id)
							if err == nil {
								_ = rows.Close()
							}
						})
					}
					wg.Wait()
					return tx.Commit()
				})
			}, nil, nil, 0)
			if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "depends on their order") {
				t.Fatalf("want the exploration stopped for order-dependent statements, got:\n%s", res.report())
			}
		})
	}
}

// Goroutines sharing a transaction that update rows of their own in a table
// with a unique constraint may each decide whether the other's update fails,
// which their keys do not tell, so the exploration stops.
func TestGoroutinesSharingSQLTxUniqueUpdatesStop(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE u (id bigint PRIMARY KEY, code text NOT NULL UNIQUE)`); err != nil {
			t.Fatal(err)
		}
		s.Seed(func() {
			store.SeedRow("u", Row{"id": int64(1), "code": "a"})
			store.SeedRow("u", Row{"id": int64(2), "code": "b"})
		})
		s.Manual("pod", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var wg sync.WaitGroup
			wg.Go(func() { _, _ = tx.Exec(`UPDATE u SET code = 'c' WHERE id = 1`) })
			wg.Go(func() { _, _ = tx.Exec(`UPDATE u SET code = 'a' WHERE id = 2`) })
			wg.Wait()
			return tx.Commit()
		})
	}, nil, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "depends on their order") {
		t.Fatalf("want the exploration stopped for order-dependent statements, got:\n%s", res.report())
	}
}

// A process that runs a statement without a yield point, such as a SET,
// after its goroutines sharing its transaction finished goes on, as the
// WaitGroup orders it after them.
func TestSetAfterGoroutinesSharingSQLTx(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE marks (id bigint PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		s.Manual("pod", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var wg sync.WaitGroup
			for _, id := range []int64{1, 2} {
				wg.Go(func() { _, _ = tx.Exec(`INSERT INTO marks (id) VALUES ($1)`, id) })
			}
			wg.Wait()
			if _, err := tx.Exec(`SET LOCAL lock_timeout = '1s'`); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.AtQuiescence(func(st *State) error {
			if n := len(st.Rows(store, "marks")); n != 2 {
				return fmt.Errorf("%d marks", n)
			}
			return nil
		})
	})
}

// Goroutines sharing a transaction whose writes to rows of their own run a
// CHECK that changes a sequence leave its value to their order, so the
// exploration stops.
func TestGoroutinesSharingSQLTxEffectfulCheckStop(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		for _, ddl := range []string{
			`CREATE SEQUENCE s`,
			`CREATE TABLE marks (id bigint PRIMARY KEY, CHECK (setval('s', id) > 0))`,
		} {
			if _, err := db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
		}
		s.Manual("pod", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var wg sync.WaitGroup
			for _, id := range []int64{1, 2} {
				wg.Go(func() { _, _ = tx.Exec(`INSERT INTO marks (id) VALUES ($1)`, id) })
			}
			wg.Wait()
			return tx.Commit()
		})
	}, nil, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "depends on their order") {
		t.Fatalf("want the exploration stopped for order-dependent statements, got:\n%s", res.report())
	}
}

// A process that inserts on another connection right after waking a
// goroutine that inserts on their shared transaction takes its sequence
// value after the goroutine's, in every run, rather than in the order the
// Go runtime hands out the engine.
func TestProcessInsertAfterGoroutineOnSharedSQLTx(t *testing.T) {
	for range 10 {
		Explore(t, func(t *testing.T, s *Sim) {
			db, store := s.DB("app", postgres.New())
			if _, err := db.Exec(`CREATE TABLE marks (id serial PRIMARY KEY, who text NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			s.Manual("pod", 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				var wg sync.WaitGroup
				wg.Go(func() { _, _ = tx.Exec(`INSERT INTO marks (who) VALUES ('goroutine')`) })
				if _, err := db.Exec(`INSERT INTO marks (who) VALUES ('process')`); err != nil {
					return err
				}
				wg.Wait()
				return tx.Commit()
			})
			s.AtQuiescence(func(st *State) error {
				for _, r := range st.Rows(store, "marks") {
					if r.Str("who") == "goroutine" && r.Int64("id") != 1 {
						return fmt.Errorf("the goroutine's insert got id %d", r.Int64("id"))
					}
				}
				return nil
			})
		})
	}
}

// A goroutine that outlives the process whose transaction it shares, which
// rolls the transaction back as it returns, finds the connection gone
// rather than changing the closed transaction.
func TestGoroutineOnSharedSQLTxAfterItsProcessReturned(t *testing.T) {
	var mu sync.Mutex
	var errs []error
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		s.Manual("pod", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`SAVEPOINT a`); err != nil {
				return err
			}
			go func() {
				time.Sleep(time.Second)
				_, err := tx.Exec(`ROLLBACK TO SAVEPOINT a`)
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}()
			return nil
		})
		// Keeps the run going while the goroutine wakes.
		s.Manual("other", 1, func(p *Proc) error {
			time.Sleep(2 * time.Second)
			return nil
		})
	})
	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 {
		t.Fatal("the goroutine never ran its statement")
	}
	for _, err := range errs {
		if !errors.Is(err, driver.ErrBadConn) {
			t.Errorf("a statement on the transaction of a process that returned got %v, want driver.ErrBadConn", err)
		}
	}
}
