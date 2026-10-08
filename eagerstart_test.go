package detest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// lastItemModel declares two buyers of the last item. Without the lock both
// may read it in stock and sell it twice.
func lastItemModel(lock bool) func(t *testing.T, s *Sim) {
	return func(t *testing.T, s *Sim) {
		db, _ := s.DB("shop", postgres.New())
		if _, err := db.Exec(`CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Seed(func() {
			if _, err := db.Exec(`INSERT INTO stock VALUES ('apple', 1)`); err != nil {
				t.Fatal(err)
			}
		})
		read := `SELECT n FROM stock WHERE sku = 'apple'`
		if lock {
			read += ` FOR UPDATE`
		}
		sold := 0
		s.Seed(func() { sold = 0 })
		for _, buyer := range []string{"alice", "bob"} {
			s.Manual(buyer, 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				var n int
				if err := tx.QueryRowContext(p.Context(), read).Scan(&n); err != nil {
					return err
				}
				if n == 0 {
					return nil
				}
				if _, err := tx.ExecContext(p.Context(), `UPDATE stock SET n = $1 WHERE sku = 'apple'`, n-1); err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				sold++
				return nil
			})
		}
		s.AtQuiescence(func(*State) error {
			if sold > 1 {
				return fmt.Errorf("sold %d of 1", sold)
			}
			return nil
		})
	}
}

func TestEagerStartLeavesOutWhereProcessesStart(t *testing.T) {
	without, _ := exploreBubble(t, lastItemModel(true), nil, nil, 0)
	with, _ := exploreBubble(t, lastItemModel(true), []Option{EagerStart()}, nil, 0)
	for _, res := range []*result{without, with} {
		if res.Fatal != nil || res.Violated || !res.Complete {
			t.Fatalf("got %s", res.report())
		}
	}
	if with.Runs >= without.Runs {
		t.Fatalf("explored %d runs with EagerStart, %d without", with.Runs, without.Runs)
	}
	if !strings.Contains(with.report(), "EagerStart") {
		t.Errorf("the report does not say EagerStart applied: %s", with.report())
	}
}

func TestEagerStartStillFindsTheRace(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		lastItemModel(false)(t, s)
		s.ExpectViolation("sold 2 of 1")
	}, EagerStart())
}

// Selling twice takes one switch away from a buyer that could go on, so a
// start that spent the budget would hide it.
func TestEagerStartSpendsNoPreemption(t *testing.T) {
	for _, tc := range []struct {
		preemptions int
		violated    bool
	}{{0, false}, {1, true}} {
		t.Run(fmt.Sprint(tc.preemptions), func(t *testing.T) {
			for _, opts := range [][]Option{
				{MaxPreemptions(tc.preemptions)},
				{MaxPreemptions(tc.preemptions), EagerStart()},
			} {
				res, _ := exploreBubble(t, lastItemModel(false), opts, nil, 0)
				if res.Fatal != nil || res.Violated != tc.violated {
					t.Fatalf("with %d options, got %s", len(opts), res.report())
				}
			}
		})
	}
}

// A start that draws a generated key would get another key wherever else it
// started, which EagerStart would leave unexplored.
func TestEagerStartRefusesADrawBeforeTheFirstYield(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE orders (id serial PRIMARY KEY, buyer text NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for _, buyer := range []string{"alice", "bob"} {
			s.Manual(buyer, 1, func(p *Proc) error {
				_, err := db.ExecContext(p.Context(), `INSERT INTO orders (buyer) VALUES ($1)`, buyer)
				return err
			})
		}
	}, []Option{EagerStart()}, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "alice#1 drew a sequence") {
		t.Fatalf("got %v, want EagerStart refusing alice's draw", res.Fatal)
	}
}

// After and When read state, so their processes start where the schedule
// picks, as without EagerStart.
func TestEagerStartLeavesAfterToTheSchedule(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE flags (id text PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		s.Manual("setter", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `INSERT INTO flags VALUES ('set')`)
			return err
		})
		s.Manual("reader", 1, func(p *Proc) error {
			var n int
			if err := db.QueryRowContext(p.Context(), `SELECT count(*) FROM flags`).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return errors.New("read no flag after it was set")
			}
			return nil
		}, After(func(st *State) bool { return len(st.Rows(store, "flags")) > 0 }))
	}, EagerStart())
}
