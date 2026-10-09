package detest

import (
	"slices"
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// observeDependence hands each run's trace to fn, under PartialOrder.
func observeDependence(fn func(events []*depEvent)) Option {
	return func(s *Sim) {
		s.partialOrder = true
		s.depObserve = func(r *run, _ *violation) { fn(r.dep().events) }
	}
}

// firstTrace explores model under PartialOrder and returns the trace of its
// first run, the one taking the first option at every choice.
func firstTrace(t *testing.T, model func(t *testing.T, s *Sim), opts ...Option) []*depEvent {
	t.Helper()
	var first []*depEvent
	opts = append(opts, observeDependence(func(events []*depEvent) {
		if first == nil {
			first = events
		}
	}))
	res, _ := exploreBubble(t, model, opts, nil, 0)
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	if first == nil {
		t.Fatal("no run was traced")
	}
	return first
}

// stepsOf returns the resources of the steps of the named process, in order,
// each as its sorted "res=r" and "res=w" entries joined by spaces.
func stepsOf(events []*depEvent, proc string) []string {
	var out []string
	for _, e := range events {
		if e.proc == proc && e.kind == "resume" {
			out = append(out, strings.Join(e.depSorted(), " "))
		}
	}
	return out
}

func hasStep(steps []string, want ...string) bool {
	for _, s := range steps {
		ok := true
		for _, w := range want {
			if !strings.Contains(s, w) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// A statement whose WHERE pins the primary key touches that row; one by a
// predicate touches the table; a subquery reads everything.
func TestDependenceRowsAndTables(t *testing.T) {
	t.Parallel()
	events := firstTrace(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, v int)`)
		mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY, n int)`)
		s.Seed(func() {
			mustExec(t, db, `INSERT INTO t VALUES (1, 0), (2, 0)`)
			mustExec(t, db, `INSERT INTO u VALUES (1, 0)`)
		})
		s.Manual("a", 1, func(p *Proc) error {
			var v int
			if err := db.QueryRowContext(p.Context(), `SELECT v FROM t WHERE id = $1`, 1).Scan(&v); err != nil {
				return err
			}
			if _, err := db.ExecContext(p.Context(), `UPDATE t SET v = v + 1 WHERE id = 2`); err != nil {
				return err
			}
			if _, err := db.ExecContext(p.Context(), `UPDATE t SET v = 0 WHERE v > 5`); err != nil {
				return err
			}
			_, err := db.ExecContext(p.Context(), `UPDATE u SET n = (SELECT count(*) FROM t) WHERE id = 1`)
			return err
		})
	})
	a := stepsOf(events, "a#1")
	for _, want := range [][]string{
		{"db:app:public.t#1=r"},
		{"db:app:public.t#2=w"},
		{"db:app:public.t=w"},
		{"*=r", "db:app:public.u#1=w"},
	} {
		if !hasStep(a, want...) {
			t.Errorf("no step of a touches %v; steps:\n  %s", want, strings.Join(a, "\n  "))
		}
	}
	if hasStep(a, "db:app:public.t=r") {
		t.Errorf("the point read touched the whole table; steps:\n  %s", strings.Join(a, "\n  "))
	}
	if n := 0; true {
		for _, st := range a {
			if strings.Contains(st, "*=r") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("want only the subquery to read everything, got %d steps; steps:\n  %s", n, strings.Join(a, "\n  "))
		}
	}
}

// A locking read and the lock it waits for are writes of the row, every
// wait touches "waits", and a commit publishes what the transaction locked.
func lockModel(t *testing.T, s *Sim) {
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, v int)`)
	s.Seed(func() { mustExec(t, db, `INSERT INTO t VALUES (1, 0)`) })
	for _, name := range []string{"a", "b"} {
		s.Manual(name, 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(p.Context(), `SELECT v FROM t WHERE id = 1 FOR UPDATE`); err != nil {
				return err
			}
			p.Step("hold")
			return tx.Commit()
		})
	}
}

// allTraces explores model under PartialOrder and returns every run's trace.
func allTraces(t *testing.T, model func(t *testing.T, s *Sim), opts ...Option) [][]*depEvent {
	t.Helper()
	var all [][]*depEvent
	opts = append(opts, observeDependence(func(events []*depEvent) { all = append(all, events) }))
	if res, _ := exploreBubble(t, model, opts, nil, 0); res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	return all
}

func TestDependenceLocksAndWaits(t *testing.T) {
	t.Parallel()
	events := firstTrace(t, lockModel, MaxPreemptions(1))
	a := stepsOf(events, "a#1")
	if !hasStep(a, "db:app:public.t#1=w") {
		t.Errorf("want a to lock the row:\n   %s", strings.Join(a, "\n   "))
	}
	// a's commit publishes the lock it held.
	commits := 0
	for _, st := range a {
		if strings.Contains(st, "db:app:public.t#1=w") {
			commits++
		}
	}
	if commits < 2 {
		t.Errorf("a's commit did not publish the row:\n   %s", strings.Join(a, "\n   "))
	}
	// In some run b waits for a's lock: the attempt is a write of the row
	// and a wait.
	waited := false
	for _, run := range allTraces(t, lockModel, MaxPreemptions(1)) {
		if hasStep(stepsOf(run, "b#1"), "db:app:public.t#1=w", "waits=w") {
			waited = true
		}
	}
	if !waited {
		t.Error("no run has b waiting for a's lock with the row and the wait recorded")
	}
}

// Queues, Externals and mutexes are resources; a step touches everything.
func TestDependenceOtherResources(t *testing.T) {
	t.Parallel()
	events := firstTrace(t, func(t *testing.T, s *Sim) {
		q := s.Queue("events")
		ext := s.External("svc")
		mu := s.Mutex("mu")
		s.Manual("a", 1, func(p *Proc) error {
			q.Enqueue(p, Msg{"id": "1"})
			if err := ext.Do(p, "call", func() error { return nil }); err != nil {
				return err
			}
			ext.Observe(p)
			mu.Lock()
			mu.Unlock()
			p.Step("free")
			return nil
		})
		s.OnMessage("c", q, func(p *Proc, msg Msg) error { return nil })
	})
	a := stepsOf(events, "a#1")
	for _, want := range []string{"queue:events=w", "ext:svc=w", "mutex:mu=w", "*=w"} {
		if !hasStep(a, want) {
			t.Errorf("no step of a touches %s; steps:\n  %s", want, strings.Join(a, "\n  "))
		}
	}
	var deliver *depEvent
	for _, e := range events {
		if e.kind == "deliver" {
			deliver = e
		}
	}
	if deliver == nil || !slices.Contains(deliver.depSorted(), "queue:events=w") || deliver.creator == nil {
		t.Errorf("the delivery is not a step writing the queue, created by the enqueue: %+v", deliver)
	}
}

// The connections of a capped pool are a resource the processes share.
func TestDependenceCappedPool(t *testing.T) {
	t.Parallel()
	model := func(cap int) func(t *testing.T, s *Sim) {
		return func(t *testing.T, s *Sim) {
			db, _ := s.DB("app", postgres.New())
			db.SetMaxOpenConns(cap)
			mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY)`)
			s.Manual("a", 1, func(p *Proc) error {
				_, err := db.ExecContext(p.Context(), `INSERT INTO t VALUES (1)`)
				return err
			})
		}
	}
	if a := stepsOf(firstTrace(t, model(1)), "a#1"); !hasStep(a, "conn:app:0=w") {
		t.Errorf("a capped pool's connection is not recorded:\n  %s", strings.Join(a, "\n  "))
	}
	if a := stepsOf(firstTrace(t, model(0)), "a#1"); hasStep(a, "conn:app:") {
		t.Errorf("an uncapped pool's connection is recorded:\n  %s", strings.Join(a, "\n  "))
	}
}
