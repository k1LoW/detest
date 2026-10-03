package detest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing/synctest"
	"time"
)

type choice struct {
	label  string
	n      int
	picked int
	replay bool
	// fp is the run's fingerprint when it made the choice: a hash of the
	// operations and the options before it. A run replaying the choice must
	// arrive at it with the same fingerprint; 0 when unknown, as for
	// DETEST_REPLAY.
	fp uint64
}

type violation struct {
	kind string
	err  error
}

type procState int

const (
	stateReady procState = iota
	stateBlockedLock
	stateBlockedTime
	stateBlockedOutside // blocked on a primitive detest does not model (channel, timer, WaitGroup)
	stateDone
)

type eventKind int

const (
	evYield eventKind = iota
	evBlocked
	evDone
)

type procEvent struct {
	kind eventKind
	err  error
	op   uint64 // evYield: hash of the operation the process yields at
}

type abortSentinel struct{}

type triggerKind int

const (
	trigMessage triggerKind = iota
	trigLoop
	trigManual
	trigSpawn
)

type procType struct {
	name      string
	kind      triggerKind
	instances int
	maxRuns   int
	queue     *Queue
	loopFn    func(p *Proc) error
	msgFn     func(p *Proc, msg Msg) error
	after     func(st *State) bool
	when      func() bool
}

// ProcOption configures a process type.
type ProcOption func(*procType)

// Instances sets how many instances of a process type may run concurrently.
func Instances(n int) ProcOption { return func(pt *procType) { pt.instances = n } }

// After restricts a manual process to start only once the predicate holds.
func After(pred func(st *State) bool) ProcOption { return func(pt *procType) { pt.after = pred } }

// When lets a loop or manual process start only while pred holds. Use it to
// express that a periodic sweep has work to do: a tick that would find nothing
// is a no-op whose position in the schedule does not matter, and skipping it
// removes the branching those no-op ticks would add. pred reads committed
// state (for example through DB.Peek) and must not yield.
func When(pred func() bool) ProcOption { return func(pt *procType) { pt.when = pred } }

// OnMessage registers a process type started by delivering a message of q.
// A returned error redelivers the message, bounded by MaxRedeliveries.
func (s *Sim) OnMessage(name string, q *Queue, fn func(p *Proc, msg Msg) error, opts ...ProcOption) {
	s.declare("OnMessage")
	pt := &procType{name: name, kind: trigMessage, instances: s.pods, queue: q, msgFn: fn}
	for _, o := range opts {
		o(pt)
	}
	q.consumers = append(q.consumers, pt)
	s.types = append(s.types, pt)
}

// Loop registers a periodic process type that the scheduler may start at any
// point, at most maxRuns times per run.
func (s *Sim) Loop(name string, maxRuns int, fn func(p *Proc) error, opts ...ProcOption) {
	s.declare("Loop")
	pt := &procType{name: name, kind: trigLoop, instances: s.pods, maxRuns: maxRuns, loopFn: fn}
	for _, o := range opts {
		o(pt)
	}
	s.types = append(s.types, pt)
}

// Manual registers a process type started at an arbitrary point, such as a
// user-issued cancel, at most maxRuns times per run.
func (s *Sim) Manual(name string, maxRuns int, fn func(p *Proc) error, opts ...ProcOption) {
	s.declare("Manual")
	pt := &procType{name: name, kind: trigManual, instances: 1, maxRuns: maxRuns, loopFn: fn}
	for _, o := range opts {
		o(pt)
	}
	s.types = append(s.types, pt)
}

type run struct {
	s        *Sim
	prefix   []choice
	choices  []choice
	pos      int
	procs    []*Proc
	opts     []option // reused by enabled, whose result lives for one step
	nextID   int
	clock    int64
	trace    []step
	failures int
	crashes  int
	runs     map[*procType]int
	abort    chan struct{}
	ctx      context.Context // Proc.Context; canceled when the run ends
	cancel   context.CancelFunc
	current  *Proc
	preempts int
	prev     *State
	version  int
	idleAt   map[*procType]int
	pending  *violation // raised by a simulated resource during a step
	fp       uint64     // fingerprint of the run so far (see choice.fp)
	tracing  bool       // keep the trace (see note)
	snap     *State     // the latest snapshot, whose tables the next one reuses
	// queuesTouched records a queue change since snap was taken.
	queuesTouched bool
	// outside counts the processes blocked outside detest, which may wake and
	// call into detest without being resumed. Those calls run concurrently with
	// the resumed process, hence atomic.
	outside atomic.Int32
	gidMu   sync.Mutex
	byGid   map[string]*Proc // process by goroutine id, for attributing statements
}

