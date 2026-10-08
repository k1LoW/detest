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
	if !strings.Contains(res.report(), "explored without EagerStart, as alice#1 moved a sequence") {
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

// A process's name is numbered within its type, so which name it gets does
// not depend on where the processes of other types started, which EagerStart
// fixes and branching on starts varies.
func TestProcessNamesDoNotDependOnStartOrder(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprint(on), func(t *testing.T) {
			res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
				db, store := s.DB("app", postgres.New())
				if _, err := db.Exec(`CREATE TABLE owners (name text PRIMARY KEY)`); err != nil {
					t.Fatal(err)
				}
				for _, typ := range []string{"alice", "bob"} {
					s.Manual(typ, 1, func(p *Proc) error {
						p.Step("before reading its name")
						_, err := db.ExecContext(p.Context(), `INSERT INTO owners VALUES ($1)`, p.Name())
						return err
					})
				}
				s.AtQuiescence(func(st *State) error {
					for _, want := range []string{"alice#1", "bob#1"} {
						if _, ok := st.Row(store, "owners", want); !ok {
							return fmt.Errorf("no owner %s", want)
						}
					}
					return nil
				})
			}, []Option{EagerStart(on)}, nil, 0)
			if res.Fatal != nil || res.Violated || !res.Complete {
				t.Fatalf("got %s", res.report())
			}
		})
	}
}

// A goroutine a start sets going reaches its first call within the start, so
// a generated key it draws there counts as the start's.
func TestEagerStartCountsADrawOfAGoroutineItStarted(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE orders (id serial PRIMARY KEY, buyer text NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for _, buyer := range []string{"alice", "bob"} {
			s.Manual(buyer, 1, func(p *Proc) error {
				done := make(chan error)
				go func() {
					_, err := db.ExecContext(p.Context(), `INSERT INTO orders (buyer) VALUES ($1)`, buyer)
					done <- err
				}()
				return <-done
			})
		}
	}, nil)
	if res.Fatal != nil || res.lazy == "" {
		t.Fatalf("got %v, %s", res.Fatal, res.report())
	}
}

// Every process Proc.Spawn spawns under one name gets a number of its own.
func TestSpawnedProcessesOfOneNameGetNumbersOfTheirOwn(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE owners (name text PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		s.Manual("parent", 1, func(p *Proc) error {
			for range 2 {
				p.Spawn("runner", func(p *Proc) error {
					_, err := db.ExecContext(p.Context(), `INSERT INTO owners VALUES ($1)`, p.Name())
					return err
				})
			}
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			for _, want := range []string{"runner#1", "runner#2"} {
				if _, ok := st.Row(store, "owners", want); !ok {
					return fmt.Errorf("no owner %s", want)
				}
			}
			return nil
		})
	})
}

// A lock taken in the values of a first INSERT goes to whichever process
// started first, so both orders are explored.
func TestEagerStartDroppedWhenAStartTakesALock(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE tries (buyer text PRIMARY KEY, got boolean NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for _, buyer := range []string{"alice", "bob"} {
			s.Manual(buyer, 1, func(p *Proc) error {
				_, err := db.ExecContext(p.Context(), `INSERT INTO tries VALUES ($1, pg_try_advisory_xact_lock(1))`, buyer)
				return err
			})
		}
		s.AtQuiescence(func(st *State) error {
			if row, ok := st.Row(store, "tries", "bob"); ok && row.Bool("got") {
				return errors.New("bob got the lock")
			}
			return nil
		})
	}, nil)
	if !res.Violated || !strings.Contains(res.lazy, "took a lock") {
		t.Fatalf("got %s", res.report())
	}
}

