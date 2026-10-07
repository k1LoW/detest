# detest

In-Process Deterministic Simulation Testing for Go Applications, based on Stateless Model Checking (SMC)

*Because we **detest** race conditions, deadlocks, and flaky tests.*

---

`detest` is a lightweight, in-process testing framework that brings **Deterministic Simulation Testing (DST)** to standard Go applications using **Stateless Model Checking (SMC)**.

Instead of virtualizing OS threads or low-level network packets, `detest` operates at Go's standard client boundaries (`database/sql`, `http.RoundTripper`) and at mutexes injected in place of `sync.Mutex`, running goroutines inside a [`testing/synctest`](https://pkg.go.dev/testing/synctest) bubble. It injects failure scenarios and systematically explores every execution interleaving within configurable bounds, without modifying your production business logic or ORMs.

## Getting started

The recommended way to use detest is through its agent skill, [`skills/detest`](skills/detest). detest is meant to be applied by an AI agent that knows the code under test, and the skill lets you use it without learning it. You decide to use detest, and the agent proposes where to look, wires your real code into a simulation, runs the exploration, and reports what was run, what the result guarantees and what it leaves out. It stops for your decision on the target, on the scenario and its invariants, on adding the dependency, and on the conditions of the run. The tests it writes carry the `detest` build tag, so the everyday `go test ./...` does not run them.

### Installing the skill

Install it with the GitHub CLI (`gh skill` is in preview).

``` console
$ gh skill install k1LoW/detest detest
```

The command asks which agent to install it for, and whether into the current repository or for your user. `gh skill update` brings an installed skill up to date.

