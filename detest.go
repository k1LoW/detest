// Package detest explores every interleaving of Go backend code under a
// deterministic scheduler. Processes running the code under test, real storage
// layers and handlers or a hand-written model, interact only through simulated
// resources: databases (through database/sql), queues, mutexes and external
// services, whose semantics detest implements. Every operation on them is a
// scheduling point, and detest enumerates the schedules, the failure outcomes
// and the duplicate deliveries, checking user-supplied invariants on each run.
// Random draws them from a seed instead, for spaces too large to enumerate.
//
// Explore is the only entry point. It runs the declaration function and the
// exploration inside a testing/synctest bubble.
package detest

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
)

// Errors an operation on a simulated resource can return. Models branch on
// these the way production code branches on database and RPC errors.
// The database errors are *DBError values, which match these with errors.Is.
var (
	ErrDeadlock                  = sqlir.ErrDeadlock
	ErrUniqueViolation           = sqlir.ErrUniqueViolation
	ErrNotNullViolation          = sqlir.ErrNotNullViolation
	ErrLockNotAvailable          = sqlir.ErrLockNotAvailable
	ErrUndefinedTable            = sqlir.ErrUndefinedTable
	ErrForeignKeyViolation       = sqlir.ErrForeignKeyViolation
	ErrDivisionByZero            = sqlir.ErrDivisionByZero
	ErrNumericValueOutOfRange    = sqlir.ErrNumericValueOutOfRange
	ErrInvalidTextRepresentation = sqlir.ErrInvalidTextRepresentation
	ErrInvalidDatetimeFormat     = sqlir.ErrInvalidDatetimeFormat
	ErrSyntaxError               = sqlir.ErrSyntaxError
	ErrUndefinedParameter        = sqlir.ErrUndefinedParameter
	ErrInvalidColumnReference    = sqlir.ErrInvalidColumnReference
	ErrDuplicateTable            = sqlir.ErrDuplicateTable
	ErrWrongObjectType           = sqlir.ErrWrongObjectType
	ErrInvalidTableDefinition    = sqlir.ErrInvalidTableDefinition
	ErrNoActiveTransaction       = sqlir.ErrNoActiveTransaction
	ErrInvalidSavepoint          = sqlir.ErrInvalidSavepoint
	ErrCheckViolation            = sqlir.ErrCheckViolation
	ErrInvalidParameterValue     = sqlir.ErrInvalidParameterValue
	ErrCardinalityViolation      = sqlir.ErrCardinalityViolation
	ErrUndefinedColumn           = sqlir.ErrUndefinedColumn
	ErrAmbiguousColumn           = sqlir.ErrAmbiguousColumn
	ErrDuplicateColumn           = sqlir.ErrDuplicateColumn
	ErrStringDataRightTruncation = sqlir.ErrStringDataRightTruncation
	ErrDataTruncated             = sqlir.ErrDataTruncated
	ErrUnavailable               = errors.New("detest: external call failed (transport)")
	ErrNotFound                  = errors.New("detest: not found")
	ErrFailedPrecondition        = errors.New("detest: failed precondition")
	ErrAborted                   = sqlir.ErrInFailedTx
	ErrSelfWait                  = errors.New("detest: waiting for a lock held by the same process (would hang)")
)

// Option configures a Sim.
type Option func(*Sim)

// Pods sets how many pods the system runs on. It is the default instance
// count of loop and message process types. A manual process, such as an
// operation a user issues, runs one instance at a time unless Instances says
// otherwise.
func Pods(n int) Option { return func(s *Sim) { s.pods = n } }

// MaxFailures bounds the external-call failures per run. It is the fairness
// assumption: the environment eventually cooperates, so a run cannot fail every
// call forever.
func MaxFailures(n int) Option { return func(s *Sim) { s.maxFailures = n } }

// MaxRedeliveries bounds how many times a queue redelivers a message whose
// handler returned an error.
func MaxRedeliveries(n int) Option { return func(s *Sim) { s.maxRedeliveries = n } }

