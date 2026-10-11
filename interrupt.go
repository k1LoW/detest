package detest

import (
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
)

// interruptedBy is the signal that interrupted an exploration of this test
// binary, after which the explorations of later tests are skipped.
var interruptedBy atomic.Value // os.Signal

// interrupt stops an exploration that saves to DETEST_CHECKPOINT at the first
// SIGINT or SIGTERM, as MaxDuration does, so that the runs in flight finish
// and the rest is saved. The go command does not kill a test binary it was
// interrupted with, it waits for it, so the save has the time it needs.
type interrupt struct {
	mu   sync.Mutex
	f    *frontier
	sig  os.Signal
	ch   chan os.Signal
	done chan struct{}
}

// watchInterrupt starts catching the signals until stop is called.
func watchInterrupt() *interrupt {
	in := &interrupt{ch: make(chan os.Signal, 1), done: make(chan struct{})}
	signal.Notify(in.ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-in.ch:
			// Stop rather than Reset, which would also undo a handler the code
			// under test installed. With no other handler the second signal
			// ends the process as it would without detest.
			signal.Stop(in.ch)
			in.mu.Lock()
			defer in.mu.Unlock()
			in.sig = sig
			interruptedBy.Store(sig)
			if in.f != nil {
				in.f.expire()
			}
		case <-in.done:
		}
	}()
	return in
}

// watch stops f too, the frontier an exploration started over without
// EagerStart takes, or at once when the signal came already.
func (in *interrupt) watch(f *frontier) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.f = f
	if in.sig != nil {
		f.expire()
	}
}

// signal returns the signal caught, or nil.
func (in *interrupt) signal() os.Signal {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.sig
}

func (in *interrupt) stop() {
	signal.Stop(in.ch)
	close(in.done)
}
