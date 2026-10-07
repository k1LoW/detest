# Wiring application code into a simulation

The goal is to run the production code unchanged, with its database, outgoing calls, queues and mutexes replaced by detest's at the client boundary. Build every object the code uses inside the declaration function.

## Contents

- Shared helpers
- Database access layers
- pgx native pools
- Schema
- Seeding data
- Choosing the entry point
- Outgoing calls to other services
- Queues and event publishing
- Mutexes and in-memory state
- Background workers and goroutines
- Time and randomness

## Shared helpers

Put these in `detest_helpers_test.go` in the package, behind `//go:build detest` like every detest test file, adjusting the migration loader to the project's layout. The file is shared by every detest test of the package. If an earlier round already created it, reuse it and add only what is missing.

```go
//go:build detest

// failOnUnsupported fails the test when the code under test sends SQL detest
// cannot run, which the code may otherwise swallow as an ordinary error.
func failOnUnsupported(t *testing.T) detest.Option {
	var seen sync.Map
	return detest.ObserveSQL(func(q string, err error) {
		var u *detest.ErrUnsupportedSQL
		if errors.As(err, &u) {
			if _, dup := seen.LoadOrStore(q, true); !dup {
				t.Errorf("detest cannot run a statement of the code under test: %v", err)
			}
		}
	})
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// go test runs in the package directory, so the path is relative to it.
const migrations = "../../db/migrations/*.up.sql"

func loadMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	files, err := filepath.Glob(migrations)
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}
```

A file may hold several statements; one `Exec` runs them all.

## Database access layers

Every one of these is built on the `*sql.DB` that `s.DB` returns.

| Layer | Construction |
| --- | --- |
| `database/sql` | pass `db` |
| sqlc (database/sql mode) | `queries.New(db)`; `queries.New(tx)` inside transactions works as in production |
| sqlx | `sqlx.NewDb(db, "postgres")` or `"mysql"` |
| GORM, Postgres | `gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: gormpool.New(db)}), &gorm.Config{DisableAutomaticPing: true, TranslateError: <as production>})` |
| GORM, MySQL | `gorm.Open(gormmysql.New(gormmysql.Config{Conn: gormpool.New(db), SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, TranslateError: <as production>})` |
| ent | `drv := entsql.OpenDB(dialect.Postgres, db); client := ent.NewClient(ent.Driver(drv))` |
| bun | `bun.NewDB(db, pgdialect.New())` / `mysqldialect.New()` |

A library with a package under `github.com/k1LoW/detest/db/seam` is wired through it whenever the code uses that library, without waiting for a goroutine problem to show up. For GORM that is `gormpool.New(db)` (`github.com/k1LoW/detest/db/seam/gormpool`), as in the table. It changes nothing for transactions that stay on one goroutine, and goroutines sharing a GORM transaction, such as an errgroup deleting the rows its transaction locked, are then scheduled, wait for locks and take part in deadlocks like any process, rather than run unscheduled and be refused where they would wait.

sqlc and sqlx have seam packages too, `sqlcdbtx` and `sqlxext`, but they apply only when the code begins its transactions through something the test can replace, an interface or a function it is given, and hands the transaction on as an interface (sqlc's `DBTX` with `New(tx)` rather than `WithTx(tx)`, sqlx's `ExtContext`). Look for that shape and, where it is there, give the code `sqlcdbtx.New(db).BeginTx` or `sqlxext.New(sqlx.NewDb(db, "postgres")).BeginTxx` as what begins its transactions. Where the code begins them on the `*sql.DB` or `*sqlx.DB` itself, wire it as in the table. ent and bun have no seam package.

Copy the production `gorm.Config` options that change behavior (`TranslateError`, `SkipDefaultTransaction`, naming strategy); set `Logger: logger.Discard` to keep output readable. Leave `PrepareStmt` off even when production sets it. A prepared statement runs without the pool, and detest runs the same SQL either way, so nothing the application observes changes.

Match the driver's error type with `postgres.Errors(pgxerr.Convert)`, `postgres.Errors(pqerr.Convert)` or `mysql.Errors(mysqlerr.Convert)`, whichever the production driver is (`pgx/v5/stdlib` and GORM postgres use pgx). Code that checks `errors.As(err, &pgErr)` or GORM's `ErrDuplicatedKey` then takes the production branch.

If the app's constructor opens the database itself from a DSN, look for a lower constructor that takes a `*sql.DB` or an interface. If none exists, propose to the user the smallest change that adds one.

