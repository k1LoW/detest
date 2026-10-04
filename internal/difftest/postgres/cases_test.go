package postgres_test

import (
	"testing"
	"time"

	"github.com/k1LoW/detest/internal/difftest"
)

var (
	stockSchema = []string{`CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`}
	stockSeed   = []string{`INSERT INTO stock VALUES ('apple', 1), ('pear', 1)`}
)

var cases = []difftest.Case{
	{
		Name:   "update waits for the row lock",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = n - 1 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = n - 1 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "read committed rechecks the predicate after the wait",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = n - 1 WHERE sku = 'apple' AND n > 0`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "concurrent insert of the same key",
		Schema: stockSchema, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `INSERT INTO stock VALUES ('apple', 1)`),
			difftest.S(1, `INSERT INTO stock VALUES ('apple', 2)`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "failed statement aborts the transaction",
		Schema: stockSchema, Seed: stockSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `INSERT INTO stock VALUES ('apple', 1)`),
			difftest.S(0, `SELECT n FROM stock WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		// The first waiter's deadlock check runs after the cycle closed, and
		// it aborts itself.
		Name:   "deadlock breaks the first waiter",
		Schema: stockSchema, Seed: stockSeed, Conns: 2, Racy: true,
		Steps: deadlockSteps(0),
	},
	{
		// The first waiter's check found no cycle yet, so the session that
		// closed it aborts itself.
		Name:   "deadlock breaks the session closing the cycle",
		Schema: stockSchema, Seed: stockSeed, Conns: 2, Racy: true,
		Steps: deadlockSteps(300 * time.Millisecond), // longer than deadlock_timeout
	},
	{
		Name:   "failed statement releases the locks",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `INSERT INTO stock VALUES ('pear', 1)`),
			difftest.S(1, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "failed statement after a savepoint keeps the earlier locks",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `SAVEPOINT s`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(0, `INSERT INTO stock VALUES ('pear', 1)`),
			difftest.S(1, `UPDATE stock SET n = 5 WHERE sku = 'pear'`),
			difftest.S(1, `BEGIN`),
			difftest.S(1, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.S(0, `ROLLBACK TO SAVEPOINT s`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
			difftest.S(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "failed statement after a savepoint undoes a lock upgrade",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR KEY SHARE`),
			difftest.S(0, `SAVEPOINT s`),
			difftest.S(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR UPDATE`),
			difftest.S(0, `INSERT INTO stock VALUES ('apple', 1)`),
			difftest.S(1, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.S(1, `DELETE FROM stock WHERE sku = 'apple'`),
			difftest.S(0, `ROLLBACK`),
		},
	},
	{
		Name:   "rollback to savepoint undoes a lock upgrade",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR KEY SHARE`),
			difftest.S(0, `SAVEPOINT s`),
			difftest.S(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR UPDATE`),
			difftest.S(0, `ROLLBACK TO SAVEPOINT s`),
			difftest.S(1, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.S(1, `DELETE FROM stock WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "skip locked",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(1, `BEGIN`),
			difftest.S(0, `SELECT sku FROM stock ORDER BY sku LIMIT 1 FOR UPDATE SKIP LOCKED`),
			difftest.S(1, `SELECT sku FROM stock ORDER BY sku LIMIT 1 FOR UPDATE SKIP LOCKED`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
		},
	},
}

func deadlockSteps(pause time.Duration) []difftest.Step {
	return []difftest.Step{
		difftest.S(0, `BEGIN`),
		difftest.S(1, `BEGIN`),
		difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
		difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
		difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
		{Conn: 1, SQL: `UPDATE stock SET n = 0 WHERE sku = 'apple'`, Pause: pause},
		difftest.S(0, `ROLLBACK`),
		difftest.S(1, `ROLLBACK`),
	}
}

func TestDiff(t *testing.T) {
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