type step struct {
	proc string
	op   string
	loc  string
}

// Proc is one running process instance.
type Proc struct {
	name     string
	pt       *procType
	r        *run
	resume   chan struct{}
	ev       chan procEvent
	state    procState
	waitRow  *rowWait // the row lock we wait for
	waitLock waitable // Mutex or RWMutex we wait for
	waitAt   int64
	tx       *Tx   // transaction opened by a hand-written model through DB.Tx
	txs      []*Tx // transactions opened through the database/sql driver
	msg      *qmsg
	exited   chan struct{} // closed when the process's goroutine returns
	err      error
	gid      string // goroutine id, for inspecting its state in runtime.Stack
}

// Name returns the instance name, such as "sweeper#2".
func (p *Proc) Name() string { return p.name }

// Current returns the process whose goroutine is calling. Production code
// called from a process does not carry the *Proc, so fakes injected at its
// boundaries (clients, drivers) use this instead. A goroutine the process
// itself spawned is not registered and falls back to the process the
// scheduler resumed last.
func (s *Sim) Current() *Proc {
	if s.run == nil {
		return nil
	}
	r := s.run
	// While no process is blocked outside detest, every process but the
	// resumed one is parked inside detest and cannot be calling, so the lookup
	// below would return r.current anyway. It is skipped because goroutineID
	// takes a runtime-wide lock, which parallel workers contend on.
	if r.outside.Load() == 0 {
		return r.current
	}
	gid := goroutineID()
	r.gidMu.Lock()
	p, ok := r.byGid[gid]
	r.gidMu.Unlock()
	if ok {
		return p
	}
	return r.current
}

func (r *run) choose(label string, n int) int {
	if n <= 0 {
		panic("detest: choose with n <= 0")
	}
	fp := r.fp
	picked := 0
	if r.pos < len(r.prefix) {
		c := r.prefix[r.pos]
		var mismatch error
		switch {
		case !c.replay && c.n != n:
			mismatch = fmt.Errorf("detest: the simulation is nondeterministic, or the checkpoint is of another version of it: choice %d (%s) had %d options, now %d", r.pos, label, c.n, n)
		case c.picked >= n:
			mismatch = fmt.Errorf("detest: the schedule picks %d of %d options at choice %d (%s): it is of another version of the simulation", c.picked, n, r.pos, label)
		case c.fp != 0 && c.fp != fp:
			mismatch = fmt.Errorf("detest: the simulation is nondeterministic, or the checkpoint is of another version of it: the operations before choice %d (%s) differ from the run that recorded it. Look for what varies between runs of the code under test: map iteration order, the wall clock, randomness, or goroutines detest does not schedule", r.pos, label)
		}
		if mismatch != nil {
			// This may be a process's goroutine, which cannot fail the test.
			// The scheduler ends the run after the step and Explore fails the
			// test; the run goes on until then with the first option.
			r.pending = &violation{kind: "fatal", err: mismatch}
		} else {
			picked = c.picked
		}
	}
	r.choices = append(r.choices, choice{label: label, n: n, picked: picked, fp: fp})
	r.pos++
	r.mixString(label)
	r.mixInt(int64(picked))
	return picked
}

// option is one thing the scheduler may do next. It is a plain value rather
// than a closure because enabled builds every option at every step.
type option struct {
	kind optionKind
	p    *Proc     // optResume
	q    *Queue    // optDeliver
	i    int       // optDeliver: index of the message in q
	pt   *procType // optDeliver, optStart
}

type optionKind uint8

const (
	optResume optionKind = iota
	optDeliver
	optStart
	optCrash
)

