package detest

import (
	"errors"
	"fmt"
	"path/filepath"
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
	without, _ := exploreBubble(t, lastItemModel(true), []Option{EagerStart(false)}, nil, 0)
	with, _ := exploreBubble(t, lastItemModel(true), nil, nil, 0)
	for _, res := range []*result{without, with} {
		if res.Fatal != nil || res.Violated || !res.Complete {
			t.Fatalf("got %s", res.report())
		}
	}
	if with.Runs >= without.Runs {
		t.Fatalf("explored %d runs with EagerStart, %d without", with.Runs, without.Runs)
	}
	if !strings.Contains(with.report(), "EagerStart") || strings.Contains(without.report(), "EagerStart") {
		t.Errorf("the reports do not tell EagerStart apart:\n%s\n%s", with.report(), without.report())
	}
}

func TestEagerStartStillFindsTheRace(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		lastItemModel(false)(t, s)
		s.ExpectViolation("sold 2 of 1")
	})
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
				{MaxPreemptions(tc.preemptions), EagerStart(false)},
				{MaxPreemptions(tc.preemptions)},
			} {
				res, _ := exploreBubble(t, lastItemModel(false), opts, nil, 0)
				if res.Fatal != nil || res.Violated != tc.violated {
					t.Fatalf("with %d options, got %s", len(opts), res.report())
				}
			}
		})
	}
}

// firstIDModel declares two buyers whose orders take generated ids before
// their first yield point, and the rule that bob never gets the first one,
// which only the order of their starts breaks.
func firstIDModel(t *testing.T, s *Sim) {
	db, _ := s.DB("app", postgres.New())
	if _, err := db.Exec(`CREATE TABLE orders (id serial PRIMARY KEY, buyer text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int{}
	s.Seed(func() { clear(ids) })
	for _, buyer := range []string{"alice", "bob"} {
		s.Manual(buyer, 1, func(p *Proc) error {
			var id int
			if err := db.QueryRowContext(p.Context(), `INSERT INTO orders (buyer) VALUES ($1) RETURNING id`, buyer).Scan(&id); err != nil {
				return err
			}
			ids[buyer] = id
			return nil
		})
	}
	s.AtQuiescence(func(*State) error {
		if ids["bob"] == 1 {
			return errors.New("bob took the first id")
		}
		return nil
	})
}

// Which id a start draws depends on where it starts, so the exploration
// starts over without EagerStart, finds the order that breaks the rule, and
// prints a schedule that replays without it in a test that leaves it on.
func TestEagerStartDroppedWhenAStartDrawsAValue(t *testing.T) {
	res, _ := explore(t, firstIDModel, nil)
	if !res.Violated || res.lazy == "" || !strings.HasPrefix(res.Schedule, lazyPrefix) {
		t.Fatalf("got %s", res.report())
	}
	if !strings.Contains(res.report(), "explored without EagerStart, as alice#1 drew a sequence") {
		t.Errorf("the report does not say why EagerStart was dropped:\n%s", res.report())
	}
	replayed, _ := explore(t, firstIDModel, []Option{Replay(res.Schedule)})
	if !replayed.Violated || replayed.Schedule != res.Schedule {
		t.Fatalf("replaying %s got %s", res.Schedule, replayed.report())
	}
}

// A checkpoint of an exploration that dropped EagerStart resumes without it.
func TestEagerStartDroppedAcrossACheckpoint(t *testing.T) {
	ckpt := filepath.Join(t.TempDir(), "ckpt.json")
	t.Setenv("DETEST_CHECKPOINT", ckpt)
	model := func(t *testing.T, s *Sim) {
		firstIDModel(t, s)
		s.ExpectViolation("bob took the first id")
	}
	first, _ := explore(t, model, []Option{MaxRuns(1)})
	if first.Violated || first.Checkpoint == "" || first.lazy == "" {
		t.Fatalf("got %s", first.report())
	}
	rest, _ := explore(t, model, nil)
	if !rest.Violated || rest.lazy == "" {
		t.Fatalf("resumed, got %s", rest.report())
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
	})
}

// A sequence may be named uuid, and its draw is no less a draw.
func TestEagerStartTellsASequenceNamedUUIDFromUUIDs(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE SEQUENCE uuid`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE orders (id bigint PRIMARY KEY DEFAULT nextval('uuid'), buyer text NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Manual("alice", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `INSERT INTO orders (buyer) VALUES ('alice')`)
			return err
		})
	}, nil)
	if res.Fatal != nil || res.lazy == "" {
		t.Fatalf("got %v, %s", res.Fatal, res.report())
	}
}

// A run that made no choice prints "lazy:" alone once EagerStart was
// dropped, which counts no choice and replays.
func TestEagerStartDroppedOnARunWithoutChoices(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE orders (id serial PRIMARY KEY, buyer text NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Manual("alice", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `INSERT INTO orders (buyer) VALUES ('alice')`)
			return err
		})
		s.AtQuiescence(func(*State) error { return errors.New("always broken") })
	}
	res, _ := explore(t, model, nil)
	if !res.Violated || res.Schedule != lazyPrefix || !strings.Contains(res.report(), "(0 choices)") {
		t.Fatalf("got %s", res.report())
	}
	replayed, _ := explore(t, model, []Option{Replay(res.Schedule)})
	if replayed.Fatal != nil || !replayed.Violated {
		t.Fatalf("replaying %q got %v, %s", res.Schedule, replayed.Fatal, replayed.report())
	}
}
