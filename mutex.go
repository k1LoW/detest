package detest

import (
	"fmt"
	"sync"
)

// waitable is a lock modeled by detest that a process can wait for.
type waitable interface {
	// holders returns the processes waiter is waiting for.
	holders(waiter *Proc) []*Proc
	reset()
	// crash drops what p holds or waits for, reporting whether p held it.
	crash(p *Proc) bool
}

// Mutex is a sync.Locker for injecting into production code in place of a
// sync.Mutex. Lock is a yield point of the calling process, and a process that
// finds the mutex held waits inside detest, so the scheduler knows who holds
// it and who waits for it. A wait that closes a cycle with other mutexes or row
// locks is reported as a progress violation.
type Mutex struct {
	name   string
	s      *Sim
	held   bool
	holder *Proc // nil while held by code outside any process, such as a seed
}

// Mutex registers a mutex. It is unlocked at the start of every run.
func (s *Sim) Mutex(name string) *Mutex {
	s.declare("Mutex")
	mu := &Mutex{name: name, s: s}
	s.locks = append(s.locks, mu)
	return mu
}

// Lock acquires the mutex, waiting while another process holds it.
func (mu *Mutex) Lock() {
	p := mu.s.Current()
	defer func() { absorbAbort(recover(), p, nil) }()
	goneStale(p)
	if p == nil {
		if mu.held {
			panic(fmt.Sprintf("detest: mutex %s locked outside a process while held", mu.name))
		}
		mu.held = true
		return
	}
	p.yieldf("lock %s", mu.name)
	for mu.held {
		p.blockOnLock(mu, fmt.Sprintf("mutex %s held by %s", mu.name, procName(mu.holder)))
	}
	mu.held, mu.holder = true, p
}

// Unlock releases the mutex. As with sync.Mutex, any goroutine may unlock it.
func (mu *Mutex) Unlock() {
	if mu.s.Current().stale() {
		return // it never took the lock in this run
	}
	if r := mu.s.run; r != nil && r.over() {
		// An adopted goroutine whose Lock the end of the run cut short unlocks
		// a mutex it never got. The next run resets the mutex anyway.
		mu.held, mu.holder = false, nil
		return
	}
	if !mu.held {
		panic(fmt.Sprintf("detest: unlock of unlocked mutex %s", mu.name))
	}
	mu.held, mu.holder = false, nil
	mu.s.released(mu, "unlock "+mu.name)
}

func (mu *Mutex) reset() { mu.held, mu.holder = false, nil }

func (mu *Mutex) crash(p *Proc) bool {
	if !mu.held || mu.holder != p {
		return false
	}
	mu.held, mu.holder = false, nil
	return true
}

func (mu *Mutex) holders(*Proc) []*Proc {
	if mu.holder == nil {
		return nil
	}
	return []*Proc{mu.holder}
}

// RWMutex is the sync.RWMutex counterpart of Mutex. As with sync.RWMutex, a
// process waiting in Lock keeps new readers out, so a reader that takes the
// read lock again while a writer waits deadlocks; detest reports that cycle.
type RWMutex struct {
	name           string
	s              *Sim
	writing        bool
	writer         *Proc // nil while write-locked outside any process
	readers        map[*Proc]int
	outsideReaders int
	pendingWriters map[*Proc]bool
}

// RWMutex registers a reader/writer mutex. It is unlocked at the start of
// every run.
func (s *Sim) RWMutex(name string) *RWMutex {
	s.declare("RWMutex")
	rw := &RWMutex{name: name, s: s}
	rw.reset()
	s.locks = append(s.locks, rw)
	return rw
}

// Lock acquires the write lock, waiting while any reader or writer holds it.
func (rw *RWMutex) Lock() {
	p := rw.s.Current()
	defer func() { absorbAbort(recover(), p, nil) }()
	goneStale(p)
	if p == nil {
		if rw.writing || rw.readLocked() {
			panic(fmt.Sprintf("detest: rwmutex %s locked outside a process while held", rw.name))
		}
		rw.writing = true
		return
	}
	p.yieldf("lock %s", rw.name)
	for rw.writing || rw.readLocked() {
		rw.pendingWriters[p] = true
		held := "readers"
		if rw.writing {
			held = "writer " + procName(rw.writer)
		}
		p.blockOnLock(rw, fmt.Sprintf("rwmutex %s held by %s", rw.name, held))
	}
	delete(rw.pendingWriters, p)
	rw.writing, rw.writer = true, p
}

// Unlock releases the write lock.
func (rw *RWMutex) Unlock() {
	if rw.s.Current().stale() {
		return
	}
	if r := rw.s.run; r != nil && r.over() {
		rw.writing, rw.writer = false, nil // as Mutex.Unlock
		return
	}
	if !rw.writing {
		panic(fmt.Sprintf("detest: unlock of unlocked rwmutex %s", rw.name))
	}
	rw.writing, rw.writer = false, nil
	rw.s.released(rw, "unlock "+rw.name)
}

