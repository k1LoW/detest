package detest

import (
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing/synctest"
	"time"
)

// Goroutines the code under test starts itself, such as errgroup's or a
// worker pool's, are adopted as processes of their own the first time they
// call into detest. Each one is then scheduled like any process, so its
// statements and calls interleave with the others' instead of running under
// the name of whichever process the scheduler resumed last.

// procMainEntry is the entry of Proc.main, the function at the bottom of
// every goroutine detest starts for a process.
var procMainEntry = runtime.FuncForPC(reflect.ValueOf((*Proc).main).Pointer()).Entry()

// onProcGoroutine reports whether the calling goroutine is one detest started
// for a process, and whether it could tell. It walks the stack rather than
// reading the goroutine id, as runtime.Stack takes a lock shared by the whole
// runtime, which parallel workers would contend on at every statement.
func onProcGoroutine() (yes, sure bool) {
	var pcs [512]uintptr
	n := runtime.Callers(2, pcs[:])
	if n == len(pcs) {
		return false, false // deeper than the buffer, whose frames miss the bottom
	}
	// The bottom frames are runtime.goexit and the goroutine's function.
	for i := n - 1; i >= 0 && i >= n-3; i-- {
		if f := runtime.FuncForPC(pcs[i] - 1); f != nil && f.Entry() == procMainEntry {
			return true, true
		}
	}
	return false, true
}

// goroutineCreator returns the calling goroutine's id and the id of the
// goroutine that started it, from its full stack.
func goroutineCreator() (gid, creator string) {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, false)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	s := string(buf)
	gid = strings.TrimPrefix(s, "goroutine ")
	if i := strings.IndexByte(gid, ' '); i > 0 {
		gid = gid[:i]
	}
	const in = " in goroutine "
	if i := strings.LastIndex(s, in); i >= 0 {
		creator = s[i+len(in):]
		if j := strings.IndexAny(creator, " \n"); j >= 0 {
			creator = creator[:j]
		}
	}
	return gid, creator
}

// liveGoroutines returns the ids of every goroutine of the program. It stops
// the world, so it is called only when the run cannot go on without knowing
// which adopted goroutines returned.
func liveGoroutines() map[string]bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	live := map[string]bool{}
	for line := range strings.SplitSeq(string(buf), "\n") {
		if !strings.HasPrefix(line, "goroutine ") {
			continue
		}
		id := strings.TrimPrefix(line, "goroutine ")
		if i := strings.IndexByte(id, ' '); i > 0 {
			live[id[:i]] = true
		}
	}
	return live
}

// lookup resolves the calling goroutine when it may not be the resumed
// process, as when a process woke outside detest or the caller is not a
// goroutine detest started.
func (s *Sim) lookup(r *run, onProc bool, first func() string) *Proc {
	gid := goroutineID()
	r.gidMu.Lock()
	p, ok := r.byGid[gid]
	r.gidMu.Unlock()
	if ok {
		return p
	}
	if p := s.retired(gid); p != nil {
		return p
	}
	if onProc || gid == s.schedGid {
		// The scheduler's own goroutine (a seed, an invariant) and a process
		// of an ended run that reap waited for keep attributing their calls
		// as before.
		return r.current
	}
	return r.adopt(gid, first)
}

// adopt registers the calling goroutine as a process and parks it until the
// scheduler takes it in. It does not return before then, so that nothing it
// does in detest, not even a choice made before its first yield point, runs
// alongside the scheduler.
func (r *run) adopt(gid string, first func() string) *Proc {
	_, creator := goroutineCreator()
	np := &Proc{r: r, gid: gid, creator: creator, adopted: true,
		resume: make(chan struct{}), ev: make(chan procEvent, 1), exited: make(chan struct{})}
	if first != nil {
		np.first = first()
	}
	r.gidMu.Lock()
	if r.over() {
		// Its run ended before it first called, so it is turned away at
		// every call, in this run's cleanup and in any later run alike.
		np.state = stateDone
		r.byGid[gid] = np
		r.gidMu.Unlock()
		r.s.staleMu.Lock()
		if r.s.stale == nil {
			r.s.stale = map[string]*Proc{}
		}
		r.s.stale[gid] = np
		r.s.recount()
		r.s.staleMu.Unlock()
		return np
	}
	r.byGid[gid] = np
	r.adopting = append(r.adopting, np)
	r.gidMu.Unlock()
	select {
	case r.adoptCh <- struct{}{}:
	default:
	}
	np.handshake()
	return np
}

// handshake hands an adopted process back to the scheduler and waits until
// it is run. Unlike wait, it returns at the end of the run instead of
// unwinding the goroutine, which detest did not start and cannot recover.
func (p *Proc) handshake() {
	select {
	case p.ev <- procEvent{kind: evSync}:
	case <-p.r.abort:
		return
	}
	select {
	case <-p.resume:
	case <-p.r.abort:
	}
}

