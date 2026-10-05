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

### Database

`s.DB(name, server)` returns a `*sql.DB` backed by an in-memory driver. Production code, including GORM, sqlx and sqlc, runs on it unchanged.

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
- Column types `uuid`, `smallint` and `integer` are checked on write

**Statements**

- `SELECT` with inner, left and cross joins, `LATERAL`, subqueries, non-recursive CTEs, `GROUP BY` and `HAVING`, aggregates (`count`, `sum`, `min`, `max`, `avg`), window functions (`row_number`, `rank`, `dense_rank`, `lag`, `lead`, `first_value`, `last_value` and the aggregates) over the default frame or the whole partition, `DISTINCT`, `DISTINCT ON`, `UNION`, `INTERSECT`, `EXCEPT`, `VALUES`, `ORDER BY` with `NULLS FIRST | LAST`, `LIMIT` and `OFFSET`
- `INSERT` with `ON CONFLICT DO NOTHING | DO UPDATE` and `RETURNING`, `INSERT ... SELECT`
- `UPDATE ... FROM` and `DELETE ... USING`, with `RETURNING`
- Functions: `coalesce`, `nullif`, `greatest`, `least`, `lower`, `upper`, `length`, `char_length`, `octet_length`, `left`, `concat`, `abs`, `ceil`, `ceiling`, `floor`, `round`, `power`, `pow`, `random`, `now`, `clock_timestamp`, `statement_timestamp`, `transaction_timestamp`, `make_interval`, `nextval`, `setval`, `gen_random_uuid`, `uuid_generate_v4`, `hashtext`, and the transaction advisory locks `pg_advisory_xact_lock` and `pg_try_advisory_xact_lock`
- That is the whole list of functions. Any other fails with `detest.ErrUnsupportedSQL` where a statement evaluates it, and under `detest.CheckSQL` wherever it stands in a `SELECT`, `INSERT`, `UPDATE` or `DELETE`. An expression in a default, a `CHECK` or a generated column loads with the schema and is refused by the write that reads it
- `CURRENT_TIMESTAMP` and `LOCALTIMESTAMP`, and `generate_series` over integers in `FROM`
- Operators: the comparisons, `AND`, `OR`, `NOT`, `+ - * / %`, `||` of text, `LIKE`, `ILIKE`, `IN`, `BETWEEN`, `IS NULL`, `EXISTS`, `CASE`, `= ANY (ARRAY[...])` and `<> ALL (ARRAY[...])` over constants and parameters, and casts to the integer types, `numeric`, the float types, `text`, `varchar`, `char`, `boolean` and `interval` (seconds only). A cast to another type, such as `uuid` or `timestamptz`, leaves the value as it is
- Expressions follow SQL's three-valued logic
- Column references are resolved against the schema before a statement runs, so a column none of the tables in scope has fails with 42703 as on the server. A name is let through when an item in scope has columns detest does not know, such as a function or a table no schema declared

**Schema**

- `CREATE`, `ALTER` and `DROP` of tables, columns, constraints, indexes, views and materialized views, `CREATE TABLE AS`, `REFRESH MATERIALIZED VIEW`, renames
- Defaults, serial and identity columns, sequences with their `START`, `INCREMENT` and `RESTART`, with values that repeat from run to run
- Stored generated columns, computed on every write. A Postgres 18 virtual column loads through `ddl.From`, which writes it as stored, but not from a Postgres 18 dump, whose `GENERATED ALWAYS AS (...)` without `STORED` the grammar detest parses with rejects
- Schemas and `search_path` (`postgres.SearchPath`)
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

- Recursive CTEs, and `WITH` on `UPDATE`, `DELETE` or `INSERT ... VALUES` (`WITH` on `SELECT` and on `INSERT ... SELECT` runs)
- Column alias lists on a table, a CTE or a view (`FROM t AS x(a, b)`, `WITH c(a) AS (...)`)
- `RIGHT` and `FULL` joins, `JOIN ... USING`, `NATURAL JOIN`, `TABLESAMPLE`, `SELECT INTO`, `INSERT ... DEFAULT VALUES`
- Set-returning functions other than `generate_series` in `FROM`, such as `unnest`, and any set-returning function in the select list, `generate_series` included
- `ANY (subquery)` with an operator other than `=`, row comparisons of different shapes
- Window frames other than the default and the whole partition, window functions other than the seven listed and the aggregates, and a window call with other arguments than the function takes
- Aggregates other than `count`, `sum`, `min`, `max` and `avg`, such as `array_agg`, `string_agg`, `bool_or`, `every` and `json_agg`, and `FILTER`, `ORDER BY` and `WITHIN GROUP` in aggregates

*Functions and operators*

