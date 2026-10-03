// Package detest explores every interleaving of Go backend code under a
// deterministic scheduler. Processes running the code under test, real storage
// layers and handlers or a hand-written model, interact only through simulated
// resources: databases (through database/sql), queues, mutexes and external
// services, whose semantics detest implements. Every operation on them is a
// scheduling point, and detest enumerates the schedules, the failure outcomes
// and the duplicate deliveries, checking user-supplied invariants on each run.
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
	"testing"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
)

// Errors an operation on a simulated resource can return. Models branch on
// these the way production code branches on database and RPC errors.
// The database errors are *SQLError values, which match these with errors.Is.
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
	ErrSyntaxError               = sqlir.ErrSyntaxError
	ErrUndefinedParameter        = sqlir.ErrUndefinedParameter
	ErrInvalidColumnReference    = sqlir.ErrInvalidColumnReference
	ErrDuplicateTable            = sqlir.ErrDuplicateTable
	ErrWrongObjectType           = sqlir.ErrWrongObjectType
	ErrInvalidTableDefinition    = sqlir.ErrInvalidTableDefinition
	ErrNoActiveTransaction       = sqlir.ErrNoActiveTransaction
	ErrInvalidSavepoint          = sqlir.ErrInvalidSavepoint
	ErrCheckViolation            = sqlir.ErrCheckViolation
	ErrUnavailable               = errors.New("detest: external call failed (transport)")
	ErrNotFound                  = errors.New("detest: not found")
	ErrFailedPrecondition        = errors.New("detest: failed precondition")
	ErrAborted                   = sqlir.ErrInFailedTx
	ErrSelfWait                  = errors.New("detest: waiting for a lock held by the same process (would hang)")
)

// Option configures a Sim.
type Option func(*Sim)

// Pods sets how many pods the system runs on. It is the default instance count
// of every process type.
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

// MaxPreemptions bounds the context switches away from a runnable process per
// run (CHESS-style). 0 means unbounded.
func MaxPreemptions(n int) Option {
	return func(s *Sim) { s.maxPreemptions = n; s.boundPreemptions = true }
}

// MaxRuns caps the number of runs an exhaustive exploration performs.
func MaxRuns(n int) Option { return func(s *Sim) { s.maxRuns = n } }

// defaultShardDepth is how many leading choices pick a schedule's shard.
const defaultShardDepth = 12

// Shard makes this process explore only its share of the schedule tree, so an
// exhaustive exploration can be split across machines. Schedules are assigned
// by hashing their first depth choices: shard index of total explores the
// subtrees whose hash maps to it. Runs shorter than depth are explored by
// every shard. Each shard reports its own run count; a violation is found by
// the shard owning its subtree. DETEST_SHARD=index/total[/depth] sets it from
// the environment.
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
// (the value printed after DETEST_REPLAY=) instead of exploring. A
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
	maxPreemptions   int
	boundPreemptions bool
	maxRuns          int
	verbose          bool
	sqlObserver      func(query string, err error)
	shardIndex       int
	shardTotal       int
	shardDepth       int
	workers          int
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
}

func newSimDefaults() *Sim {
	return &Sim{pods: 1, maxFailures: 1, maxRedeliveries: 1, maxRuns: 200000}
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
// An exploration that stopped early (MaxRuns, a violation) or explored one
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
	MaxDepth int
	Complete bool
	Shard    string // "index/total" when DETEST_SHARD or Shard splits the exploration across machines
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
	Schedule  string
	Trace     string
	Elapsed   time.Duration
}

// report formats the outcome for humans.
func (r *result) report() string {
	if !r.Violated && r.Replay {
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
		msg := fmt.Sprintf("detest: explored %s%s (max depth %d, complete=%v%s) in %s", runs, scope, r.MaxDepth, r.Complete, workers, r.Elapsed.Round(time.Millisecond))
		if r.Checkpoint != "" {
			msg += fmt.Sprintf("; the rest is saved: run again with DETEST_CHECKPOINT=%s to continue", r.Checkpoint)
		}
		if len(r.Unreached) > 0 {
			msg += "; Sometimes not held yet: " + strings.Join(r.Unreached, ", ")
		}
		return msg
	}
	return fmt.Sprintf("detest: %s violated: %v\nrun %d, schedule (%d choices): DETEST_REPLAY=%s\n%s",
		r.Kind, r.Err, r.Runs, len(strings.Split(r.Schedule, ",")), r.Schedule, r.Trace)
}

// check runs the exhaustive exploration. DETEST_REPLAY or Replay replays
// one schedule instead.
func (s *Sim) check() *result {
	start := time.Now()
	if sched, fromEnv := replaySchedule(s.schedule); sched != "" {
		var prefix []choice
		for f := range strings.SplitSeq(sched, ",") {
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
		return res
	}
	f := s.frontier
	if f == nil {
		// A lone exploration, as the package's own tests start one; Explore
		// always passes the frontier it saves and resumes.
		f = newFrontier(1, s.maxRuns)
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
	res := &result{Runs: runs, MaxDepth: depth, Complete: complete, Elapsed: time.Since(start),
		Schedule: strings.Join(sched, ","), Trace: r.traceString()}
	if v != nil {
		res.Violated, res.Kind, res.Err = true, v.kind, v.err
	}
	return res
}