// A process waiting for the one connection of a pool before its first yield
// point gets it when the process holding it lets go, which depends on where
// each started, so both orders are explored.
func TestEagerStartDroppedWhenAStartWaitsOutside(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(`CREATE TABLE firsts (buyer text PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		for _, buyer := range []string{"alice", "bob"} {
			s.Manual(buyer, 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				if _, err := tx.ExecContext(p.Context(), `INSERT INTO firsts VALUES ($1)`, buyer); err != nil {
					return err
				}
				return tx.Commit()
			})
		}
	}, nil)
	if res.Fatal != nil || !strings.Contains(res.lazy, "stopped short of its first yield point") {
		t.Fatalf("got %v, %s", res.Fatal, res.report())
	}
}

// A call that returns without yielding reads or changes a resource where the
// start is, so the exploration starts over without eager starts.
func TestEagerStartDroppedOnACallThatDoesNotYield(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model func(t *testing.T, s *Sim)
	}{
		{"peek", func(t *testing.T, s *Sim) {
			db, store := s.DB("app", postgres.New())
			if _, err := db.Exec(`CREATE TABLE flags (id text PRIMARY KEY)`); err != nil {
				t.Fatal(err)
			}
			seen := false
			s.Seed(func() { seen = false })
			s.Manual("setter", 1, func(p *Proc) error {
				_, err := db.ExecContext(p.Context(), `INSERT INTO flags VALUES ('set')`)
				return err
			})
			s.Manual("watcher", 1, func(p *Proc) error {
				seen = len(store.Peek("flags")) > 0
				p.Step("after peeking")
				return nil
			})
			s.Sometimes("the watcher sees the flag", func(*State) bool { return seen })
		}},
		{"unlock", func(t *testing.T, s *Sim) {
			mu := s.Mutex("m")
			s.Seed(func() { mu.Lock() })
			s.Manual("releaser", 1, func(p *Proc) error {
				mu.Unlock()
				p.Step("after unlocking")
				return nil
			})
			s.Manual("taker", 1, func(p *Proc) error {
				mu.Lock()
				defer mu.Unlock()
				return nil
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := explore(t, tc.model, nil)
			if res.Fatal != nil || !strings.Contains(res.lazy, "without reaching a yield point") {
				t.Fatalf("got %v, %s", res.Fatal, res.report())
			}
		})
	}
}

// Asking the scheduler for a choice touches no resource, so a start that
// does keeps eager starts.
func TestEagerStartKeptOnAChoice(t *testing.T) {
	model := func(t *testing.T, s *Sim) {
		lastItemModel(true)(t, s)
		s.Manual("chooser", 1, func(p *Proc) error {
			p.Choose("which", 2)
			p.Step("after choosing")
			return nil
		})
	}
	with, _ := explore(t, model, nil)
	without, _ := explore(t, model, []Option{EagerStart(false)})
	if with.Fatal != nil || with.lazy != "" || with.Runs >= without.Runs {
		t.Fatalf("with %s\nwithout %s", with.report(), without.report())
	}
}

// A spawn takes the next number of its name, so two starts spawning under one
// name number their children by where they started, and the exploration
// starts over without eager starts.
func TestEagerStartDroppedOnASpawn(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE children (parent text PRIMARY KEY, name text NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for _, parent := range []string{"alice", "bob"} {
			s.Manual(parent, 1, func(p *Proc) error {
				p.Spawn("runner", func(c *Proc) error {
					c.Step("before reading its name")
					_, err := db.ExecContext(c.Context(), `INSERT INTO children VALUES ($1, $2)`, parent, c.Name())
					return err
				})
				return nil
			})
		}
		s.AtQuiescence(func(st *State) error {
			if row, ok := st.Row(store, "children", "bob"); ok && row.Str("name") == "runner#1" {
				return errors.New("bob's runner came first")
			}
			return nil
		})
	}, nil)
	if !res.Violated || !strings.Contains(res.lazy, "without reaching a yield point") {
		t.Fatalf("got %s", res.report())
	}
}

// Two types of one name would share its numbers, so which got #1 would
// depend on which started first.
func TestProcessTypesOfOneNameAreRefused(t *testing.T) {
	s := newSim(t)
	s.Manual("worker", 1, func(*Proc) error { return nil })
	defer func() {
		if rec := recover(); rec == nil || !strings.Contains(fmt.Sprint(rec), `"worker" is declared twice`) {
			t.Fatalf("got %v, want a refusal of the second worker", rec)
		}
	}()
	s.Loop("worker", 1, func(*Proc) error { return nil })
}

func TestSpawnUnderADeclaredNameIsRefused(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		s.Manual("worker", 1, func(*Proc) error { return nil })
		s.Manual("parent", 1, func(p *Proc) error {
			p.Step("before spawning")
			p.Spawn("worker", func(*Proc) error { return nil })
			return nil
		})
	}, nil, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), `Spawn("worker") takes the name of a declared process type`) {
		t.Fatalf("got %v", res.Fatal)
	}
}

// A session setting stays on the connection, which the pool hands to the
// process that takes one next, so a process starting after the setter
// returned it may run under the setting, which eager starts leave out.
func TestEagerStartDroppedOnASessionSetting(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE counters (id text PRIMARY KEY, n int)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO counters VALUES ('c', 0)`) })
		timedOut := false
		s.Seed(func() { timedOut = false })
		s.Manual("holder", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(p.Context(), `UPDATE counters SET n = n + 1 WHERE id = 'c'`); err != nil {
				return err
			}
			p.Step("holds the row")
			return tx.Commit()
		})
		s.Manual("setter", 1, func(p *Proc) error {
			p.Step("before setting")
			_, err := db.ExecContext(p.Context(), `SET lock_timeout = '1s'`)
			return err
		})
		s.Manual("waiter", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `UPDATE counters SET n = n + 1 WHERE id = 'c'`)
			if errors.Is(err, ErrLockNotAvailable) {
				timedOut = true
				return nil
			}
			return err
		})
		s.Sometimes("the waiter times out under the setter's setting", func(*State) bool { return timedOut })
	}, nil)
	if res.Fatal != nil || !strings.Contains(res.lazy, "session setting") || len(res.Unreached) > 0 {
		t.Fatalf("got %v, %s", res.Fatal, res.report())
	}
}

