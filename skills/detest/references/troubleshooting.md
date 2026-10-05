# Troubleshooting the harness

Problems with the simulation itself, as opposed to violations in the code under test. Fix the harness; never loosen the invariant or rewrite production SQL in the test to make a run pass.

## `detest: unsupported SQL (...)`

detest refuses SQL it cannot run with the server's exact semantics, instead of approximating. The README of the module lists what is unsupported.

- **During schema load.** The statement is in a migration. If it is plain DDL for tables the flow uses, load the schema another way (a dump, `ddl.From`, or the affected tables hand-written; see `wiring.md`), and consider reporting it upstream. Statements that declare nothing relevant are normally ignored already.
- **In the code under test.** The flow uses a form detest does not model (system catalogs, recursive CTEs, `RIGHT`/`FULL` joins, `COPY`, `BEGIN` sent as SQL, a case-insensitive collation on MySQL, ...). Tell the user which statement and where. The options are to explore another flow, to drive a lower layer that avoids the statement if that still covers the hazard, or, with the user's agreement, to change the production code. Do not substitute a different statement in the test: the result would describe code that does not run in production.
- **MySQL `_ci` collations.** detest compares strings exactly and refuses the statements whose outcome depends on how a case-insensitive column compares. If the production server's default collation really is a `_bin` one, pass `mysql.Collation("utf8mb4_bin")`. Otherwise do not change the collation on your own, since that runs the code under other semantics than production's. Ask the user, and change it only with their explicit agreement:
  - name the columns to declare `COLLATE utf8mb4_bin` in the test's copy of the schema, never in the production schema, and change only those;
  - give the reason the scenario's outcome cannot differ under every operation the flow performs on those columns, namely comparisons with stored values and with query literals and parameters, ordering, grouping and uniqueness. A seeded `Alice` queried as `alice`, or an `ORDER BY` or `LIMIT` over the column, can differ, and then the refusal stands;
  - state what this leaves unchecked, such as two owners `Alice` and `alice` colliding on the unique index in production.

  Without agreement the refusal stands. Say which flows cannot be checked, and offer a target that does not touch the column. In the report, list an agreed override under what the result does not cover.
- **ORM auto-migration or startup queries** (`AutoMigrate`, version checks, pings). Skip them in the test (`DisableAutomaticPing`, `SkipInitializeWithVersion`, schema from files).

`detest.CheckSQL(server, query)` checks a statement in isolation, refusing the functions and operators detest does not know wherever they stand in a `SELECT`, `INSERT`, `UPDATE` or `DELETE`; a case decided by the schema or the values passes it and fails in the run, as does an expression in a `CHECK` or a generated column, which the write that evaluates it refuses. A default detest cannot evaluate passes both, and a write that leaves the column out stores the `Unknown` marker unless the table's own schema reads the column. `ObserveSQL` lists every statement a flow sends.

## `detest: no scheduling progress for 30s ... Goroutines blocked in the bubble`

A process is blocked on something detest does not see. The stacks printed show where.

- `sync.Mutex`/`sync.RWMutex` held across a database call. Inject `s.Mutex`/`s.RWMutex`.
- Real network I/O: an HTTP client with the default transport, a Redis or cloud SDK client. Replace it with `External.Transport`, a fake, or `store.Ignore` for tables.
- Objects created outside the declaration function (pools, channels, timers). Create them inside.
- A connection pool limit (`db.SetMaxOpenConns(1)` copied from production) with a transaction open while another statement waits for a connection.

`DETEST_STALL=5s` shortens the wait while debugging; `DETEST_DEBUG=1` prints scheduler events.

## `process X is blocked on a channel or WaitGroup that nothing left running can release`

The code waits for a goroutine it started, or for something that only happens outside the simulation. Drive a synchronous entry point, model the goroutine as a process (`p.Spawn`), or replace the background machinery with `Loop`/`OnMessage`.

## `the simulation is nondeterministic`

Replaying the same choices produced different operations. Look for:

- map iteration deciding the order or set of statements. In the harness's own code, iterate sorted keys. In production code, do not sort it for the test: the order varies in production too and may be the bug itself, such as rows locked in map order deadlocking, so a sorted test path would hide it. Report it to the user as a finding, propose sorting in the production code, or stop as for a fidelity blocker;
- `math/rand` or random IDs deciding control flow (not just values);
- caches, `sync.Once`, package-level variables surviving from one run to the next. The declaration function runs once per worker, so building them there is not enough. Clear or rebuild them in a `Seed`, rebuilding the service there when its cache cannot be cleared;
- goroutines the code starts and does not wait for;
- Go state shared across workers (with `Workers(n)`, the declaration function runs once per worker; avoid package-level state).

## `waits for ... held by its own open transaction`

A process opened a transaction and then, while it is open, used the pool (`db` rather than `tx`) or called another service that touches the same rows. In production this hangs until a lock timeout. It is usually a real finding; report it as one.

## `closing a cycle of waits ...`, `blocked forever on a lock`

These are progress violations, deadlocks the database cannot detect (through mutexes, or a process blocked by a lock nobody will release). Real findings unless the harness created the mutex wrongly.

## The exploration is too large or slow

The summary line shows `complete=false` or the run takes minutes.

- Fewer actors (two), fewer runs per actor (one), fewer seeded rows.
- `store.Ignore(...)` for tables nothing in the flow reads back (see `wiring.md` for the conditions).
- `detest.MaxPreemptions(2)`, since most races need few context switches.
- `detest.When(...)` on loops so idle ticks do not branch.
- `DETEST_WORKERS=$(getconf _NPROCESSORS_ONLN)` and `GOGC=400`, with `-p 1` so that explorations of different packages do not compete for the cores.
- For long explorations, `MaxDuration` with `DETEST_CHECKPOINT=<file>` to resume across runs, or `DETEST_SHARD` across CI jobs.

Report an incomplete exploration as incomplete, with the run count and the bounds.

## `Sometimes not held`

No run reached the situation. Either the scenario cannot produce it (wrong seed data, an actor that exits early on an error you are not recording, a `When` that never holds), or the bug's precondition is impossible in this code. Find out which before reporting a pass.

## The process returned an error but nothing failed

detest only notes a process's error in the trace. Record errors that should not happen and check them in an invariant.

## `go: requires go >= 1.26`

detest needs Go 1.26. Raising the module's `go` line is the user's decision; if they decline, detest cannot run in this module.
