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
	"sync/atomic"
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
func Pods(n int) Option { return func(sim *Sim) { sim.pods = n } }

// MaxFailures bounds the external-call failures per run. It is the fairness
// assumption: the environment eventually cooperates, so a run cannot fail every
// call forever.
func MaxFailures(n int) Option { return func(sim *Sim) { sim.maxFailures = n } }

// MaxRedeliveries bounds how many times a queue redelivers a message whose
// handler returned an error.
func MaxRedeliveries(n int) Option { return func(sim *Sim) { sim.maxRedeliveries = n } }

// MaxPreemptions bounds the context switches away from a runnable process per
// run (CHESS-style). 0 means unbounded.
func MaxPreemptions(n int) Option {
	return func(sim *Sim) { sim.maxPreemptions = n; sim.boundPreemptions = true }
}

// MaxRuns caps the number of runs an exhaustive exploration performs.
func MaxRuns(n int) Option { return func(sim *Sim) { sim.maxRuns = n } }

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
	return func(sim *Sim) { sim.shardIndex, sim.shardTotal, sim.shardDepth = index, total, depth }
}

// Workers explores with n workers running in parallel, each in its own
// synctest bubble and subtest. Every finished run reveals the subtrees it did
// not enter, and idle workers take them, so each run is executed once. Explore
// calls the declaration function once per worker: state the function shares
// across runs through captured variables is shared across workers too and
// needs synchronization. DETEST_WORKERS overrides n.
func Workers(n int) Option { return func(sim *Sim) { sim.workers = n } }

// Verbose prints every run's trace.
func Verbose() Option { return func(sim *Sim) { sim.verbose = true } }

// ObserveSQL calls fn for every SQL statement the database/sql driver
// receives, with the error detest produced for it (nil when executed). Use it
// to measure which statements of a storage layer detest can execute.
func ObserveSQL(fn func(query string, err error)) Option {
	return func(sim *Sim) { sim.sqlObserver = fn }
}

// Sim is the simulated world of one run: its simulated resources, process
// types, seeds and invariants.
type Sim struct {
	t                *testing.T
	pods             int
	maxFailures      int
	maxRedeliveries  int
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
	always    []func(s *State) error
	atQuiesce []func(s *State) error
	sqlDBs    []*sql.DB
	expect    *string // ExpectViolation

	frozen   bool          // the declaration function returned
	progress *atomic.Int64 // bumped on every scheduler step, read by the stall watchdog

	run *run // current run
}

func newSimDefaults() *Sim {
	return &Sim{pods: 1, maxFailures: 1, maxRedeliveries: 1, maxRuns: 200000}
}

func newSim(t *testing.T, opts ...Option) *Sim {
	t.Helper()
	sim := newSimDefaults()
	sim.t = t
	for _, o := range opts {
		o(sim)
	}
	if s := os.Getenv("DETEST_SHARD"); s != "" {
		var idx, total, depth int
		depth = defaultShardDepth
		if n, _ := fmt.Sscanf(s, "%d/%d/%d", &idx, &total, &depth); n < 2 {
			t.Fatalf("detest: bad DETEST_SHARD %q, want index/total[/depth]", s)
		}
		sim.shardIndex, sim.shardTotal, sim.shardDepth = idx, total, depth
	}
	return sim
}

// declare panics when a declaration method is called after the declaration
// function returned: the exploration has started and would ignore it.
func (sim *Sim) declare(what string) {
	if sim.frozen {
		panic(fmt.Sprintf("detest: Sim.%s called after the declaration function returned", what))
	}
}

// Seed registers a function that populates simulated resources at the start
// of every run.
func (sim *Sim) Seed(fn func()) { sim.declare("Seed"); sim.seeds = append(sim.seeds, fn) }

// Always registers an invariant checked after every commit and every process
// completion. s.Prev() is the state at the previous check.
func (sim *Sim) Always(fn func(s *State) error) {
	sim.declare("Always")
	sim.always = append(sim.always, fn)
}

