package detest

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// earlyRaceModel is a lost update between the first steps of two processes,
// each followed by a long tail of steps of its own, the shape of #226.
func earlyRaceModel(s *Sim, tail int) {
	var x int
	s.Seed(func() { x = 0 })
	handler := func(p *Proc) error {
		p.Step("read")
		v := x
		p.Step("write")
		x = v + 1
		for i := range tail {
			p.Step("tail %d", i)
		}
		return nil
	}
	s.Manual("a", 1, handler)
	s.Manual("b", 1, handler)
	s.AtQuiescence(func(st *State) error {
		if x != 2 {
			return fmt.Errorf("lost update: x = %d", x)
		}
		return nil
	})
}

// A complete exploration makes the runs DepthFirst makes, each once.
func TestIterativeBoundingMakesEveryRunOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model func(t *testing.T, s *Sim)
		opts  []Option
	}{
		{"counter bounded", func(t *testing.T, s *Sim) { counterModel(s, true) }, []Option{MaxPreemptions(2)}},
		{"counter lazy", func(t *testing.T, s *Sim) { counterModel(s, true) }, []Option{MaxPreemptions(1), EagerStart(false)}},
		{"steps unbounded", func(t *testing.T, s *Sim) {
			earlyRaceModel(s, 2)
			s.ExpectViolation("lost update")
		}, []Option{exploreAll()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dfs, _ := exploreBubble(t, tc.model, tc.opts, nil, 0)
			icb, _ := exploreBubble(t, tc.model, append(tc.opts, IterativeBounding()), nil, 0)
			// A violation leaves the exploration incomplete, though
			// exploreAll makes every run.
			if dfs.Runs != icb.Runs || dfs.Complete != icb.Complete || dfs.Violated != icb.Violated {
				t.Fatalf("DepthFirst: %s\nIterativeBounding: %s", dfs.report(), icb.report())
			}
		})
	}
}

// One worker makes the runs in order of the preemptions they spend, and a
// run made again on the way to a level's runs spends fewer.
func TestIterativeBoundingOrdersRunsByPreemptions(t *testing.T) {
	var levels []int
	model := func(t *testing.T, s *Sim) {
		counterModel(s, true)
		s.AtQuiescence(func(*State) error {
			switch level := s.frontier.level; {
			case s.run.preempts == level:
				levels = append(levels, level)
			case s.run.preempts > level:
				return fmt.Errorf("a run of %d preemptions in the pass of %d", s.run.preempts, level)
			}
			return nil
		})
	}
	res, _ := exploreBubble(t, model, []Option{MaxPreemptions(2), IterativeBounding()}, nil, 0)
	if !res.Complete || res.Violated || len(levels) != res.Runs || res.Again == 0 {
		t.Fatalf("%d runs at their level: %s", len(levels), res.report())
	}
	for i := 1; i < len(levels); i++ {
		if levels[i] < levels[i-1] {
			t.Fatalf("run %d spent %d preemptions after a run that spent %d", i, levels[i], levels[i-1])
		}
	}
	if levels[0] != 0 || levels[len(levels)-1] != 2 {
		t.Fatalf("levels from %d to %d, want 0 to 2", levels[0], levels[len(levels)-1])
	}
}

// A race between the first steps of a run, which depth first reaches only
// after the tree below the earlier order, is found within the runs that
// leave DepthFirst without a violation.
func TestIterativeBoundingFindsAnEarlyRace(t *testing.T) {
	model := func(t *testing.T, s *Sim) { earlyRaceModel(s, 8) }
	opts := []Option{MaxRuns(200)}
	dfs, _ := exploreBubble(t, model, opts, nil, 0)
	if dfs.Violated || dfs.Complete {
		t.Fatalf("want DepthFirst cut short without the race, got %s", dfs.report())
	}
	icb, _ := exploreBubble(t, model, append(opts, IterativeBounding()), nil, 0)
	if !icb.Violated || !strings.Contains(icb.Err.Error(), "lost update") {
		t.Fatalf("want the lost update, got %s", icb.report())
	}
	// The race needs one preemption, and so does the schedule reported.
	replayed, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		earlyRaceModel(s, 8)
		s.AtQuiescence(func(*State) error {
			if s.run.preempts != 1 {
				return fmt.Errorf("%d preemptions", s.run.preempts)
			}
			return nil
		})
	}, []Option{Replay(icb.Schedule)}, nil, 0)
	if !replayed.Violated || !strings.Contains(replayed.Err.Error(), "lost update") {
		t.Fatalf("replaying %s: %s", icb.Schedule, replayed.report())
	}
}