// apply does o. preempt reports whether it takes the CPU from the current
// process while that process could still run.
func (r *run) apply(o option, preempt bool) {
	if preempt {
		r.preempts++
	}
	switch o.kind {
	case optResume:
		r.resume(o.p)
	case optDeliver:
		r.deliver(o.q, o.i, o.pt)
	case optStart:
		r.runs[o.pt]++
		r.resume(r.spawn(o.pt, nil))
	case optCrash:
		r.crash(o.p)
	}
}

func (r *run) execute() (v *violation) {
	defer func() {
		close(r.abort)
		// Let the processes left parked unwind, and database/sql roll back the
		// transactions they left open, before the next run resets the simulated resources.
		r.cancel()
		synctest.Wait()
		r.reap()
		if rec := recover(); rec != nil {
			if _, ok := rec.(abortSentinel); !ok {
				panic(rec)
			}
		}
	}()
	for {
		if r.s.progress != nil {
			r.s.progress.steps.Add(1)
		}
		r.settleOutside()
		opts := r.enabled()
		if len(opts) == 0 {
			if r.advanceClock() {
				continue
			}
			if r.waitOutside() {
				continue
			}
			break
		}
		i := 0
		if len(opts) > 1 {
			r.mixOptions(opts)
			i = r.choose("step", len(opts))
		}
		o := opts[i]
		cur := r.current
		r.apply(o, o.kind != optCrash && cur != nil && cur.state == stateReady && (o.kind != optResume || o.p != cur))
		if r.pending != nil {
			return r.pending
		}
		if v := r.checkAlways(); v != nil {
			return v
		}
	}
	for _, p := range r.procs {
		if p.state == stateBlockedLock {
			return &violation{kind: "progress", err: fmt.Errorf("process %s blocked forever on a lock", p.name)}
		}
		if p.state == stateBlockedOutside {
			return &violation{kind: "progress", err: fmt.Errorf("process %s is blocked on a channel or WaitGroup that nothing left running can release", p.name)}
		}
	}
	st := r.snapshot()
	for _, fn := range r.s.atQuiesce {
		if err := fn(st); err != nil {
			return &violation{kind: "quiescence invariant", err: err}
		}
	}
	return nil
}

// checkSometimes records the Sometimes conditions that hold now. A condition
// is not checked again once this worker saw it hold, so that a test with
// conditions met early pays for no snapshots after.
func (r *run) checkSometimes() {
	var st *State
	for i := range r.s.sometimes {
		c := &r.s.sometimes[i]
		if c.reached {
			continue
		}
		if st == nil {
			st = r.snapshot()
		}
		if c.fn(st) {
			c.reached = true
			if r.s.frontier != nil {
				r.s.frontier.reach(c.name)
			}
		}
	}
}

func (r *run) checkAlways() *violation {
	r.checkSometimes()
	if len(r.s.always) == 0 {
		return nil
	}
	st := r.snapshot()
	st.prev = r.prev
	for _, fn := range r.s.always {
		if err := fn(st); err != nil {
			return &violation{kind: "always invariant", err: err}
		}
	}
	st.prev = nil
	r.prev = st
	return nil
}

func (r *run) active(pt *procType) int {
	n := 0
	for _, p := range r.procs {
		if p.pt == pt && p.state != stateDone {
			n++
		}
	}
	return n
}