// RLock acquires a read lock, waiting while a writer holds the lock or waits
// for it.
func (rw *RWMutex) RLock() {
	p := rw.s.Current()
	defer func() { absorbAbort(recover(), p, nil) }()
	goneStale(p)
	if p == nil {
		if rw.writing {
			panic(fmt.Sprintf("detest: rwmutex %s read-locked outside a process while write-locked", rw.name))
		}
		rw.outsideReaders++
		return
	}
	p.yieldf("rlock %s", rw.name)
	for rw.writing || len(rw.pendingWriters) > 0 {
		held := "a waiting writer"
		if rw.writing {
			held = "writer " + procName(rw.writer)
		}
		p.blockOnLock(rw, fmt.Sprintf("rwmutex %s held by %s", rw.name, held))
	}
	rw.readers[p]++
}

// RUnlock releases a read lock. A read lock taken by another goroutine is
// released when the calling process holds none, as sync.RWMutex allows.
func (rw *RWMutex) RUnlock() {
	// Resolved first, as a goroutine of an ended run must not read s.run,
	// which the scheduler may be setting for the next run.
	p := rw.s.Current()
	if p.stale() {
		return
	}
	if r := rw.s.run; r != nil && r.over() {
		return // as Mutex.Unlock, the next run resets the read locks
	}
	switch {
	case p != nil && rw.readers[p] > 0:
	case rw.outsideReaders > 0:
		rw.outsideReaders--
		p = nil
	default:
		p = nil
		for _, q := range rw.s.run.procs {
			if rw.readers[q] > 0 {
				p = q
				break
			}
		}
		if p == nil {
			panic(fmt.Sprintf("detest: runlock of unlocked rwmutex %s", rw.name))
		}
	}
	if p != nil {
		if rw.readers[p]--; rw.readers[p] == 0 {
			delete(rw.readers, p)
		}
	}
	rw.s.released(rw, "runlock "+rw.name)
}

// RLocker returns a sync.Locker that calls RLock and RUnlock.
func (rw *RWMutex) RLocker() sync.Locker { return rlocker{rw} }

func (rw *RWMutex) reset() {
	rw.writing, rw.writer = false, nil
	rw.readers = map[*Proc]int{}
	rw.outsideReaders = 0
	rw.pendingWriters = map[*Proc]bool{}
}

func (rw *RWMutex) holders(waiter *Proc) []*Proc {
	var out []*Proc
	if rw.writer != nil {
		out = append(out, rw.writer)
	}
	if rw.pendingWriters[waiter] {
		for _, p := range rw.s.run.procs {
			// Including waiter itself catches an upgrade from a read lock.
			if rw.readers[p] > 0 {
				out = append(out, p)
			}
		}
		return out
	}
	for _, p := range rw.s.run.procs {
		if rw.pendingWriters[p] && p != waiter {
			out = append(out, p)
		}
	}
	return out
}

func (rw *RWMutex) crash(p *Proc) bool {
	delete(rw.pendingWriters, p)
	held := rw.readers[p] > 0
	delete(rw.readers, p)
	if rw.writing && rw.writer == p {
		rw.writing, rw.writer = false, nil
		held = true
	}
	return held
}

func (rw *RWMutex) readLocked() bool { return rw.outsideReaders > 0 || len(rw.readers) > 0 }

type rlocker struct{ rw *RWMutex }

func (l rlocker) Lock()   { l.rw.RLock() }
func (l rlocker) Unlock() { l.rw.RUnlock() }

func procName(p *Proc) string {
	if p == nil {
		return "code outside any process"
	}
	return p.name
}

// released records the release by the calling process and lets every process
// waiting for l retry; each re-checks the lock when it is resumed.
func (s *Sim) released(l waitable, op string) {
	r := s.run
	if r == nil || r.over() {
		return // after the run, the next one resets the locks
	}
	if p := s.Current(); p != nil {
		r.noteAt(p, "%s", op)
	}
	for _, w := range r.procs {
		if w.state == stateBlockedLock && w.waitLock == l {
			w.state = stateReady
			w.waitLock = nil
		}
	}
}

// blockOnLock blocks p until l is released. Unlike Postgres for row locks,
// nothing breaks a cycle through a mutex: the processes hang. A wait that
// closes one is reported instead.
func (p *Proc) blockOnLock(l waitable, what string) {
	for _, h := range l.holders(p) {
		if p.r.waitsFor(h, p) {
			p.r.pending = &violation{kind: "progress", err: fmt.Errorf("process %s waits for %s, closing a cycle of waits that nothing can break", p.name, what)}
			break
		}
	}
	p.state = stateBlockedLock
	p.waitLock = l
	p.r.note(p, "waits for %s", what)
	p.send(procEvent{kind: evBlocked})
	p.wait()
}

// blockers returns the processes p waits for: the owner of the transaction
// whose row lock it needs, or the holders of the lock it needs.
func (p *Proc) blockers() []*Proc {
	if p.state != stateBlockedLock {
		return nil
	}
	if p.waitRow != nil {
		var out []*Proc
		for _, o := range p.waitRow.blockers() {
			out = append(out, o.p)
		}
		return out
	}
	if p.waitLock != nil {
		return p.waitLock.holders(p)
	}
	return nil
}

// waitsFor reports whether from is, directly or through other waiting
// processes, waiting for target.
func (r *run) waitsFor(from, target *Proc) bool {
	seen := map[*Proc]bool{}
	stack := []*Proc{from}
	for len(stack) > 0 {
		w := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if w == target {
			return true
		}
		if seen[w] {
			continue
		}
		seen[w] = true
		stack = append(stack, w.blockers()...)
	}
	return false
}