// Parallel workers report the violation a single worker reports.
func TestIterativeBoundingWorkersReportTheFirstViolation(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, false) }
	opts := []Option{IterativeBounding()}
	one, _ := exploreBubble(t, model, opts, nil, 0)
	if !one.Violated {
		t.Fatal("expected a violation")
	}
	for i := range 20 {
		f := newFrontier(4, 200000)
		f.iterative = true
		got, _ := exploreWorkers(t, model, opts, f, 4)
		if got.Schedule != one.Schedule || got.Err.Error() != one.Err.Error() {
			t.Fatalf("attempt %d: 4 workers reported %s (%v), 1 worker %s (%v)", i, got.Schedule, got.Err, one.Schedule, one.Err)
		}
	}
}

// An exploration cut short says up to how many preemptions it made every
// run.
func TestIterativeBoundingReportsTheLevelExplored(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, true) }
	res, _ := explore(t, model, []Option{MaxPreemptions(2), IterativeBounding(), MaxRuns(1)})
	if res.Complete || res.CompleteLevel != -1 || !strings.Contains(res.report(), "not every run without a preemption explored yet") {
		t.Fatalf("after one run: %s", res.report())
	}
	full, _ := explore(t, model, []Option{MaxPreemptions(2), IterativeBounding()})
	res, _ = explore(t, model, []Option{MaxPreemptions(2), IterativeBounding(), MaxRuns(full.Runs - 1)})
	if res.Complete || res.CompleteLevel != 1 || !strings.Contains(res.report(), "every run of up to 1 preemption explored") {
		t.Fatalf("one run short of %d: %s", full.Runs, res.report())
	}
}

// An exploration cut short and resumed from its checkpoint, again and again,
// makes the runs of one exploration to the end.
func TestIterativeBoundingCheckpointResumes(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprintf("%d workers", workers), func(t *testing.T) {
			var full, resumed atomic.Int64
			Explore(t, func(t *testing.T, s *Sim) {
				counterModel(s, true)
				s.Seed(func() { full.Add(1) })
			}, MaxPreemptions(2), Workers(workers), IterativeBounding())

			path := filepath.Join(t.TempDir(), "ckpt")
			t.Setenv("DETEST_CHECKPOINT", path)
			rounds := 0
			for {
				rounds++
				Explore(t, func(t *testing.T, s *Sim) {
					counterModel(s, true)
					s.Seed(func() { resumed.Add(1) })
				}, MaxPreemptions(2), Workers(workers), MaxRuns(30), IterativeBounding())
				if _, err := os.Stat(path); os.IsNotExist(err) {
					break
				}
				if rounds > 100 {
					t.Fatal("the exploration does not finish")
				}
			}
			if rounds < 3 || resumed.Load() != full.Load() {
				t.Fatalf("%d rounds explored %d runs, one exploration %d", rounds, resumed.Load(), full.Load())
			}
		})
	}
}

// A checkpoint keeps the pass and each subtree's level, and one strategy
// does not resume another's.
func TestIterativeBoundingCheckpointKeepsLevels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ckpt.json")
	f := newFrontier(1, 10)
	f.iterative, f.stack, f.level, f.higher = true, nil, 2, true
	f.push([]subtree{
		{prefix: []choice{{label: "step", n: 2, picked: 1}}, level: 1},
		{prefix: []choice{{label: "step", n: 3, picked: 2}}, level: 2},
	})
	if err := f.save(path); err != nil {
		t.Fatal(err)
	}
	g := newFrontier(1, 10)
	g.iterative = true
	if err := g.load(path); err != nil {
		t.Fatal(err)
	}
	if g.level != 2 || !g.higher || len(g.stack) != 2 || g.stack[0].level != 1 || g.stack[1].level != 2 {
		t.Fatalf("level %d, higher %v, stack %v", g.level, g.higher, g.stack)
	}
	if err := newFrontier(1, 10).load(path); err == nil || !strings.Contains(err.Error(), "only IterativeBounding resumes") {
		t.Fatalf("DepthFirst resuming it: %v", err)
	}

	dfs := filepath.Join(t.TempDir(), "dfs.json")
	if err := newFrontier(1, 10).save(dfs); err != nil {
		t.Fatal(err)
	}
	if err := g.load(dfs); err == nil || !strings.Contains(err.Error(), "without IterativeBounding") {
		t.Fatalf("IterativeBounding resuming DepthFirst's: %v", err)
	}
}