func (r *run) enabled() []option {
	opts := r.opts[:0]
	defer func() { r.opts = opts[:0] }()
	// Resume a runnable process. Preemption bounding: once the budget is spent,
	// the current process keeps running while it is runnable.
	cur := r.current
	curRunnable := cur != nil && cur.state == stateReady
	bounded := r.s.boundPreemptions && r.preempts >= r.s.maxPreemptions && curRunnable
	for _, p := range r.procs {
		if p.state != stateReady {
			continue
		}
		if bounded && p != cur {
			continue
		}
		opts = append(opts, option{kind: optResume, p: p})
	}
	if r.crashes < r.s.maxCrashes {
		for _, p := range r.procs {
			if p.state == stateReady || p.state == stateBlockedLock {
				opts = append(opts, option{kind: optCrash, p: p})
			}
		}
	}
	if bounded {
		return opts
	}
	// Deliver a message to a consumer.
	for _, q := range r.s.queues {
		for _, pt := range q.consumers {
			if r.active(pt) >= pt.instances {
				continue
			}
			for i := range q.msgs {
				opts = append(opts, option{kind: optDeliver, q: q, i: i, pt: pt})
			}
		}
	}
	// Start a loop tick or a manual action.
	for _, pt := range r.s.types {
		if pt.kind != trigLoop && pt.kind != trigManual {
			continue
		}
		if r.runs[pt] >= pt.maxRuns || r.active(pt) >= pt.instances {
			continue
		}
		// A loop that went idle keeps ticking only once something changed.
		if v, idle := r.idleAt[pt]; idle && v == r.version {
			continue
		}
		if pt.after != nil && !pt.after(r.snapshot()) {
			continue
		}
		if pt.when != nil && !pt.when() {
			continue
		}
		opts = append(opts, option{kind: optStart, pt: pt})
	}
	return opts
}

func (r *run) advanceClock() bool {
	var min int64
	found := false
	for _, p := range r.procs {
		if p.state == stateBlockedTime && (!found || p.waitAt < min) {
			min, found = p.waitAt, true
		}
	}
	if !found {
		return false
	}
	r.clock = min
	r.note(nil, "clock advances to %d", min)
	for _, p := range r.procs {
		if p.state == stateBlockedTime && p.waitAt <= r.clock {
			p.state = stateReady
		}
	}
	return true
}

func (r *run) spawn(pt *procType, msg *qmsg) *Proc {
	r.nextID++
	p := &Proc{name: pt.name + "#" + strconv.Itoa(r.nextID), pt: pt, r: r,
		// A process sends at most one event before it parks or exits, so with
		// room for it the send never blocks, saving a goroutine wakeup per step.
		resume: make(chan struct{}), ev: make(chan procEvent, 1), exited: make(chan struct{}), msg: msg}
	r.procs = append(r.procs, p)
	go p.main()
	return p
}

// Step records a step with no effect on a simulated resource, such as a decision made
// by an external environment. It is a yield point.
func (p *Proc) Step(format string, args ...any) {
	p.yieldf(format, args...)
}

// goroutineID returns the current goroutine's id from its stack header.
func goroutineID() string {
	buf := make([]byte, 64)
	n := runtime.Stack(buf, false)
	s := string(buf[:n])
	s = strings.TrimPrefix(s, "goroutine ")
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return ""
}