// takeAdopted takes in the goroutines that called into detest since the last
// step, and runs each to its first yield point. They arrived in the order the
// runtime ran them, so they are ordered by their parents and, between the
// goroutines of one parent, by the call each was adopted at, such as a
// statement with its arguments, which tells apart goroutines a loop started
// for different items. Only goroutines making the same first call are
// ordered by goroutine id, which mostly follows the order they were started
// in but not always, since the runtime hands ids out from per-processor
// caches. A misordering would make the replay of a schedule take another
// path, which detest reports as nondeterminism.
func (r *run) takeAdopted() bool {
	r.gidMu.Lock()
	pending := r.adopting
	r.adopting = nil
	r.gidMu.Unlock()
	if len(pending) == 0 {
		return false
	}
	// The parent is looked up only now. A child can reach detest before the
	// goroutine that started it registers, and after synctest.Wait every
	// goroutine of the step that was going to has registered.
	r.gidMu.Lock()
	for _, np := range pending {
		np.parent = r.byGid[np.creator]
		if np.parent == nil {
			// Its creator never called into detest, such as a goroutine that
			// only starts others. The step's process stands for it.
			np.parent = r.current
		}
	}
	r.gidMu.Unlock()
	// A goroutine may start one of its own before its first call into
	// detest, so a parent can be pending in the same batch as its child. The
	// batch is taken in by generations, a goroutine once its parent is in,
	// so that a child is named after a parent that has a name.
	var order []*Proc
	for len(pending) > 0 {
		var ready, rest []*Proc
		for _, np := range pending {
			if np.parent == nil || np.parent.pt != nil {
				ready = append(ready, np)
			} else {
				rest = append(rest, np)
			}
		}
		if len(ready) == 0 {
			// Only a parent that is never taken in, such as one adopted
			// after its run ended, is left. The step's process stands for it.
			for _, np := range rest {
				np.parent = r.current
			}
			ready, rest = rest, nil
		}
		sort.SliceStable(ready, func(i, j int) bool {
			a, b := ready[i], ready[j]
			if a.parent != b.parent {
				return parentName(a) < parentName(b)
			}
			if a.first != b.first {
				return a.first < b.first
			}
			ai, _ := strconv.ParseUint(a.gid, 10, 64)
			bi, _ := strconv.ParseUint(b.gid, 10, 64)
			return ai < bi
		})
		for _, np := range ready {
			r.enlist(np)
		}
		order = append(order, ready...)
		pending = rest
	}
	pending = order
	for _, np := range pending {
		<-np.ev // the handshake's evSync
		if np.root().killed {
			// Its pod crashed before it first called, such as while it slept,
			// so it is dead as well and is never run. It stays parked until
			// the run ends, as a goroutine the crash caught at a step does.
			r.finish(np)
			r.note(np, "belongs to a crashed pod and never runs")
			continue
		}
		r.runSync(np)
	}
	return true
}

func parentName(p *Proc) string {
	if p.parent == nil {
		return ""
	}
	return p.parent.name
}

// enlist names an adopted process after its parent and adds it to the run.
func (r *run) enlist(np *Proc) {
	parent := np.parent
	fromLoop := false
	name := "goroutine"
	if parent != nil {
		parent.kids++
		name = parent.name + "." + strconv.Itoa(parent.kids)
		fromLoop = parent.pt.kind == trigLoop || parent.pt.fromLoop
		np.family = parent.root()
	}
	np.name = name
	np.pt = &procType{name: name, kind: trigSpawn, instances: 1 << 30, fromLoop: fromLoop}
	np.started = r.version
	np.state = stateBlockedOutside
	if r.prio != nil {
		r.prio.spawned(np)
	}
	r.procs = append(r.procs, np)
	r.mixString(name)
	r.note(parent, "starts goroutine %s", name)
}

// root returns the process a pod's goroutines all belong to, which is p
// itself or the process detest started that p descends from.
func (p *Proc) root() *Proc {
	if p.family != nil {
		return p.family
	}
	return p
}

// reapAdopted marks the adopted goroutines that returned as done. A goroutine
// gives detest no sign when it returns, so one that did is first taken for
// one blocked outside detest; this tells the two apart once nothing else can
// run.
func (r *run) reapAdopted() bool {
	var outside []*Proc
	for _, p := range r.procs {
		if p.adopted && p.state == stateBlockedOutside {
			outside = append(outside, p)
		}
	}
	if len(outside) == 0 {
		return false
	}
	live := liveGoroutines()
	done := false
	for _, p := range outside {
		if live[p.gid] {
			continue
		}
		r.finish(p)
		p.returned = true
		r.note(p, "done")
		done = true
	}
	return done
}

// finish ends a process. It no longer runs, and the transactions it left
// open roll back.
func (r *run) finish(p *Proc) {
	p.state = stateDone
	if p.tx != nil && !p.tx.closed {
		p.tx.rollback()
	}
	for _, tx := range p.txs {
		if !tx.closed {
			tx.rollback()
		}
	}
	p.txs = nil
}

