# detest

In-Process Deterministic Simulation Testing for Go Applications, based on Stateless Model Checking (SMC)

*Because we **detest** race conditions, deadlocks, and flaky tests.*

---

`detest` is a lightweight, in-process testing framework that brings **Deterministic Simulation Testing (DST)** to standard Go applications using **Stateless Model Checking (SMC)**.

Instead of virtualizing OS threads or low-level network packets, `detest` operates at Go's standard client boundaries (`database/sql`, `http.RoundTripper`) and at mutexes injected in place of `sync.Mutex`, running goroutines inside a [`testing/synctest`](https://pkg.go.dev/testing/synctest) bubble. It injects failure scenarios and systematically explores every execution interleaving within configurable bounds, without modifying your production business logic or ORMs.

## Usage

``` go
// reserve is production code. It takes one item from stock.
func reserve(ctx context.Context, db *sql.DB, sku string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT n FROM stock WHERE sku = $1`, sku).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("out of stock")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE stock SET n = $1 WHERE sku = $2`, n-1, sku); err != nil {
		return err
	}
	return tx.Commit()
}

func TestReserve(t *testing.T) {
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		db, store := s.DB("shop", postgres.New())
		if _, err := db.Exec(`CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		s.Seed(func() {
			_, _ = db.Exec(`INSERT INTO stock (sku, n) VALUES ('apple', 1)`)
		})
		reserved := 0
		s.Seed(func() { reserved = 0 })
		for _, name := range []string{"alice", "bob"} {
			s.Manual(name, 1, func(p *detest.Proc) error {
				if err := reserve(p.Context(), db, "apple"); err == nil {
					reserved++
				}
				return nil
			})
		}
		s.AtQuiescence(func(st *detest.State) error {
			if reserved > 1 {
				row, _ := st.Row(store, "stock", "apple")
				return fmt.Errorf("%d reservations of 1 item (stock now %d)", reserved, row.Int64("n"))
			}
			return nil
		})
	})
}
```

```
--- FAIL: TestReserve (0.67s)
    stock_test.go:34: detest: quiescence invariant violated: 2 reservations of 1 item (stock now 0)
        run 4, schedule (8 choices): DETEST_REPLAY=0,0,0,0,1,1,1,0
            1  alice#1  shop: begin   (stock_test.go:15)
            2  alice#1  shop: select stock where sku = apple   (stock_test.go:21)
            3  alice#1  shop: update stock set {n=0} where sku = apple   (stock_test.go:27)
            4  alice#1  shop: commit   (stock_test.go:30)
            5  bob#2    shop: begin   (stock_test.go:15)
            6  bob#2    shop: select stock where sku = apple   (stock_test.go:21)
            7  bob#2    shop: update stock set {n=0} where sku = apple   (stock_test.go:27)
            8  alice#1  done
            9  bob#2    shop: commit   (stock_test.go:30)
           10  bob#2    done
