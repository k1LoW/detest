# Design

This document explains how detest works and why it is built the way it is. The README shows how to use it.

## The problem

Backend code fails under concurrency in ways that tests rarely catch. Two requests read the same row before either writes, a worker dies between claiming a job and finishing it, a message is delivered twice, a mutex is taken in the opposite order on another path. Each of these needs a specific interleaving of a few operations, and an ordinary test runs one interleaving, chosen by the Go runtime and the database, and usually the same one every time.

detest runs the code under test many times, once for every interleaving of its operations within bounds, and checks invariants on each run. When a run breaks one, it reports the sequence of choices that produced it, which replays that run exactly.

## Position

detest is deterministic simulation testing (DST) done in process, with stateless model checking (SMC) as the search.

- **In process.** The code under test, the scheduler and every simulated resource run inside one `go test` process. There is no container, no real database and no network.
- **Simulation at the client boundary.** detest does not intercept system calls, packets or disks. It replaces what the application talks to at the level of its client libraries, such as a `database/sql` driver, an `http.RoundTripper`, queues and mutexes. The production code above that boundary runs unchanged.
- **Exhaustive within bounds.** Most DST tools pick schedules at random from a seed. detest enumerates them depth first by default, as stateless model checkers do. A passing exploration means no schedule within the bounds breaks an invariant, rather than that the samples tried did not. The bounds (preemptions, failures, crashes, runs) are reported with the result, because the search is still not a proof.

## Architecture

```
Explore(t, fn, opts...)
 └─ per worker: a testing/synctest bubble
     ├─ fn declares the simulation on a Sim
     │    simulated resources, process types, seeds, invariants
     └─ runs, one per schedule
          scheduler ── resumes one process at a time
             │
             ├─ processes: goroutines running the code under test
             │     └─ call into simulated resources, which yield
             └─ choices: which process, which outcome, which fault
```

`Explore` is the only entry point. Everything a test declares goes through the function passed to it, which runs once per worker. A narrow entry point keeps the rules about determinism (below) enforceable, and a public API can be widened later but not narrowed.

### Processes and yield points

A process is a goroutine running user code, such as a request handler, a worker loop or a message consumer. The scheduler resumes exactly one process at a time and waits until it reaches a **yield point**, an operation on a simulated resource such as a SQL statement, a commit, a lock, an external call or an enqueue. There the process parks and the scheduler chooses what happens next.

Switching only at yield points is the main modeling decision. Code between two yield points runs atomically. This keeps the number of schedules proportional to the operations that interact through shared state, rather than to every instruction. The cost is that a data race on memory between two yield points is out of scope; `go test -race` covers that.

### Goroutines the code under test starts

Production code often starts goroutines of its own, such as an `errgroup` polling several jobs at once or a worker handing a claimed job to a goroutine and going back to its loop. Such a goroutine becomes a process of its own the first time it calls into detest, named after the process that started it (`worker#1.1`, and `worker#1.1.1` for one it starts in turn). Its operations are then scheduled like any process's, rather than run under the name of whichever process the scheduler resumed last.

- A goroutine detest started for a process is told apart by the function at the bottom of its stack, which `runtime.Callers` reads cheaply. Only other goroutines are looked up by their goroutine id, since `runtime.Stack` takes a lock the whole runtime shares, which parallel workers would contend on at every statement.
- The goroutine is parked in its first call into detest before it does anything there, and the scheduler takes it in after the step that started it. Goroutines that arrive together are ordered by their parents, and between siblings by the call each arrived at, such as a statement with its arguments or a request with its body, which tells apart the goroutines a loop starts for different items. Only siblings making the same first call fall back to goroutine id, which mostly follows the order they were started in but not always, since the runtime hands ids out from per-processor caches. A misordering would replay another path, which detest reports as nondeterminism.
- A goroutine gives no sign when it returns. One that stops reporting back is first taken as blocked outside detest, and once nothing else can run, a dump of the goroutines tells the ones that returned from the ones still blocked.
- A goroutine still alive when its run ends, such as one asleep on a timer, unwinds as a process of that run when it wakes in a later one, and is never adopted by the later run.

A goroutine that runs a statement on a `*sql.Tx` another process began, such as an `errgroup` inside a transaction, is not adopted. database/sql holds the connection's and the transaction's locks while it calls the driver, and a goroutine blocked on them is not durably blocked, so a statement parked at a yield point would keep its siblings from ever settling. The statement runs at once instead, in the transaction and in the step of whichever process woke the goroutine, under a mutex that keeps it apart from the process running in that step. Its result is the server's, but its interleaving with other processes is not explored, which the report states as a bound, and a statement that would wait for a lock, or a commit, fails as unsupported. The scheduler wakes the processes such a statement releases, at its next step. Goroutines that run in the same step reach the connection in an order the Go runtime decides, which a replay does not repeat, so their statements are kept for the step and compared. Two that could depend on their order stop the exploration, as nondeterminism does. Statements that touch rows of their own by primary key, in a table no foreign key reaches, commute and go on. Any other statement stands for the whole database, and two plain reads never conflict.

