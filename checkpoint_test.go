package detest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
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

// A SIGINT or SIGTERM stops an exploration that saves to DETEST_CHECKPOINT as
// MaxDuration does: the runs in flight finish, the rest is saved and the test
// fails naming the signal, and resuming explores the runs one exploration
// makes. A second signal ends the test binary as it would without detest.
// The signals are real, so the exploration runs in a child process.
func TestCheckpointSavedOnSignal(t *testing.T) {
	if mode := os.Getenv("DETEST_SIGNAL_CHILD"); mode != "" {
		var runs atomic.Int64
		Explore(t, func(t *testing.T, s *Sim) {
			counterModel(s, true)
			s.Seed(func() {
				if runs.Add(1) != 3 {
					return
				}
				p, _ := os.FindProcess(os.Getpid())
				_ = p.Signal(map[string]os.Signal{"INT": os.Interrupt, "TERM": syscall.SIGTERM, "TWICE": os.Interrupt}[mode])
				if mode == "TWICE" {
					for interruptedBy.Load() == nil {
						runtime.Gosched()
					}
					_ = p.Signal(os.Interrupt)
					select {} // the second signal ends the process
				}
			})
		}, MaxPreemptions(2))
		return
	}
	var full atomic.Int64
	Explore(t, func(t *testing.T, s *Sim) {
		counterModel(s, true)
		s.Seed(func() { full.Add(1) })
	}, MaxPreemptions(2))

	child := func(mode, path string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointSavedOnSignal$", "-test.v") //nolint:gosec // the test binary itself
		cmd.Env = append(os.Environ(), "DETEST_SIGNAL_CHILD="+mode, "DETEST_CHECKPOINT="+path)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, sig := range []string{"INT", "TERM"} {
		t.Run(sig, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ckpt")
			out, err := child(sig, path)
			if err == nil || !strings.Contains(out, "detest: interrupted by") || !strings.Contains(out, "DETEST_CHECKPOINT="+path) {
				t.Fatalf("expected the child to fail naming the signal and the checkpoint (%v):\n%s", err, out)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var ck checkpoint
			if err := json.Unmarshal(b, &ck); err != nil {
				t.Fatal(err)
			}
			if ck.Runs != 3 {
				t.Fatalf("the checkpoint holds %d runs, want the 3 before the signal", ck.Runs)
			}
			var resumed atomic.Int64
			t.Setenv("DETEST_CHECKPOINT", path)
			for range 100 {
				Explore(t, func(t *testing.T, s *Sim) {
					counterModel(s, true)
					s.Seed(func() { resumed.Add(1) })
				}, MaxPreemptions(2))
				if _, err := os.Stat(path); os.IsNotExist(err) {
					break
				}
			}
			if got := int64(ck.Runs) + resumed.Load(); got != full.Load() {
				t.Fatalf("%d runs before the signal and %d after, one exploration %d", ck.Runs, resumed.Load(), full.Load())
			}
		})
	}
	t.Run("twice", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ckpt")
		out, err := child("TWICE", path)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Success() || strings.Contains(out, "detest: interrupted by") {
			t.Fatalf("expected the second signal to end the child (%v):\n%s", err, out)
		}
	})
}
