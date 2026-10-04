package detest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

// A loop tick that finds nothing returns ErrIdle and does not spend the
// loop's budget, so the loop ticks again once something changes.
func TestLoopIdleTicksDoNotSpendTheBudget(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE work (id text PRIMARY KEY, done bool NOT NULL)`)
		s.Manual("producer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `INSERT INTO work VALUES ('w1', false)`)
			return err
		})
		s.Loop("sweeper", 1, func(p *Proc) error {
			res, err := db.ExecContext(p.Context(), `UPDATE work SET done = true WHERE NOT done`)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrIdle
			}
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(store, "work", "w1"); !row.Bool("done") {
				return fmt.Errorf("w1 not swept")
			}
			return nil
		})
	})
}

// A change committed while an idle tick is still running, after the tick
// read, lets the loop tick again: the tick did not see it.
func TestLoopTicksAgainAfterAChangeDuringAnIdleTick(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE work (id text PRIMARY KEY, done bool NOT NULL)`)
		s.Manual("producer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `INSERT INTO work VALUES ('w1', false)`)
			return err
		})
		s.Loop("sweeper", 1, func(p *Proc) error {
			res, err := db.ExecContext(p.Context(), `UPDATE work SET done = true WHERE NOT done`)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			p.Step("reports the sweep")
			if n == 0 {
				return ErrIdle
			}
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(store, "work", "w1"); !row.Bool("done") {
				return fmt.Errorf("w1 not swept")
			}
			return nil
		})
	})
}

// When keeps a process from starting while its predicate is false.
func TestWhenGatesTheStart(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE flags (id text PRIMARY KEY)`)
		s.Manual("setter", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `INSERT INTO flags VALUES ('ready')`)
			return err
		})
		ranEarly := false
		s.Seed(func() { ranEarly = false })
		s.Manual("waiter", 1, func(p *Proc) error {
			if len(store.Peek("flags")) == 0 {
				ranEarly = true
			}
			return nil
		}, When(func() bool { return len(store.Peek("flags")) > 0 }))
		s.AtQuiescence(func(st *State) error {
			if ranEarly {
				return fmt.Errorf("waiter started before the flag was set")
			}
			return nil
		})
	})
}

// Instances and Pods let instances of a process type run at the same time.
func TestInstancesRunConcurrently(t *testing.T) {
	for _, opt := range []struct {
		name string
		opts []Option
		proc []ProcOption
	}{
		{"Instances", nil, []ProcOption{Instances(2)}},
		{"Pods", []Option{Pods(2)}, nil},
	} {
		t.Run(opt.name, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				active := 0
				s.Seed(func() { active = 0 })
				work := func(p *Proc) error {
					active++
					p.Step("works")
					active--
					return nil
				}
				if opt.proc == nil {
					s.Loop("worker", 2, work) // Pods sets the instances of loops
				} else {
					s.Manual("worker", 2, work, opt.proc...)
				}
				s.Sometimes("two workers at once", func(*State) bool { return active == 2 })
			}, opt.opts...)
		})
	}
}

// A process can start another, which runs as a process of its own.
func TestSpawn(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		childRan := false
		s.Seed(func() { childRan = false })
		s.Manual("parent", 1, func(p *Proc) error {
			p.Spawn("child", func(c *Proc) error {
				if !strings.HasPrefix(c.Name(), "child") {
					return fmt.Errorf("child named %s", c.Name())
				}
				childRan = true
				return nil
			})
			return nil
		})
		s.AtQuiescence(func(*State) error {
			if !childRan {
				return fmt.Errorf("child did not run")
			}
			return nil
		})
	})
}

// The simulated clock advances to a waiting process's deadline once nothing
// else can run.
func TestWaitUntilAdvancesTheClock(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		woke := int64(-1)
		s.Seed(func() { woke = -1 })
		s.Manual("sleeper", 1, func(p *Proc) error {
			p.WaitUntil(10)
			woke = p.Now()
			return nil
		})
		s.AtQuiescence(func(*State) error {
			if woke != 10 || s.Now() != 10 {
				return fmt.Errorf("woke at %d, clock at %d", woke, s.Now())
			}
			return nil
		})
	})
}

// An Always invariant sees the state at the previous check through Prev.
func TestStatePrev(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE counters (id text PRIMARY KEY, n int NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO counters VALUES ('c', 0)`) })
		s.Manual("incr", 2, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `UPDATE counters SET n = n + 1`)
			return err
		})
		s.Always(func(st *State) error {
			prev := st.Prev()
			if prev == nil {
				return nil
			}
			now, _ := st.Row(store, "counters", "c")
			was, _ := prev.Row(store, "counters", "c")
			if now.Int64("n") < was.Int64("n") {
				return fmt.Errorf("counter went back from %d to %d", was.Int64("n"), now.Int64("n"))
			}
			return nil
		})
	})
}

