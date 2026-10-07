package sqlxext_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/db/postgres"
	"github.com/k1LoW/detest/db/seam/sqlxext"
)

// Tx and Store are the application's: it begins its transactions through
// begin, which is the *sqlx.DB's BeginTxx in production, and hands the
// transaction on as an interface.
type Tx interface {
	sqlx.ExtContext
	GetContext(ctx context.Context, dest any, query string, args ...any) error
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
	NamedExecContext(ctx context.Context, query string, arg any) (sql.Result, error)
	Commit() error
	Rollback() error
}

var _ Tx = (*sqlxext.Tx)(nil)

type Store struct {
	begin func(ctx context.Context) (Tx, error)
}

func (s *Store) inTx(ctx context.Context, fn func(tx Tx) error) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
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
	db := sqlxext.New(sqlDB, "postgres")
	return &Store{begin: func(ctx context.Context) (Tx, error) { return db.BeginTxx(ctx, nil) }}, store
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

// Goroutines sharing a transaction through sqlx take turns on its
// connection, and both orders of two updates of one row are explored. The
// reads go through GetContext and SelectContext.
func TestGoroutinesSharingTxExplored(t *testing.T) {
	fails := &failures{}
	defer fails.report(t)
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		st, store := open(t, s)
		s.Seed(func() { store.SeedRow("counters", detest.Row{"id": "c", "n": int64(0)}) })
		s.Manual("pod", 1, func(p *detest.Proc) error {
			ctx := p.Context()
			return fails.check(st.inTx(ctx, func(tx Tx) error {
				var wg sync.WaitGroup
				errs := make([]error, 2)
				for i, n := range []int{1, 2} {
					wg.Go(func() {
						var cur int
						if errs[i] = tx.GetContext(ctx, &cur, `SELECT n FROM counters WHERE id = $1`, "c"); errs[i] == nil {
							_, errs[i] = tx.ExecContext(ctx, tx.Rebind(`UPDATE counters SET n = ? WHERE id = ?`), n, "c")
						}
					})
				}
				wg.Wait()
				if err := errors.Join(errs...); err != nil {
					return err
				}
				var ns []int
				return tx.SelectContext(ctx, &ns, `SELECT n FROM counters`)
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
// detected as a deadlock. The goroutine writes with NamedExecContext.
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
	type bump struct{ ID string }
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		st, store := open(t, s)
		s.Seed(func() {
			store.SeedRow("counters", detest.Row{"id": "a", "n": int64(0)})
			store.SeedRow("counters", detest.Row{"id": "b", "n": int64(0)})
		})
		s.Manual("x", 1, func(p *detest.Proc) error {
			ctx := p.Context()
			return count(st.inTx(ctx, func(tx Tx) error {
				var err error
				var wg sync.WaitGroup
				wg.Go(func() {
					for _, id := range []string{"a", "b"} {
						if _, err = tx.NamedExecContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = :id`, bump{ID: id}); err != nil {
							return
						}
					}
				})
				wg.Wait()
				return err
			}))
		})
		s.Manual("y", 1, func(p *detest.Proc) error {
			ctx := p.Context()
			return count(st.inTx(ctx, func(tx Tx) error {
				for _, id := range []string{"b", "a"} {
					if _, err := tx.ExecContext(ctx, `UPDATE counters SET n = n + 1 WHERE id = $1`, id); err != nil {
						return err
					}
				}
				return nil
			}))
		})
	})
	if deadlocks == 0 {
		t.Error("no run detected the deadlock through the goroutine")
	}
}