// MaxCrashes bounds the process crashes per run, 0 (the default) for none.
// A crash is a choice at every step: any process that could be resumed or is
// waiting for a lock may die there instead. Its open transactions roll back
// as the connection drops, the mutexes it holds are freed with its memory,
// and the message it was handling is redelivered, not having been acked. It
// checks what durable execution promises: that work a process claimed is
// recovered when the process dies halfway. A crash kills one process, as
// detest does not know which processes share a pod.
func MaxCrashes(n int) Option { return func(s *Sim) { s.maxCrashes = n } }

// MaxStalls bounds the process stalls per run, 0 (the default) for none,
// and sets how long each lasts. A stall is a choice at every step. Any
// process that could be resumed may sleep for d on the fake clock instead,
// as in a long GC pause or a slow call detest does not see. The others go
// on meanwhile, and the clock reaches the end of the stall once none of
// them can run, so a lock's TTL or a lease that is shorter than d expires
// under the stalled process. It checks code that trusts a lock or a lease
// with a deadline to still be held when it acts. d must be positive and at
// most a day, the longest the scheduler waits on the fake clock.
func MaxStalls(n int, d time.Duration) Option {
	if n > 0 && (d <= 0 || d >= outsideWaitLimit) {
		panic(fmt.Sprintf("detest: MaxStalls duration %s is not positive and at most a day", d))
	}
	return func(s *Sim) { s.maxStalls, s.stallFor = n, d }
}

// MaxIdleTicks bounds how many idle ticks of a loop in a row leave its
// budget unspent while nothing else makes progress (3 by default). An idle
// tick is free so that a sweep keeps ticking until it has work, and a change
// by a manual process, a message handler or a loop tick that did work starts
// the count again. Loops whose idle ticks lock or write rows can still wake
// each other without end with no progress in between, so a run whose loop
// goes idle once more is cut there, without checking the invariants at
// quiescence, and the result counts it.
func MaxIdleTicks(n int) Option { return func(s *Sim) { s.maxIdleTicks = n } }

// MaxSpins bounds how many steps one process takes without changing
// committed state (a commit, an enqueue) while another could run (100 by
// default). The steps other processes take in between do not reset the
// count, so that a schedule switching at almost every step, as Random does,
// reaches it too, while a change or a process blocking or ending does. Past
// it, the process waits after each step until another has taken one. It is
// the fairness assumption a real scheduler gives a process that busy-waits
// through a yield point, such as one polling a flag or a row another process
// sets, which would otherwise be resumed for ever by the first run and keep
// the setter from running. Schedules where a process goes on longer than
// that without changing anything are left out. A process that takes that
// many steps while nothing else can run would spin for ever in production
// too, and is reported as a progress violation. Processes that give way to
// each other n times with no change in between, such as two that each wait
// for the other, are kept out while any other process can take a step, and
// reported as one once none can; raise n for code that does that much work
// without committing.
func MaxSpins(n int) Option { return func(s *Sim) { s.maxSpins = n } }

// MaxPreemptions bounds the context switches away from a runnable process per
// run (CHESS-style). 0 means unbounded.
func MaxPreemptions(n int) Option {
	return func(s *Sim) { s.maxPreemptions = n; s.boundPreemptions = true }
}

// EagerStart sets whether a Manual process without After or When starts as
// soon as it may start, without a choice, instead of at every step at which
// it could start. It is on by default. Starting a process runs it up to its
// first yield point, which reaches no simulated resource but the generated
// values below, so where among the other processes' steps it starts changes
// nothing they observe. When its first operation then runs is still
// explored, so every interleaving of the processes' operations within the
// bounds is still tried, and the runs that differ only in where a process
// started are left out. Started this way, a process spends no preemption and
// the process that ran last stays the one a switch is counted from.
//
// It assumes that the code a Manual process runs before its first yield
// point reads or writes no state outside detest, such as a package variable,
// in an order that matters. detest cannot see such state, as it explores no
// race through it anywhere else. A test that records when a process began
// in a variable should record it after a yield point, such as a Proc.Step,
// so that it reads the time the schedule picks.
//
// A process that draws a sequence, AUTO_INCREMENT or gen_random_uuid value
// before its first yield point, as an INSERT into a table with a generated
// key does when it is the first statement and runs outside a transaction,
// gets a value that depends on where it starts. Explore then starts the
// exploration over without EagerStart and says so in its report, and the
// schedules it prints replay without it. The clock needs no such check, since
// while a start is left the scheduler has a step to take and the clock does
// not move.
func EagerStart(on bool) Option { return func(s *Sim) { s.eagerStart = on } }

