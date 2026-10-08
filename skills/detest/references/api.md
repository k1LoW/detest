# detest API cheat sheet

The parts of the API a simulation uses most. `go doc -all github.com/k1LoW/detest` and the module's README are the source of truth; check them when something here does not match.

## Entry point

```go
detest.Explore(t, func(t *testing.T, s *detest.Sim) { ...declare... }, opts...)
```

The function declares the simulation and returns; the exploration runs after it, once per schedule. With `Workers(n)` the function runs once per worker, so variables it captures are per worker. An exploration that starts over without `EagerStart` runs it once more per worker.

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
dropped := st.Dropped(q)                    // dropped after MaxRedeliveries; not settled, a broker would redeliver
```

## Options (`opts` of Explore)

| Option | Default | Effect |
| --- | --- | --- |
| `MaxCrashes(n)` | 0 | Any process may die at any step: its transactions roll back, mutexes free, message redelivered |
| `MaxStalls(n, d)` | 0 | Any process may sleep for `d` on the fake clock at any step while the others go on, so a TTL or lease shorter than `d` expires under it |
| `MaxFailures(n)` | 1 | External-call failures per run |
| `MaxRedeliveries(n)` | 1 | Redeliveries of a failed message |
| `MaxSpins(n)` | 100 | Steps of one process with no commit or enqueue in between, counted across the steps of others, while another could run; past it the process waits for another to step, so a busy-wait lets its setter run. With nothing else able to run it is a progress violation, and so are processes that give way to each other that many times with no change in between, once nothing else can run; raise it only for code that does that much work without committing |
| `MaxIdleTicks(n)` | 3 | Idle ticks of a loop in a row that leave its budget unspent, counted again from zero after progress: a change by a manual process, a message handler or the scheduler, or a tick of any loop, this one included, that did work; a run past it is cut, not checked at quiescence, and counted as `N runs cut at MaxIdleTicks`. Loops waking each other is the usual cause, rarely a need to raise it |
| `MaxPreemptions(n)` | unbounded | Context switches away from a runnable process per run. 2 or 3 keeps large scenarios tractable and still finds most races |
| `EagerStart(on)` | on | Starts a `Manual` without `After` or `When` at once instead of at every possible step, which leaves out the runs that differ only in where it started. Assumes the code before its first statement or call touches no package variable or other state outside detest in an order that matters, so record when a process began after a `p.Step`. A process that blocks or calls into detest without yielding before its first statement, a `SET` of a session setting, a `LAST_INSERT_ID()` read or a wait for a pool connection makes the exploration start over without it, which the summary line states; its schedules start with `lazy:`. Off under `Shard` |
| `MaxRuns(n)` | 200000 | Cap on runs; the exploration is then incomplete |
| `MaxDuration(d)` | none | Wall-clock cap |
| `Workers(n)` | 1 | Parallel workers |
| `DepthFirst()`, `Random(seed)`, `Prioritized(seed, depth)` | `DepthFirst()` | `Random(seed)` draws every choice from a seeded generator until `MaxRuns` or `MaxDuration`; `Prioritized(seed, depth)` follows PCT, running each process on and switching at `depth`-1 drawn steps (depth 2 or 3 covers most races); both never complete, no `DETEST_CHECKPOINT` |
| `Shard(index, total, depth)` | | One shard of the schedule tree, for splitting across machines (`DETEST_SHARD`); under `Random` or `Prioritized`, every `total`-th run, with `depth` unused |
| `Pods(n)` | 1 | Default instances of loops and message consumers |
| `ObserveSQL(fn)` | | Called with every statement and its error |
| `Replay(choices)` | | Run one schedule only |
| `Verbose()` | | Print every run's trace |

The environment variables are `DETEST_REPLAY=<choices>` replays one run, `DETEST_WORKERS`, `DETEST_MAX_RUNS`, `DETEST_MAX_DURATION=<dur>` and `DETEST_SEED` (for `Random` and `Prioritized` only) override their options, `DETEST_SHARD=i/n`, `DETEST_CHECKPOINT=<file>` (resume a capped exploration), `DETEST_STALL=<dur>` (default 30s), `DETEST_DEBUG=1`. `GOGC=400` often speeds long explorations.

## Database servers

```go
postgres.New(postgres.Errors(pgxerr.Convert))   // github.com/k1LoW/detest/db/postgres/pgxerr: *pgconn.PgError (pgx stdlib, GORM postgres)
postgres.New(postgres.Errors(pqerr.Convert))    // .../postgres/pqerr: *pq.Error (lib/pq)
postgres.New(postgres.SearchPath("app", "public"))
postgres.New(postgres.Collation(c))              // the database's collation and ctype when not C.UTF-8: postgres.C for the C locale, else c from a package of its own
mysql.New(mysql.Errors(mysqlerr.Convert))       // .../mysql/mysqlerr: *mysql.MySQLError (go-sql-driver, GORM mysql)
mysql.New(mysql.Isolation(detest.ReadCommitted), mysql.Database("app"), mysql.Collation("utf8mb4_bin"))
```

A libc collation, such as glibc's `en_US.utf8`, comes from `github.com/k1LoW/glibctext`, one package per glibc version and locale. It implements `LC_COLLATE` only, so embed the ctype's case mapping.

```go
type enUS struct{ detest.UnicodeCase } // en_US.UTF-8 maps case by Unicode
func (enUS) Compare(a, b string) int { return en_us_utf8.Collation.Compare(a, b) } // github.com/k1LoW/glibctext/collate/glibc2_41/en_us_utf8
postgres.New(postgres.Collation(enUS{}))
```

GORM is always given its connection through `gormpool`, so that goroutines sharing a GORM transaction (an errgroup deleting the rows its transaction locked) are scheduled and their lock waits explored rather than refused. Keep `PrepareStmt` off.

```go
sqlDB, store := s.DB("app", postgres.New())
gdb, err := gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: gormpool.New(sqlDB)}), &gorm.Config{}) // github.com/k1LoW/detest/db/seam/gormpool
```

sqlc and sqlx code that begins its transactions through an interface or a function it is given gets the same by beginning them on a seam package in the test. sqlc's queries must then be built with `New(tx)`, not `WithTx(tx)`.

```go
pool := sqlcdbtx.New(sqlDB)                           // github.com/k1LoW/detest/db/seam/sqlcdbtx
tx, err := pool.BeginTx(ctx, nil)                     // *sqlcdbtx.Tx: sqlc's DBTX, Commit, Rollback
xdb := sqlxext.New(sqlDB, "postgres")                 // github.com/k1LoW/detest/db/seam/sqlxext; default mapper, safe mode
xtx, err := xdb.BeginTxx(ctx, nil)                    // *sqlxext.Tx: sqlx.ExtContext, GetContext, SelectContext, NamedExecContext, Commit, Rollback
```

Without `Errors`, the app sees `*detest.DBError`, which matches `detest.ErrUniqueViolation`, `detest.ErrDeadlock`, `detest.ErrLockNotAvailable` and the other sentinels with `errors.Is`.

`detest.CheckSQL(postgres.New(), query)` tells whether detest can run a statement without a schema, and refuses a function or an operator detest does not know, or a call with the wrong arguments, wherever it stands in a `SELECT`, `INSERT`, `UPDATE` or `DELETE`. A case decided by the schema or the values passes it and fails in the run, as does an expression in a `CHECK` or a generated column, which the write that evaluates it refuses. A default detest cannot evaluate passes both: a write that leaves the column out stores the `Unknown` marker, and is refused only when the table's own schema reads the column. `ddl.From(ctx, liveDB, postgres.New())` (package `github.com/k1LoW/detest/db/ddl`) writes the tables of a live database as DDL detest accepts.

## Hand-written model API

`store.Tx(p, func(tx *detest.Tx) error)`, `tx.Get`, `tx.GetForUpdate`, `tx.Insert`, `tx.Update`, `tx.CAS`, `tx.UpdateWhere`, `tx.Delete`, `tx.Enqueue(q, msg)` (on commit), `store.SeedRow`, `store.Peek`. Use these for fakes of other systems, not to replace the code under test.
