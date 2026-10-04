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

### Declaring a simulation

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

`s.DB(name, server)` returns a `*sql.DB` backed by an in-memory driver. Production code, including GORM, sqlx and sqlc, runs on it unchanged.

Each kind of server has its own package, which parses its SQL dialect with the server's real grammar and gives the locking, constraints and errors of that server, since they differ between servers. `postgres.New()` is PostgreSQL at Read Committed, which the lists below describe. `mysql.New()` is MySQL with InnoDB, described after them.

**Concurrency**

- Row locks of the four strengths (`FOR UPDATE`, `FOR NO KEY UPDATE`, `FOR SHARE`, `FOR KEY SHARE`) with PostgreSQL's conflict table, taken by writes and by locking reads
- Waits on locked rows, with the predicate re-checked on the new version after the wait, as Read Committed does
- `NOWAIT`, `SKIP LOCKED` and `SET LOCAL lock_timeout`
- Deadlock detection for cycles of row lock waits (40P01), and reports of waits the database cannot detect, through a mutex or an open transaction of the same process
- Unique indexes, where a concurrent insert of the same value waits for the first transaction and then conflicts
- Savepoints, and a failed statement aborting its transaction (25P02)

**Constraints**

- Primary keys (composite too), unique constraints and unique indexes, including partial, expression and `NULLS NOT DISTINCT` indexes
- Foreign keys, with `FOR KEY SHARE` on the parent, `ON DELETE` and `ON UPDATE` actions (`NO ACTION`, `RESTRICT`, `CASCADE`, `SET NULL`, `SET DEFAULT`), `MATCH FULL`, and deferrable constraints with `SET CONSTRAINTS`. A statement's checks run when it has written all its rows, as Postgres's do
- `CHECK` and `NOT NULL`
- Column types `uuid`, `smallint` and `integer` are checked on write

**Statements**

- `SELECT` with inner, left and cross joins, `LATERAL`, subqueries, non-recursive CTEs, `GROUP BY` and `HAVING`, aggregates (`count`, `sum`, `min`, `max`, `avg`), window functions (`row_number`, `rank`, `dense_rank`, `lag`, `lead`, `first_value`, `last_value` and the aggregates) over the default frame or the whole partition, `DISTINCT`, `DISTINCT ON`, `UNION`, `INTERSECT`, `EXCEPT`, `VALUES`, `ORDER BY` with `NULLS FIRST | LAST`, `LIMIT` and `OFFSET`
- `INSERT` with `ON CONFLICT DO NOTHING | DO UPDATE` and `RETURNING`, `INSERT ... SELECT`
- `UPDATE ... FROM` and `DELETE ... USING`, with `RETURNING`
- Common functions, among them `coalesce`, `nullif`, `greatest`, `least`, `lower`, `upper`, `length`, `concat`, `now`, `nextval`, `setval`, `gen_random_uuid`, `generate_series` and the transaction advisory locks
- Expressions follow SQL's three-valued logic
- Column references are resolved against the schema before a statement runs, so a column no table in scope has fails with 42703 as on the server

**Schema**

- `CREATE`, `ALTER` and `DROP` of tables, columns, constraints, indexes, views and materialized views, `CREATE TABLE AS`, `REFRESH MATERIALIZED VIEW`, renames
- Defaults, serial and identity columns, sequences with their `START`, `INCREMENT` and `RESTART`, with values that repeat from run to run
- Stored generated columns, computed on every write. A Postgres 18 virtual column loads through `ddl.From`, which writes it as stored, but not from a Postgres 18 dump, whose `GENERATED ALWAYS AS (...)` without `STORED` the grammar detest parses with rejects
- Schemas and `search_path` (`postgres.SearchPath`)
- Migrations and `pg_dump --schema-only` output run as they are. Statements that declare nothing detest needs, such as functions, grants and comments, are accepted and ignored
- `ddl.From` reads the tables of a live database and writes DDL that detest accepts
- `store.Ignore(...)` takes tables the invariants do not look at, such as an audit log, out of the simulation. Writes to them are dropped and are no scheduling points, which keeps the exploration small

**Errors**

Errors are `*detest.DBError` with the server's SQLSTATE (and, for MySQL, its error number), the table, the column and the constraint, and match `detest.ErrUniqueViolation` and the other sentinels with `errors.Is`. Code that branches on its driver's error type gets that type by converting.

