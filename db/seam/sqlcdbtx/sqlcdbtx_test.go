package sqlcdbtx_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/db/postgres"
	"github.com/k1LoW/detest/db/seam/sqlcdbtx"
)

// DBTX, Queries and New are as sqlc generates them for database/sql.
type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	PrepareContext(context.Context, string) (*sql.Stmt, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var (
	_ DBTX = (*sqlcdbtx.DB)(nil)
	_ DBTX = (*sqlcdbtx.Tx)(nil)
)

type Queries struct{ db DBTX }

func New(db DBTX) *Queries { return &Queries{db: db} }

func (q *Queries) SetCounter(ctx context.Context, id string, n int) error {
	_, err := q.db.ExecContext(ctx, `UPDATE counters SET n = $1 WHERE id = $2`, n, id)
	return err
}

func (q *Queries) BumpCounter(ctx context.Context, id string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = $1`, id)
	return err
}

func (q *Queries) GetCounter(ctx context.Context, id string) (int, error) {
	var n int
	err := q.db.QueryRowContext(ctx, `SELECT n FROM counters WHERE id = $1`, id).Scan(&n)
	return n, err
}

// Tx and Store are the application's: it begins its transactions through
// begin, which is the *sql.DB's BeginTx in production, and builds its
// queries on a transaction with New.
type Tx interface {
	DBTX
	Commit() error
	Rollback() error
}

type Store struct {
	begin func(ctx context.Context) (Tx, error)
}

func (s *Store) inTx(ctx context.Context, fn func(q *Queries) error) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(New(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func open(t *testing.T, s *detest.Sim) (*Store, *detest.DB) {
	t.Helper()
	sqlDB, store := s.DB("app", postgres.New())
	if _, err := sqlDB.Exec(`CREATE TABLE counters (id text PRIMARY KEY, n int NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	pool := sqlcdbtx.New(sqlDB)
	return &Store{begin: func(ctx context.Context) (Tx, error) { return pool.BeginTx(ctx, nil) }}, store
}

// failures collects the errors processes return, which detest only records
// in the trace, so that a test requires every explored schedule to succeed.
type failures struct {
	mu   sync.Mutex
	errs []error
}

func (f *failures) check(err error) error {
	// A process cut short by the end of its run unwinds with this error,
	// and nothing it does counts.
	if err != nil && !strings.Contains(err.Error(), "detest: the run ended") {
		f.mu.Lock()
		f.errs = append(f.errs, err)
		f.mu.Unlock()
	}
	return err
}

func (f *failures) report(t *testing.T) {
	t.Helper()
	if len(f.errs) > 0 {
		t.Errorf("%d runs failed, the first with: %v", len(f.errs), f.errs[0])
	}
}

// Goroutines sharing a transaction through the queries sqlc generates take
// turns on its connection, and both orders of two updates of one row are
// explored.
func TestGoroutinesSharingTxExplored(t *testing.T) {
	fails := &failures{}
	defer fails.report(t)
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		st, store := open(t, s)
		s.Seed(func() { store.SeedRow("counters", detest.Row{"id": "c", "n": int64(0)}) })
		s.Manual("pod", 1, func(p *detest.Proc) error {
			ctx := p.Context()
			return fails.check(st.inTx(ctx, func(q *Queries) error {
				var wg sync.WaitGroup
				errs := make([]error, 2)
				for i, n := range []int{1, 2} {
					wg.Go(func() { errs[i] = q.SetCounter(ctx, "c", n) })
				}
				wg.Wait()
				if err := errors.Join(errs...); err != nil {
					return err
				}
				_, err := q.GetCounter(ctx, "c")
				return err
			}))
		})
		for _, n := range []int{1, 2} {
			s.Sometimes(fmt.Sprintf("n is %d", n), func(st *detest.State) bool {
				r, ok := st.Row(store, "counters", "c")
				return ok && r.Int("n") == n
			})
		}
	})
}

// A goroutine sharing a transaction waits for a row another process holds,
// rather than being refused, and the cycle it closes with that process is
// detected as a deadlock.
func TestGoroutineSharingTxWaitsAndDeadlocks(t *testing.T) {
	fails := &failures{}
	defer fails.report(t)
	var mu sync.Mutex
	deadlocks := 0
	count := func(err error) error {
		if errors.Is(err, detest.ErrDeadlock) {
			mu.Lock()
			deadlocks++
			mu.Unlock()
			return nil
		}
		return fails.check(err)
	}
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		st, store := open(t, s)
		s.Seed(func() {
			store.SeedRow("counters", detest.Row{"id": "a", "n": int64(0)})
			store.SeedRow("counters", detest.Row{"id": "b", "n": int64(0)})
		})
		s.Manual("x", 1, func(p *detest.Proc) error {
			ctx := p.Context()
			return count(st.inTx(ctx, func(q *Queries) error {
				var err error
				var wg sync.WaitGroup
				wg.Go(func() {
					if err = q.BumpCounter(ctx, "a"); err == nil {
						err = q.BumpCounter(ctx, "b")
					}
				})
				wg.Wait()
				return err
			}))
		})
		s.Manual("y", 1, func(p *detest.Proc) error {
			ctx := p.Context()
			return count(st.inTx(ctx, func(q *Queries) error {
				if err := q.BumpCounter(ctx, "b"); err != nil {
					return err
				}
				return q.BumpCounter(ctx, "a")
			}))
		})
	})
	if deadlocks == 0 {
		t.Error("no run detected the deadlock through the goroutine")
	}
}