// withoutEagerStart is EagerStart(false) after a start drew a generated
// value, which the report and the schedules it prints carry.
func withoutEagerStart(why string) Option {
	return func(s *Sim) { s.eagerStart, s.lazy = false, why }
}

// eagerDrawError is a start that drew a generated value under EagerStart.
type eagerDrawError struct{ proc string }

func (e *eagerDrawError) Error() string {
	return e.proc + " drew a sequence, AUTO_INCREMENT or gen_random_uuid value before its first yield point, so the value depends on where it starts"
}

// lazyPrefix marks a schedule explored without EagerStart after a start drew
// a generated value, so that it replays without it in a test that leaves it on.
const lazyPrefix = "lazy:"

// MaxRuns caps the number of runs an exhaustive exploration performs.
// DETEST_MAX_RUNS overrides n.
func MaxRuns(n int) Option { return func(s *Sim) { s.maxRuns = n } }

// MaxDuration caps the wall-clock time an exhaustive exploration takes. Once
// d has passed, no worker starts another run, and the runs under way finish.
// The first run is always made, so an exploration resumed from a checkpoint
// makes progress however short d is; a negative d has passed already.
// 0 (the default) means unbounded. DETEST_MAX_DURATION overrides d, as a
// duration such as 10m.
func MaxDuration(d time.Duration) Option { return func(s *Sim) { s.maxDuration = d } }

// defaultShardDepth is how many leading choices pick a schedule's shard.
const defaultShardDepth = 12

// Shard makes this process explore only its share of the schedule tree, so an
// exhaustive exploration can be split across machines. Schedules are assigned
// by hashing their first depth choices: shard index of total explores the
// subtrees whose hash maps to it. Runs shorter than depth are explored by
// every shard. Each shard reports its own run count; a violation is found by
// the shard owning its subtree. Under Random, the shards split the runs
// instead (see Random). DETEST_SHARD=index/total[/depth] sets it from the
// environment.
func Shard(index, total, depth int) Option {
	return func(s *Sim) { s.shardIndex, s.shardTotal, s.shardDepth = index, total, depth }
}

// Workers explores with n workers running in parallel, each in its own
// synctest bubble and subtest. Every finished run reveals the subtrees it did
// not enter, and idle workers take them, so each run is executed once. Explore
// calls the declaration function once per worker: state the function shares
// across runs through captured variables is shared across workers too and
// needs synchronization. DETEST_WORKERS overrides n.
func Workers(n int) Option { return func(s *Sim) { s.workers = n } }

// Replay makes Explore run the one schedule a violation was reported with
// (the value printed after DETEST_REPLAY=) instead of exploring. One printed
// by an exploration that dropped EagerStart starts with "lazy:" and replays
// without it. A
// regression test pins the counterexample of a bug with it: with
// ExpectViolation it fails once the bug is fixed, without it it fails while
// the bug is there. A schedule recorded before the code under test changed
// shape fails the test as stale. DETEST_REPLAY overrides it.
func Replay(schedule string) Option { return func(s *Sim) { s.schedule = schedule } }

// Verbose prints every run's trace.
func Verbose() Option { return func(s *Sim) { s.verbose = true } }

// ObserveSQL calls fn for every SQL statement the database/sql driver
// receives, with the error detest produced for it (nil when executed). Use it
// to measure which statements of a storage layer detest can execute.
func ObserveSQL(fn func(query string, err error)) Option {
	return func(s *Sim) { s.sqlObserver = fn }
}