func debugStack() string {
	buf := make([]byte, 1<<14)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

// resume lets p run until its next yield, block or completion. A process that
// blocks on a primitive detest does not model (a channel, a timer, a
// WaitGroup) never reports back; resume detects that once the bubble is idle
// and parks it as blocked outside, so other processes can run.
var debugSched = os.Getenv("DETEST_DEBUG") != ""

func (r *run) resume(p *Proc) {
	r.current = p
	if debugSched {
		fmt.Fprintf(os.Stderr, "[detest] resume %s (state %d, gid %s)\n", p.name, p.state, p.gid)
	}
	p.resume <- struct{}{}
	ev, ok := r.awaitEvent(p)
	if debugSched {
		fmt.Fprintf(os.Stderr, "[detest] %s -> event ok=%v kind=%d\n", p.name, ok, ev.kind)
	}
	if !ok {
		p.state = stateBlockedOutside
		r.outside.Add(1)
		r.note(p, "blocks outside detest (channel, timer or WaitGroup); parked until it reaches a yield point")
		return
	}
	// Only the events of the resumed process enter the fingerprint: when
	// several processes blocked outside wake together, the order their
	// events arrive in is not the code's.
	r.mixString(p.name)
	r.mixInt(int64(ev.kind))
	r.mixInt(int64(ev.op)) //nolint:gosec // a hash, reinterpreted bit for bit
	r.handleEvent(p, ev)
}

// awaitEvent waits until every other goroutine of the bubble is durably
// blocked, then takes p's event if p is blocked sending one. A p blocked
// anywhere else is blocked outside detest.
//
// synctest.Wait does not return while a goroutine is blocked on a sync.Mutex,
// which is not a durable block. Such a hang is left to the stall watchdog;
// inspecting goroutine stacks here instead stops the world on every step.
func (r *run) awaitEvent(p *Proc) (procEvent, bool) {
	synctest.Wait()
	select {
	case ev := <-p.ev:
		return ev, true
	default:
		return procEvent{}, false
	}
}

// settleOutside picks up processes that were blocked outside detest and have
// since reached a yield point or finished. synctest.Wait makes the set of
// processes reporting back depend only on the schedule, so scheduling stays
// deterministic.
func (r *run) settleOutside() {
	var outside []*Proc
	for _, p := range r.procs {
		if p.state == stateBlockedOutside {
			outside = append(outside, p)
		}
	}
	if len(outside) == 0 {
		return
	}
	synctest.Wait()
	for _, p := range outside {
		select {
		case ev := <-p.ev:
			r.outside.Add(-1)
			r.note(p, "resumes from the primitive it blocked on")
			r.handleEvent(p, ev)
		default:
		}
	}
}

// waitOutside blocks until a process parked outside detest reports back, when
// nothing else can run. Blocking here is what lets the bubble advance its fake
// clock for a process sleeping on a timer.
func (r *run) waitOutside() bool {
	var waiting []*Proc
	for _, p := range r.procs {
		if p.state == stateBlockedOutside {
			waiting = append(waiting, p)
		}
	}
	if len(waiting) == 0 {
		return false
	}
	cases := make([]reflect.SelectCase, 0, len(waiting)+1)
	for _, p := range waiting {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(p.ev)})
	}
	timeout := time.NewTimer(outsideWaitLimit)
	defer timeout.Stop()
	cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timeout.C)})
	chosen, v, _ := reflect.Select(cases)
	if chosen == len(waiting) {
		return false // nothing reported back in time: left to the quiescence check
	}
	p := waiting[chosen]
	r.outside.Add(-1)
	r.note(p, "resumes from the primitive it blocked on")
	ev, _ := reflect.TypeAssert[procEvent](v) // p.ev carries only procEvent
	r.handleEvent(p, ev)
	return true
}

// crash kills p where it stands, at a yield point or waiting for a lock. Its
// goroutine is not resumed again: it stays parked until the run ends and then
// unwinds with the others, its deferred calls finding the run over.
func (r *run) crash(p *Proc) {
	r.crashes++
	r.note(p, "crashes: its transactions roll back, the mutexes it held are freed")
	p.state = stateDone
	p.waitRow, p.waitLock = nil, nil
	if p.tx != nil && !p.tx.closed {
		p.tx.rollback()
	}
	for _, tx := range p.txs {
		if !tx.closed {
			tx.rollback()
		}
	}
	p.txs = nil
	freed := false
	for _, l := range r.s.locks {
		if l.crash(p) {
			freed = true
		}
	}
	if freed {
		for _, w := range r.procs {
			if w.state == stateBlockedLock && w.waitLock != nil {
				w.state, w.waitLock = stateReady, nil // each re-checks its lock
			}
		}
	}
	if p.msg != nil {
		r.redeliver(p)
	}
	r.version++
}

// redeliver puts back the message p failed to handle, unless it was
// redelivered MaxRedeliveries times already.
func (r *run) redeliver(p *Proc) {
	p.msg.redelivered++
	if p.msg.redelivered <= r.s.maxRedeliveries {
		p.pt.queue.msgs = append(p.pt.queue.msgs, p.msg)
		r.queuesTouched = true
	} else {
		r.note(p, "drop %s after %d redeliveries", p.msg, p.msg.redelivered-1)
	}
}

// reap waits for the goroutines of the run's processes to return. A process
// asleep on a timer when a violation cut the run short would otherwise wake
// in the next run and act on its simulated resources, attributed to whichever
// process that run resumed last. Waiting on the bubble's clock lets the
// timers fire at once; the process then finds its run over and unwinds at its
// next call into detest. A process blocked on something that never comes,
// such as a channel nobody sends on, is left behind after reapLimit.
func (r *run) reap() {
	limit := time.NewTimer(reapLimit)
	defer limit.Stop()
	for _, p := range r.procs {
		select {
		case <-p.exited:
		case <-limit.C:
			return
		}
	}
}