[`skills`](https://github.com/vercel-labs/skills) installs it as well, through npx.

``` console
$ npx skills add k1LoW/detest --skill detest
```

`npx skills update` brings it up to date.

### Using it

Ask the agent to use detest, for example "use detest to check whether two concurrent requests can oversell in `Reserve`", or say yes when it suggests detest for a concurrency problem. It does not start on its own.

The rest of this README describes the API the skill writes tests with, for reading those tests or writing them by hand.

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
            4  bob#2    shop: begin   (stock_test.go:15)
            5  bob#2    shop: select stock where sku = apple   (stock_test.go:21)
            6  alice#1  shop: commit   (stock_test.go:30)
            7  alice#1  done
            8  bob#2    shop: update stock set {n=0} where sku = apple   (stock_test.go:27)
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
| `s.Always(fn)`, `s.AtQuiescence(fn)` | Invariants checked after every commit and every process completion, or once every process is done |
| `s.Sometimes(name, fn)` | A condition at least one run must meet, so that an exploration that never gets near the bug fails |

## Simulated resources

Processes interact only through simulated resources. Every operation on them is a scheduling point, and each one models how its real counterpart behaves under concurrency.

The simulations replace what the application talks to at the client boundary, the level of its client libraries, rather than system calls, packets or disks. The code above that boundary, business logic, ORMs and generated clients included, runs unchanged. A race reaches detest only through what crosses one of these boundaries, which are a PostgreSQL or MySQL database through `database/sql`, a call to another service through an `http.RoundTripper` or `ext.Do`, a queue declared with `s.Queue`, and a mutex injected in place of `sync.Mutex`. Shared state outside them is not simulated, and races through it are not explored.

- A database reached without `database/sql`, such as through pgx's native `pgxpool.Pool` or `pgx.Conn`, or through a vendor SDK
- A store detest has no simulation of, such as SQLite, MongoDB, DynamoDB, Spanner, or Redis holding locks or counters. A key-value store the code reaches through an interface of a few commands can be faked on `ext.Do`, as [examples/kvlock](examples/kvlock) does for GET, SET, SET NX and DEL with a TTL
- Go memory shared between goroutines without one of the mutexes above, which `go test -race` checks

Goroutines the code under test starts itself, such as an `errgroup`'s or the one a worker hands a claimed job to, are scheduled too. Each becomes a process of its own the first time it calls into one of these boundaries, named after the process that started it (`worker#1.1`), and a crash takes it down with that process. A goroutine running a statement on a `*sql.Tx` its process began is the exception. database/sql holds the transaction's locks while the statement runs, so the statement runs at once, in that transaction and without a yield point, and its interleaving with other processes is not explored. The report counts such statements. One that would wait for a lock another transaction holds fails with `detest.ErrUnsupportedSQL`, and a commit or a rollback from such a goroutine stops the exploration, as database/sql ends the transaction before detest hears of it, and which of the other goroutines' statements ran in it is the runtime's choice. Goroutines that run in the same step run in the order database/sql hands them the connection, which a replay does not repeat, so their statements must commute. The exploration stops when two of them could depend on their order, unless each touches rows of its own by primary key in a table without foreign keys, as an outbox poller deleting the rows it locked does. Goroutines the code orders itself within one step, such as by a channel or by one WaitGroup after another, are compared all the same, as detest cannot see that order, and may stop the exploration although they never race. A goroutine whose statement reaches the driver after its context ended, such as when a sibling cancels a context they share, stops the exploration too. One that `database/sql` turns away before the driver, having seen the context end first, cannot be told apart from one that never ran, so code that cancels a context while goroutines sharing a transaction run statements under it is not checked for this, whether `errgroup.WithContext` cancels it on the first error or the code calls a cancel function of its own.

Code on GORM can have such goroutines scheduled as well, by giving GORM the connection from `github.com/k1LoW/detest/db/seam/gormpool` in the test, `gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: gormpool.New(sqlDB)}), cfg)`, with no change to production code. Each operation of a GORM transaction then waits for the transaction's connection before it enters database/sql, where detest can park it. A goroutine sharing the transaction is adopted like any other, its statements yield and wait for locks, a deadlock through it is detected, and the order of the goroutines is explored rather than compared. A commit or a rollback by such a goroutine still stops the exploration, and GORM's `PrepareStmt` must stay off, as a prepared statement runs without the pool. Code on sqlc or sqlx gets the same through `db/seam/sqlcdbtx` or `db/seam/sqlxext` when it begins its transactions through something the test can replace, such as an interface or a function it is given, and hands the goroutines the transaction as an interface, sqlc's `DBTX` with the queries built by `New(tx)` rather than `WithTx(tx)`, or sqlx's `ExtContext`. The test then begins the transactions with `sqlcdbtx.New(sqlDB).BeginTx` or `sqlxext.New(sqlDB, "postgres").BeginTxx`. `sqlxext` refuses a mapper other than sqlx's default and unsafe mode, which sqlx's named functions would not see on its transaction. Code that begins them on the `*sql.DB` or `*sqlx.DB` itself and hands the goroutines the `*sql.Tx` or `*sqlx.Tx`, as `WithTx` does, leaves no place for such a seam, and the rules above hold for it, as for plain database/sql.

A statement whose context ends while it waits for a lock, or stands at its yield point, fails with `detest.ErrUnsupportedSQL` rather than the context's error. pgx and go-sql-driver only close the connection, and the server goes on running the statement. A canceled autocommit `UPDATE` is applied once the row is free, on Postgres and on MySQL, and a MySQL transaction keeps its locks until then. A transaction whose context ends while it is idle is rolled back as `database/sql` rolls it back, and a statement whose context ended before it reached the driver returns the context's error.

### Database

`s.DB(name, server)` returns a `*sql.DB` backed by an in-memory driver. Production code, including GORM, sqlx and sqlc, runs on it unchanged. GORM whose transactions goroutines share is given it through `gormpool`, and sqlc and sqlx code that begins its transactions through an interface through `sqlcdbtx` and `sqlxext`, as described above.

Each kind of server has its own package, which parses its SQL dialect with the server's real grammar and gives the locking, constraints and errors of that server, since they differ between servers. `postgres.New()` is PostgreSQL at Read Committed, which the lists below describe up to **MySQL**. `mysql.New()` is MySQL with InnoDB, which that section describes by its differences from PostgreSQL.

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
- Column types `uuid`, `smallint` and `integer` are checked on write, and a `uuid` is stored in the form Postgres prints it in, lower case with hyphens, however it was written, and a string literal or parameter compared with one is read as a `uuid` too

**Statements**

- `SELECT` with inner, left and cross joins, `LATERAL`, subqueries, non-recursive CTEs, `GROUP BY` and `HAVING`, aggregates (`count`, `sum`, `min`, `max`, `avg`), window functions (`row_number`, `rank`, `dense_rank`, `lag`, `lead`, `first_value`, `last_value` and the aggregates) over the default frame or the whole partition, `DISTINCT`, `DISTINCT ON`, `UNION`, `INTERSECT`, `EXCEPT`, `VALUES`, `ORDER BY` with `NULLS FIRST | LAST`, `LIMIT` and `OFFSET`
- `INSERT` with `ON CONFLICT DO NOTHING | DO UPDATE` and `RETURNING`, `INSERT ... SELECT`
- `UPDATE ... FROM` and `DELETE ... USING`, with `RETURNING`, and `WITH` on both, as in `WITH c AS (SELECT id FROM jobs ... LIMIT 1 FOR UPDATE SKIP LOCKED) UPDATE jobs ... FROM c`. A CTE runs once, when the statement first reads it, so one the statement does not read, or reads only where no row gets, does not run
- Functions: `coalesce`, `nullif`, `greatest`, `least`, `lower`, `upper`, `length`, `char_length`, `octet_length`, `left`, `concat`, `abs`, `ceil`, `ceiling`, `floor`, `round`, `power`, `pow`, `random`, `now`, `clock_timestamp`, `statement_timestamp`, `transaction_timestamp`, `localtimestamp`, `make_interval`, `date_trunc` of a timestamp, a date or an interval, `extract` and `date_part` of a timestamp, a date or an interval, `nextval`, `setval`, `gen_random_uuid`, `uuid_generate_v4`, the transaction advisory locks `pg_advisory_xact_lock` and `pg_try_advisory_xact_lock`, and `hashtext` as the key of one of them, where only its equality matters
- That is the whole list of functions. Any other fails with `detest.ErrUnsupportedSQL` where a statement evaluates it, and under `detest.CheckSQL` wherever it stands in a `SELECT`, `INSERT`, `UPDATE` or `DELETE`. An expression in a `CHECK` or a generated column loads with the schema and is refused by the write that evaluates it. A default detest cannot evaluate loads too, and a write that leaves the column out stores the `Unknown` marker, unless the table's key, a constraint, a generated column or a checked type reads the column, in which case the write is refused
- `CURRENT_TIMESTAMP`, `LOCALTIMESTAMP` and `CURRENT_DATE`, and `generate_series` over integers in `FROM`
- Dates and times are computed in the session's `TimeZone`, UTC unless the server is given another with `postgres.TimeZone("Asia/Tokyo")`, as `postgresql.conf` sets it: `CURRENT_DATE`, `LOCALTIMESTAMP`, the date, the fields and `date_trunc` of a `timestamptz`, the days and months added to one, and a `timestamp`, a `date` or text without a zone read as a `timestamptz`, which keeps its instant apart from a `timestamp`'s clock. A clock time a zone skips or shows twice at a daylight saving change is read as Postgres reads it, with the offset before the change and after it. `SET TIME ZONE` to an IANA time zone name or to an offset in hours runs, for the session or with `LOCAL` for the transaction, and `ROLLBACK`, also to a savepoint, undoes it as in Postgres. A POSIX zone (`'UTC+9'`, `'+09:00'`), whose sign Postgres reverses, an abbreviation, an interval and a name `time.LoadLocation` does not read as written, such as `'asia/tokyo'`, are refused. A `date` column holds the date it is written as, and compares with a timestamp as midnight of that date. A `time.Time` written to or compared with a `date` or a `timestamp` takes its own date and clock, whatever its zone, as pgx and lib/pq send it, and one for a `timestamptz` keeps its instant. The transaction API and `State` read a `timestamptz` as a `time.Time`
- Operators: the comparisons, `AND`, `OR`, `NOT`, `+ - * / %`, `||` of text, `LIKE`, `ILIKE`, `IN`, `BETWEEN`, `IS NULL`, `EXISTS`, `CASE`, `= ANY (ARRAY[...])` and `<> ALL (ARRAY[...])` over constants and parameters, `= ANY` and `<> ALL` over an array bound to a parameter or written as array text (`id = ANY($1)`, `id = ANY($1::bigint[])`, `id = ANY('{1,2}')`), and casts to the integer types, `numeric`, the float types, `text`, `varchar`, `char`, `boolean`, `uuid`, `date` and `interval`. An interval is read from text such as `'2 days 3 hours'`, `'90 min'`, `'1:30'`, `'1 week ago'` or `'1 year 2 months'`, in microseconds to millennia, and malformed text fails with 22007. It holds months, days and microseconds apart, as Postgres does: adding a month keeps the day of the month unless the month is shorter, two intervals compare by their length with a month as 30 days and a day as 24 hours, and an interval reads back as Postgres writes it (`1 mon 15 days`). A cast of text to `timestamp`, or to `timestamptz` with a zone, reads the time. A cast to another type leaves the value as it is
- An array parameter is bound as a Go slice, which pgx's `stdlib` driver takes, or as the array text `pq.Array` sends. Each element is compared with `x` as `x = $1` compares a parameter holding it, and the array is read before the statement runs, so a malformed one fails with 22P02 on an empty table too
- Expressions follow SQL's three-valued logic
- Column references are resolved against the schema before a statement runs, so a column none of the tables in scope has fails with 42703 as on the server. A name is let through when an item in scope has columns detest does not know, such as a function or a table no schema declared

**Schema**

- `CREATE`, `ALTER` and `DROP` of tables, columns, constraints, indexes, views and materialized views, `CREATE TABLE AS`, `REFRESH MATERIALIZED VIEW`, renames
- Defaults, serial and identity columns, sequences with their `START`, `INCREMENT` and `RESTART`, with values that repeat from run to run
- Stored generated columns, computed on every write. A Postgres 18 virtual column loads through `ddl.From`, which writes it as stored, but not from a Postgres 18 dump, whose `GENERATED ALWAYS AS (...)` without `STORED` the grammar detest parses with rejects
- Schemas and `search_path` (`postgres.SearchPath`), and the server's `TimeZone` (`postgres.TimeZone`)
- Migrations and `pg_dump --schema-only` output run as they are, in a transaction or not, apart from the DDL forms listed under **Not supported**. Statements that declare nothing detest needs, such as functions, grants and comments, are accepted and ignored
- `ddl.From` reads the tables of a live database and writes DDL that detest accepts
- `store.Ignore(...)` takes tables the invariants do not look at, such as an audit log, out of the simulation. Writes to them are dropped and are no scheduling points, which keeps the exploration small

**Errors**

Errors are `*detest.DBError` with the server's SQLSTATE (and, for MySQL, its error number), the table, the column and the constraint, and match `detest.ErrUniqueViolation` and the other sentinels with `errors.Is`. Code that branches on its driver's error type gets that type by converting.

``` go
s.DB("app", postgres.New(postgres.Errors(pgxerr.Convert))) // *pgconn.PgError
s.DB("app", postgres.New(postgres.Errors(pqerr.Convert)))  // *pq.Error
```

**Not supported**

Statements of these forms fail with `detest.ErrUnsupportedSQL` rather than being approximated. The list is by what a statement does, so that the SQL of a flow can be checked against it before a simulation is written.

*Isolation*

- Isolation levels other than Read Committed, asked for in `BeginTx` or set as the session default by SQL (`SET SESSION CHARACTERISTICS AS TRANSACTION`, `default_transaction_isolation`). `READ UNCOMMITTED` as the session default runs, as Postgres runs it as Read Committed; `sql.LevelReadUncommitted` in `BeginTx` is refused
- `SET TRANSACTION` in any form, `SET TRANSACTION SNAPSHOT`, and the `transaction_isolation` and `transaction_read_only` settings in any form (`SET`, `RESET`, `TO DEFAULT`, `FROM CURRENT`), as for MySQL. Postgres fails them once the transaction has run a query, which detest does not track, and the level is `BeginTx`'s to set
- A read-only session default (`SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY`, `default_transaction_read_only`), under which the server fails every write. `TxOptions.ReadOnly` in `BeginTx` is accepted and ignored, as before

*Query forms*

- Recursive CTEs, a CTE that writes (`WITH d AS (DELETE ... RETURNING ...)`), `WITH` on `INSERT ... VALUES`, a CTE of `UPDATE` or `DELETE` named as the table it writes, a CTE of `UPDATE` or `DELETE` that locks rows or calls a function with effects (`FOR UPDATE`, `nextval`), unless it is the only item of `FROM` or `USING`, reads the target table alone and the `WHERE` only equates the target's columns with its own, as `WITH c AS (SELECT id FROM jobs ... FOR UPDATE SKIP LOCKED) UPDATE jobs ... FROM c WHERE jobs.id = c.id` does. Postgres runs such a CTE only as far as the statement asks for its rows, which in any other shape depends on its plan, and `RETURNING *` of an `UPDATE ... FROM` or a `DELETE ... USING`. `WITH` on `SELECT`, `INSERT ... SELECT`, `UPDATE` and `DELETE` runs
- Column alias lists on a table, a CTE or a view (`FROM t AS x(a, b)`, `WITH c(a) AS (...)`)
- `RIGHT` and `FULL` joins, `JOIN ... USING`, `NATURAL JOIN`, `TABLESAMPLE`, `SELECT INTO`, `INSERT ... DEFAULT VALUES`
- Set-returning functions other than `generate_series` in `FROM`, such as `unnest`, and any set-returning function in the select list, `generate_series` included
- `ANY (subquery)` with an operator other than `=`, row comparisons of different shapes
- Window frames other than the default and the whole partition, window functions other than the seven listed and the aggregates, and a window call with other arguments than the function takes
- Aggregates other than `count`, `sum`, `min`, `max` and `avg`, such as `array_agg`, `string_agg`, `bool_or`, `every` and `json_agg`, and `FILTER`, `ORDER BY` and `WITHIN GROUP` in aggregates

*Functions and operators*

- Any function not in the list above, and a call of one with other arguments than it takes, such as `make_interval` with an argument other than `secs => n`. Among the ones applications write: the date and time functions (`to_char`, `age`, `to_timestamp`, `make_date`, `date_trunc` with a time zone), the string functions (`trim`, `replace`, `substring`, `strpos`, `position`, `split_part`, `starts_with`, `md5`), the math functions beyond the list (`mod`, `sqrt`, `trunc`, `sign`), `current_setting`, `set_config` other than the bare `SELECT set_config('search_path', ...)` of a dump described under **Statements**, `currval` and `lastval`, the session advisory locks (`pg_advisory_lock`), `pg_notify`, `pg_sleep`, `version`, `hashtext` anywhere but as an advisory lock's key, since detest does not compute Postgres's hash, and a function of another schema (`app.f()`)
- `CURRENT_TIME`, `CURRENT_USER` and the other SQL value functions, except `CURRENT_TIMESTAMP`, `LOCALTIMESTAMP` and `CURRENT_DATE`
- `IS DISTINCT FROM`, `IS TRUE` and `IS FALSE`, the regular expression operators (`~`, `~*`, `SIMILAR TO`), `LIKE ... ESCAPE`, and `||` of a `numeric`, a float, a timestamp or an interval
- Arrays, apart from `= ANY` and `<> ALL` over an `ARRAY[...]` of constants and parameters, or over a parameter or array text: `ARRAY[...]` written or read as a value, an array parameter anywhere else (`VALUES ($1)` with a slice), `= ANY` over a column or a function result (`'x' = ANY(tags)`), a multidimensional array or array text with dimension information (`'[0:1]={1,2}'`), an array cast to a type other than `smallint`, `integer`, `bigint`, `text`, `varchar` or `uuid` (`$1::numeric[]`), `= ANY` with neither side typed (`$1 = ANY($2)`), an array cast to a type x is not of, among the integers, text and `varchar`, and `uuid` (`name = ANY($1::bigint[])`), and a parameter used as the array of `= ANY` and as a scalar, or against operands of other types, the array operators (`@>`, `<@`, `&&`), the array functions (`array_append`, `cardinality`, `array_length`), and writes to an array element or a field (`SET tags[1] = ...`)
- JSON: the operators (`->`, `->>`, `@>`, `?`) and the functions (`jsonb_set`, `jsonb_build_object`, `row_to_json`)
- Interval text in ISO 8601 (`'P1D'`), with a fraction of a microsecond, or with a fraction of a year that is not a whole number of months (`'1.3 years'`), which Postgres rounds. `date_trunc` of an interval to `week`, and `extract` of the units of a calendar position (`dow`, `doy`), which Postgres refuses for an interval. A `time.Duration` bound for or compared with an interval, which reaches detest as an integer. A write to an interval column declared with a precision or fields (`interval(0)`, `interval day`), and a cast to `interval`, `timestamp` or `timestamptz` with them (`::timestamp(0)`, `interval '1 day' DAY`), which Postgres rounds or cuts, and `sum` and `avg` of intervals. A string literal, a parameter or a value of another type beside an interval among the branches of `CASE`, `COALESCE`, `GREATEST`, `LEAST`, `NULLIF` or the value and default of `LAG` and `LEAD`, which Postgres converts to an interval or refuses, and, as its type may be an interval, a column of a `SELECT *` subquery, a set operation or a set-returning function in `FROM` beside such a value. `date - date` and a date plus or minus an integer, which give days. `extract` of a time-of-day unit from a `date`, which Postgres refuses, and the `julian` and `timezone` units

*Comparisons and conversions*

- Text compared with a number. Two cases run, a string literal or parameter that reads as an integer, which takes the number's type, and a parameter holding text or an integer compared with text, which takes the text type
- A number, or text other than a string literal or parameter, compared with a boolean, a time or a `uuid`, a parameter holding a boolean or a time compared with a value of another of these types, and a parameter compared with operands of different types (`id = $1 OR name = $1`)
- A string literal or parameter compared with a `timestamp` or a `date` (`created_on >= '2024-01-01'`), which Postgres reads as the one or the other and a value does not tell apart. Pass the time as a `time.Time` parameter, or cast the text (`'2024-01-01'::date`). Text compared with a `timestamptz` is read as one, in the session's `TimeZone` when it has no zone
- A cast of a `numeric` or a float to text, and `||` of one
- `round` of a value ending in .5, and a cast of one to an integer, when the statement does not show whether it is a `numeric` or a float
- Text and a number in the same column of a set operation, or among the branches of `CASE`, `COALESCE`, `GREATEST` or `LEAST`
- Values written to a column that Postgres converts by rules detest does not model, namely a value shorter than its `char(n)` column, a cast to `char(n)` or `char`, which pads text with spaces it compares without, text written to a timestamp column in a form other than `YYYY-MM-DD[( |T)HH:MM[:SS[.ffffff]]][Z|±hh[:mm]]`, an integer written to a boolean column, and number text written as `0x10`, `0x1p2` or `1_000`

*Locking*

- Locking reads over a view, a subquery or a `LATERAL` item, over the nullable side of an outer join, in a set operation or with `GROUP BY`, `DISTINCT` or a window function, and more than one locking clause
- `LOCK TABLE`

*Writes*

- A write to the columns of a `DEFERRABLE` primary key or unique constraint, a value other than `DEFAULT` for a `GENERATED ALWAYS` identity column without `OVERRIDING SYSTEM VALUE`, `OVERRIDING USER VALUE`, `nextval` of a sequence with `CACHE` above 1, and `ON CONFLICT ... WHERE` with a predicate other than the partial index's own
- `TRUNCATE` (MySQL runs it outside a transaction) and `COPY`

*Schema*

- `ALTER COLUMN ... TYPE ... USING`, `CREATE TABLE ... LIKE`, `ADD CONSTRAINT ... USING INDEX`, `CREATE TEMPORARY TABLE`, and adding a generated column to a table that holds rows
- The system catalogs and `information_schema`, which are not there, so a query of them fails as of an undefined table

*Statements*

- `BEGIN`, `COMMIT` and `ROLLBACK` sent as SQL (use `database/sql`'s transactions), `PREPARE`, `DEALLOCATE`, `DISCARD`, `LISTEN`, `NOTIFY`, `EXPLAIN`, `VACUUM`, `ANALYZE`, `SHOW`
- On a `*sql.Tx` used by another goroutine than the one that began it, such as an `errgroup` running statements on its parent's transaction, a statement that would wait for a lock, and a commit (only the commit through `gormpool`, `sqlcdbtx` or `sqlxext`)
- `SET` of the settings listed under **Isolation**, and `SET search_path` to a path other than the one `postgres.SearchPath` declares, since detest resolves every name on the declared path. `SET search_path` to the declared path, as a migration writes it, and to an empty path, as `pg_dump` output does, runs, and so does the `set_config('search_path', ...)` a dump writes, under the same rule; any other call of `set_config` is a function detest does not run. `SET LOCAL lock_timeout` is acted on, and `RESET ALL` clears it, and `SET` of any other setting, such as `statement_timeout`, `application_name` or a custom setting, is accepted and ignored. `SET TIME ZONE` is described with the dates above. `current_setting` reading one back is refused

`detest.CheckSQL` tells whether detest can run a statement, for the cases the statement decides on its own, and refuses a function or an operator it does not know, or a call with other arguments than the function takes, wherever it stands in a `SELECT`, `INSERT`, `UPDATE` or `DELETE`, where a run reaches only the expressions it evaluates. A case that depends on the schema or on the values, such as a generated column detest cannot compute or a comparison decided by the column's type, passes `CheckSQL` and fails when the statement runs, and so does an expression in a `CHECK` or a generated column, which the write that evaluates it refuses. A default detest cannot evaluate passes both, and a write that leaves the column out stores the `Unknown` marker unless the table's own schema reads the column. The statements refused in a run are listed after the exploration's report, as an application that drops the error hides them.

A `numeric` value is kept as a float. It reads back without the trailing zeros it was written with (`1.50` as `1.5`), and one with more digits than a float keeps, such as an integer beyond 2^53, fails with `detest.ErrUnsupportedSQL`.

Arithmetic on numerics is exact where the statement shows that its operands are numerics, as literals and casts do, so `0.1 + 0.2 = 0.3` holds. A column's value does not show whether the column is a `numeric` or a float, which the two compute differently, so a result such as `amount + 0.2` is kept as the range of values the server may give. A comparison, more arithmetic, or a write to a `numeric(p, s)` column goes on when every value in the range gives the same outcome, and any other use of it, such as reading it back, fails with `detest.ErrUnsupportedSQL`. Sums and averages are kept the same way.

A `numeric(p, s)` column rounds what it stores to `s` places, half away from zero, and refuses a value of more than `p - s` digits before the point (22003). A `varchar(n)` or `char(n)` column refuses a longer value (22001), after dropping the spaces past `n` as Postgres does, and a cast to `varchar(n)` cuts it to `n` characters.

**MySQL**

`mysql.New()` parses with the MySQL grammar of TiDB's parser and gives InnoDB's semantics at Repeatable Read (the default), Read Committed and Serializable (`mysql.Isolation`). The statements and constraints above carry over, written in MySQL's syntax, with these differences.

- Shared and exclusive row locks only (`FOR SHARE`, `LOCK IN SHARE MODE`, `FOR UPDATE`), and `SET innodb_lock_wait_timeout` for lock wait timeouts
- A foreign key check takes a shared lock on the parent. When the referenced key is a unique secondary index, the lock is on that index's record, as a duplicate key check on such an index takes it
- At Repeatable Read, plain reads come from a snapshot taken at the transaction's first read, while locking reads, `UPDATE` and `DELETE` read the latest rows. A value read from the snapshot and written back loses a concurrent update, as in MySQL
- At Repeatable Read and Serializable, locking reads, `UPDATE` and `DELETE` take next-key locks along the index they search by. An insert into a locked gap waits, and two transactions that lock the same gap and then insert into it deadlock (1213). Without a usable index, the whole table is locked, as a full scan locks it
- A locking search meets the rows other transactions inserted and have not committed, and waits for them
- A locking read joining two tables, the first looked up by a unique key, locks the joined table's search for each row found, as MySQL's nested loop runs it. A locking join of another shape fails with `detest.ErrUnsupportedSQL`, because which table MySQL reads first is its optimizer's choice
- At Serializable, plain reads in a transaction take shared locks
- A failed statement rolls back only itself, and keeps its locks. The rows it inserted, and those a rollback to a savepoint undoes, leave gap locks where they were, so an insert into such a gap waits for the transaction
- A deadlock rolls back the whole transaction of the victim InnoDB picks. That is the lighter one by the undo records it wrote and the lock structs it holds, counting its waits and the implicit locks turned explicit, and of two equal ones the one whose wait closed the cycle. detest counts a table's records as one index page, so the weights of transactions over tables that span many pages can differ from the server's
- `AUTO_INCREMENT` with `LastInsertId` and `LAST_INSERT_ID()`, `INSERT IGNORE` (which skips rows with a duplicate key, and refuses as unsupported a row whose other error MySQL would turn into a warning), `ON DUPLICATE KEY UPDATE` with `VALUES(col)` or a row alias, `UPDATE` and `DELETE` with `ORDER BY` and `LIMIT`, `UPDATE` of the first table joined with others, `ON UPDATE CURRENT_TIMESTAMP`, `IF`, `IFNULL` and `<=>`
- `NULL` sorts first when ascending, and the integer types are checked against their ranges, unsigned ones too, except that `BIGINT UNSIGNED` stops at 2^63 - 1, refusing larger values as out of range. `CHAR(n)` and `VARCHAR(n)` lengths and `ENUM` and `SET` members are checked as strict mode does, and `DATE`, `DATETIME` and `TIMESTAMP` columns hold a `time.Time` (UTC) whether written as a time or as a string
- `DECIMAL(p, s)` columns round what they store to s places, half away from zero, and refuse a value of more than p - s digits before the point (1264). `DECIMAL` without a precision is `DECIMAL(10, 0)`. Arithmetic on DECIMAL values is exact or kept as a range as described above for `numeric`, a literal with an exponent (`1e-1`) being a `DOUBLE`, and a quotient or an average that needs more than four places, which MySQL rounds by the scale of the dividend, is kept as a range
- A string compared with a number is compared as the number it converts to
- Strings compare exactly, as a `_bin` collation compares them. A text column of a case-insensitive (`_ci`) collation, which MySQL 8 gives a column that declares none (`utf8mb4_0900_ai_ci`), loads with the schema, but a statement whose outcome depends on how it compares fails with `detest.ErrUnsupportedSQL`: a comparison, `LIKE`, `IN` or a join on it, `ORDER BY`, `GROUP BY`, `DISTINCT`, `UNION` or a window over it, `MIN` and `MAX` of it, a write that a unique index or a foreign key over it checks, and a `LIMIT` scan along a primary key that holds it. Reading and writing its values as they are runs. Declare `COLLATE utf8mb4_bin` on the column or the table, or pass `mysql.Collation("utf8mb4_bin")` when the server's default collation is a `_bin` one
- Migrations, `mysqldump` output and `SHOW CREATE TABLE` output, which `ddl.From` writes for MySQL, run as they are. `mysql.Database` sets the current database
- Errors carry MySQL's error number and SQLSTATE, and `mysqlerr.Convert` turns them into go-sql-driver's `*mysql.MySQLError`, which GORM's `TranslateError` reads

``` go
s.DB("app", mysql.New(mysql.Errors(mysqlerr.Convert))) // *mysql.MySQLError
```

For MySQL, these fail with `detest.ErrUnsupportedSQL` besides the forms above.

- `REPLACE`, an `UPDATE` that sets columns of a joined table, multi-table `DELETE`, and `RETURNING`
- A locking read, `UPDATE` or `DELETE` whose index locks depend on MySQL's optimizer or on an index detest does not model. This applies where the search takes gap locks, at Repeatable Read and Serializable, or takes the record locks of a secondary index, at Read Committed, and covers a search
  - by a prefix index (`KEY (name(3))`) or a descending index
  - that more than one index serves without a unique point lookup among them, since MySQL's optimizer picks one by its statistics
  - with `OR` on an indexed column (write it as `IN`), or with `<>`, `!=` or `NOT IN` on one
- Descending primary keys and unique indexes
- A `sql_mode` without strict mode, other than the `NO_AUTO_VALUE_ON_ZERO` a dump sets, and a `time_zone` other than UTC (`'+00:00'` as a dump sets it)
- Temporal strings in formats other than `YYYY-MM-DD[ HH:MM:SS[.ffffff]]`, and values of `TIME` columns
- Generated columns
- Functions other than `IF`, `IFNULL`, `COALESCE`, `NULLIF`, `NOW`, `CURRENT_TIMESTAMP`, `LOCALTIMESTAMP`, `LOCALTIME`, `UUID`, `LENGTH`, `CHAR_LENGTH`, `CHARACTER_LENGTH`, `LOWER`, `LCASE`, `UPPER`, `UCASE`, `LEFT`, `CONCAT`, `GREATEST`, `LEAST`, `ABS`, `FLOOR`, `CEIL`, `CEILING`, `ROUND` with one argument, `POWER`, `POW` and `LAST_INSERT_ID()`, the aggregates and the window functions above. Among the refused: `INTERVAL` expressions (`NOW() - INTERVAL 1 DAY`, `DATE_SUB`, `ADDDATE`, `TIMESTAMPDIFF`), `DATE`, `UTC_TIMESTAMP`, `DATE_FORMAT`, `UNIX_TIMESTAMP`, `FROM_UNIXTIME`, `DATEDIFF`, the JSON functions and operators (`JSON_EXTRACT`, `->>`, `JSON_SET`), `GROUP_CONCAT`, `FIND_IN_SET`, `GET_LOCK`, `ROW_COUNT`, `FOUND_ROWS`, `UUID_TO_BIN` and `BIN_TO_UUID`
- `REGEXP` and `RLIKE`, `MATCH ... AGAINST`, `BINARY`, `CAST` to `UNSIGNED` or `DECIMAL`, the bit operators and `DIV`, `COLLATE` in an expression, user variables (`@x`), index hints, `WITH` on `UPDATE` or `DELETE`, `SET TRANSACTION ISOLATION LEVEL` and `SET autocommit = 0`, `SHOW`, `CREATE TEMPORARY TABLE`, and DDL or `TRUNCATE` inside a transaction

### Queue

`s.Queue(name)` is an at-least-once, unordered queue, and `s.OnMessage(name, q, fn)` its consumer.

- A handler that returns an error has its message redelivered, up to `detest.MaxRedeliveries` times
- `detest.Duplicates(n)` delivers up to n messages twice per run
- `detest.Losses(n)` loses up to n messages per run, to check that a backstop covers them
- `q.Enqueue(p, msg)` publishes at once, and `tx.Enqueue(q, msg)` on commit

### External service

`s.External(name)` is a call to another service. The explorer tries success, failure before the effect, and failure after it (the effect applied but the response lost), where the caller sees `detest.ErrUnavailable`.

- `ext.Transport(handler)` returns an `http.RoundTripper` serving an `http.Handler` in process, to give generated clients (connect, gRPC-Web, REST) the real handler of the other service
- `ext.Do(p, desc, fn)` wraps any call, such as a command of a fake key-value store (see [examples/kvlock](examples/kvlock))
- `detest.Failures(...)` chooses the failures to try, and `detest.ReadOnly()` drops failure after the effect for reads

### Mutex

`s.Mutex(name)` and `s.RWMutex(name)` replace `sync.Mutex` and `sync.RWMutex` in production code. Waiting for them is visible to the scheduler, so cycles that mix mutexes and row locks are reported. As with `sync.RWMutex`, a waiting writer keeps new readers out.

## Options and environment variables

The search space grows quickly. These options of `detest.Explore` bound it or change how it is explored.

| Option | Effect |
| --- | --- |
| `detest.MaxPreemptions(n)` | Bounds the context switches away from a runnable process per run (unbounded by default) |
| `detest.MaxFailures(n)` | Bounds the external-call failures per run (default 1) |
| `detest.MaxRedeliveries(n)` | Bounds how many times a queue redelivers a message whose handler returned an error (default 1) |
| `detest.MaxIdleTicks(n)` | Bounds a loop's idle ticks in a row with no progress in between. A run whose loop goes idle once more is cut, without the checks at quiescence (default 3) |
| `detest.MaxSpins(n)` | Bounds the steps one process takes without a commit or an enqueue in between, counted across the steps of others, while another could run. Past it, the process waits for another to take a step, as a busy-wait would let a real scheduler run the process it waits for. With nothing else able to run, it is a progress violation. Processes that give way to each other that many times with no change in between are kept out while any other process can run, and a progress violation once none can (default 100) |
| `detest.MaxRuns(n)` | Caps the runs of an exploration (default 200000) |
| `detest.MaxDuration(d)` | Stops starting runs once `d` has passed (unbounded by default) |
| `detest.Workers(n)` | Explores with n workers in parallel (default 1) |
| `detest.DepthFirst()`, `detest.Random(seed)`, `detest.Prioritized(seed, depth)` | Picks schedules depth first (the default), or from a seed until `MaxRuns` or `MaxDuration`. `Random` draws every choice uniformly. `Prioritized` follows [PCT](https://www.microsoft.com/en-us/research/wp-content/uploads/2016/02/asplos277-pct.pdf): it runs each process on and switches at `depth`-1 steps drawn at random, which reaches bugs deep in long runs that `Random` almost never does. A sampled exploration is never complete and does not take `DETEST_CHECKPOINT`. The one passed last applies |
| `detest.Shard(index, total, depth)` | Explores one shard of the schedule tree, for splitting across machines. Under `detest.Random` and `detest.Prioritized` it makes every `total`-th run instead, and `depth` is not used. `DETEST_SHARD` sets it from the environment |
| `detest.MaxCrashes(n)` | Lets up to n processes per run crash at any step (none by default). Their transactions roll back, their mutexes are freed and their messages are redelivered, to check that work survives a process dying halfway |
| `detest.MaxStalls(n, d)` | Lets up to n processes per run stall for `d` at any step (none by default). A stalled process sleeps on the fake clock while the others go on, and the clock reaches the end of the stall once none of them can run, to check that a lock's TTL or a lease running out under a process does not let another act alongside it |

These environment variables override or add to them.

| Variable | Effect |
| --- | --- |
| `DETEST_REPLAY` | Replay one run, given the choices printed with a violation |
| `DETEST_WORKERS` | Number of workers, overriding `detest.Workers` |
| `DETEST_MAX_RUNS` | Cap on runs, overriding `detest.MaxRuns` |
| `DETEST_MAX_DURATION` | Wall-clock cap such as `10m`, overriding `detest.MaxDuration` |
| `DETEST_SEED` | Seed of a test under `detest.Random` or `detest.Prioritized`, overriding the one it passes. A test under `detest.DepthFirst` is left as it is |
| `DETEST_SHARD` | `index/total[/depth]`, explore one shard of the space (for splitting across CI jobs) |
| `DETEST_CHECKPOINT` | A file to save the unexplored part to when `MaxRuns` or `MaxDuration` is reached, and to resume from on the next run |
| `DETEST_STALL` | How long a process may block outside the scheduler before it is reported (default `30s`) |
| `DETEST_DEBUG` | Print scheduler events to stderr |

Exploration allocates a lot. Raising `GOGC` (for example `GOGC=400`) often shortens long explorations.

## Requirements

Go 1.26 or later.

## License

[MIT](LICENSE)