// AtQuiescence registers an invariant checked when a run ends: every process
// finished, every queue drained, every loop and manual budget spent.
func (sim *Sim) AtQuiescence(fn func(s *State) error) {
	sim.declare("AtQuiescence")
	sim.atQuiesce = append(sim.atQuiesce, fn)
}

// ExpectViolation declares that the exploration must find a violation whose
// message contains substr. Explore then fails the test when it finds none or a
// different one. Use it to pin a known bug, such as the code before a fix.
func (sim *Sim) ExpectViolation(substr string) { sim.declare("ExpectViolation"); sim.expect = &substr }

// Now returns the simulated clock of the current run.
func (sim *Sim) Now() int64 { return sim.run.clock }

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
	// PriorRuns are the runs of the earlier explorations a checkpoint resumes;
	// Checkpoint is the file the rest of the exploration was saved to.
	PriorRuns  int
	Checkpoint string
	// Fatal is a misuse that stopped the exploration, which fails the test
	// rather than being reported as a violation.
	Fatal    error
	Schedule string
	Trace    string
	Elapsed  time.Duration
}

// report formats the outcome for humans.
func (r *result) report() string {
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
		return msg
	}
	return fmt.Sprintf("detest: %s violated: %v\nrun %d, schedule (%d choices): DETEST_SCHEDULE=%s\n%s",
		r.Kind, r.Err, r.Runs, len(strings.Split(r.Schedule, ",")), r.Schedule, r.Trace)
}

// check runs the exhaustive exploration. DETEST_SCHEDULE replays one schedule
// instead.
func (sim *Sim) check() *result {
	start := time.Now()
	if s := os.Getenv("DETEST_SCHEDULE"); s != "" {
		var prefix []choice
		for _, f := range strings.Split(s, ",") {
			v, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil {
				sim.t.Fatalf("detest: bad DETEST_SCHEDULE: %v", err)
			}
			prefix = append(prefix, choice{picked: v, replay: true})
		}
		r := sim.newRun(prefix)
		r.tracing = true
		v := r.execute()
		fmt.Fprintf(os.Stderr, "--- replay\n%s\n", r.traceString())
		if v != nil && v.kind == "fatal" {
			return &result{Runs: 1, Fatal: v.err}
		}
		return sim.makeResult(r, v, 1, len(r.choices), true, start)
	}
	f := sim.frontier
	if f == nil {
		// A lone exploration, as the package's own tests start one; Explore
		// always passes the frontier it saves and resumes.
		f = newFrontier(1, sim.maxRuns)
		sim.frontier = f
		return f.merge([]*result{sim.checkShared(f, 0)}, 1)
	}
	return sim.checkShared(f, sim.worker)
}

func (sim *Sim) printRun(n int, r *run) {
	fmt.Fprintf(os.Stderr, "--- run %d\n%s\n", n, r.traceString())
}

// retrace runs a violating schedule again with the trace kept, which exploring
// runs skip. The run is deterministic, so it violates again; if it does not,
// the original violation is reported with the code's nondeterminism noted.
func (sim *Sim) retrace(r *run, v *violation) (*run, *violation) {
	rr := sim.newRun(append([]choice(nil), r.choices...))
	rr.tracing = true
	if rv := rr.execute(); rv != nil {
		return rr, rv
	}
	rr.note(nil, "(the schedule did not violate again when rerun to record this trace: the code under test is nondeterministic)")
	return rr, v
}

// ownsPrefix reports whether the subtree under the first shardDepth choices
// belongs to this shard.
func (sim *Sim) ownsPrefix(prefix []choice) bool {
	// FNV-1a over the picks, which like the shard count are small and
	// non-negative, so the conversions cannot overflow.
	h := uint32(2166136261)
	for _, c := range prefix[:sim.shardDepth] {
		h ^= uint32(c.picked) //nolint:gosec
		h *= 16777619
	}
	return int(h%uint32(sim.shardTotal)) == sim.shardIndex //nolint:gosec
}

func (sim *Sim) makeResult(r *run, v *violation, runs, depth int, complete bool, start time.Time) *result {
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