// reapLimit is on the bubble's clock, which advances only while every
// goroutine is blocked, so it costs no real time.
const reapLimit = time.Hour

// outsideWaitLimit bounds how long the scheduler waits for a process parked
// outside detest when nothing else can run. The bubble's clock is fake, so it
// only fires when no earlier timer exists.
const outsideWaitLimit = 10 * time.Second

func (r *run) handleEvent(p *Proc, ev procEvent) {
	switch ev.kind {
	case evYield:
		p.state = stateReady // trace already recorded by the process
	case evBlocked:
		// state set by the process
	case evDone:
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
		switch {
		case errors.Is(ev.err, Idle):
			r.note(p, "done (idle, budget not consumed)")
			if p.pt.kind == trigLoop {
				r.runs[p.pt]--
				r.idleAt[p.pt] = r.version
			}
			ev.err = nil
		case ev.err != nil && !errors.Is(ev.err, errNack):
			r.note(p, "done with error: %v", ev.err)
		case ev.err == nil:
			r.note(p, "done")
		}
		if p.msg != nil && ev.err != nil {
			r.note(p, "nack %s", p.msg)
			r.redeliver(p)
		}
		if ev.err != nil && strings.HasPrefix(ev.err.Error(), "panic in ") {
			panic(ev.err)
		}
	}
}

var errNack = fmt.Errorf("detest: nack")

// Idle is returned by a loop process that found nothing to do. The tick does
// not consume the loop's run budget, which encodes the fairness assumption that
// a periodic sweep keeps ticking until it has work. It is named for the signal
// a loop gives, return detest.Idle, rather than as the error it technically is.
var Idle = fmt.Errorf("detest: idle tick") //nolint:staticcheck

func (r *run) deliver(q *Queue, i int, pt *procType) {
	msg := q.msgs[i]
	q.msgs = append(q.msgs[:i:i], q.msgs[i+1:]...)
	r.queuesTouched = true
	if q.dupBudget > 0 {
		if r.choose("duplicate delivery", 2) == 1 {
			q.dupBudget--
			dup := *msg
			dup.duplicate = true
			q.msgs = append(q.msgs, &dup)
			r.note(nil, "duplicate %s stays in %s", msg, q.name)
		}
	}
	p := r.spawn(pt, msg)
	r.note(p, "receives %s", msg.String())
	r.resume(p)
}

// over reports whether the run ended. Processes left parked then unwind and
// database/sql rolls back the transactions they left open, concurrently with
// each other and with nothing scheduling them.
func (r *run) over() bool {
	select {
	case <-r.abort:
		return true
	default:
		return false
	}
}

// Now returns the simulated clock.
func (p *Proc) Now() int64 { return p.r.clock }

// WaitUntil blocks until the simulated clock reaches t. The clock advances only when
// nothing else can run, as in testing/synctest.
func (p *Proc) WaitUntil(t int64) {
	if p.r.clock >= t {
		return
	}
	p.state = stateBlockedTime
	p.waitAt = t
	p.r.noteAt(p, "waits until clock %d", t)
	p.send(procEvent{kind: evBlocked})
	p.wait()
}

// Choose picks one of n alternatives; the explorer tries them all.
func (p *Proc) Choose(label string, n int) int {
	return p.r.choose(label, n)
}

// Spawn starts another process instance from this one, such as a scheduler
// starting a runner.
func (p *Proc) Spawn(name string, fn func(p *Proc) error) {
	pt := &procType{name: name, kind: trigSpawn, instances: 1 << 30, loopFn: fn}
	np := p.r.spawn(pt, nil)
	p.r.noteAt(p, "spawns %s", np.name)
}

// Context returns a context for production code called from this process. It
// is canceled when the run ends, which lets database/sql roll back a
// transaction the process left open when the run was cut short.
func (p *Proc) Context() context.Context { return p.r.ctx }

