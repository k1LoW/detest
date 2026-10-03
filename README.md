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
	detest.Explore(t, func(t *testing.T, sim *detest.Sim) {
		db, store := sim.DB("shop", postgres.New())
		if _, err := db.Exec(`CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		sim.Seed(func() {
			_, _ = db.Exec(`INSERT INTO stock (sku, n) VALUES ('apple', 1)`)
		})
		reserved := 0
		sim.Seed(func() { reserved = 0 })
		for _, name := range []string{"alice", "bob"} {
			sim.Manual(name, 1, func(p *detest.Proc) error {
				if err := reserve(p.Context(), db, "apple"); err == nil {
					reserved++
				}
				return nil
			})
		}
		sim.AtQuiescence(func(s *detest.State) error {
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

The function passed to `detest.Explore` declares the simulation. It runs once per explored schedule, so state that the processes change has to be reset in `sim.Seed`. Run the test again with the printed `DETEST_SCHEDULE` to replay exactly that run.

A test that pins a known violation calls `sim.ExpectViolation(substr)`. It passes while the violation is found and fails once it is gone.

### Declaring a model

| Method | What it declares |
| --- | --- |
| `sim.DB(name, server)` | A database. It returns a `*sql.DB` to hand to production code and a `*detest.DB` to read in invariants |
| `sim.Manual(name, n, fn)` | A process type started up to `n` times |
| `sim.Loop(name, n, fn)` | A process that runs `fn` repeatedly, such as a poller or a reaper |
| `sim.Queue(name)`, `sim.OnMessage(name, q, fn)` | A message queue and its consumer, with redelivery |
| `sim.External(name)` | A call to another service, which may fail before its effect or lose the response after it. `Transport(h)` gives an `http.RoundTripper` serving an `http.Handler` |
| `sim.Mutex(name)`, `sim.RWMutex(name)` | Locks the scheduler can see, to inject in place of `sync.Mutex` |
| `sim.Always(fn)`, `sim.AtQuiescence(fn)` | Invariants checked after every step, or once every process is done |

### Databases

`postgres.New()` parses SQL with the PostgreSQL parser and models Read Committed. Row locks of the four strengths, unique index entries, foreign keys (`FOR KEY SHARE` on the parent, cascades), `NOWAIT`, `SKIP LOCKED`, `lock_timeout`, savepoints and deadlock detection behave as they do on a real server. Migrations and schema dumps can be executed as is. Statements detest does not support fail with `detest.ErrUnsupportedSQL`, and `detest.CheckSQL` checks a statement without running a model.

Errors are `*detest.SQLError` with the SQLSTATE of the real server. Code that branches on driver error types gets them by converting.

``` go
sim.DB("app", postgres.New(postgres.Errors(pgxerr.Convert))) // *pgconn.PgError
sim.DB("app", postgres.New(postgres.Errors(pqerr.Convert)))  // *pq.Error
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
