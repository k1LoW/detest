package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/postgres"
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

// An idle tick's own commit does not wake the loop again. On MySQL an UPDATE
// that leaves a row as it was reports no affected rows but still commits.
func TestLoopIdleTickIsNotWokenByItsOwnCommit(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysql.New())
		mustExec(t, db, `CREATE TABLE work (id varchar(8) COLLATE utf8mb4_bin PRIMARY KEY, done bool NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO work VALUES ('w1', true)`) })
		s.Loop("sweeper", 1, func(p *Proc) error {
			res, err := db.ExecContext(p.Context(), `UPDATE work SET done = true WHERE id = 'w1'`)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrIdle
			}
			return nil
		})
	}, nil, nil, 0)
	if res.Violated || res.Fatal != nil || !res.Complete || res.CutRuns > 0 {
		t.Fatalf("want a complete exploration with no run cut, got %s", res.report())
	}
}

// Two idle loops whose commits change nothing do not wake each other.
func TestIdleLoopsAreNotWokenByUnchangedCommits(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysql.New())
		mustExec(t, db, `CREATE TABLE work (id varchar(8) COLLATE utf8mb4_bin PRIMARY KEY, done bool NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO work VALUES ('w1', true), ('w2', true)`) })
		for _, id := range []string{"w1", "w2"} {
			s.Loop("sweeper_"+id, 1, func(p *Proc) error {
				res, err := db.ExecContext(p.Context(), `UPDATE work SET done = true WHERE id = ?`, id)
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
		}
	}, nil, nil, 0)
	if res.Violated || res.Fatal != nil || !res.Complete || res.CutRuns > 0 {
		t.Fatalf("want a complete exploration with no run cut, got %s", res.report())
	}
}

// A loop that went idle after SKIP LOCKED passed over a row ticks again
// once the transaction holding the row lets it go, even without changing it.
func TestIdleLoopRetriesARowSkipLockedPassedOver(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE work (id text PRIMARY KEY, done bool NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO work VALUES ('w1', false)`) })
		s.Manual("toucher", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(p.Context(), `UPDATE work SET done = done WHERE id = 'w1'`); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.Loop("worker", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var id string
			err = tx.QueryRowContext(p.Context(), `SELECT id FROM work WHERE NOT done LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrIdle
			} else if err != nil {
				return err
			}
			if _, err := tx.ExecContext(p.Context(), `UPDATE work SET done = true WHERE id = $1`, id); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(store, "work", "w1"); !row.Bool("done") {
				return fmt.Errorf("w1 not done")
			}
			return nil
		})
	})
}

