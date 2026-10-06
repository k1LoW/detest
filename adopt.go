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
func (s *Sim) lookup(r *run, onProc bool) *Proc {
	gid := goroutineID()
	r.gidMu.Lock()
	p, ok := r.byGid[gid]
	r.gidMu.Unlock()
	if ok {
		return p
	}
	if onProc || gid == s.schedGid {
		// The scheduler's own goroutine (a seed, an invariant) and a process
		// of an ended run keep attributing their calls as before.
		return r.current
	}
	if p := s.retired(gid); p != nil {
		return p
	}
	return r.adopt(gid)
}

// adopt registers the calling goroutine as a process and parks it until the
// scheduler takes it in. It does not return before then, so that nothing it
// does in detest, not even a choice made before its first yield point, runs
// alongside the scheduler.
func (r *run) adopt(gid string) *Proc {
	_, creator := goroutineCreator()
	np := &Proc{r: r, gid: gid, adopted: true,
		resume: make(chan struct{}), ev: make(chan procEvent, 1), exited: make(chan struct{})}
	r.gidMu.Lock()
	np.parent = r.byGid[creator]
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
// goroutines of one parent, by goroutine id, which the runtime hands out in
// the order a goroutine starts them. A misordering would make the replay of a
// schedule take another path, which detest reports as nondeterminism.
func (r *run) takeAdopted() bool {
	r.gidMu.Lock()
	pending := r.adopting
	r.adopting = nil
	r.gidMu.Unlock()
	if len(pending) == 0 {
		return false
	}
	for _, np := range pending {
		if np.parent == nil {
			np.parent = r.current
		}
	}
	sort.SliceStable(pending, func(i, j int) bool {
		a, b := pending[i], pending[j]
		if a.parent != b.parent {
			return parentName(a) < parentName(b)
		}
		ai, _ := strconv.ParseUint(a.gid, 10, 64)
		bi, _ := strconv.ParseUint(b.gid, 10, 64)
		return ai < bi
	})
	for _, np := range pending {
		r.enlist(np)
	}
	for _, np := range pending {
		<-np.ev // the handshake's evSync
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
	var keep []*Proc
	for _, p := range r.procs {
		if p.adopted && p.state != stateDone {
			keep = append(keep, p)
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

func (s *Sim) retired(gid string) *Proc {
	s.staleMu.Lock()
	defer s.staleMu.Unlock()
	return s.stale[gid]
}

// stale reports whether p is an adopted goroutine whose run ended. Unlike a
// process detest started, which reap waits for before the next run, such a
// goroutine may wake while a later run goes on, so it is turned away at every
// call into detest before the call touches anything that run uses.
func (p *Proc) stale() bool { return p != nil && p.adopted && p.r.over() }

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
	if _, ok := rec.(abortSentinel); !ok || p == nil || !p.adopted {
		panic(rec)
	}
	if err != nil {
		*err = errRunOver
	}
}