A goroutine whose creator never called into detest belongs to the first process up its chain of creators, which is read from the goroutines still alive. When a creator on the chain has already returned, the chain cannot be followed, and the process of the step the goroutine arrived in stands for its owner. A goroutine that sleeps before its first call into detest is invisible until it makes that call, so the run may end at quiescence without it. Likewise, a goroutine started in one run whose first call comes after that run ended is adopted by the run it calls in, unless it calls while that run's seeds run, before any of its processes, when it can only be an earlier run's and is turned away.

### Choices

Every nondeterministic decision is a numbered choice among options.

- which runnable process to resume, which message to deliver, which process type to start;
- how an external call ends, among success, failure before its effect, and failure after it (the response lost);
- whether a lock wait times out, when `lock_timeout` is set;
- whether a process crashes here (`MaxCrashes`), whether a message is lost (`Losses`) or delivered twice (`Duplicates`);
- choices a fake makes with `Proc.Choose`.

A run is fully determined by its sequence of picks. That sequence is what a violation reports (`DETEST_REPLAY=0,0,1,...`), what `Replay` pins in a regression test, and what the explorer enumerates.

### testing/synctest

Every run happens inside a `testing/synctest` bubble. The bubble gives the code under test a fake clock, so `time.Sleep` and timers cost no real time and are deterministic, and it lets the scheduler know when the resumed process has parked, as `synctest.Wait` returns once every goroutine in the bubble is durably blocked. A process blocked on something detest does not model (a channel, a timer) is recognised that way and parked as blocked outside, and the scheduler moves on. When nothing else can run, the scheduler waits for such a process on the fake clock for up to a day, so a sleep of minutes, such as waiting out a lease or a backoff, costs no real time. A process that nothing will ever wake is reported as blocked, and so is one that sleeps for a day or more, as the scheduler cannot tell a timer from a wait that never ends.

A goroutine blocked on a `sync.Mutex` is not durably blocked in synctest's sense, so a real mutex held across a yield point would hang the scheduler. A watchdog outside the bubble, on real time, reports that with the stacks of the blocked goroutines. The fix is to inject `detest.Mutex` or `detest.RWMutex`, which the scheduler sees.

## Exploration

### Stateless depth-first search

The explorer does not store states. It re-executes the simulation from the beginning for every run, following a prefix of picks and then taking the first option at every new choice. When a run ends, the alternatives it did not take at each choice past its prefix become new prefixes to explore. Re-executing is simpler and cheaper in memory than snapshotting the state of arbitrary Go code, which is impossible in general.

Each run starts from a reset. Databases, queues and mutexes return to their state after the declaration, and seeds run again.

### Parallel workers

`Workers(n)` explores with n workers, each in its own bubble with its own `Sim`, because `synctest.Wait` waits for every goroutine of a bubble. They share a frontier, a stack of unexplored prefixes. A worker takes a prefix, runs it, and pushes the subtrees its run revealed. Every run is executed exactly once, and the load balances itself, unlike a split decided in advance.

With several violating schedules, workers would report whichever they found first, which changes from one execution to the next. The frontier keeps the violation that comes first in depth-first order and drops the subtrees after it, while still exploring the ones before it. The reported counterexample is then the one a single worker reports.

### Shrinking the counterexample

The first violating schedule in depth-first order is often not the simplest one. Before reporting it, detest goes through its choices in order, tries the first option where the run took a later one while keeping the other picks, and keeps the change when the run still breaks the same way. The reported schedule then departs from the default at fewer choices. This costs one run per choice and is deterministic, so the counterexample reported does not change from one execution to the next.

### Checkpoints and shards

`MaxRuns` caps an exploration by runs, and `MaxDuration` by wall-clock time. With `DETEST_CHECKPOINT` set, the unexplored prefixes are saved when either cap is reached, and the next execution resumes from them, so a large space can be explored across several CI runs.

`DETEST_SHARD` (or `Shard`) splits the space across machines by hashing a fixed number of leading picks. The split is decided in advance, so the shallow runs above the split depth repeat on every machine.

### Replays must reach the same place