## pgx native pools

Code holding `*pgxpool.Pool`, `*pgx.Conn` or sqlc's pgx `DBTX` does not go through `database/sql`, and detest cannot intercept it. Tell the user plainly and offer the options:

- Test another flow that does use `database/sql`.
- If the repository layer depends on a small interface, write a test adapter from that interface to `*sql.DB` (only when the interface is small, such as `Exec`/`Query`/`QueryRow`/`Begin`).
- Leave it untested by detest.

Do not switch the production driver for the test's sake without the user's decision.

## Schema

The simulated schema must have the production constraints, because uniqueness, foreign keys and checks decide outcomes. In order of preference:

1. **The migration files** as they are, applied in order. For goose, apply only the `-- +goose Up` part of each file (cut at `-- +goose Down`). For golang-migrate, the `*.up.sql` files.
2. **A schema dump** (`pg_dump --schema-only`, `mysqldump --no-data`, `schema.sql`, `structure.sql`).
3. **`ddl.From`** against a database the project already runs for integration tests, saved to a `testdata/schema.sql` file once, then loaded from that file. For PostgreSQL it writes tables, defaults, primary keys, unique constraints and unique indexes, but no foreign keys or `CHECK` constraints, so add those from the production DDL for the tables in the flow, or prefer 1 or 2. For MySQL it writes `SHOW CREATE TABLE`, which has them.
4. **Hand-written `CREATE TABLE`** for only the tables in the flow, copied from the migrations with every key, unique, foreign key and check. Tell the user which tables were transcribed, because a drift from production would change outcomes.

