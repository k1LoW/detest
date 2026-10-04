# detest API cheat sheet

The parts of the API a simulation uses most. `go doc -all github.com/k1LoW/detest` and the module's README are the source of truth; check them when something here does not match.

## Entry point

```go
detest.Explore(t, func(t *testing.T, s *detest.Sim) { ...declare... }, opts...)
```

The function declares the simulation and returns; the exploration runs after it, once per schedule. With `Workers(n)` the function runs once per worker, so variables it captures are per worker.

## Declaring (`s *detest.Sim`)

| Call | Use |
| --- | --- |
| `db, store := s.DB(name, postgres.New(opts...))` / `mysql.New(opts...)` | A database. `db` is a `*sql.DB` for the app, `store` a `*detest.DB` for invariants. `store.Open()` gives a second pool on the same database for a second service |
| `store.Ignore("audit_logs", ...)` | Write-only tables nothing in the flow reads back. Writes are dropped, reads find nothing, no locks, constraints or scheduling points. It shrinks the search, but only for tables whose reads, counts, constraints and locks cannot change an outcome (see `wiring.md`) |
| `s.Seed(func(){...})` | Runs at the start of every run. Insert rows, reset Go variables |
| `s.Manual(name, n, fn, opts...)` | A process type started at any point, up to `n` times per run, **one instance at a time** unless `detest.Instances(k)` |
| `s.Loop(name, n, fn, opts...)` | A periodic process (sweeper, poller, reaper), up to `n` ticks per run. Return `detest.ErrIdle` from a tick that found nothing to do |
| `s.Queue(name, detest.Duplicates(n), detest.Losses(n))` | At-least-once unordered queue |
| `s.OnMessage(name, q, func(p *detest.Proc, m detest.Msg) error, opts...)` | Consumer. A returned error redelivers, up to `MaxRedeliveries` |
| `s.External(name, detest.Failures(...), detest.ReadOnly())` | A call to another service: success, failure before effect, failure after effect (response lost) |
| `ext.Transport(handler)` | `http.RoundTripper` serving an `http.Handler` in process, with those outcomes |
| `ext.Do(p, desc, func() error)` | Wrap any call with those outcomes |
| `s.Mutex(name)`, `s.RWMutex(name)` | Inject in place of `sync.Mutex`/`sync.RWMutex` held across DB calls |
| `s.Always(func(st *detest.State) error)` | Invariant after every commit and process completion. `st.Prev()` is the previous state |
| `s.AtQuiescence(func(st *detest.State) error)` | Invariant when all processes are done, queues drained, budgets spent |
| `s.Sometimes(name, func(st *detest.State) bool)` | Must hold in at least one run; a complete exploration fails otherwise |
| `s.ExpectViolation(substr)` | Pin a known bug: passes while found, fails once gone |
| `s.Current()` | The process running the calling goroutine, for fakes that need a `*Proc` |

The process options are `detest.Instances(k)`, `detest.When(func() bool)` (start only while true, reading with `store.Peek` and never yielding), `detest.After(func(*State) bool)`.

## In a process (`p *detest.Proc`)

`p.Context()` (always pass to the app), `p.Name()`, `p.Step("...")` (a visible yield point with no effect), `p.Choose(label, n)` (explorer tries all n), `p.Spawn(name, fn)`, `p.Now()`, `p.WaitUntil(t)`.

## Reading state in invariants (`st *detest.State`)

```go
rows := st.Rows(store, "orders")            // []RowView sorted by key, committed only
row, ok := st.Row(store, "products", "p1")  // by primary key values, in order
row.Str("status"); row.Int64("stock"); row.Bool("paid"); v, ok := row.Get("col")
msgs := st.Queue(q)
```

## Options (`opts` of Explore)

| Option | Default | Effect |
| --- | --- | --- |
| `MaxCrashes(n)` | 0 | Any process may die at any step: its transactions roll back, mutexes free, message redelivered |
| `MaxFailures(n)` | 1 | External-call failures per run |
| `MaxRedeliveries(n)` | 1 | Redeliveries of a failed message |
| `MaxIdleTicks(n)` | 3 | Idle ticks per loop and run that leave its budget unspent; a run past it is cut, not checked, and counted as `N runs cut at MaxIdleTicks` |
| `MaxPreemptions(n)` | unbounded | Context switches away from a runnable process per run. 2 or 3 keeps large scenarios tractable and still finds most races |
| `MaxRuns(n)` | 200000 | Cap on runs; the exploration is then incomplete |
| `MaxDuration(d)` | none | Wall-clock cap |
| `Workers(n)` | 1 | Parallel workers |
| `Pods(n)` | 1 | Default instances of loops and message consumers |
| `ObserveSQL(fn)` | | Called with every statement and its error |
| `Replay(choices)` | | Run one schedule only |
| `Verbose()` | | Print every run's trace |

The environment variables are `DETEST_REPLAY=<choices>` replays one run, `DETEST_WORKERS`, `DETEST_SHARD=i/n`, `DETEST_CHECKPOINT=<file>` (resume a capped exploration), `DETEST_STALL=<dur>` (default 30s), `DETEST_DEBUG=1`. `GOGC=400` often speeds long explorations.

## Database servers

```go
postgres.New(postgres.Errors(pgxerr.Convert))   // github.com/k1LoW/detest/postgres/pgxerr: *pgconn.PgError (pgx stdlib, GORM postgres)
postgres.New(postgres.Errors(pqerr.Convert))    // .../postgres/pqerr: *pq.Error (lib/pq)
postgres.New(postgres.SearchPath("app", "public"))
mysql.New(mysql.Errors(mysqlerr.Convert))       // .../mysql/mysqlerr: *mysql.MySQLError (go-sql-driver, GORM mysql)
mysql.New(mysql.Isolation(detest.ReadCommitted), mysql.Database("app"), mysql.Collation("utf8mb4_bin"))
```

Without `Errors`, the app sees `*detest.DBError`, which matches `detest.ErrUniqueViolation`, `detest.ErrDeadlock`, `detest.ErrLockNotAvailable` and the other sentinels with `errors.Is`.

`detest.CheckSQL(postgres.New(), query)` tells whether detest can run a statement without a schema. `ddl.From(ctx, liveDB, postgres.New())` (package `github.com/k1LoW/detest/ddl`) writes the tables of a live database as DDL detest accepts.

## Hand-written model API

`store.Tx(p, func(tx *detest.Tx) error)`, `tx.Get`, `tx.GetForUpdate`, `tx.Insert`, `tx.Update`, `tx.CAS`, `tx.UpdateWhere`, `tx.Delete`, `tx.Enqueue(q, msg)` (on commit), `store.SeedRow`, `store.Peek`. Use these for fakes of other systems, not to replace the code under test.
