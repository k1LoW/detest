package detest

import (
	"os"
	"path/filepath"
	"testing"
)

// The schedule a violation is reported with, pinned with Replay, replays
// that violation.
func TestReplayPinsACounterexample(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, false) }
	res, _ := exploreBubble(t, model, nil, nil, 0)
	if !res.Violated {
		t.Fatal("expected a violation")
	}
	Explore(t, func(t *testing.T, s *Sim) {
		model(t, s)
		s.ExpectViolation("lost update")
	}, Replay(res.Schedule))
}

// A pinned schedule runs once: no other schedule of the simulation is
// explored, so a violation elsewhere goes unnoticed.
func TestReplayRunsOneSchedule(t *testing.T) {
	model := func(t *testing.T, s *Sim) { counterModel(s, false) }
	Explore(t, model, Replay("0")) // every process runs to completion in turn
}

// A replay neither resumes nor ends the exploration a checkpoint holds.
func TestReplayLeavesTheCheckpoint(t *testing.T) {
	ckpt := filepath.Join(t.TempDir(), "ckpt")
	if err := os.WriteFile(ckpt, []byte(`{"version":1,"runs":10,"subtrees":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DETEST_CHECKPOINT", ckpt)
	Explore(t, func(t *testing.T, s *Sim) { counterModel(s, true) }, Replay("0"))
	if _, err := os.Stat(ckpt); err != nil {
		t.Fatalf("the checkpoint is gone: %v", err)
	}
}