// '30 days' changed to '1 mon' is a change that wakes an idle loop, though
// the two compare equal: added to a time, they land on other days.
func TestIdleLoopIsWokenByAnIntervalOfOtherFields(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE plan (id int PRIMARY KEY, every interval NOT NULL, done bool NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO plan VALUES (1, '30 days', false)`) })
		s.Manual("admin", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `UPDATE plan SET every = '1 month' WHERE id = 1`)
			return err
		})
		s.Loop("renewer", 1, func(p *Proc) error {
			res, err := db.ExecContext(p.Context(), `UPDATE plan SET done = true WHERE NOT done AND timestamp '2024-01-31' + every = timestamp '2024-02-29'`)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrIdle
			}
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(store, "plan", "1"); !row.Bool("done") {
				return fmt.Errorf("plan 1 not renewed")
			}
			return nil
		})
	})
}

// Idle loops that lock rows the other passes over with SKIP LOCKED end, cut
// at MaxIdleTicks rather than waking each other forever.
func TestIdleLoopsWakingEachOtherAreCut(t *testing.T) {
	t.Parallel()
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysql.New())
		mustExec(t, db, `CREATE TABLE work (id varchar(8) COLLATE utf8mb4_bin PRIMARY KEY, done bool NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO work VALUES ('w1', true), ('w2', true)`) })
		for _, ids := range [][2]string{{"w1", "w2"}, {"w2", "w1"}} {
			s.Loop("sweeper_"+ids[0], 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				if _, err := tx.ExecContext(p.Context(), `UPDATE work SET done = true WHERE id = ?`, ids[0]); err != nil {
					return err
				}
				rows, err := tx.QueryContext(p.Context(), `SELECT id FROM work WHERE id = ? FOR UPDATE SKIP LOCKED`, ids[1])
				if err != nil {
					return err
				}
				_ = rows.Close()
				if err := tx.Commit(); err != nil {
					return err
				}
				return ErrIdle
			})
		}
	}, []Option{MaxIdleTicks(1)}, nil, 0)
	if res.Violated || !res.Complete || res.CutRuns == 0 {
		t.Fatalf("want a complete exploration with runs cut at MaxIdleTicks, got %s", res.report())
	}
}

// A loop that went idle after SKIP LOCKED passed over a row ticks again once a
// rollback to a savepoint lets the row go, while the holder is still open.
func TestIdleLoopRetriesARowASavepointRollbackLetGo(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE work (id text PRIMARY KEY, done bool NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO work VALUES ('w1', false)`) })
		open, idled, takenWhileOpen := false, false, false
		s.Seed(func() { open, idled, takenWhileOpen = false, false, false })
		s.Manual("toucher", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			open = true
			for _, q := range []string{`SAVEPOINT s`, `UPDATE work SET done = done WHERE id = 'w1'`, `ROLLBACK TO SAVEPOINT s`} {
				if _, err := tx.ExecContext(p.Context(), q); err != nil {
					return err
				}
			}
			p.Step("works on")
			open = false
			return tx.Commit()
		})
		s.Loop("worker", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var id string
			err = tx.QueryRowContext(p.Context(), `SELECT id FROM work WHERE NOT done LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				idled = true
				return ErrIdle
			} else if err != nil {
				return err
			}
			if _, err := tx.ExecContext(p.Context(), `UPDATE work SET done = true WHERE id = $1`, id); err != nil {
				return err
			}
			if open && idled {
				takenWhileOpen = true
			}
			return tx.Commit()
		})
		s.Sometimes("worker went idle, then took w1 after the rollback, before the toucher ended", func(*State) bool { return takenWhileOpen })
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
	t.Parallel()
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

// The trace shows a SET value computed from the row as its expression, since
// the value is not known before the row is read.
func TestTraceShowsComputedSetValues(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE counters (id text PRIMARY KEY, n int NOT NULL, m int, label text)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO counters (id, n, label) VALUES ('c1', 0, 'x')`) })
		s.Manual("bump", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `UPDATE counters SET n = (n + 1) * 2, m = -(n + 1) * 2, label = $1 WHERE id = 'c1'`, "y")
			return err
		})
		s.AtQuiescence(func(*State) error { return errors.New("show the trace") })
	}, nil, nil, 0)
	if !strings.Contains(res.Trace, "set {label=y m=- (n + 1) * 2 n=(n + 1) * 2}") {
		t.Fatalf("trace does not show the computed value:\n%s", res.Trace)
	}
}

// idleLoopModel declares a loop that goes idle at once, beside a manual
// process, so that MaxIdleTicks(0) cuts every run the loop ticks in.
func idleLoopModel(s *Sim, bad *bool) {
	s.Seed(func() { *bad = false })
	s.Manual("caller", 1, func(p *Proc) error {
		p.Step("calls")
		return nil
	})
	s.Loop("sweeper", 1, func(p *Proc) error {
		*bad = true
		return ErrIdle
	})
}

// The step a run is cut at is checked as any other: only the checks at
// quiescence are skipped.
func TestCutRunStillChecksAlways(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		var bad bool
		idleLoopModel(s, &bad)
		s.Always(func(*State) error {
			if bad {
				return errors.New("the idle tick left bad state")
			}
			return nil
		})
	}, []Option{MaxIdleTicks(0)}, nil, 0)
	if !res.Violated || !strings.Contains(res.Err.Error(), "bad state") {
		t.Fatalf("want the Always violation of the cut step, got %s", res.report())
	}
}