// Sim is the simulated world of one run: its simulated resources, process
// types, seeds and invariants.
type Sim struct {
	t                *testing.T
	pods             int
	maxFailures      int
	maxRedeliveries  int
	maxCrashes       int
	maxStalls        int
	stallFor         time.Duration
	maxPreemptions   int
	maxIdleTicks     int
	maxSpins         int
	boundPreemptions bool
	eagerStart       bool
	lazy             string // why EagerStart was dropped (see withoutEagerStart)
	maxRuns          int
	maxDuration      time.Duration
	verbose          bool
	sqlObserver      func(query string, err error)
	shardIndex       int
	shardTotal       int
	shardDepth       int
	workers          int
	strategy         strategy
	frontier         *frontier // shared with the other workers, when Workers splits the exploration
	worker           int

	dbs       []*DB
	queues    []*Queue
	externals []*External
	locks     []waitable
	types     []*procType
	seeds     []func()
	schedule  string // the schedule Replay pinned, replayed instead of exploring
	always    []func(st *State) error
	sometimes []sometimes
	atQuiesce []func(st *State) error
	sqlDBs    []*sql.DB
	expect    *string // ExpectViolation

	frozen   bool            // the declaration function returned
	progress *workerProgress // bumped on every scheduler step, read by the stall watchdog

	run *run // current run
	// live is run for Current, which goroutines adopted from an ended run
	// call while the scheduler starts the next one.
	live atomic.Pointer[run]

	// stale maps the adopted goroutines still alive after their run ended to
	// their processes (see run.retire).
	staleMu sync.Mutex
	stale   map[string]*Proc
	// staleN is len(stale), read without staleMu by Current while a seed
	// runs, which looks a goroutine up only when some is retired.
	staleN atomic.Int32
	// hb carries happens-before from processes to the scheduler (see leave).
	hb atomic.Uint64
	// lingerN counts the processes detest started among stale, whose
	// goroutines would otherwise take Current's fast path in a later run.
	lingerN atomic.Int32
	// schedGid is the goroutine that runs the seeds and the scheduler, whose
	// calls into detest belong to no process.
	schedGid string
	// schedEntry is the function at the bottom of that goroutine's stack,
	// which tells it apart from others without reading the goroutine id.
	schedEntry uintptr
	// ended is a run that is over, which goroutines of earlier runs turned
	// away before they were ever known are given as theirs.
	ended *run
	// em is held by whoever runs in the simulated databases' engine, so that
	// a goroutine running a statement on a transaction it shares with its
	// process (see sqlConn.runShared) does not run alongside the process the
	// scheduler resumed. Everything else in detest runs one goroutine at a
	// time by the scheduler's design and needs no lock.
	em sync.Mutex
	// emHolder is the process holding em, nil while none or a goroutine
	// that is no process does, so that a call that does not resolve its
	// process can tell whether its caller holds it already (see enterAny).
	emHolder atomic.Pointer[Proc]
	// epoch counts the times synctest.Wait returned, so that the goroutines
	// that ran between two of them, and only those, share one (see
	// recordShared).
	epoch atomic.Int64
}

func newSimDefaults() *Sim {
	return &Sim{pods: 1, maxFailures: 1, maxRedeliveries: 1, maxRuns: 200000, maxIdleTicks: 3, maxSpins: 100, eagerStart: true}
}

func newSim(t *testing.T, opts ...Option) *Sim {
	t.Helper()
	s := newSimDefaults()
	s.t = t
	for _, o := range opts {
		o(s)
	}
	if env := os.Getenv("DETEST_SHARD"); env != "" {
		var idx, total, depth int
		depth = defaultShardDepth
		if n, _ := fmt.Sscanf(env, "%d/%d/%d", &idx, &total, &depth); n < 2 {
			t.Fatalf("detest: bad DETEST_SHARD %q, want index/total[/depth]", env)
		}
		s.shardIndex, s.shardTotal, s.shardDepth = idx, total, depth
	}
	return s
}

// Seed registers a function that populates simulated resources at the start
// of every run.
func (s *Sim) Seed(fn func()) { s.declare("Seed"); s.seeds = append(s.seeds, fn) }

