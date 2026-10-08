package detest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An exploration cut short by MaxRuns and resumed from its checkpoint, again
// and again, explores the same runs as one exploration to the end.
func TestCheckpointResumes(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(map[int]string{1: "one worker", 4: "four workers"}[workers], func(t *testing.T) {
			var full, resumed atomic.Int64 // the workers' seeds run concurrently
			Explore(t, func(t *testing.T, s *Sim) {
				counterModel(s, true)
				s.Seed(func() { full.Add(1) })
			}, MaxPreemptions(2), Workers(workers))

			path := filepath.Join(t.TempDir(), "ckpt")
			t.Setenv("DETEST_CHECKPOINT", path)
			rounds := 0
			for {
				rounds++
				Explore(t, func(t *testing.T, s *Sim) {
					counterModel(s, true)
					s.Seed(func() { resumed.Add(1) })
				}, MaxPreemptions(2), Workers(workers), MaxRuns(30))
				if _, err := os.Stat(path); os.IsNotExist(err) {
					break // finished: the checkpoint is gone
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

// An exploration cut short by MaxDuration is saved to its checkpoint as one
// cut short by MaxRuns is, and makes progress however short the duration.
func TestCheckpointResumesAfterMaxDuration(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(map[int]string{1: "one worker", 4: "four workers"}[workers], func(t *testing.T) {
			var full, resumed atomic.Int64
			Explore(t, func(t *testing.T, s *Sim) {
				counterModel(s, true)
				s.Seed(func() { full.Add(1) })
			}, MaxPreemptions(2), Workers(workers))

			path := filepath.Join(t.TempDir(), "ckpt")
			t.Setenv("DETEST_CHECKPOINT", path)
			rounds := 0
			for {
				rounds++
				Explore(t, func(t *testing.T, s *Sim) {
					counterModel(s, true)
					s.Seed(func() { resumed.Add(1) })
				}, MaxPreemptions(2), Workers(workers), MaxDuration(time.Nanosecond))
				if _, err := os.Stat(path); os.IsNotExist(err) {
					break
				}
				if int64(rounds) > full.Load() {
					t.Fatal("the exploration does not finish")
				}
			}
			if rounds < 2 || resumed.Load() != full.Load() {
				t.Fatalf("%d rounds explored %d runs, one exploration %d", rounds, resumed.Load(), full.Load())
			}
		})
	}
}

// A violation found in a resumed exploration ends it and removes the
// checkpoint; the rounds before it do not fail an ExpectViolation test.
func TestCheckpointFindsTheViolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ckpt")
	t.Setenv("DETEST_CHECKPOINT", path)
	for range 100 {
		Explore(t, func(t *testing.T, s *Sim) {
			counterModel(s, false)
			s.ExpectViolation("lost update")
		}, MaxRuns(2))
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
	}
	t.Fatal("the violation was not found")
}

// A checkpoint or schedule of another version of the simulation fails the test
// with the reason, rather than crashing. The exploration runs in a child
// process because the test is meant to fail.
func TestStaleCheckpointFailsTheTest(t *testing.T) {
	t.Parallel()
	if os.Getenv("DETEST_STALE_CHILD") == "1" {
		Explore(t, func(t *testing.T, s *Sim) { counterModel(s, true) })
		return
	}
	stale := filepath.Join(t.TempDir(), "ckpt")
	if err := os.WriteFile(stale, fmt.Appendf(nil, `{"version":%d,"runs":10,"subtrees":[[{"label":"step","n":99,"picked":1}]]}`, checkpointVersion), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, env := range [][]string{
		{"DETEST_CHECKPOINT=" + stale},
		{"DETEST_REPLAY=7"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStaleCheckpointFailsTheTest$") //nolint:gosec // the test binary itself
		cmd.Env = append(append(os.Environ(), "DETEST_STALE_CHILD=1"), env...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("%v: expected the child to fail:\n%s", env, out)
		}
		if !strings.Contains(string(out), "another version of") || strings.Contains(string(out), "panic:") {
			t.Fatalf("%v: expected a test failure naming the cause, not a panic:\n%s", env, out)
		}
	}
}

// A resumed exploration still lists the statements refused before the
// checkpoint.
func TestCheckpointKeepsRefusedStatements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ckpt.json")
	f := newFrontier(1, 10)
	f.refuse("detest: unsupported SQL (x): SELECT 1")
	if err := f.save(path); err != nil {
		t.Fatal(err)
	}
	g := newFrontier(1, 10)
	if err := g.load(path); err != nil {
		t.Fatal(err)
	}
	if got, want := g.refusals(), []string{"detest: unsupported SQL (x): SELECT 1"}; !slices.Equal(got, want) {
		t.Fatalf("refusals %q, want %q", got, want)
	}
}

// A checkpoint of an earlier format names prefixes of another tree, so it is
// refused rather than resumed.
func TestCheckpointOfAnEarlierVersionIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ckpt.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"runs":0,"subtrees":[[]]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newFrontier(1, 10).load(path); err == nil || !strings.Contains(err.Error(), "version 1") {
		t.Fatalf("got %v, want a refusal of version 1", err)
	}
}