// A replayed schedule cut at MaxIdleTicks says so instead of passing.
func TestReplayReportsACut(t *testing.T) {
	cut := 0
	for _, sched := range []string{"0", "1"} {
		res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
			var bad bool
			idleLoopModel(s, &bad)
		}, []Option{MaxIdleTicks(0), Replay(sched)}, nil, 0)
		if res.CutRuns > 0 {
			cut++
			if !strings.Contains(res.report(), "cut at MaxIdleTicks") {
				t.Fatalf("a cut replay reports %q", res.report())
			}
		}
	}
	if cut == 0 {
		t.Fatal("no replayed schedule was cut")
	}
}

// Runs cut before a checkpoint are counted after it is resumed.
func TestCheckpointKeepsCutRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ckpt")
	f := newFrontier(1, 10)
	f.cut()
	if err := f.save(path); err != nil {
		t.Fatal(err)
	}
	g := newFrontier(1, 10)
	if err := g.load(path); err != nil {
		t.Fatal(err)
	}
	if res := g.merge(nil, 1); res.CutRuns != 1 || !strings.Contains(res.report(), "1 runs cut at MaxIdleTicks") {
		t.Fatalf("resumed exploration reports %q", res.report())
	}
}

// A MySQL locking read that waits rather than skips marks no holder, so an
// idle loop is not woken when the holder lets go of a row it did not change.
func TestBlockingLockingReadWakesNoIdleLoop(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysql.New())
		mustExec(t, db, `CREATE TABLE work (id varchar(8) COLLATE utf8mb4_bin PRIMARY KEY, done bool NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO work VALUES ('w1', true)`) })
		ticks := 0
		s.Seed(func() { ticks = 0 })
		for _, name := range []string{"a", "b"} {
			s.Manual(name, 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				rows, err := tx.QueryContext(p.Context(), `SELECT id FROM work WHERE id >= 'w1' FOR UPDATE`)
				if err != nil {
					return err
				}
				_ = rows.Close()
				return tx.Commit()
			})
		}
		s.Loop("sweeper", 1, func(p *Proc) error {
			ticks++
			p.Step("finds nothing")
			return ErrIdle
		})
		s.AtQuiescence(func(*State) error {
			if ticks > 1 {
				return fmt.Errorf("the idle sweeper ticked %d times with no row changed", ticks)
			}
			return nil
		})
	})
}

// Explore's merge keeps the cut of a replay, which no frontier counted.
func TestMergeKeepsAReplaysCut(t *testing.T) {
	res := newFrontier(1, 10).merge([]*result{{Replay: true, CutRuns: 1, Schedule: "0"}}, 1)
	if res.CutRuns != 1 || !strings.Contains(res.report(), "cut at MaxIdleTicks") {
		t.Fatalf("merged replay reports %q", res.report())
	}
}

