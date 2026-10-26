package detest

import (
	"fmt"
	"os"
	"path/filepath"
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

// One worker makes the runs in order of the preemptions they spend.
func TestIterativeBoundingOrdersRunsByPreemptions(t *testing.T) {
	var levels []int
	model := func(t *testing.T, s *Sim) {
		counterModel(s, true)
		s.AtQuiescence(func(*State) error {
			levels = append(levels, s.run.preempts)
			return nil
		})
	}
	res, _ := exploreBubble(t, model, []Option{MaxPreemptions(2), IterativeBounding()}, nil, 0)
	if !res.Complete || res.Violated {
		t.Fatal(res.report())
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

// A checkpoint keeps each subtree's level, and one strategy does not resume
// another's.
func TestIterativeBoundingCheckpointKeepsLevels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ckpt.json")
	f := newFrontier(1, 10)
	f.iterative, f.stack = true, nil
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
	if g.level != 1 || len(g.stack) != 1 || g.stack[0].level != 1 || len(g.deferred) < 3 || len(g.deferred[2]) != 1 {
		t.Fatalf("level %d, stack %v, deferred %v", g.level, g.stack, g.deferred)
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

// The subtrees of one run are saved as branches off its path, and a
// checkpoint resumed and saved again keeps every branch's prefix.
func TestIterativeBoundingCheckpointSharesPaths(t *testing.T) {
	run := []choice{{label: "step", n: 2}, {label: "step", n: 3, picked: 1}, {label: "external", n: 2}}
	want := []subtree{
		{from: run[:3], pick: 1, level: 0},
		{from: run[:1], pick: 1, level: 1},
	}
	f := newFrontier(1, 10)
	f.iterative, f.stack = true, nil
	f.push(want)
	dir := t.TempDir()
	for round := range 2 {
		path := filepath.Join(dir, fmt.Sprint(round))
		if err := f.save(path); err != nil {
			t.Fatal(err)
		}
		f = newFrontier(1, 10)
		f.iterative = true
		if err := f.load(path); err != nil {
			t.Fatal(err)
		}
	}
	got := f.subtrees()
	if len(got) != len(want) {
		t.Fatalf("%d subtrees, want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i].choices(), want[i].choices()
		if fmt.Sprint(g) != fmt.Sprint(w) || got[i].level != want[i].level {
			t.Fatalf("subtree %d: %v at level %d, want %v at level %d", i, g, got[i].level, w, want[i].level)
		}
	}
}

func TestIterativeBoundingRefusesPartialOrder(t *testing.T) {
	err := partialOrderRefusal([]Option{PartialOrder(), IterativeBounding()}, false)
	if err == nil || !strings.Contains(err.Error(), "IterativeBounding") {
		t.Fatalf("got %v, want a refusal naming IterativeBounding", err)
	}
}