// A checkpoint whose levels do not fit its pass is refused, rather than made
// again unchecked or counted at another pass.
func TestIterativeBoundingCheckpointRefusesLevelsOutsideItsPass(t *testing.T) {
	for _, body := range []string{
		`{"version":5,"runs":0,"subtrees":[[]],"level":-1,"levels":[0]}`,
		`{"version":5,"runs":0,"subtrees":[[]],"level":1,"levels":[2]}`,
		`{"version":5,"runs":0,"subtrees":[[]],"level":1,"levels":[-1]}`,
		`{"version":5,"runs":0,"subtrees":[[]],"level":1}`,
	} {
		path := filepath.Join(t.TempDir(), "ckpt.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		f := newFrontier(1, 10)
		f.iterative = true
		if err := f.load(path); err == nil || !strings.Contains(err.Error(), "is not a checkpoint of this detest") {
			t.Errorf("%s: got %v, want a refusal", body, err)
		}
	}
}

// levelOutcomes explores model under IterativeBounding to the end and returns
// the outcomes of the runs each pass made at its own level. The runs a pass
// makes again report nothing, so an outcome only they reach is one the
// exploration misses.
func levelOutcomes(t *testing.T, model func(t *testing.T, s *Sim), opts ...Option) []outcome {
	t.Helper()
	var out []outcome
	opts = append(opts, IterativeBounding(), exploreAll(), func(s *Sim) {
		s.depObserve = func(r *run, v *violation) {
			if r.preempts == r.s.frontier.level {
				out = append(out, outcomeOf(r, v))
			}
		}
	})
	res, _ := exploreBubble(t, model, opts, nil, 0)
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	if !res.Complete && !res.Violated {
		t.Fatalf("the exploration did not finish: %s", res.report())
	}
	return out
}

// Under PartialOrder each pass is a reduced exploration under its level, and
// the runs the passes make at their own level reach every outcome the passes
// without the reduction reach at theirs.
func TestIterativeBoundingPartialOrderReachesEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		model func(t *testing.T, s *Sim)
		opts  []Option
	}{
		{"counter", func(t *testing.T, s *Sim) { counterModel(s, true) }, nil},
		{"lost update", func(t *testing.T, s *Sim) { counterModel(s, false) }, nil},
		{"lock", lockModel, []Option{MaxPreemptions(1)}},
		{"lock unbounded", lockModel, nil},
		{"contended c=2", contendedModel, []Option{MaxPreemptions(2)}},
		{"crash tx", crashTxModel, []Option{MaxCrashes(1), MaxPreemptions(2)}},
		{"dropped", droppedModel, nil},
		{"unique savepoint", uniqueSavepointModel(false), []Option{EagerStart(false)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			full := levelOutcomes(t, c.model, slices.Clone(c.opts)...)
			reduced := levelOutcomes(t, c.model, append(slices.Clone(c.opts), PartialOrder())...)
			keys := map[string]bool{}
			for _, o := range reduced {
				keys[o.key()] = true
			}
			missing := 0
			for _, o := range full {
				if !keys[o.key()] {
					missing++
					if missing <= 3 {
						t.Errorf("the reduction misses the outcome of the run %s, verdict %q, histories %v", o.picks, o.verdict, slices.Sorted(maps.Keys(o.locals)))
					}
				}
			}
			t.Logf("%d runs at their level, %d under PartialOrder", len(full), len(reduced))
			if missing > 0 {
				t.Errorf("%d outcomes missed", missing)
			}
			if len(reduced) > len(full) {
				t.Errorf("the reduction made more runs at their level than the passes without it")
			}
		})
	}
}