// An INSERT that waits for a row equal to its own in a table without a
// primary key marks no holder, so an idle loop is not woken when the holder
// lets go of a row it did not change.
func TestWaitingInsertWakesNoIdleLoop(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE t (a int UNIQUE)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO t VALUES (1)`) })
		ticks := 0
		s.Seed(func() { ticks = 0 })
		s.Manual("holder", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(p.Context(), `UPDATE t SET a = a WHERE a = 1`); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.Manual("inserter", 1, func(p *Proc) error {
			_, _ = db.ExecContext(p.Context(), `INSERT INTO t VALUES (1)`)
			return nil
		})
		s.Loop("sweeper", 1, func(p *Proc) error {
			ticks++
			p.Step("finds nothing")
			return ErrIdle
		})
		s.AtQuiescence(func(*State) error {
			if ticks > 1 {
				return fmt.Errorf("the idle sweeper ticked %d times with no row changed", ticks)
			}
			return nil
		})
	})
}

// A loop that goes idle once the clock has advanced is cut as one that goes
// idle at a step, rather than having its run checked at quiescence.
func TestLoopIdleAfterASleepIsCut(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		s.Loop("sweeper", 1, func(p *Proc) error {
			time.Sleep(time.Second)
			return ErrIdle
		})
		s.AtQuiescence(func(*State) error { return errors.New("checked at quiescence") })
	}, []Option{MaxIdleTicks(0)}, nil, 0)
	if res.Violated || res.CutRuns == 0 {
		t.Fatalf("want the run cut, got %s", res.report())
	}
	res, _ = exploreBubble(t, func(t *testing.T, s *Sim) {
		// Atomic: the race detector sees no edge from the scheduler to a
		// process the fake clock woke, though they never run at once.
		var bad atomic.Bool
		s.Seed(func() { bad.Store(false) })
		s.Loop("sweeper", 1, func(p *Proc) error {
			time.Sleep(time.Second)
			bad.Store(true)
			return ErrIdle
		})
		s.Always(func(*State) error {
			if bad.Load() {
				return errors.New("the idle tick left bad state")
			}
			return nil
		})
	}, []Option{MaxIdleTicks(0)}, nil, 0)
	if !res.Violated || !strings.Contains(res.Err.Error(), "bad state") {
		t.Fatalf("want the Always violation of the cut tick, got %s", res.report())
	}
}

// A loop that went idle after a lock timeout gave up on a row ticks again
// once the holder lets it go, even without changing it.
func TestIdleLoopRetriesARowALockTimeoutGaveUpOn(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE work (id text PRIMARY KEY, done bool NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO work VALUES ('w1', false)`) })
		s.Manual("toucher", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(p.Context(), `SELECT id FROM work WHERE id = 'w1' FOR UPDATE`); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.Loop("worker", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(p.Context(), `SET LOCAL lock_timeout = '1s'`); err != nil {
				return err
			}
			var id string
			err = tx.QueryRowContext(p.Context(), `SELECT id FROM work WHERE NOT done LIMIT 1 FOR UPDATE`).Scan(&id)
			if errors.Is(err, ErrLockNotAvailable) {
				return ErrIdle
			} else if err != nil {
				return err
			}
			if _, err := tx.ExecContext(p.Context(), `UPDATE work SET done = true WHERE id = $1`, id); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(store, "work", "w1"); !row.Bool("done") {
				return fmt.Errorf("w1 not done")
			}
			return nil
		})
	})
}