// retire keeps the adopted goroutines still alive at the end of a run, so that
// one waking in a later run unwinds as a process of its own ended run instead
// of being adopted by the later one.
func (r *run) retire() {
	// A goroutine that registered in the run's last step is still pending,
	// its handshake released by the end of the run, and may call again.
	r.gidMu.Lock()
	keep := r.adopting
	r.adopting = nil
	r.gidMu.Unlock()
	for _, p := range r.procs {
		// A goroutine that crashed with its family is done but may still run,
		// such as one asleep or one its cut call returned errRunOver to.
		if p.adopted && !p.returned {
			keep = append(keep, p)
			continue
		}
		if !p.adopted {
			select {
			case <-p.exited:
			default:
				// reap gave up on it, such as a crashed process blocked on
				// a channel that a later run may close.
				p.lingering = true
				keep = append(keep, p)
			}
		}
	}
	if len(keep) == 0 {
		return
	}
	s := r.s
	s.staleMu.Lock()
	defer s.staleMu.Unlock()
	if s.stale == nil {
		s.stale = map[string]*Proc{}
	}
	for _, p := range keep {
		s.stale[p.gid] = p
	}
	if len(s.stale) > staleLimit {
		live := liveGoroutines()
		for gid := range s.stale {
			if !live[gid] {
				delete(s.stale, gid)
			}
		}
	}
	s.recount()
}

// staleLimit bounds the retired goroutines kept before the ones that returned
// are dropped. Goroutine ids are not reused, so a returned one never calls in.
const staleLimit = 1024

// drainAdopted lets the adopted goroutines still asleep after the last run
// wake and unwind, as the bubble cannot end while they are blocked. Each finds
// its run over at its next call into detest.
func (s *Sim) drainAdopted() {
	for range drainRounds {
		s.staleMu.Lock()
		n := len(s.stale)
		s.staleMu.Unlock()
		if n == 0 {
			return
		}
		live := liveGoroutines()
		s.staleMu.Lock()
		for gid := range s.stale {
			if !live[gid] {
				delete(s.stale, gid)
			}
		}
		n = len(s.stale)
		s.recount()
		s.staleMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(reapLimit) // on the bubble's clock, which costs no real time
		synctest.Wait()
	}
}

// drainRounds bounds drainAdopted, for a goroutine that sleeps again and again.
const drainRounds = 8

// recount updates the counts Current reads without staleMu, which the caller
// holds.
func (s *Sim) recount() {
	linger := 0
	for _, p := range s.stale {
		if p.lingering {
			linger++
		}
	}
	s.staleN.Store(int32(len(s.stale))) //nolint:gosec // far below 2^31
	s.lingerN.Store(int32(linger))      //nolint:gosec // far below 2^31
}

// staleCaller reports whether the calling goroutine is retired, without
// adopting it as Current would. It reads the goroutine id only while some
// goroutine is retired.
func (s *Sim) staleCaller() bool {
	if s.staleN.Load() == 0 {
		return false
	}
	return s.retired(goroutineID()) != nil
}

func (s *Sim) retired(gid string) *Proc {
	s.staleMu.Lock()
	defer s.staleMu.Unlock()
	return s.stale[gid]
}

// stale reports whether p is an adopted goroutine whose run ended. Unlike a
// process detest started, which reap waits for before the next run, such a
// goroutine may wake while a later run goes on, so it is turned away at every
// call into detest before the call touches anything that run uses.
func (p *Proc) stale() bool { return p != nil && (p.adopted || p.lingering) && p.r.over() }

// absorbAbort ends, at a call into detest, the unwinding of a goroutine
// detest adopted when the end of the run cut the call short, returning
// errRunOver from it. Nothing recovers such a goroutine further up, and the
// panic would take the test binary down. A goroutine detest started unwinds on,
// to the recover in Proc.main. rec is what the caller's deferred function
// recovered, since recover works only when called by that function itself.
func absorbAbort(rec any, p *Proc, err *error) {
	if rec == nil {
		return
	}
	if _, ok := rec.(abortSentinel); !ok {
		panic(rec)
	}
	adopted := p != nil && p.adopted
	if p == nil {
		// A call that does not name its process, such as one of a Tx, which
		// another goroutine than its owner may run.
		onProc, sure := onProcGoroutine()
		adopted = !onProc && sure
	}
	if !adopted {
		panic(rec)
	}
	if err == nil {
		// Returning would read as success, such as a Lock that did not take
		// the mutex, so the goroutine ends here, its deferred calls running.
		runtime.Goexit()
	}
	*err = errRunOver
}

// comeBack hands p to the scheduler before a call into detest when it woke
// outside detest, so that nothing the call does, not even a choice, runs
// alongside the scheduler. Current does this for a call that resolves its
// process, and every entry point that is handed one does it too.
func (p *Proc) comeBack() {
	switch p.away.Load() {
	case awayCrashed:
		// Crashed with its family while outside detest, it wakes to nothing.
		// Its own run's end is awaited, which a later run it woke in has
		// already passed, rather than the end of that later run.
		<-p.r.abort
		if !p.adopted {
			panic(abortSentinel{})
		}
	case awayOutside:
		if p.adopted {
			p.handshake()
		} else {
			p.send(procEvent{kind: evSync})
			p.wait()
		}
	}
}

// goneStale ends a stale goroutine at an entry point that has no error to
// return, as absorbAbort does.
func goneStale(p *Proc) {
	if p.stale() {
		runtime.Goexit()
	}
}