- Any function not in the list above, and a call of one with other arguments than it takes, such as `make_interval` with an argument other than `secs => n`. Among the ones applications write: the date and time functions (`date_trunc`, `extract`, `to_char`, `age`, `to_timestamp`), the string functions (`trim`, `replace`, `substring`, `strpos`, `position`, `split_part`, `starts_with`, `md5`), the math functions beyond the list (`mod`, `sqrt`, `trunc`, `sign`), `current_setting` and `set_config`, `currval` and `lastval`, the session advisory locks (`pg_advisory_lock`), `pg_notify`, `pg_sleep`, `version`, and a function of another schema (`app.f()`)
- `CURRENT_DATE`, `CURRENT_USER` and the other SQL value functions, except `CURRENT_TIMESTAMP` and `LOCALTIMESTAMP`
- `IS DISTINCT FROM`, `IS TRUE` and `IS FALSE`, the regular expression operators (`~`, `~*`, `SIMILAR TO`), `LIKE ... ESCAPE`, and `||` of a `numeric`, a float, a timestamp or an interval
- Arrays, apart from `= ANY` and `<> ALL` over an `ARRAY[...]` of constants and parameters: `ARRAY[...]` written or read as a value, `= ANY` over a parameter, a column or a function result (`id = ANY($1)`, `'x' = ANY(tags)`), the array operators (`@>`, `<@`, `&&`), the array functions (`array_append`, `cardinality`, `array_length`), and writes to an array element or a field (`SET tags[1] = ...`)
- JSON: the operators (`->`, `->>`, `@>`, `?`) and the functions (`jsonb_set`, `jsonb_build_object`, `row_to_json`)
- Timestamp arithmetic with an interval literal other than seconds (`now() - interval '1 day'`). `now() - interval '30 seconds'` and `now() - make_interval(secs => n)` run, as does a comparison with a `time.Time` parameter

*Comparisons and conversions*

- Text compared with a number. Two cases run, a string literal or parameter that reads as an integer, which takes the number's type, and a parameter holding text or an integer compared with text, which takes the text type
- A number, or text other than a string literal or parameter, compared with a boolean or a time
- A string literal or parameter compared with a timestamp, through a cast too (`created_at >= '2024-01-01'`, `created_at >= '2024-01-01'::timestamptz`). Pass the time as a `time.Time` parameter
- A cast of a `numeric` or a float to text, and `||` of one
- `round` of a value ending in .5, and a cast of one to an integer, when the statement does not show whether it is a `numeric` or a float
- Text and a number in the same column of a set operation, or among the branches of `CASE`, `COALESCE`, `GREATEST` or `LEAST`
- Values written to a column that Postgres converts by rules detest does not model, namely a value shorter than its `char(n)` column, text without a time zone written to a `timestamptz` column, text written to a timestamp column in a form other than `YYYY-MM-DD[( |T)HH:MM[:SS[.ffffff]]][Z|±hh[:mm]]`, an integer written to a boolean column, and number text written as `0x10`, `0x1p2` or `1_000`

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
- `SET` of the settings listed under **Isolation**, and `SET search_path` to a path other than the one `postgres.SearchPath` declares, since detest resolves every name on the declared path. `SET search_path` to the declared path, as a migration writes it, and to an empty path, as `pg_dump` output does, runs, and so does the `set_config('search_path', ...)` a dump writes, under the same rule; any other call of `set_config` is a function detest does not run. `SET LOCAL lock_timeout` is acted on, and `SET` of any other setting, such as `statement_timeout`, `application_name`, `TIME ZONE` or a custom setting, is accepted and ignored. `current_setting` reading one back is refused

`detest.CheckSQL` tells whether detest can run a statement, for the cases the statement decides on its own, and refuses a function or an operator it does not know, or a call with other arguments than the function takes, wherever it stands in a `SELECT`, `INSERT`, `UPDATE` or `DELETE`, where a run reaches only the expressions it evaluates. A case that depends on the schema or on the values, such as a generated column detest cannot compute or a comparison decided by the column's type, passes `CheckSQL` and fails when the statement runs, and so does an expression in a default, a `CHECK` or a generated column, which the write that reads it refuses. The statements refused in a run are listed after the exploration's report, as an application that drops the error hides them.

A `numeric` value is kept as a float. It reads back without the trailing zeros it was written with (`1.50` as `1.5`), and one with more digits than a float keeps, such as an integer beyond 2^53, fails with `detest.ErrUnsupportedSQL`.

Arithmetic on numerics is exact where the statement shows that its operands are numerics, as literals and casts do, so `0.1 + 0.2 = 0.3` holds. A column's value does not show whether the column is a `numeric` or a float, which the two compute differently, so a result such as `amount + 0.2` is kept as the range of values the server may give. A comparison, more arithmetic, or a write to a `numeric(p, s)` column goes on when every value in the range gives the same outcome, and any other use of it, such as reading it back, fails with `detest.ErrUnsupportedSQL`. Sums and averages are kept the same way.

A `numeric(p, s)` column rounds what it stores to `s` places, half away from zero, and refuses a value of more than `p - s` digits before the point (22003). A `varchar(n)` or `char(n)` column refuses a longer value (22001), after dropping the spaces past `n` as Postgres does.

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
- `ext.Do(p, desc, fn)` wraps any call
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
| `detest.MaxRuns(n)` | Caps the runs of an exploration (default 200000) |
| `detest.MaxDuration(d)` | Stops starting runs once `d` has passed (unbounded by default) |
| `detest.Workers(n)` | Explores with n workers in parallel (default 1) |
| `detest.Shard(index, total, depth)` | Explores one shard of the schedule tree, for splitting across machines. `DETEST_SHARD` sets it from the environment |
| `detest.MaxCrashes(n)` | Lets up to n processes per run crash at any step (none by default). Their transactions roll back, their mutexes are freed and their messages are redelivered, to check that work survives a process dying halfway |

These environment variables override or add to them.

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