ORM auto-migration (`gorm.AutoMigrate`, ent's `Schema.Create`) reads system catalogs, which detest refuses. Use one of the options above instead.

Statements that declare nothing detest needs (grants, comments, extensions) are accepted and ignored. Functions and triggers are accepted and ignored too, which is not harmless when a trigger fires in the flow. Its effect, such as an outbox row written on INSERT or an error raised on UPDATE, is missing from the simulation, and `failOnUnsupported` does not reveal it. Check the triggers on the tables the flow writes. When one changes rows or errors the code or the invariants observe, tell the user the flow cannot be simulated faithfully and stop, as for any blocker (see SKILL.md). If a migration fails to load, find the statement, and check the README's unsupported list. Plain DDL failing to load is worth reporting upstream (https://github.com/k1LoW/detest/issues).

`store.Ignore(...)` takes a table out of the simulation, which shrinks the search. An ignored table reads as empty, takes no locks and checks no constraints, so that no invariant reads it is not enough. Ignore a table only when no read, affected-row count, constraint or lock on it can change what the code or the invariants observe, as for a write-only audit log or history. A table the code reads to decide, such as processed events or idempotency keys for deduplication, stays, even when no invariant reads it.

## Seeding data

Insert the rows the scenario needs in `s.Seed`, with SQL or with the app's own creation functions (called with `context.Background()` inside `Seed`). Keep it minimal, since every extra row can add locks and branches. Insert every initial row in a `Seed`, never in the declaration function itself, since each run empties the tables before the seeds run.

## Choosing the entry point

Drive the highest in-process layer that contains the logic, such as a use-case or service method, or an HTTP/gRPC handler.

```go
h := api.NewRouter(api.Deps{DB: db, ...})        // built inside the declaration function
s.Manual("alice", 1, func(p *detest.Proc) error {
	req := httptest.NewRequestWithContext(p.Context(), http.MethodPost, "/orders", strings.NewReader(`{"product":"p1"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	statuses = append(statuses, rec.Code)        // check in an invariant
	return nil
})
```

Middleware that opens connections, starts goroutines or calls real services needs the same replacements as the handler.

## Outgoing calls to other services

Inject an `*http.Client` whose transport is detest's, serving a fake or the real handler of the other service:

```go
payments := s.External("payments")
charged := 0
s.Seed(func() { charged = 0 })
fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { charged++; w.WriteHeader(http.StatusOK) })
client := &http.Client{Transport: payments.Transport(fake)}
svc := checkout.New(db, checkout.WithHTTPClient(client))
```

The explorer tries success, failure before the handler runs, and the response lost after it ran (`detest.ErrUnavailable` to the caller). An invariant such as `charged <= 1` then checks idempotency of retries. For SDK clients that are not HTTP-shaped, wrap the call site with `ext.Do(s.Current(), "charge", func() error {...})` in a fake that implements the app's interface. Use `detest.ReadOnly()` for reads.

## Key-value stores

Redis, memcached and similar stores have no simulation, but code that reaches one through an interface of its own (`Get`, `Set`, `SetNX`, `Del` with a TTL) can take a fake whose every command is one `ext.Do` on a map, with expiry read from `time.Now()`, which is the fake clock. [examples/kvlock](../../../examples/kvlock/kvlock_test.go) has one to copy. Clear the map in a `Seed`. Code that uses Lua scripts, `MULTI`/`EXEC`, `WATCH` or data structures beyond strings needs more than this fake, so treat it as a blocker unless the user accepts a fake of those commands.

## Queues and event publishing

Replace the app's publisher with a fake that enqueues into a detest queue, and drive its consumer with `OnMessage`:

```go
q := s.Queue("order-events", detest.Duplicates(1))
pub := publisherFunc(func(ctx context.Context, ev Event) error {
	q.Enqueue(s.Current(), detest.Msg{"type": ev.Type, "order": ev.OrderID})
	return nil
})
s.OnMessage("shipper", q, func(p *detest.Proc, m detest.Msg) error {
	return shipping.Handle(p.Context(), db, m.Str("order"))
})
```

`Duplicates(1)` checks idempotency, `Losses(1)` checks that a sweeper (`s.Loop`) covers a lost message, `MaxCrashes(1)` checks a publish between commit and crash. A transactional outbox in the app's own tables needs no fake: the relay is a `Loop` reading the outbox table.

## Mutexes and in-memory state

A `sync.Mutex` held across a database call blocks outside the scheduler and stalls the exploration. If the code takes the mutex through a field or constructor, inject `s.Mutex("name")` (it implements `sync.Locker`). If the field is a value `sync.Mutex`, propose to the user changing it to a `sync.Locker` field defaulting to `&sync.Mutex{}`.

In-memory caches, `sync.Once` and package-level variables persist across runs, since the declaration function runs once per worker, not once per run. Clear or rebuild them in a `Seed`, rebuilding the service there when its cache cannot be cleared. Code between two database calls runs atomically under detest, so races on Go memory are out of scope; mention `go test -race` for those.

## Background workers and goroutines

Do not start the worker's infinite loop. Call one iteration from `s.Loop`:

```go
s.Loop("reaper", 2, func(p *detest.Proc) error {
	n, err := jobs.ReapExpired(p.Context(), db)
	if err == nil && n == 0 {
		return detest.ErrIdle
	}
	return err
})
```

Goroutines the code under test starts itself, such as an `errgroup` polling jobs or a worker handing a claimed job to a goroutine, need no wiring. Each becomes a process the first time it calls into detest, named after the one that started it (`worker#1.1`), and is scheduled like any other, so an iteration such as `claimAndRun` that starts one can be called as it is. Two cases stay out of sight. A goroutine that sleeps before its first call into detest is seen only once it calls, so a process that only starts such a goroutine may reach quiescence without it. A goroutine that blocks for good on a channel is reported like a process doing so. Goroutines that run statements on their process's `*sql.Tx`, an errgroup inside a transaction, are the exception. Their statements run at once in the transaction, without being scheduled, and the report counts them. One that would wait for a lock, a commit or rollback from such a goroutine, and statements of two goroutines whose outcome depends on their order, such as two updates of one row, are refused or stop the exploration, which the report says. Each goroutine touching rows of its own by primary key, as an outbox poller deleting the rows it locked does, runs as it is. None of this applies to code wired through a seam package (`gormpool`, or `sqlcdbtx` and `sqlxext` where the code begins its transactions through an interface), where such goroutines are scheduled like any other and only a commit or rollback from one stops the exploration. Code that hands the goroutines the `*sql.Tx` or `*sqlx.Tx` itself, as sqlc's `WithTx` does, has no such seam; if the user's code hits these limits there, say so, and name beginning the transaction through an interface and handing it on as `DBTX` or `ExtContext` as the change that would lift them only when it is small.

## Time and randomness

The simulation runs in a `testing/synctest` bubble, where `time.Now`, `time.Sleep` and timers use a fake clock, so sleeps and timeouts cost nothing. Random values (UUIDs, tokens) are fine as data. Randomness or map iteration that changes *which* operations run, or their order, makes the simulation nondeterministic; see troubleshooting.