// Sometimes declares a condition that must hold in at least one run: fn is
// checked after every step, as an Always invariant is, and Explore fails the
// test when it held in none. An exploration that never gets where a bug
// would be passes for nothing; Sometimes is how a test says where it expects
// to get, such as both processes holding a reservation at once.
//
// An exploration that stopped early (MaxRuns, MaxDuration, a violation) or explored one
// shard of a split may not have reached the condition yet, so it only logs
// the conditions it has not seen hold.
func (s *Sim) Sometimes(name string, fn func(st *State) bool) {
	s.declare("Sometimes")
	s.sometimes = append(s.sometimes, sometimes{name: name, fn: fn})
}

type sometimes struct {
	name    string
	fn      func(st *State) bool
	reached bool // by this worker; the frontier knows about all of them
}

// Always registers an invariant checked after every commit and every process
// completion. s.Prev() is the state at the previous check.
func (s *Sim) Always(fn func(st *State) error) {
	s.declare("Always")
	s.always = append(s.always, fn)
}

// AtQuiescence registers an invariant checked when a run ends: every process
// finished, every queue drained, every loop and manual budget spent.
func (s *Sim) AtQuiescence(fn func(st *State) error) {
	s.declare("AtQuiescence")
	s.atQuiesce = append(s.atQuiesce, fn)
}

// ExpectViolation declares that the exploration must find a violation whose
// message contains substr. Explore then fails the test when it finds none or a
// different one. Use it to pin a known bug, such as the code before a fix.
func (s *Sim) ExpectViolation(substr string) { s.declare("ExpectViolation"); s.expect = &substr }

// Now returns the simulated clock of the current run.
func (s *Sim) Now() int64 { return s.run.clock }

// enter takes the engine mutex for p, or for a goroutine that is no process
// when p is nil, and returns the function that gives it back. p gives it up
// while it is parked (see Proc.wait), so it must be the process of the
// calling goroutine. A p that holds it already, such as one inside an
// external call's effect, keeps it.
func (s *Sim) enter(p *Proc) func() {
	if p != nil && p.inSim {
		return func() {}
	}
	s.em.Lock()
	if p == nil {
		return s.em.Unlock
	}
	p.inSim = true
	s.emHolder.Store(p)
	return func() {
		if p.inSim {
			p.inSim = false
			s.emHolder.Store(nil)
			s.em.Unlock()
		}
	}
}

// enterAny is enter for an entry point that does not resolve its caller,
// such as SeedRowNow, which a fake may call from inside an external call's
// effect, whose process holds the mutex already.
func (s *Sim) enterAny() func() {
	if h := s.emHolder.Load(); h != nil && h.isCaller() {
		return func() {}
	}
	return s.enter(nil)
}

// declare panics when a declaration method is called after the declaration
// function returned: the exploration has started and would ignore it.
func (s *Sim) declare(what string) {
	if s.frozen {
		panic(fmt.Sprintf("detest: Sim.%s called after the declaration function returned", what))
	}
}

// result is the outcome of an exploration.
type result struct {
	Violated bool
	Kind     string
	Err      error
	Runs     int
	// CutRuns are the runs cut at MaxIdleTicks, the ones before a checkpoint
	// included.
	CutRuns  int
	MaxDepth int
	Complete bool
	Shard    string // "index/total" when DETEST_SHARD or Shard splits the exploration across machines
	strategy strategy
	eager    bool   // EagerStart applied
	lazy     string // why EagerStart was dropped, if it was
	Workers  int
	Replay   bool // one schedule replayed rather than an exploration
	// PriorRuns are the runs of the earlier explorations a checkpoint resumes;
	// Checkpoint is the file the rest of the exploration was saved to.
	PriorRuns  int
	Checkpoint string
	// Fatal is a misuse that stopped the exploration, which fails the test
	// rather than being reported as a violation.
	Fatal error
	// Unreached are the Sometimes conditions no run explored so far met.
	Unreached []string
	// Unsupported are the statements refused with ErrUnsupportedSQL in any
	// run, which an application that does not report the error hides.
	Unsupported []string
	// Shared counts the statements goroutines ran on a transaction they share
	// with their process, each at once, so that its interleaving with other
	// processes was not explored.
	Shared   int64
	Schedule string
	Trace    string
	Elapsed  time.Duration
}

