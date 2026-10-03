package detest

import (
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// racyPanic panics in b only when a has run first.
func racyPanic(s *Sim) {
	set := false
	s.Seed(func() { set = false })
	s.Manual("a", 1, func(p *Proc) error {
		p.Step("set")
		set = true
		return nil
	})
	s.Manual("b", 1, func(p *Proc) error {
		p.Step("check")
		if set {
			panic("boom")
		}
		return nil
	})
}

// A panic under one interleaving is a violation like any other.
func TestPanicIsAViolation(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		racyPanic(s)
		s.ExpectViolation("panic in b#2: boom")
	})
}

// A panic fails the test with the schedule that replays it, rather than
// taking the test binary down, and the schedule replays it. It runs in a child
// process because the test is meant to fail.
func TestPanicReportsItsSchedule(t *testing.T) {
	if os.Getenv("DETEST_PANIC_CHILD") == "1" {
		Explore(t, func(t *testing.T, s *Sim) { racyPanic(s) })
		return
	}
	run := func(env ...string) string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPanicReportsItsSchedule$") //nolint:gosec // the test binary itself
		cmd.Env = append(append(os.Environ(), "DETEST_PANIC_CHILD=1"), env...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("%v: expected the child to fail:\n%s", env, out)
		}
		if !regexp.MustCompile(`panic in b#2: boom`).Match(out) || regexp.MustCompile(`(?m)^panic: `).Match(out) {
			t.Fatalf("%v: expected a test failure naming the panic, not a crash:\n%s", env, out)
		}
		return string(out)
	}
	m := regexp.MustCompile(`DETEST_REPLAY=(\S+)`).FindStringSubmatch(run())
	if m == nil {
		t.Fatal("no schedule reported")
	}
	run("DETEST_REPLAY=" + m[1])
}
