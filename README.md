# detest

Deterministic Testing Framework for Go Applications

Because we detest race conditions, deadlocks, and flaky tests.

`detest` explores every interleaving of concurrent Go backend code that talks to a database, and reports the first schedule that breaks an invariant. It runs your production code as is. Processes (request handlers, workers, consumers) run inside a [`testing/synctest`](https://pkg.go.dev/testing/synctest) bubble under a deterministic scheduler, and the database is an in-memory `database/sql` driver that models PostgreSQL's Read Committed row locking. Each run follows one sequence of scheduling choices, and detest walks all of them depth first. A violation comes with the schedule that produced it, so it can be replayed step by step.

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
	detest.Explore(t, func(t *testing.T, m *detest.Model) {
		db, store := m.DB("shop", postgres.New())
		if _, err := db.Exec(`CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		m.Seed(func() {
			_, _ = db.Exec(`INSERT INTO stock (sku, n) VALUES ('apple', 1)`)
		})
		reserved := 0
		m.Seed(func() { reserved = 0 })
		for _, name := range []string{"alice", "bob"} {
			m.Manual(name, 1, func(p *detest.Proc) error {
				if err := reserve(p.Context(), db, "apple"); err == nil {
					reserved++
				}
				return nil
			})
		}
		m.AtQuiescence(func(s *detest.State) error {
			if reserved > 1 {
				row, _ := s.Row(store, "stock", "apple")
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
        run 4, schedule (8 choices): DETEST_SCHEDULE=0,0,0,0,1,1,1,0
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

The function passed to `detest.Explore` declares the model. It runs once per explored schedule, so state that the processes change has to be reset in `m.Seed`. Run the test again with the printed `DETEST_SCHEDULE` to replay exactly that run.

A test that pins a known violation calls `m.ExpectViolation(substr)`. It passes while the violation is found and fails once it is gone.

### Declaring a model

| Method | What it declares |
| --- | --- |
| `m.DB(name, server)` | A database. It returns a `*sql.DB` to hand to production code and a `*detest.DB` to read in invariants |
| `m.Manual(name, n, fn)` | A process type started up to `n` times |
| `m.Loop(name, n, fn)` | A process that runs `fn` repeatedly, such as a poller or a reaper |
| `m.Queue(name)`, `m.OnMessage(name, q, fn)` | A message queue and its consumer, with redelivery |
| `m.External(name)` | A call to another service, which may fail before its effect or lose the response after it. `Transport(h)` gives an `http.RoundTripper` serving an `http.Handler` |
| `m.Mutex(name)`, `m.RWMutex(name)` | Locks the scheduler can see, to inject in place of `sync.Mutex` |
| `m.Always(fn)`, `m.AtQuiescence(fn)` | Invariants checked after every step, or once every process is done |

### Databases

`postgres.New()` parses SQL with the PostgreSQL parser and models Read Committed. Row locks of the four strengths, unique index entries, foreign keys (`FOR KEY SHARE` on the parent, cascades), `NOWAIT`, `SKIP LOCKED`, `lock_timeout`, savepoints and deadlock detection behave as they do on a real server. Migrations and schema dumps can be executed as is. Statements detest does not support fail with `detest.ErrUnsupportedSQL`, and `detest.CheckSQL` checks a statement without running a model.

Errors are `*detest.SQLError` with the SQLSTATE of the real server. Code that branches on driver error types gets them by converting.

``` go
m.DB("app", postgres.New(postgres.Errors(pgxerr.Convert))) // *pgconn.PgError
m.DB("app", postgres.New(postgres.Errors(pqerr.Convert)))  // *pq.Error
```

`ddl.From` reads the tables of a live database and writes DDL that detest accepts.

### Options and environment variables

The search space grows quickly. `detest.MaxPreemptions`, `detest.MaxFailures`, `detest.MaxRedeliveries` and `detest.MaxRuns` bound it, and `detest.Workers` explores it in parallel.

| Variable | Effect |
| --- | --- |
| `DETEST_SCHEDULE` | Replay one run, given the choices printed with a violation |
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