func (p *Proc) main() {
	defer close(p.exited)
	defer func() {
		if rec := recover(); rec != nil {
			if _, ok := rec.(abortSentinel); ok {
				return
			}
			p.send(procEvent{kind: evDone, err: fmt.Errorf("panic in %s: %v\n%s", p.name, rec, debugStack())})
			return
		}
		p.send(procEvent{kind: evDone, err: p.err})
	}()
	p.gid = goroutineID()
	p.r.gidMu.Lock()
	p.r.byGid[p.gid] = p
	p.r.gidMu.Unlock()
	<-p.resume
	switch p.pt.kind {
	case trigMessage:
		p.err = p.pt.msgFn(p, p.msg.msg)
	default:
		p.err = p.pt.loopFn(p)
	}
}

// yieldf hands control to the scheduler after recording the operation.
func (p *Proc) yieldf(format string, args ...any) {
	if p.r.over() {
		return // cleanup after the run runs through without scheduling
	}
	p.r.noteAt(p, format, args...)
	p.send(procEvent{kind: evYield, op: opHash(format)})
	p.wait()
}

// send reports ev to the scheduler. Once the run is over nobody reads, and a
// process unwinding then must not block on its full channel.
func (p *Proc) send(ev procEvent) {
	select {
	case p.ev <- ev:
	case <-p.r.abort:
	}
}

func (p *Proc) wait() {
	select {
	case <-p.resume:
	case <-p.r.abort:
		panic(abortSentinel{})
	}
}

// note records an operation in the trace. The trace is kept only by a run that
// prints it (a replay, Verbose, or the rerun of a violating schedule), so
// exploring runs neither format operations nor walk the stack for locations.
func (r *run) note(p *Proc, format string, args ...any) {
	if r.tracing {
		r.addStep(p, format, args, "")
	}
}

// noteAt is note with the location in the code under test.
func (r *run) noteAt(p *Proc, format string, args ...any) {
	if r.tracing {
		r.addStep(p, format, args, callerLoc())
	}
}

func (r *run) addStep(p *Proc, format string, args []any, loc string) {
	if r.over() {
		return // cleanup after the run, such as a deferred Unlock while unwinding
	}
	name := "-"
	if p != nil {
		name = p.name
	}
	op := format
	if len(args) > 0 {
		op = fmt.Sprintf(format, args...)
	}
	r.trace = append(r.trace, step{proc: name, op: op, loc: loc})
}

// lazyString defers building an argument of note until the trace is kept.
type lazyString func() string

func (f lazyString) String() string { return f() }

func (r *run) traceString() string {
	var b strings.Builder
	w := 0
	for _, s := range r.trace {
		if len(s.proc) > w {
			w = len(s.proc)
		}
	}
	for i, s := range r.trace {
		fmt.Fprintf(&b, "  %3d  %-*s  %s", i+1, w, s.proc, s.op)
		if s.loc != "" {
			fmt.Fprintf(&b, "   (%s)", s.loc)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func callerLoc() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "github.com/k1LoW/detest.") && !isLibraryFrame(f.Function) {
			short := f.File
			if i := strings.LastIndex(short, "/"); i >= 0 {
				short = short[i+1:]
			}
			return fmt.Sprintf("%s:%d", short, f.Line)
		}
		if !more {
			return ""
		}
	}
}

// isLibraryFrame reports frames of the database and ORM plumbing between a
// model and detest's driver, so trace locations point at the caller's code.
func isLibraryFrame(fn string) bool {
	for _, prefix := range []string{"database/sql", "gorm.io/", "github.com/pganalyze/", "github.com/wasilibs/", "reflect.", "runtime."} {
		if strings.HasPrefix(fn, prefix) {
			return true
		}
	}
	return false
}

// State is a snapshot of the committed contents of databases and queues for invariants.
type State struct {
	dbs    map[string]map[string]map[string]Row
	queues map[string][]Msg
	prev   *State
}

// Prev returns the snapshot taken at the previous Always check, or nil.
func (st *State) Prev() *State { return st.prev }

// Rows returns the committed rows of a table sorted by key.
func (st *State) Rows(db *DB, table string) []RowView {
	t := st.dbs[db.name][db.resolve(table)]
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]RowView, 0, len(keys))
	for _, k := range keys {
		out = append(out, RowView{t[k]})
	}
	return out
}