// LAST_INSERT_ID() reads the connection's, which the process that used it
// last left there.
func TestEagerStartDroppedOnLastInsertID(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, `CREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, v INT)`)
		var seen int64
		s.Seed(func() { seen = 0 })
		s.Manual("inserter", 1, func(p *Proc) error {
			p.Step("before inserting")
			_, err := db.ExecContext(p.Context(), `INSERT INTO t (v) VALUES (1)`)
			return err
		})
		s.Manual("reader", 1, func(p *Proc) error {
			return db.QueryRowContext(p.Context(), `SELECT LAST_INSERT_ID()`).Scan(&seen)
		})
		s.Sometimes("the reader sees the inserter's id", func(*State) bool { return seen != 0 })
	}, nil)
	if res.Fatal != nil || !strings.Contains(res.lazy, "LAST_INSERT_ID") || len(res.Unreached) > 0 {
		t.Fatalf("got %v, %s", res.Fatal, res.report())
	}
}

// A process waiting for the one connection of a pool that an eagerly
// started process holds from its start could have run first.
func TestEagerStartDroppedOnAPoolWait(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		db.SetMaxOpenConns(1)
		mustExec(t, db, `CREATE TABLE commits (seq serial PRIMARY KEY, buyer text NOT NULL)`)
		first := ""
		s.Seed(func() { first = "" })
		s.Manual("alice", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			p.Step("in the transaction")
			if first == "" {
				first = "alice"
			}
			return tx.Commit()
		})
		s.Manual("bob", 1, func(p *Proc) error {
			if _, err := db.ExecContext(p.Context(), `SELECT 1`); err != nil {
				return err
			}
			if first == "" {
				first = "bob"
			}
			return nil
		}, After(func(*State) bool { return true }))
		s.Sometimes("bob goes first", func(*State) bool { return first == "bob" })
	}, nil)
	if res.Fatal != nil || !strings.Contains(res.lazy, "waited for a connection of a pool") || len(res.Unreached) > 0 {
		t.Fatalf("got %v, %s", res.Fatal, res.report())
	}
}

// SET LOCAL ends with its transaction and leaves the connection as it was,
// so eager starts stay.
func TestEagerStartKeptOnSetLocal(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE counters (id text PRIMARY KEY, n int)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO counters VALUES ('c', 0)`) })
		for _, name := range []string{"a", "b"} {
			s.Manual(name, 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				if _, err := tx.ExecContext(p.Context(), `SET LOCAL lock_timeout = '1s'`); err != nil {
					return err
				}
				if _, err := tx.ExecContext(p.Context(), `UPDATE counters SET n = n + 1 WHERE id = 'c'`); err != nil && !errors.Is(err, ErrLockNotAvailable) {
					return err
				}
				return tx.Commit()
			})
		}
	}, nil)
	if res.Fatal != nil || res.lazy != "" {
		t.Fatalf("got %v, %s", res.Fatal, res.report())
	}
}

// Shards could not agree on starting over without eager starts, so a
// sharded exploration branches on starts from the start.
func TestEagerStartOffUnderShard(t *testing.T) {
	t.Setenv("DETEST_SHARD", "")
	res, _ := exploreBubble(t, lastItemModel(true), []Option{Shard(0, 2, 1)}, nil, 0)
	if res.Fatal != nil || res.eager || strings.Contains(res.report(), "EagerStart") {
		t.Fatalf("got %v, %s", res.Fatal, res.report())
	}
}

// A start that moves a counter and moves it back leaves it as it found it,
// but the value it drew still depends on where it started.
func TestEagerStartDroppedWhenAStartRestoresACounter(t *testing.T) {
	res, _ := explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE SEQUENCE s`)
		mustExec(t, db, `CREATE TABLE t (a bigint, b bigint)`)
		s.Seed(func() { mustExec(t, db, `SELECT setval('s', 10)`) })
		var got int64
		s.Seed(func() { got = 0 })
		s.Manual("drawer", 1, func(p *Proc) error {
			if _, err := db.ExecContext(p.Context(), `INSERT INTO t VALUES (nextval('s'), setval('s', 10))`); err != nil {
				return err
			}
			return db.QueryRowContext(p.Context(), `SELECT max(a) FROM t`).Scan(&got)
		})
		s.Manual("advancer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `SELECT nextval('s')`)
			return err
		})
		s.Sometimes("the drawer draws after the advancer", func(*State) bool { return got == 12 })
	}, nil)
	if res.Fatal != nil || !strings.Contains(res.lazy, "moved a sequence") || len(res.Unreached) > 0 {
		t.Fatalf("got %v, %s", res.Fatal, res.report())
	}
}