// report formats the outcome for humans, with the statements refused as
// unsupported after it.
func (r *result) report() string {
	var msg strings.Builder
	msg.WriteString(r.outcome())
	if r.lazy != "" {
		fmt.Fprintf(&msg, "\ndetest: explored without EagerStart, as %s; the runs branched on where each process started", r.lazy)
	}
	if r.Shared > 0 {
		fmt.Fprintf(&msg, "\ndetest: %d statements ran from goroutines sharing a transaction with their process, each at once, without interleaving with other processes", r.Shared)
	}
	if len(r.Unsupported) > 0 {
		msg.WriteString("\ndetest: statements refused as unsupported (ErrUnsupportedSQL), which the application got as an error:")
		for _, u := range r.Unsupported {
			msg.WriteString("\n  " + u)
		}
	}
	return msg.String()
}

func (r *result) outcome() string {
	if !r.Violated && r.Replay {
		if r.CutRuns > 0 {
			return fmt.Sprintf("detest: replayed schedule %s without violation, but it was cut at MaxIdleTicks and its invariants at quiescence were not checked", r.Schedule)
		}
		return fmt.Sprintf("detest: replayed schedule %s without violation", r.Schedule)
	}
	if !r.Violated {
		scope, workers := "", ""
		if r.Shard != "" {
			scope = " in shard " + r.Shard
		}
		if r.Workers > 1 {
			workers = fmt.Sprintf(", %d workers", r.Workers)
		}
		runs := fmt.Sprintf("%d runs", r.Runs)
		if r.PriorRuns > 0 {
			runs = fmt.Sprintf("%d runs, %d in all with the ones before the checkpoint,", r.Runs, r.Runs+r.PriorRuns)
		}
		if r.strategy.random {
			workers += ", " + r.strategy.String()
		}
		if r.eager {
			workers += ", EagerStart"
		}
		msg := fmt.Sprintf("detest: explored %s%s (max depth %d, complete=%v%s) in %s", runs, scope, r.MaxDepth, r.Complete, workers, r.Elapsed.Round(time.Millisecond))
		if r.CutRuns > 0 {
			msg += fmt.Sprintf("; %d runs cut at MaxIdleTicks", r.CutRuns)
		}
		if r.Checkpoint != "" {
			msg += fmt.Sprintf("; the rest is saved: run again with DETEST_CHECKPOINT=%s to continue", r.Checkpoint)
		}
		if len(r.Unreached) > 0 {
			msg += "; Sometimes not held yet: " + strings.Join(r.Unreached, ", ")
		}
		return msg
	}
	what := fmt.Sprintf("%s violated: %v", r.Kind, r.Err)
	if r.Kind == "panic" {
		what = r.Err.Error()
	}
	choices := 0
	if picks := strings.TrimPrefix(r.Schedule, lazyPrefix); picks != "" {
		choices = len(strings.Split(picks, ","))
	}
	seed := ""
	if r.strategy.random && !r.Replay { // a replay draws nothing from the seed
		seed = " of " + r.strategy.String()
	}
	return fmt.Sprintf("detest: %s\nrun %d%s, schedule (%d choices): DETEST_REPLAY=%s\n%s",
		what, r.Runs, seed, choices, r.Schedule, r.Trace)
}

// refuse records a statement refused as unsupported, for the report.
func (s *Sim) refuse(err *ErrUnsupportedSQL) {
	// A statement the declaration runs is refused before any run exists.
	if s.frontier != nil && (s.run == nil || !s.run.measuring) {
		s.frontier.refuse(err.Error())
	}
}

// countShared counts a statement run at once on a shared transaction, for
// the report.
func (s *Sim) countShared(r *run) {
	if s.frontier != nil && !r.measuring {
		s.frontier.shared.Add(1)
	}
}