// Row returns one committed row by its primary key: the values of the
// declared primary key columns in order, or the id column's value.
func (st *State) Row(db *DB, table string, key ...any) (RowView, bool) {
	r, ok := st.dbs[db.name][db.resolve(table)][encodeKey(key)]
	return RowView{r}, ok
}

// RowView is a committed row as an invariant sees it. It shares the row with
// the database rather than copying it, so it offers no way to change it.
type RowView struct{ r Row }

// Get returns a column's value and whether the row has the column. A NULL
// column returns nil and true.
func (v RowView) Get(col string) (any, bool) {
	x, ok := v.r[col]
	if b, isBytes := x.([]byte); isBytes {
		x = append([]byte(nil), b...) // the database's bytes stay unchanged
	}
	return x, ok
}

// Str returns a column formatted as a string ("" when absent or NULL).
func (v RowView) Str(col string) string { return v.r.Str(col) }

// Int returns an integer column (0 when absent, NULL or not an integer).
func (v RowView) Int(col string) int { return v.r.Int(col) }

// Int64 returns an integer column (0 when absent, NULL or not an integer).
func (v RowView) Int64(col string) int64 { return v.r.Int64(col) }

// Bool returns a boolean column (false when absent, NULL or not a boolean).
func (v RowView) Bool(col string) bool { return v.r.Bool(col) }

// Key returns the row's key, which State.Row looks the row up by.
func (v RowView) Key() string { return v.r.Key() }

func (v RowView) String() string { return v.r.String() }

// Clone returns a copy of the row to change.
func (v RowView) Clone() Row { return v.r.clone() }

// Queue returns the messages currently in a queue.
func (st *State) Queue(q *Queue) []Msg { return st.queues[q.name] }

// snapshot shares the committed rows instead of copying them: a commit
// replaces a row rather than changing it, and State clones a row only when an
// invariant reads it. A table untouched since the previous snapshot of the run
// reuses that snapshot's table, so a step that commits nothing copies no rows.
func (r *run) snapshot() *State {
	if r.snap != nil && !r.queuesTouched && !r.dbsTouched() {
		return r.snap
	}
	r.queuesTouched = false
	st := &State{dbs: make(map[string]map[string]map[string]Row, len(r.s.dbs)), queues: map[string][]Msg{}}
	for _, db := range r.s.dbs {
		var prev map[string]map[string]Row
		if r.snap != nil {
			prev = r.snap.dbs[db.name]
		}
		ts := make(map[string]map[string]Row, len(db.committed))
		for tn, t := range db.committed {
			if pt, ok := prev[tn]; ok && !db.touched[tn] {
				ts[tn] = pt
				continue
			}
			rows := make(map[string]Row, len(t))
			maps.Copy(rows, t)
			ts[tn] = rows
		}
		clear(db.touched)
		st.dbs[db.name] = ts
	}
	r.snap = st
	for _, q := range r.s.queues {
		for _, m := range q.msgs {
			st.queues[q.name] = append(st.queues[q.name], m.msg)
		}
	}
	return st
}

func (r *run) dbsTouched() bool {
	for _, db := range r.s.dbs {
		if len(db.touched) > 0 {
			return true
		}
	}
	return false
}

func (s *Sim) newRun(prefix []choice) *run {
	r := &run{s: s, prefix: prefix, abort: make(chan struct{}), runs: map[*procType]int{}, idleAt: map[*procType]int{}, byGid: map[string]*Proc{}, fp: fnvOffset}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	s.run = r
	for _, db := range s.dbs {
		db.reset()
	}
	for _, q := range s.queues {
		q.reset()
	}
	for _, l := range s.locks {
		l.reset()
	}
	for _, fn := range s.seeds {
		fn()
	}
	return r
}

// blockOnRow blocks until a row lock we need may be free.
func (p *Proc) blockOnRow(w rowWait, holder *Tx) {
	p.state = stateBlockedLock
	p.waitRow = &w
	p.r.note(p, "waits for a %s lock on %s/%s held by %s", w.mode, w.key.table, w.key.key, procName(holder.p))
	p.send(procEvent{kind: evBlocked})
	p.wait()
}