// State.Queue shows the messages waiting in a queue.
func TestStateQueue(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		q := s.Queue("events")
		if q.Name() != "events" {
			t.Fatalf("queue named %s", q.Name())
		}
		s.Manual("producer", 1, func(p *Proc) error {
			q.Enqueue(p, Msg{"id": "e1"})
			return nil
		})
		s.Sometimes("e1 waiting", func(st *State) bool {
			msgs := st.Queue(q)
			return len(msgs) == 1 && msgs[0].Str("id") == "e1"
		})
	})
}

// RowView reads a committed row without copying it.
func TestRowView(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE items (id text PRIMARY KEY, n int, ok bool, note text)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO items VALUES ('a', 3, true, NULL)`) })
		s.AtQuiescence(func(st *State) error {
			row, ok := st.Row(store, "items", "a")
			if !ok {
				return fmt.Errorf("no row")
			}
			note, has := row.Get("note")
			switch {
			case row.Key() != "a", row.Int("n") != 3, !row.Bool("ok"):
				return fmt.Errorf("read %s", row)
			case note != nil || !has:
				return fmt.Errorf("NULL note read as %v, %v", note, has)
			}
			c := row.Clone()
			c["n"] = int64(9)
			if row.Int("n") != 3 {
				return fmt.Errorf("changing a clone changed the row")
			}
			return nil
		})
	})
}

// External.Call tries every outcome: the effect applied with success, not
// applied with a failure before it, applied with a failure after it.
func TestExternalCallOutcomes(t *testing.T) {
	type outcome struct {
		applied bool
		err     bool
	}
	seen := map[outcome]bool{}
	Explore(t, func(t *testing.T, s *Sim) {
		remote, rstore := s.DB("remote", postgres.New())
		mustExec(t, remote, `CREATE TABLE charges (id text PRIMARY KEY)`)
		pay := s.External("pay", Failures(FailBefore, FailAfter))
		var callErr error
		s.Seed(func() { callErr = nil })
		s.Manual("checkout", 1, func(p *Proc) error {
			callErr = pay.Call(p, rstore, "charge c1", func(tx *Tx) error { return tx.Insert("charges", Row{"id": "c1"}) })
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			_, applied := st.Row(rstore, "charges", "c1")
			if callErr != nil && !errors.Is(callErr, ErrUnavailable) {
				return fmt.Errorf("call failed with %w", callErr)
			}
			seen[outcome{applied, callErr != nil}] = true
			return nil
		})
	})
	for _, want := range []outcome{{true, false}, {false, true}, {true, true}} {
		if !seen[want] {
			t.Errorf("no run with effect applied=%v, error=%v", want.applied, want.err)
		}
	}
}

// A read-only external call never loses an effect it does not have.
func TestExternalReadOnly(t *testing.T) {
	failures := 0
	Explore(t, func(t *testing.T, s *Sim) {
		lookup := s.External("lookup", ReadOnly())
		s.Manual("reader", 1, func(p *Proc) error {
			ran := false
			err := lookup.Do(p, "get", func() error { ran = true; return nil })
			if err != nil {
				failures++
				if ran {
					return fmt.Errorf("a read-only call failed after running")
				}
			}
			return nil
		})
	})
	if failures == 0 {
		t.Fatal("no run where the call failed")
	}
}

// Duplicates delivers a message a second time in some run.
func TestDuplicates(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		q := s.Queue("events", Duplicates(1))
		s.Seed(func() { q.SeedMsg(Msg{"id": "e1"}) })
		handled := 0
		s.Seed(func() { handled = 0 })
		s.OnMessage("consumer", q, func(p *Proc, msg Msg) error {
			handled++
			return nil
		})
		s.Sometimes("handled twice", func(*State) bool { return handled == 2 })
	})
}

// A message whose handler keeps failing is redelivered MaxRedeliveries
// times, then dropped.
func TestMaxRedeliveries(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		q := s.Queue("events")
		s.Seed(func() { q.SeedMsg(Msg{"id": "e1"}) })
		attempts := 0
		s.Seed(func() { attempts = 0 })
		s.OnMessage("consumer", q, func(p *Proc, msg Msg) error {
			attempts++
			return errors.New("boom")
		})
		s.AtQuiescence(func(st *State) error {
			if attempts != 3 || len(st.Queue(q)) != 0 {
				return fmt.Errorf("%d attempts, %d left", attempts, len(st.Queue(q)))
			}
			return nil
		})
	}, MaxRedeliveries(2))
}

// Shards split the runs: each explores less than the whole, and together
// they cover it, the shallow runs above the split depth on every shard.
func TestShardsSplitTheRuns(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, true) }
	whole, _ := exploreBubble(t, model, nil, nil, 0)
	sum := 0
	for i := range 2 {
		res, _ := exploreBubble(t, model, []Option{Shard(i, 2, 2)}, nil, 0)
		if res.Runs >= whole.Runs {
			t.Fatalf("shard %d explored %d runs of %d", i, res.Runs, whole.Runs)
		}
		sum += res.Runs
	}
	if sum < whole.Runs {
		t.Fatalf("the shards explored %d runs of %d", sum, whole.Runs)
	}
}

// ObserveSQL sees every statement with the error detest produced for it.
func TestObserveSQL(t *testing.T) {
	var seen []string
	var failed int
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE items (id text PRIMARY KEY)`)
		_, _ = db.Exec(`SELEC 1`)
	}, ObserveSQL(func(q string, err error) {
		seen = append(seen, q)
		if err != nil {
			failed++
		}
	}), Verbose())
	if len(seen) != 2 || failed != 1 {
		t.Fatalf("observed %q with %d failures", seen, failed)
	}
}