Replaying a prefix assumes the code under test is deterministic given the picks. Code that depends on map iteration order, randomness or goroutines detest does not schedule breaks that silently, as the replay then explores a different program. Each run therefore keeps a fingerprint of the shape of its operations (which process yielded, the format of the operation, the options at each step) and records it at every choice. A replay that reaches a choice with another fingerprint stops the exploration and reports the code as nondeterministic.

The fingerprint leaves argument values out. Real code puts generated ids and wall-clock times into its statements, and the bubble's clock keeps advancing across runs, so values differ between runs of the same schedule while the operations do not.

## Simulated resources

### Database

The database is an in-memory `database/sql` driver. The code under test, including ORMs such as GORM, sqlx and sqlc, runs on the `*sql.DB` it returns.

Each kind of server has a package of its own. It parses the server's dialect with the server's real grammar into an internal representation shared by all of them, which one executor runs, and it describes what differs between servers, such as the isolation levels and the error codes. PostgreSQL (`postgres`, with pg_query) and MySQL (`mysql`, with TiDB's parser) are implemented. Statement shapes particular to one server, such as MySQL's `INSERT IGNORE`, `ON DUPLICATE KEY UPDATE` and `UPDATE ... ORDER BY ... LIMIT`, map onto the shared representation, and the semantics that differ, InnoDB's in particular, are switched on by the server's description.

For PostgreSQL, detest follows Read Committed, because the interleavings that matter come from its locking rather than from its query planner. It covers the following.

- the four row lock strengths and their conflict table, waits, `NOWAIT`, `SKIP LOCKED`, `lock_timeout`, and deadlock detection for cycles of row lock waits;
- statement-level snapshots, with the re-check of the predicate after a lock wait that Read Committed does;
- unique indexes (including partial and expression indexes) with the wait-then-conflict behavior of concurrent inserts;
- foreign keys, with `FOR KEY SHARE` on the parent, every referential action, `MATCH FULL` and deferred constraints checked at commit;
- `CHECK` and `NOT NULL`, savepoints, sequences, views and the DDL of real migrations and schema dumps.

For MySQL, detest follows InnoDB, whose concurrency differs from PostgreSQL's in ways that change which interleavings break an application.

- Row locks are shared or exclusive. A foreign key check takes a shared lock on the parent.
- At Repeatable Read, a plain read comes from a snapshot taken at the transaction's first read. Commits keep the versions they replace, numbered by a commit sequence, so a snapshot reads the version current at its number. Locking reads, `UPDATE` and `DELETE` read the latest version, which is how InnoDB lets a value read from the snapshot and written back lose an update.
- At Repeatable Read and Serializable, locking reads, `UPDATE` and `DELETE` take next-key locks. detest models them from the table's indexes rather than from a B-tree. It finds the index the WHERE searches by, from equalities on the index's leading columns and a range on the next one it can evaluate before the scan, compares keys over all of the index's columns in key order, locks every row whose key falls in the searched range, and locks the gap from the nearest key below the range to the nearest above, with that next record. An equality search on every column of a unique index that finds its row locks only the row, and a WHERE no index serves locks every row and the whole table. Gap locks do not conflict with each other, only with inserts into them, which is what makes two transactions that check for a missing row and then insert it deadlock.
- At Serializable, plain reads in a transaction become shared locking reads.
- A failed statement rolls back only its own writes, keeping its locks, and the transaction goes on. A deadlock rolls back the whole transaction.
- Strings compare exactly, as a `_bin` collation does. A column of a case-insensitive collation, MySQL 8's default, loads with the schema, and the statements whose outcome depends on how it compares are refused, since comparison, ordering, uniqueness and gap locks under such a collation are not modeled.

Errors are `*DBError` values with the server's SQLSTATE, and MySQL's error number for MySQL. Production code branches on its driver's error type, so `postgres.Errors` and `mysql.Errors` convert them, for example into `*pgconn.PgError` with `pgxerr.Convert` or `*mysql.MySQLError` with `mysqlerr.Convert`.

The database is not a reimplementation of the server, and it is held to one standard. A difference from the server matters when it can change an outcome the application observes under concurrency, such as which rows a statement reads or locks, whether it blocks, which error it gets or how many rows it affects. Every behavior gets one of three answers by that standard.

- **Exact.** The difference can change an outcome, and detest implements the behavior. It matches the server, down to the error code and the affected-row count.
- **Unsupported.** The difference can change an outcome, and detest does not implement the behavior exactly. The statement fails with `ErrUnsupportedSQL`. `CheckSQL` reports the cases a statement decides on its own, without a schema; a case that depends on the schema, such as a generated column detest cannot compute, fails when the write runs. Approximating is never the answer here, because a result computed from the wrong semantics looks like any other result, and it would make detest report a bug the application does not have or pass a schedule that breaks production.
- **Approximate.** The difference cannot change an outcome, under the way the databases are used (the schema is built before the processes run, the processes run DML on default settings, and values are the ones applications hold). detest does the simplest deterministic thing, which may ignore the statement or the clause, or skip a check the server makes. Plain indexes, for example, are parsed and dropped for PostgreSQL, since detest has no planner for them to steer; InnoDB's gap locks follow them, so MySQL keeps them.

AGENTS.md draws this line in detail, with the forms on each side.

Isolation levels are implemented per kind of server, since their semantics differ between servers. A level detest does not implement fails when the database is declared, or when a transaction asks for it, rather than running with the wrong semantics.

`DB.Ignore` takes tables out of the simulation. Real schemas contain tables no invariant looks at, such as audit logs, and their rows, locks and yield points would only enlarge the search.

### Queues

A queue is at least once and unordered. A consumer whose handler returns an error has its message redelivered up to `MaxRedeliveries`. `Duplicates` and `Losses` add duplicate delivery and loss as choices, to check idempotency and backstops.

### External services

`External` models a call to another service. `Do` wraps a call, and `Transport` serves an `http.Handler` through an `http.RoundTripper`, so a generated client can call the real handler of another service in process. Each call is a choice among success, failure before the effect and failure after it, the case most retry logic gets wrong.

### Mutexes

`Mutex` and `RWMutex` replace `sync.Mutex` and `sync.RWMutex`. Waiting for them is a wait inside detest, so the scheduler sees cycles that involve both mutexes and row locks, which the database cannot detect, and reports them as progress violations.

### Crashes

`MaxCrashes` lets any process die at any step. Its transactions roll back as its connection drops, the mutexes it held are freed, and the message it was handling is redelivered. detest does not know which declared processes share a pod, so a crash kills one process, together with the goroutines adopted from it, as a goroutine cannot die without its pod. The crash may strike while any of them stands at a step, such as while the process waits in `errgroup.Wait` and its goroutines poll.

## Checking

- `Always` invariants hold after every step; `AtQuiescence` invariants hold once no process can run. Eventual consistency is written as the latter.
- Progress violations need no invariant. They are a process blocked forever, a cycle of waits through mutexes, and a process waiting for a lock held by its own open transaction.
- A panic in a process needs no invariant either. It is reported as a violation with its stack and the schedule that replays it, and shrunk like any other, rather than taking the test binary down, since a panic under one interleaving is what the exploration is for.
- `Sometimes` declares a condition some run must meet. Invariants only say nothing broke, which an exploration that never gets near the bug also satisfies.
- `ExpectViolation` pins a known violation, and `Replay` pins one schedule, as regression tests.

## Rules for the code under test

Determinism is a precondition of the search, so detest asks a few things of a test.

- Build everything the code under test uses inside the declaration function. Channels and timers created outside the bubble are not durably blocking inside it.
- Pass `p.Context()` to production code. It is canceled when a run ends, so `database/sql` can roll back the transactions of an aborted run. A statement parked in detest returns its context's error when its context ends, as a real driver cancels the query, and its connection is dropped with its transaction, as pgx and go-sql-driver drop it.
- Inject `detest.Mutex` where the code takes a mutex across a yield point.
- Reset state kept outside simulated resources in a seed. The declaration function runs once per worker, not once per run.

## Decisions not taken

- **Random search as the default.** Seeded random scheduling scales to large state spaces but gives no statement about the schedules it did not try. Bounded exhaustive search does, and preemption bounding keeps it tractable for the small number of interacting operations that concurrency bugs need. `Prioritized(seed, depth)`, after PCT, and `Random(seed)` are there for trees too large for a depth-first search to get past the first choices within `MaxRuns`, and their results are never reported as complete.
- **Partial order reduction.** Skipping reorderings of independent operations would shrink the search, but deciding independence requires that the code shares no state outside detest's resources, which detest cannot verify for real code.
- **A store for shared variables.** Replacing in-memory variables of production code with detest types would make their accesses yield points, at the cost of changing the code under test more than injecting a mutex does.
- **Static analysis of read and write sets.** With real code running through the driver, the explorer needs no declared read and write sets.
- **A faithful database.** Reproducing the server in full, or running a real one, would remove every difference, including the ones that cannot change what the application observes. detest implements exactly what can change an outcome under concurrency, refuses what it does not implement, and approximates the rest as simply as it can, so that the engine stays small enough to be exact where it counts.
- **Tuning the runtime from inside.** detest allocates heavily while its live heap stays small, so a higher `GOGC` speeds it up, and `GOMAXPROCS` near the worker count helps. Both are process-wide settings that would affect other tests in the same binary, so they are left to whoever runs the tests.