``` go
s.DB("app", postgres.New(postgres.Errors(pgxerr.Convert))) // *pgconn.PgError
s.DB("app", postgres.New(postgres.Errors(pqerr.Convert)))  // *pq.Error
```

**Not supported**

Isolation levels other than Read Committed, recursive CTEs, `RIGHT` and `FULL` joins, `JOIN ... USING` and `NATURAL JOIN`, `ANY (subquery)` with an operator other than `=`, window frames other than the two above, `FILTER`, `ORDER BY` and `WITHIN GROUP` in aggregates, `CURRENT_DATE`, `CURRENT_USER` and the other SQL value functions except `CURRENT_TIMESTAMP` and `LOCALTIMESTAMP`, writes to an array element or a field (`SET tags[1] = ...`), text compared with a number, except a string literal or parameter that reads as an integer, which takes the number's type, and a parameter holding text or an integer compared with text, which takes the text type, a number compared with a boolean or a time, a cast of a `numeric` or a float to text and `||` of one, `round` of a value ending in .5 and a cast of one to an integer when the statement does not show whether it is a `numeric` or a float, a set operation whose column holds text in one query and a number in the other, text and a number among the branches of `CASE`, `COALESCE`, `GREATEST` or `LEAST`, a value shorter than its `char(n)` column, a string literal or parameter compared with a timestamp, number text written as `0x10`, `0x1p2` or `1_000`, locking reads over a view, a subquery or a `LATERAL` item, a write to the columns of a `DEFERRABLE` primary key or unique constraint, a value other than `DEFAULT` for a `GENERATED ALWAYS` identity column without `OVERRIDING SYSTEM VALUE`, `nextval` of a sequence with `CACHE` above 1, `ON CONFLICT ... WHERE` with a predicate other than the partial index's own, `COPY`, system catalogs, and `BEGIN` or `COMMIT` sent as SQL (use `database/sql`'s transactions). Such statements fail with `detest.ErrUnsupportedSQL` rather than being approximated, and `detest.CheckSQL` tells whether detest can run a statement, for the cases the statement decides on its own; a case that depends on the schema, such as a generated column detest cannot compute, fails when the statement runs.

A `numeric` value is kept as a float. It reads back without the trailing zeros it was written with (`1.50` as `1.5`), and one with more digits than a float keeps, such as an integer beyond 2^53, fails with `detest.ErrUnsupportedSQL`.

A `numeric(p, s)` column rounds what it stores to `s` places, half away from zero, and refuses a value of more than `p - s` digits before the point (22003). A `varchar(n)` or `char(n)` column refuses a longer value (22001), after dropping the spaces past `n` as Postgres does.

**MySQL**

`mysql.New()` parses with the MySQL grammar of TiDB's parser and gives InnoDB's semantics at Repeatable Read (the default), Read Committed and Serializable (`mysql.Isolation`). The statements and constraints above carry over, written in MySQL's syntax, with these differences.

- Shared and exclusive row locks only (`FOR SHARE`, `LOCK IN SHARE MODE`, `FOR UPDATE`), a shared lock on the parent row for a foreign key check, and `SET innodb_lock_wait_timeout` for lock wait timeouts
- At Repeatable Read, plain reads come from a snapshot taken at the transaction's first read, while locking reads, `UPDATE` and `DELETE` read the latest rows. A value read from the snapshot and written back loses a concurrent update, as in MySQL
- At Repeatable Read and Serializable, locking reads, `UPDATE` and `DELETE` take next-key locks along the index they search by, so an insert into a locked gap waits, and two transactions that lock the same gap and then insert into it deadlock (1213). Without a usable index, the whole table is locked, as a full scan locks it. Over a join, the first table's search takes next-key locks and the joined tables lock the rows the join read, as the locks InnoDB takes around each lookup into them depend on the join plan
- At Serializable, plain reads in a transaction take shared locks
- A failed statement rolls back only itself. A deadlock rolls back the whole transaction
- `AUTO_INCREMENT` with `LastInsertId` and `LAST_INSERT_ID()`, `INSERT IGNORE` (which skips rows with a duplicate key, and refuses as unsupported a row whose other error MySQL would turn into a warning), `ON DUPLICATE KEY UPDATE` with `VALUES(col)` or a row alias, `UPDATE` and `DELETE` with `ORDER BY` and `LIMIT`, `UPDATE` of the first table joined with others, `ON UPDATE CURRENT_TIMESTAMP`, `IF`, `IFNULL` and `<=>`
- `NULL` sorts first when ascending, and the integer types are checked against their ranges, unsigned ones too, except that `BIGINT UNSIGNED` stops at 2^63 - 1, refusing larger values as out of range. `CHAR(n)` and `VARCHAR(n)` lengths and `ENUM` and `SET` members are checked as strict mode does, and `DATE`, `DATETIME` and `TIMESTAMP` columns hold a `time.Time` (UTC) whether written as a time or as a string
- A string compared with a number is compared as the number it converts to
- Strings compare exactly, as a `_bin` collation compares them. A text column of a case-insensitive (`_ci`) collation, which MySQL 8 gives a column that declares none (`utf8mb4_0900_ai_ci`), loads with the schema, but a statement whose outcome depends on how it compares fails with `detest.ErrUnsupportedSQL`: a comparison, `LIKE`, `IN` or a join on it, `ORDER BY`, `GROUP BY`, `DISTINCT`, `UNION` or a window over it, `MIN` and `MAX` of it, a write that a unique index or a foreign key over it checks, and a `LIMIT` scan along a primary key that holds it. Reading and writing its values as they are runs. Declare `COLLATE utf8mb4_bin` on the column or the table, or pass `mysql.Collation("utf8mb4_bin")` when the server's default collation is a `_bin` one
- Migrations, `mysqldump` output and `SHOW CREATE TABLE` output, which `ddl.From` writes for MySQL, run as they are. `mysql.Database` sets the current database
- Errors carry MySQL's error number and SQLSTATE, and `mysqlerr.Convert` turns them into go-sql-driver's `*mysql.MySQLError`, which GORM's `TranslateError` reads

``` go
s.DB("app", mysql.New(mysql.Errors(mysqlerr.Convert))) // *mysql.MySQLError
```

For MySQL, these are not supported besides the above. `REPLACE`, an `UPDATE` that sets columns of a joined table, multi-table `DELETE`, a locking read, `UPDATE` or `DELETE` that searches by a prefix index (`KEY (name(3))`), or that more than one index serves without a unique point lookup among them (MySQL's optimizer picks one by its statistics), or that a descending index serves, or with `OR` (write it as `IN`), `<>`, `!=` or `NOT IN` on an indexed column, at Repeatable Read or Serializable, descending primary keys and unique indexes, a `sql_mode` without strict mode (other than the `NO_AUTO_VALUE_ON_ZERO` a dump sets), a `time_zone` other than UTC (`'+00:00'` as a dump sets it), `RETURNING`, temporal strings in formats other than `YYYY-MM-DD[ HH:MM:SS[.ffffff]]`, values of `TIME` columns, generated columns, and exact DECIMAL arithmetic (DECIMAL values are kept as float64, so sums like 0.1 + 0.2 are approximate).

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

The search space grows quickly. `detest.MaxPreemptions`, `detest.MaxFailures`, `detest.MaxRedeliveries`, `detest.MaxRuns` and `detest.MaxDuration` bound it, and `detest.Workers` explores it in parallel. `detest.MaxCrashes` lets processes crash at any step (their transactions roll back, their mutexes are freed, their messages are redelivered), to check that work survives a process dying halfway.

| Variable | Effect |
| --- | --- |
| `DETEST_REPLAY` | Replay one run, given the choices printed with a violation |
| `DETEST_WORKERS` | Number of workers, overriding `detest.Workers` |
| `DETEST_SHARD` | `index/total[/depth]`, explore one shard of the space (for splitting across CI jobs) |
| `DETEST_CHECKPOINT` | A file to save the unexplored part to when `MaxRuns` or `MaxDuration` is reached, and to resume from on the next run |
| `DETEST_STALL` | How long a process may block outside the scheduler before it is reported (default `30s`) |
| `DETEST_DEBUG` | Print scheduler events to stderr |

Exploration allocates a lot. Raising `GOGC` (for example `GOGC=400`) often shortens long explorations.

## Requirements

Go 1.26 or later.

## License

[MIT](LICENSE)