// A process that crashes holding a read lock frees it for a writer.
func TestCrashFreesTheReadLock(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		rw := s.RWMutex("rw")
		s.Manual("reader", 1, func(p *Proc) error {
			l := rw.RLocker()
			l.Lock()
			p.Step("reads")
			l.Unlock()
			return nil
		})
		s.Manual("writer", 1, func(p *Proc) error {
			rw.Lock()
			p.Step("writes")
			rw.Unlock()
			return nil
		})
	}, MaxCrashes(1))
}

// Statements prepared through database/sql run like direct ones.
func TestPreparedStatements(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE items (id text PRIMARY KEY)`)
	ins, err := db.Prepare(`INSERT INTO items VALUES ($1)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ins.Close() }()
	for _, id := range []string{"a", "b"} {
		if _, err := ins.Exec(id); err != nil {
			t.Fatal(err)
		}
	}
	sel, err := db.Prepare(`SELECT count(*) FROM items`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sel.Close() }()
	var n int
	if err := sel.QueryRow().Scan(&n); err != nil || n != 2 {
		t.Fatalf("count %d, %v", n, err)
	}
}

// FOR UPDATE NOWAIT fails at once on a row another transaction holds.
func TestNowait(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE items (id text PRIMARY KEY)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO items VALUES ('a')`) })
		refused := false
		s.Seed(func() { refused = false })
		for _, name := range []string{"x", "y"} {
			s.Manual(name, 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				if _, err := tx.Exec(`SELECT id FROM items WHERE id = 'a' FOR UPDATE NOWAIT`); errors.Is(err, ErrLockNotAvailable) {
					refused = true
					return nil
				} else if err != nil {
					return err
				}
				p.Step("holds the row")
				return tx.Commit()
			})
		}
		s.Sometimes("NOWAIT refused", func(*State) bool { return refused })
	})
}