// check runs the exhaustive exploration. DETEST_REPLAY or Replay replays
// one schedule instead.
func (s *Sim) check() *result {
	start := time.Now()
	if sched, fromEnv := replaySchedule(s.schedule); sched != "" {
		if rest, ok := strings.CutPrefix(sched, lazyPrefix); ok {
			sched = rest
			if s.lazy == "" {
				s.eagerStart, s.lazy = false, "the schedule replayed was explored without it"
			}
		}
		var prefix []choice
		for f := range strings.SplitSeq(sched, ",") {
			if sched == "" {
				break // "lazy:" alone, a run that made no choice
			}
			v, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil {
				s.t.Fatalf("detest: bad schedule %q: %v", sched, err)
			}
			prefix = append(prefix, choice{picked: v, replay: true})
		}
		r := s.newRun(prefix)
		r.tracing = true
		v := r.execute()
		if fromEnv || s.verbose {
			// A schedule pinned in a test reports its trace only on a violation.
			fmt.Fprintf(os.Stderr, "--- replay\n%s\n", r.traceString())
		}
		if v != nil && v.kind == "fatal" {
			return &result{Runs: 1, Fatal: v.err}
		}
		res := s.makeResult(r, v, 1, len(r.choices), true, start)
		res.Replay = true
		if r.cut {
			res.CutRuns = 1
		}
		return res
	}
	f := s.frontier
	if f == nil {
		// A lone exploration, as the package's own tests start one; Explore
		// always passes the frontier it saves and resumes.
		f = newFrontier(1, s.maxRuns)
		f.random = s.strategy.random
		s.frontier = f
		return f.merge([]*result{s.checkShared(f, 0)}, 1)
	}
	return s.checkShared(f, s.worker)
}

func (s *Sim) printRun(n int, r *run) {
	fmt.Fprintf(os.Stderr, "--- run %d\n%s\n", n, r.traceString())
}

// retrace runs a violating schedule again with the trace kept, which exploring
// runs skip. The run is deterministic, so it violates again; if it does not,
// the original violation is reported with the code's nondeterminism noted.
// shrink looks for a simpler schedule with the same violation, so that the
// one reported is easier to read. It goes through the choices in order and
// tries the first option where the run took a later one, keeping the other
// picks, and adopts the change when the run still breaks the same way. The
// result departs from the default schedule at fewer choices, and the search
// costs one run per choice it tries.
func (s *Sim) shrink(r *run, v *violation) (*run, *violation) {
	for i := 0; i < len(r.choices); i++ {
		if r.choices[i].picked == 0 {
			continue
		}
		prefix := make([]choice, len(r.choices))
		for j, c := range r.choices {
			prefix[j] = choice{picked: c.picked, replay: true}
		}
		prefix[i].picked = 0
		cr := s.newRun(prefix)
		cv := cr.execute()
		if cv != nil && cv.same(v) {
			r, v = cr, cv
		}
	}
	return r, v
}

func (s *Sim) retrace(r *run, v *violation) (*run, *violation) {
	rr := s.newRun(append([]choice(nil), r.choices...))
	rr.tracing = true
	if rv := rr.execute(); rv != nil {
		return rr, rv
	}
	rr.note(nil, "(the schedule did not violate again when rerun to record this trace: the code under test is nondeterministic)")
	return rr, v
}

// ownsPrefix reports whether the subtree under the first shardDepth choices
// belongs to this shard.
func (s *Sim) ownsPrefix(prefix []choice) bool {
	// FNV-1a over the picks, which like the shard count are small and
	// non-negative, so the conversions cannot overflow.
	h := uint32(2166136261)
	for _, c := range prefix[:s.shardDepth] {
		h ^= uint32(c.picked) //nolint:gosec
		h *= 16777619
	}
	return int(h%uint32(s.shardTotal)) == s.shardIndex //nolint:gosec
}

func (s *Sim) makeResult(r *run, v *violation, runs, depth int, complete bool, start time.Time) *result {
	var sched []string
	for _, c := range r.choices {
		sched = append(sched, strconv.Itoa(c.picked))
	}
	schedule := strings.Join(sched, ",")
	if s.lazy != "" {
		schedule = lazyPrefix + schedule
	}
	res := &result{Runs: runs, MaxDepth: depth, Complete: complete, Elapsed: time.Since(start),
		Schedule: schedule, Trace: r.traceString()}
	if v != nil {
		res.Violated, res.Kind, res.Err = true, v.kind, v.err
	}
	return res
}