// A pass under PartialOrder makes the runs of exactly its level once each,
// and the runs of fewer again, as the passes without it do.
func TestIterativeBoundingPartialOrderOrdersRunsByPreemptions(t *testing.T) {
	var levels []int
	model := func(t *testing.T, s *Sim) {
		counterModel(s, true)
		s.AtQuiescence(func(*State) error {
			switch level := s.frontier.level; {
			case s.run.preempts == level:
				levels = append(levels, level)
			case s.run.preempts > level:
				return fmt.Errorf("a run of %d preemptions in the pass of %d", s.run.preempts, level)
			}
			return nil
		})
	}
	res, _ := exploreBubble(t, model, []Option{MaxPreemptions(2), IterativeBounding(), PartialOrder()}, nil, 0)
	if !res.Complete || res.Violated || len(levels) != res.Runs {
		t.Fatalf("%d runs at their level: %s", len(levels), res.report())
	}
	for i := 1; i < len(levels); i++ {
		if levels[i] < levels[i-1] {
			t.Fatalf("run %d spent %d preemptions after a run that spent %d", i, levels[i], levels[i-1])
		}
	}
	if levels[len(levels)-1] != 2 {
		t.Fatalf("the last run spent %d preemptions, want 2", levels[len(levels)-1])
	}
}

// A schedule found in a pass under PartialOrder carries the pass's bound,
// and replays under it with or without either option.
func TestIterativeBoundingPartialOrderViolationReplays(t *testing.T) {
	model := func(t *testing.T, s *Sim) { earlyRaceModel(s, 4) }
	found, _ := exploreBubble(t, model, []Option{IterativeBounding(), PartialOrder()}, nil, 0)
	if !found.Violated || !strings.Contains(found.Schedule, "bound:1:") {
		t.Fatalf("want a violation in the pass of 1 preemption, got %s", found.report())
	}
	for _, opts := range [][]Option{nil, {PartialOrder()}, {IterativeBounding()}, {IterativeBounding(), PartialOrder()}} {
		replayed, _ := exploreBubble(t, model, append(opts, Replay(found.Schedule)), nil, 0)
		if replayed.Fatal != nil || !replayed.Violated || replayed.Err.Error() != found.Err.Error() {
			t.Fatalf("replaying %s with %d options: %s", found.Schedule, len(opts), replayed.report())
		}
	}
}

// One worker stops at the first violation of a pass, as depth first does,
// rather than going on through the subtrees of the options before the one
// each run took.
func TestIterativeBoundingStopsAtTheFirstViolation(t *testing.T) {
	var made, first int
	model := func(t *testing.T, s *Sim) {
		var x, r1, r2 int
		s.Seed(func() { x, r1, r2 = 0, 0, 0 })
		s.Manual("a", 1, func(p *Proc) error {
			for i := range 3 {
				p.Step("a %d", i)
			}
			x = 1
			p.Step("between")
			x = 2
			return nil
		})
		s.Manual("b", 1, func(p *Proc) error {
			for i := range 3 {
				p.Step("b %d", i)
			}
			r1 = x
			p.Step("between")
			r2 = x
			return nil
		})
		s.AtQuiescence(func(*State) error {
			if s.run.preempts < s.frontier.level {
				return nil // made again
			}
			made++
			if r1 == 1 && r2 == 2 {
				if first == 0 {
					first = made
				}
				return fmt.Errorf("saw x between its writes")
			}
			return nil
		})
	}
	res, _ := exploreBubble(t, model, []Option{IterativeBounding()}, nil, 0)
	if !res.Violated || res.Runs != first {
		t.Fatalf("%d runs, the first violation at run %d: %s", res.Runs, first, res.report())
	}
}

func TestChoiceRankFollowsTheOrderOfAPass(t *testing.T) {
	var got []int
	for v := range 4 {
		got = append(got, choice{picked: v, free: 2}.rank())
	}
	if want := []int{1, 2, 0, 3}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ranks %v, want %v", got, want)
	}
}
