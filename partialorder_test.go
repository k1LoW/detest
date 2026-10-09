package detest

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// PartialOrder applies to a single worker exploring the tree depth first,
// and is refused with what would split, randomize or resume the exploration.
func TestPartialOrderRefusals(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		opts  []Option
		ckpt  bool
		wants string
	}{
		{"workers", []Option{PartialOrder(), Workers(2)}, false, "single worker"},
		{"random", []Option{PartialOrder(), Random(1)}, false, "Random"},
		{"prioritized", []Option{PartialOrder(), Prioritized(1, 2)}, false, "Random"},
		{"shard", []Option{PartialOrder(), Shard(0, 2, 3)}, false, "Shard"},
		{"checkpoint", []Option{PartialOrder()}, true, "DETEST_CHECKPOINT"},
		{"maxspins", []Option{PartialOrder(), MaxSpins(3)}, false, "MaxSpins"},
	} {
		err := partialOrderRefusal(c.opts, c.ckpt)
		if err == nil || !strings.Contains(err.Error(), c.wants) {
			t.Errorf("%s: got %v, want an error naming %q", c.name, err, c.wants)
		}
	}
	for _, opts := range [][]Option{
		{PartialOrder()},
		{PartialOrder(), Workers(1), MaxSpins(100), MaxPreemptions(2)},
		{Workers(2), Random(1)},
	} {
		if err := partialOrderRefusal(opts, false); err != nil {
			t.Errorf("%v refused: %v", opts, err)
		}
	}
}

// The report says the option was on, as the result rests on its assumption.
func TestPartialOrderReported(t *testing.T) {
	t.Parallel()
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) { counterModel(s, true) }, []Option{PartialOrder()}, nil, 0)
	if res.Violated || !strings.Contains(res.report(), "PartialOrder (assumes no state shared outside detest)") {
		t.Fatalf("got %s", res.report())
	}
}

// exploreAll keeps an exploration going past violations, for the oracle.
func exploreAll() Option { return func(s *Sim) { s.exploreAll = true } }

// outcome is what a run reached: the committed rows, queues and dropped
// messages at its end, each process's history, and the verdict. The
// reduction must reach every one the full tree reaches.
type outcome struct {
	state   string
	locals  map[string]bool // "<type>:<hash of the process's steps>"
	verdict string
	picks   string
}

var (
	uuidOrTime = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|\d{4}-\d\d-\d\d[ T][0-9:.]+`)
	uuidOnly   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	procNumber = regexp.MustCompile(`#\d+`)
)

func outcomeOf(r *run, v *violation) outcome {
	s := r.s
	var lines []string
	for _, db := range s.dbs {
		for t, rows := range db.committed {
			for k, row := range rows {
				cols := make([]string, 0, len(row))
				for c, val := range row {
					if _, ok := val.(interface{ UnixNano() int64 }); ok {
						val = "T"
					}
					cols = append(cols, uuidOrTime.ReplaceAllString(fmt.Sprintf("%s=%v", c, val), "U"))
				}
				sort.Strings(cols)
				lines = append(lines, db.name+"/"+t+"/"+uuidOrTime.ReplaceAllString(k, "U")+"/"+strings.Join(cols, ","))
			}
		}
	}
	for _, q := range s.queues {
		for _, m := range q.msgs {
			lines = append(lines, "queue/"+q.name+"/"+uuidOrTime.ReplaceAllString(fmt.Sprint(m.msg), "U"))
		}
		for _, m := range q.dropped {
			lines = append(lines, "dropped/"+q.name+"/"+uuidOrTime.ReplaceAllString(fmt.Sprint(m), "U"))
		}
	}
	sort.Strings(lines)
	o := outcome{state: strings.Join(lines, "\n"), locals: map[string]bool{}}
	// Each process's steps, with the row keys it touched numbered in the
	// order it first touched them: the uuids the code generates differ
	// between runs, the order a process meets them in does not. Waits are
	// the scheduler's, not something the process sees.
	hashes := map[string]uint64{}
	seen := map[string]map[string]string{}
	for _, e := range r.dep().events {
		if e.proc == "-" {
			continue
		}
		h, ok := hashes[e.proc]
		if !ok {
			h = fnvOffset
		}
		h = hashString(h, e.kind)
		h = hashInt(h, int64(e.op)) //nolint:gosec // a hash, reinterpreted bit for bit
		for _, c := range e.choices {
			h = hashInt(h, int64(c))
		}
		if seen[e.proc] == nil {
			seen[e.proc] = map[string]string{}
		}
		var acc []string
		for res, w := range e.acc {
			if res == "waits" {
				continue
			}
			res = uuidOnly.ReplaceAllStringFunc(res, func(u string) string {
				if id, ok := seen[e.proc][u]; ok {
					return id
				}
				id := fmt.Sprintf("U%d", len(seen[e.proc]))
				seen[e.proc][u] = id
				return id
			})
			acc = append(acc, fmt.Sprintf("%s=%v", res, w))
		}
		sort.Strings(acc)
		for _, a := range acc {
			h = hashString(h, a)
		}
		hashes[e.proc] = h
	}
	for p, h := range hashes {
		o.locals[fmt.Sprintf("%s:%x", procNumber.ReplaceAllString(p, ""), h)] = true
	}
	if v != nil {
		msg := v.err.Error()
		if pp, ok := errors.AsType[*procPanic](v.err); ok {
			msg = fmt.Sprintf("%s: %T %v", pp.proc, pp.value, pp.value)
		}
		o.verdict = v.kind + ":" + procNumber.ReplaceAllString(uuidOrTime.ReplaceAllString(msg, "U"), "#")
	}
	var b strings.Builder
	for _, c := range r.choices {
		fmt.Fprintf(&b, "%d.", c.picked)
	}
	o.picks = b.String()
	return o
}