```

The function passed to `detest.Explore` declares the simulation. It runs once per explored schedule, so state that the processes change has to be reset in `s.Seed`. Run the test again with the printed `DETEST_REPLAY` to replay exactly that run, or pass it to `detest.Replay` to pin the counterexample in a regression test.

A test that pins a known violation calls `s.ExpectViolation(substr)`. It passes while the violation is found and fails once it is gone.

### Declaring a model

| Method | What it declares |
| --- | --- |
| `s.DB(name, server)` | A database. It returns a `*sql.DB` to hand to production code and a `*detest.DB` to read in invariants |
| `s.Manual(name, n, fn)` | A process type started up to `n` times |
| `s.Loop(name, n, fn)` | A process that runs `fn` repeatedly, such as a poller or a reaper |
| `s.Queue(name)`, `s.OnMessage(name, q, fn)` | A message queue and its consumer, with redelivery, and with duplicates (`detest.Duplicates`) or losses (`detest.Losses`) when asked |
| `s.External(name)` | A call to another service, which may fail before its effect or lose the response after it. `Transport(h)` gives an `http.RoundTripper` serving an `http.Handler` |
| `s.Mutex(name)`, `s.RWMutex(name)` | Locks the scheduler can see, to inject in place of `sync.Mutex` |
| `s.Always(fn)`, `s.AtQuiescence(fn)` | Invariants checked after every step, or once every process is done |
| `s.Sometimes(name, fn)` | A condition at least one run must meet, so that an exploration that never gets near the bug fails |

## Simulated resources

Processes interact only through simulated resources. Every operation on them is a scheduling point, and each one models how its real counterpart behaves under concurrency.

### Database

`s.DB(name, postgres.New())` returns a `*sql.DB` backed by an in-memory driver. Production code, including GORM, sqlx and sqlc, runs on it unchanged. SQL is parsed with PostgreSQL's real grammar, and the semantics are those of PostgreSQL at Read Committed.

**Concurrency**

- Row locks of the four strengths (`FOR UPDATE`, `FOR NO KEY UPDATE`, `FOR SHARE`, `FOR KEY SHARE`) with PostgreSQL's conflict table, taken by writes and by locking reads
- Waits on locked rows, with the predicate re-checked on the new version after the wait, as Read Committed does
- `NOWAIT`, `SKIP LOCKED` and `SET LOCAL lock_timeout`
- Deadlock detection for cycles of row lock waits (40P01), and reports of waits the database cannot detect, through a mutex or an open transaction of the same process
- Unique indexes, where a concurrent insert of the same value waits for the first transaction and then conflicts
- Savepoints, and a failed statement aborting its transaction (25P02)

**Constraints**

- Primary keys (composite too), unique constraints and unique indexes, including partial, expression and `NULLS NOT DISTINCT` indexes
- Foreign keys, with `FOR KEY SHARE` on the parent, `ON DELETE` and `ON UPDATE` actions (`NO ACTION`, `RESTRICT`, `CASCADE`, `SET NULL`, `SET DEFAULT`), `MATCH FULL`, and deferrable constraints with `SET CONSTRAINTS`
- `CHECK` and `NOT NULL`
- Column types `uuid`, `smallint` and `integer` are checked on write

**Statements**

- `SELECT` with inner, left and cross joins, `LATERAL`, subqueries, non-recursive CTEs, `GROUP BY` and `HAVING`, aggregates (`count`, `sum`, `min`, `max`, `avg`), window functions (`row_number`, `rank`, `dense_rank`, `lag`, `lead`, `first_value`, `last_value` and the aggregates) over the default frame or the whole partition, `DISTINCT`, `DISTINCT ON`, `UNION`, `INTERSECT`, `EXCEPT`, `VALUES`, `ORDER BY` with `NULLS FIRST | LAST`, `LIMIT` and `OFFSET`
- `INSERT` with `ON CONFLICT DO NOTHING | DO UPDATE` and `RETURNING`, `INSERT ... SELECT`
- `UPDATE ... FROM` and `DELETE ... USING`, with `RETURNING`
- Common functions, among them `coalesce`, `nullif`, `greatest`, `least`, `lower`, `upper`, `length`, `concat`, `now`, `nextval`, `setval`, `gen_random_uuid`, `generate_series` and the transaction advisory locks
- Expressions follow SQL's three-valued logic

**Schema**

- `CREATE`, `ALTER` and `DROP` of tables, columns, constraints, indexes, views and materialized views, `CREATE TABLE AS`, `REFRESH MATERIALIZED VIEW`, renames
- Defaults, serial and identity columns, sequences, with values that repeat from run to run
- Schemas and `search_path` (`postgres.SearchPath`)
- Migrations and `pg_dump --schema-only` output run as they are. Statements that declare nothing detest needs, such as functions, grants and comments, are accepted and ignored
- `ddl.From` reads the tables of a live database and writes DDL that detest accepts
- `store.Ignore(...)` takes tables the invariants do not look at, such as an audit log, out of the simulation. Writes to them are dropped and are no scheduling points, which keeps the exploration small

**Errors**

Errors are `*detest.SQLError` with PostgreSQL's SQLSTATE, the table, the column and the constraint, and match `detest.ErrUniqueViolation` and the other sentinels with `errors.Is`. Code that branches on its driver's error type gets that type by converting.

``` go
s.DB("app", postgres.New(postgres.Errors(pgxerr.Convert))) // *pgconn.PgError
s.DB("app", postgres.New(postgres.Errors(pqerr.Convert)))  // *pq.Error
```

**Not supported**

Isolation levels other than Read Committed, recursive CTEs, `RIGHT` and `FULL` joins, `JOIN ... USING`, window frames other than the two above, `COPY`, system catalogs, and `BEGIN` or `COMMIT` sent as SQL (use `database/sql`'s transactions). Such statements fail with `detest.ErrUnsupportedSQL` rather than being approximated, and `detest.CheckSQL` tells whether detest can run a statement. The `mysql` package declares a MySQL server, but its parser is not implemented yet.

### Queue

`s.Queue(name)` is an at-least-once, unordered queue, and `s.OnMessage(name, q, fn)` its consumer.

- A handler that returns an error has its message redelivered, up to `detest.MaxRedeliveries` times
- `detest.Duplicates(n)` delivers up to n messages twice per run
- `detest.Losses(n)` loses up to n messages per run, to check that a backstop covers them
- `q.Enqueue(p, msg)` publishes at once, and `tx.Enqueue(q, msg)` on commit

### External service

`s.External(name)` is a call to another service. The explorer tries success, failure before the effect, and failure after it (the effect applied but the response lost), where the caller sees `detest.ErrUnavailable`.

- `ext.Transport(handler)` returns an `http.RoundTripper` serving an `http.Handler` in process, to give generated clients (connect, gRPC-Web, REST) the real handler of the other service
- `ext.Do(p, desc, fn)` wraps any call
- `detest.Failures(...)` chooses the failures to try, and `detest.ReadOnly()` drops failure after the effect for reads

### Mutex

`s.Mutex(name)` and `s.RWMutex(name)` replace `sync.Mutex` and `sync.RWMutex` in production code. Waiting for them is visible to the scheduler, so cycles that mix mutexes and row locks are reported. As with `sync.RWMutex`, a waiting writer keeps new readers out.

## Options and environment variables

The search space grows quickly. `detest.MaxPreemptions`, `detest.MaxFailures`, `detest.MaxRedeliveries` and `detest.MaxRuns` bound it, and `detest.Workers` explores it in parallel. `detest.MaxCrashes` lets processes crash at any step (their transactions roll back, their mutexes are freed, their messages are redelivered), to check that work survives a process dying halfway.

| Variable | Effect |
| --- | --- |
| `DETEST_REPLAY` | Replay one run, given the choices printed with a violation |
| `DETEST_WORKERS` | Number of workers, overriding `detest.Workers` |
| `DETEST_SHARD` | `index/total[/depth]`, explore one shard of the space (for splitting across CI jobs) |
| `DETEST_CHECKPOINT` | A file to save the unexplored part to when `MaxRuns` is reached, and to resume from on the next run |
| `DETEST_STALL` | How long a process may block outside the scheduler before it is reported (default `30s`) |
| `DETEST_DEBUG` | Print scheduler events to stderr |

Exploration allocates a lot. Raising `GOGC` (for example `GOGC=400`) often shortens long explorations.

## Requirements

Go 1.26 or later.

## License

[MIT](LICENSE)
