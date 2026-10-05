package detest

import (
	"testing"
	"time"
)

// applyEnv applies opts and then the environment's options, as Explore does.
func applyEnv(t *testing.T, opts ...Option) *Sim {
	t.Helper()
	env, err := envOptions()
	if err != nil {
		t.Fatal(err)
	}
	s := newSimDefaults()
	for _, o := range append(opts, env...) {
		o(s)
	}
	return s
}

func TestEnvOverridesTheRunBudget(t *testing.T) {
	t.Setenv("DETEST_WORKERS", "3")
	t.Setenv("DETEST_MAX_RUNS", "10")
	t.Setenv("DETEST_MAX_DURATION", "90s")
	s := applyEnv(t, Workers(8), MaxRuns(1000), MaxDuration(time.Hour))
	if s.workers != 3 || s.maxRuns != 10 || s.maxDuration != 90*time.Second {
		t.Fatalf("workers %d, maxRuns %d, maxDuration %v; want 3, 10, 1m30s", s.workers, s.maxRuns, s.maxDuration)
	}
}

func TestEnvSeedOverridesRandomOnly(t *testing.T) {
	t.Setenv("DETEST_SEED", "42")
	if s := applyEnv(t, Random(1)); !s.strategy.random || s.strategy.seed != 42 {
		t.Fatalf("got %+v, want Random with seed 42", s.strategy)
	}
	if s := applyEnv(t, Prioritized(1, 3)); !s.strategy.prioritized || s.strategy.seed != 42 || s.strategy.depth != 3 {
		t.Fatalf("got %+v, want Prioritized with seed 42 at depth 3", s.strategy)
	}
	if s := applyEnv(t, DepthFirst()); s.strategy.random {
		t.Fatalf("got %+v, want DepthFirst left as it is", s.strategy)
	}
}

func TestEnvLeavesUnsetOptionsAlone(t *testing.T) {
	for _, k := range []string{"DETEST_WORKERS", "DETEST_MAX_RUNS", "DETEST_MAX_DURATION", "DETEST_SEED"} {
		t.Setenv(k, "")
	}
	s := applyEnv(t, Workers(2), MaxRuns(7), Random(5))
	if s.workers != 2 || s.maxRuns != 7 || s.strategy.seed != 5 {
		t.Fatalf("workers %d, maxRuns %d, seed %d; want the test's 2, 7, 5", s.workers, s.maxRuns, s.strategy.seed)
	}
}

func TestEnvRefusesBadValues(t *testing.T) {
	for k, v := range map[string]string{
		"DETEST_WORKERS":      "four",
		"DETEST_MAX_RUNS":     "0",
		"DETEST_MAX_DURATION": "10",
		"DETEST_SEED":         "-1",
	} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			if _, err := envOptions(); err == nil {
				t.Fatalf("%s=%s: want an error", k, v)
			}
		})
	}
}