// outcomes explores model once, whole tree, and returns every run's outcome
// and the exploration's result.
func outcomes(t *testing.T, model func(t *testing.T, s *Sim), opts ...Option) ([]outcome, *result) {
	t.Helper()
	var out []outcome
	opts = append(opts, exploreAll(), func(s *Sim) {
		s.depObserve = func(r *run, v *violation) { out = append(out, outcomeOf(r, v)) }
	})
	res, _ := exploreBubble(t, model, opts, nil, 0)
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	if !res.Complete && !res.Violated {
		t.Fatalf("the exploration did not finish: %s", res.report())
	}
	return out, res
}

// key is what a run's outcome amounts to: its final state, its verdict and
// the history of each process, together. Two runs that reorder independent
// steps only share all three, so a reduction that keeps one run of each
// class keeps every key the full tree reaches.
func (o outcome) key() string {
	locals := make([]string, 0, len(o.locals))
	for l := range o.locals {
		locals = append(locals, l)
	}
	sort.Strings(locals)
	return o.state + "|" + o.verdict + "|" + strings.Join(locals, ",")
}

// checkReduction explores model with and without PartialOrder and fails
// the test if the reduction misses an outcome the full tree reaches: a
// final state, a verdict and a set of process histories that some run
// reaches together. model is called once per exploration and must declare
// the same simulation both times: a uuid drawn at declaration would make
// the two trees differ.
func checkReduction(t *testing.T, name string, model func(t *testing.T, s *Sim), opts ...Option) {
	t.Helper()
	full, fullRes := outcomes(t, model, opts...)
	reduced, reducedRes := outcomes(t, model, append(slices.Clone(opts), PartialOrder())...)
	// A Sometimes condition is checked after every step, so a state the
	// reduction skips on the way to the same end shows up here.
	for _, u := range reducedRes.Unreached {
		if !slices.Contains(fullRes.Unreached, u) {
			t.Errorf("%s: the reduction never meets the Sometimes condition %q, which the full tree meets", name, u)
		}
	}
	keys := map[string]bool{}
	for _, o := range reduced {
		keys[o.key()] = true
	}
	missing := 0
	for _, o := range full {
		if !keys[o.key()] {
			missing++
			if missing <= 3 {
				t.Errorf("%s: the reduction misses the outcome of the run %s, verdict %q, histories %v, state:\n%s", name, o.picks, o.verdict, slices.Sorted(maps.Keys(o.locals)), o.state)
			}
		}
	}
	t.Logf("%s: %d runs, %d under PartialOrder", name, len(full), len(reduced))
	if missing > 0 {
		t.Errorf("%s: %d outcomes missed", name, missing)
	}
	if len(reduced) > len(full) {
		t.Errorf("%s: the reduction explored more runs than the full tree", name)
	}
}

func droppedModel(t *testing.T, s *Sim) {
	q := s.Queue("events", Losses(1))
	s.Seed(func() {
		q.SeedMsg(Msg{"id": "e1"})
		q.SeedMsg(Msg{"id": "e2"})
	})
	var attempts map[string]int
	inFlight, handled := 0, 0
	s.Seed(func() { attempts, inFlight, handled = map[string]int{}, 0, 0 })
	s.OnMessage("consumer", q, func(p *Proc, msg Msg) error {
		inFlight++
		defer func() { inFlight-- }()
		id := msg.Str("id")
		attempts[id]++
		if attempts[id] == 1 {
			return errors.New("boom")
		}
		handled++
		return nil
	})
	s.Always(func(st *State) error {
		if d := st.Dropped(q); len(d) != 0 {
			return fmt.Errorf("dropped %v", d)
		}
		return nil
	})
	s.Sometimes("a message lost", func(st *State) bool {
		return inFlight == 0 && len(st.Queue(q)) == 0 && handled < 2
	})
}