// A loop that went idle as a deadlock victim ticks again once the survivor
// lets go of its locks, even without changing a row.
func TestIdleLoopRetriesAfterADeadlockSurvivorCommitsUnchanged(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE work (id text PRIMARY KEY, done bool NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO work VALUES ('a', false), ('b', true)`) })
		inTx := func(p *Proc, stmts ...string) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			for _, q := range stmts {
				if _, err := tx.ExecContext(p.Context(), q); err != nil {
					return err
				}
			}
			return tx.Commit()
		}
		s.Manual("toucher", 1, func(p *Proc) error {
			_ = inTx(p, `UPDATE work SET done = done WHERE id = 'b'`, `UPDATE work SET done = done WHERE id = 'a'`)
			return nil
		})
		s.Loop("worker", 1, func(p *Proc) error {
			err := inTx(p, `UPDATE work SET done = true WHERE id = 'a' AND NOT done`, `UPDATE work SET done = done WHERE id = 'b'`)
			if errors.Is(err, ErrDeadlock) {
				return ErrIdle
			}
			return err
		})
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(store, "work", "a"); !row.Bool("done") {
				return fmt.Errorf("a not done")
			}
			return nil
		})
	})
}

// Commits of manual processes are progress, so an idle loop they wake again
// and again is never cut, however many there are.
func TestIdleLoopWokenByProgressIsNotCut(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE a (id text PRIMARY KEY); CREATE TABLE jobs (id text PRIMARY KEY)`)
		for _, b := range []string{"x", "y"} {
			s.Manual(b, 1, func(p *Proc) error {
				for i := range 3 {
					if _, err := db.ExecContext(p.Context(), `INSERT INTO a VALUES ($1)`, fmt.Sprint(b, i)); err != nil {
						return err
					}
				}
				return nil
			})
		}
		s.Loop("sweeper", 1, func(p *Proc) error {
			var n int
			if err := db.QueryRowContext(p.Context(), `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
				return err
			}
			return ErrIdle
		})
	}, []Option{MaxPreemptions(1)}, nil, 0)
	if res.Violated || !res.Complete || res.CutRuns > 0 {
		t.Fatalf("want a complete exploration with no run cut, got %s", res.report())
	}
}

// A poller that records a heartbeat on every idle tick waits for its work
// without being cut or running out of budget.
func TestHeartbeatPollerWaitsForWork(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE workers (id text PRIMARY KEY, beats int NOT NULL); CREATE TABLE jobs (id text PRIMARY KEY, done bool NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO workers VALUES ('w', 0)`) })
		s.Manual("producer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), `INSERT INTO jobs VALUES ('j1', false)`)
			return err
		})
		s.Loop("poller", 1, func(p *Proc) error {
			if _, err := db.ExecContext(p.Context(), `UPDATE workers SET beats = beats + 1 WHERE id = 'w'`); err != nil {
				return err
			}
			res, err := db.ExecContext(p.Context(), `UPDATE jobs SET done = true WHERE NOT done`)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrIdle
			}
			return nil
		})
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(store, "jobs", "j1"); !row.Bool("done") {
				return errors.New("j1 not done")
			}
			return nil
		})
	}, nil, nil, 0)
	if res.Violated || !res.Complete || res.CutRuns > 0 {
		t.Fatalf("want a complete exploration with no run cut, got %s", res.report())
	}
}

// A loop whose idle tick spawns a process that changes a row is woken by it
// with no progress in between, so its runs end, cut at MaxIdleTicks.
func TestIdleLoopSpawningWorkIsCut(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE runs (id text PRIMARY KEY, n int NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO runs VALUES ('r', 0)`) })
		s.Loop("scheduler", 1, func(p *Proc) error {
			p.Spawn("runner", func(c *Proc) error {
				_, err := db.ExecContext(c.Context(), `UPDATE runs SET n = n + 1 WHERE id = 'r'`)
				return err
			})
			return ErrIdle
		})
	}, []Option{MaxRuns(100000)}, nil, 0)
	if res.Violated || !res.Complete || res.CutRuns == 0 {
		t.Fatalf("want a complete exploration with runs cut, got %s", res.report())
	}
}

// A tick that did work starts the loop's count of idle ticks again, even
// when nothing but the loop and what it spawned made changes around it.
func TestLoopWorkStartsTheIdleCountAgain(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE jobs (id text PRIMARY KEY, done bool NOT NULL); CREATE TABLE marks (id text PRIMARY KEY, n int NOT NULL)`)
		s.Seed(func() { mustExec(t, db, `INSERT INTO marks VALUES ('m', 0)`) })
		spawned := false
		s.Seed(func() { spawned = false })
		s.Loop("worker", 2, func(p *Proc) error {
			res, err := db.ExecContext(p.Context(), `UPDATE jobs SET done = true WHERE NOT done`)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				return nil
			}
			if !spawned {
				spawned = true
				// Changes by a process a loop spawned are no progress, so
				// only the worker's own tick can start the count again.
				p.Spawn("feeder", func(c *Proc) error {
					if _, err := db.ExecContext(c.Context(), `INSERT INTO jobs VALUES ('j1', false)`); err != nil {
						return err
					}
					_, err := db.ExecContext(c.Context(), `UPDATE marks SET n = n + 1 WHERE id = 'm'`)
					return err
				})
			}
			return ErrIdle
		})
	}, []Option{MaxIdleTicks(2)}, nil, 0)
	if res.Violated || !res.Complete || res.CutRuns > 0 {
		t.Fatalf("want a complete exploration with no run cut, got %s", res.report())
	}
}
