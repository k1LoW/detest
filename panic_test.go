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
		s.ExpectViolation("panic in b#1: boom")
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
		if !regexp.MustCompile(`panic in b#1: boom`).Match(out) || regexp.MustCompile(`(?m)^panic: `).Match(out) {
			t.Fatalf("%v: expected a test failure naming the panic, not a crash:\n%s", env, out)
		}
		if !regexp.MustCompile(`detest\.racyPanic`).Match(out) {
			t.Fatalf("%v: expected the stack the panic was raised on:\n%s", env, out)
		}
		return string(out)
	}
	m := regexp.MustCompile(`DETEST_REPLAY=(\S+)`).FindStringSubmatch(run())
	if m == nil {
		t.Fatal("no schedule reported")
	}
	run("DETEST_REPLAY=" + m[1])
}

// Shrinking keeps a rerun that panics in the same process with the same value,
// even though its stack differs, and drops one that panics otherwise.
func TestPanicSameIgnoresStack(t *testing.T) {
	v := func(proc string, value any, stack string) *violation {
		return &violation{kind: "panic", err: &procPanic{proc: proc, value: value, stack: stack}}
	}
	const stack = "goroutine 1 [running]:\n\tsched.go:+0x10"
	base := v("b#2", "boom", stack)
	tests := []struct {
		name  string
		other *violation
		want  bool
	}{
		{"different stack", v("b#2", "boom", "goroutine 7 [running]:\n\tsched.go:+0x24"), true},
		{"different process", v("a#1", "boom", stack), false},
		{"different value", v("b#2", "bang", stack), false},
		{"different kind", &violation{kind: "progress", err: base.err}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := base.same(tt.other); got != tt.want {
				t.Errorf("same() = %v, want %v", got, tt.want)
			}
		})
	}
	if v("b#2", "1", stack).same(v("b#2", 1, stack)) {
		t.Error(`panic("1") and panic(1) compare as the same`)
	}
}

// A process that panics after resuming from a primitive detest does not model
// is reported too, even when nothing else is left to run.
func TestPanicAfterBlockingOutside(t *testing.T) {
	var ch chan struct{}
	Explore(t, func(t *testing.T, s *Sim) {
		s.Seed(func() { ch = make(chan struct{}) })
		s.Manual("a", 1, func(p *Proc) error {
			select {
			case <-ch:
				return nil
			default:
			}
			<-ch
			panic("boom")
		})
		s.Manual("b", 1, func(p *Proc) error {
			p.Step("close")
			close(ch)
			return nil
		})
		s.ExpectViolation("boom")
	})
}