func crashTxModel(t *testing.T, s *Sim) {
	_, store := s.DB("app", postgres.New())
	s.Seed(func() { store.SeedRow("counters", Row{"id": "a", "n": int64(0)}) })
	var shared *Tx
	s.Seed(func() { shared = nil })
	s.Manual("z", 1, func(p *Proc) error {
		return store.Tx(p, func(tx *Tx) error {
			if _, _, err := tx.GetForUpdate("counters", "a"); err != nil {
				return err
			}
			p.Step("hold the row")
			return nil
		})
	})
	s.Manual("x", 1, func(p *Proc) error {
		return store.Tx(p, func(tx *Tx) error {
			shared = tx
			p.Step("hand it over")
			tx.Get("counters", "a")
			return nil
		})
	})
	s.Manual("y", 1, func(p *Proc) error {
		_, _, _ = shared.GetForUpdate("counters", "a")
		return nil
	}, After(func(*State) bool { return shared != nil }))
}

func poolWaitModel(t *testing.T, s *Sim) {
	db, _ := s.DB("app", postgres.New())
	db.SetMaxOpenConns(1)
	mustExec(t, db, `CREATE TABLE commits (seq serial PRIMARY KEY, buyer text NOT NULL)`)
	s.Manual("alice", 1, func(p *Proc) error {
		tx, err := db.BeginTx(p.Context(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		p.Step("in the transaction")
		return tx.Commit()
	})
	s.Manual("bob", 1, func(p *Proc) error {
		_, err := db.ExecContext(p.Context(), `SELECT 1`)
		return err
	}, After(func(*State) bool { return true }))
}

func sessionStateModel(t *testing.T, s *Sim) {
	db, _ := s.DB("app", mysqlBin())
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	s.Manual("inserter", 1, func(p *Proc) error {
		_, err := db.ExecContext(p.Context(), "INSERT INTO ai (v) VALUES (1)")
		return err
	})
	s.Manual("reader", 1, func(p *Proc) error {
		var id int64
		return db.QueryRowContext(p.Context(), "SELECT LAST_INSERT_ID()").Scan(&id)
	})
}

// contendedModel is shaped like a service handler: each of two processes
// reads its own rows, then takes the shared row for update, writes it and
// commits, so that under a preemption bound the interleavings to skip are
// the ones that only reorder the independent reads.
func contendedModel(t *testing.T, s *Sim) {
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE shared (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `CREATE TABLE own (id text PRIMARY KEY, v int)`)
	s.Seed(func() {
		mustExec(t, db, `INSERT INTO shared VALUES (1, 0)`)
		mustExec(t, db, `INSERT INTO own VALUES ('a', 0), ('b', 0)`)
	})
	for _, name := range []string{"a", "b"} {
		s.Manual(name, 1, func(p *Proc) error {
			ctx := p.Context()
			var v, n int
			if err := db.QueryRowContext(ctx, `SELECT v FROM own WHERE id = $1`, name).Scan(&v); err != nil {
				return err
			}
			if err := db.QueryRowContext(ctx, `SELECT n FROM shared WHERE id = 1`).Scan(&n); err != nil {
				return err
			}
			if _, err := db.ExecContext(ctx, `UPDATE own SET v = $1 WHERE id = $2`, n+1, name); err != nil {
				return err
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if err := tx.QueryRowContext(ctx, `SELECT n FROM shared WHERE id = 1 FOR UPDATE`).Scan(&n); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE shared SET n = $1 WHERE id = 1`, n+1); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE own SET v = v + 10 WHERE id = $1`, name); err != nil {
				return err
			}
			return tx.Commit()
		})
	}
	s.AtQuiescence(func(st *State) error {
		if n := len(st.Rows(store, "shared")); n != 1 {
			return fmt.Errorf("%d shared rows", n)
		}
		return nil
	})
}

// The reduction reaches every final state, process history and verdict the
// full tree reaches, on the models that found gaps in the dependence
// recording while PartialOrder was designed: a lost message disabling its
// delivery, a capped pool, session state, a shared transaction's connection,
// a backstop sweeper, and a crash.
func TestPartialOrderReachesEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		model func(t *testing.T, s *Sim)
		opts  []Option
	}{
		{"counter", func(t *testing.T, s *Sim) { counterModel(s, true) }, nil},
		{"lock", lockModel, []Option{MaxPreemptions(1)}},
		{"lock c=0", lockModel, []Option{MaxPreemptions(0)}},
		{"lock unbounded", lockModel, nil},
		{"contended c=2", contendedModel, []Option{MaxPreemptions(2)}},
		{"contended c=1", contendedModel, []Option{MaxPreemptions(1)}},
		{"order", orderModel(false), nil},
		{"backstop", orderModel(true), nil},
		{"dropped", droppedModel, nil},
		{"crash tx", crashTxModel, []Option{MaxCrashes(1), MaxPreemptions(2)}},
		{"pool wait", poolWaitModel, []Option{EagerStart(false)}},
		{"session state", sessionStateModel, []Option{EagerStart(false)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			checkReduction(t, c.name, c.model, c.opts...)
		})
	}
}
