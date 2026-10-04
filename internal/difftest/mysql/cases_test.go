package mysql_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

// The tables compare strings exactly, as detest does: a case-insensitive
// collation, MySQL 8's default, is refused where it decides an outcome.
const binary = ` DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`

const (
	rr  = `BEGIN ISOLATION LEVEL REPEATABLE READ`
	rc  = `BEGIN ISOLATION LEVEL READ COMMITTED`
	ser = `BEGIN ISOLATION LEVEL SERIALIZABLE`
)

var (
	stockSchema = []string{`CREATE TABLE stock (sku VARCHAR(10) PRIMARY KEY, n INT NOT NULL)` + binary}
	stockSeed   = []string{`INSERT INTO stock VALUES ('apple', 1), ('pear', 1)`}
	itemsSchema = []string{`CREATE TABLE items (id INT PRIMARY KEY, k INT, v INT NOT NULL DEFAULT 0, KEY (k))` + binary}
	itemsSeed   = []string{`INSERT INTO items (id, k) VALUES (10, 10), (20, 20), (30, 30)`}
)

var cases = []difftest.Case{
	{
		Name:   "update waits for the row lock",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `UPDATE stock SET n = n - 1 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = n - 1 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "repeatable read reads a snapshot, a locking read the latest row",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple'`),
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple' FOR UPDATE`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "an update from the snapshot loses a concurrent update",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = n + 10 WHERE sku = 'apple'`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "an insert into a locked gap waits",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.Q(0, `SELECT id FROM items WHERE id BETWEEN 12 AND 18 FOR UPDATE`),
			difftest.S(1, `INSERT INTO items (id, k) VALUES (15, 15)`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id FROM items ORDER BY id`),
		},
	},
	{
		Name:   "an insert outside the locked gaps does not wait",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.Q(0, `SELECT id FROM items WHERE id BETWEEN 12 AND 18 FOR UPDATE`),
			difftest.S(1, `INSERT INTO items (id, k) VALUES (35, 35)`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "read committed takes no gap lock",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rc),
			difftest.Q(0, `SELECT id FROM items WHERE id BETWEEN 12 AND 18 FOR UPDATE`),
			difftest.S(1, `INSERT INTO items (id, k) VALUES (15, 15)`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "a secondary index search locks its gaps",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `UPDATE items SET v = 1 WHERE k = 20`),
			difftest.S(1, `INSERT INTO items (id, k) VALUES (15, 15)`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "checking a missing row and inserting it deadlocks",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.Q(0, `SELECT id FROM items WHERE id = 15 FOR UPDATE`),
			difftest.Q(1, `SELECT id FROM items WHERE id = 16 FOR UPDATE`),
			difftest.S(0, `INSERT INTO items (id, k) VALUES (15, 15)`),
			difftest.S(1, `INSERT INTO items (id, k) VALUES (16, 16)`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
			difftest.Q(0, `SELECT id FROM items ORDER BY id`),
		},
	},
	{
		Name:   "deadlock victim",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// The transaction that closes the cycle changed more rows, so InnoDB
		// rolls back the lighter one, which waits.
		Name:   "deadlock victim is the lighter transaction",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `INSERT INTO stock VALUES ('plum', 1), ('quince', 1)`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// Each update of a row writes an undo record, so the transaction
		// that updated one row three times weighs more than the one that
		// updated two rows once.
		Name:   "deadlock victim weighs every update of a row",
		Schema: stockSchema, Seed: []string{`INSERT INTO stock VALUES ('apple', 1), ('pear', 1), ('plum', 1)`}, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(0, `UPDATE stock SET n = 2 WHERE sku = 'apple'`),
			difftest.S(0, `UPDATE stock SET n = 3 WHERE sku = 'apple'`),
			difftest.S(0, `UPDATE stock SET n = 4 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = 2 WHERE sku = 'pear'`),
			difftest.S(1, `UPDATE stock SET n = 2 WHERE sku = 'plum'`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// The range read's next-key locks are lock structs of their own,
		// which weigh too.
		Name:   "deadlock victim weighs the locks of a range read",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.Q(0, `SELECT id FROM items WHERE k BETWEEN 1 AND 15 FOR UPDATE`),
			difftest.S(0, `UPDATE items SET v = 1 WHERE id = 10`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 30`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 10`),
			difftest.S(0, `UPDATE items SET v = 1 WHERE id = 30`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		Name:   "a failed statement rolls back alone and keeps its locks",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `UPDATE stock SET n = 7 WHERE sku = 'apple'`),
			difftest.S(0, `INSERT INTO stock VALUES ('pear', 1)`),
			difftest.S(1, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "inserts failing on the same key do not wait for each other",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(0, `INSERT INTO stock VALUES ('apple', 2)`),
			difftest.S(1, `INSERT INTO stock VALUES ('apple', 3)`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		Name:   "concurrent insert of the same key",
		Schema: stockSchema, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `INSERT INTO stock VALUES ('apple', 1)`),
			difftest.S(1, `INSERT INTO stock VALUES ('apple', 2)`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "insert ignore skips a duplicate",
		Schema: stockSchema, Seed: stockSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `INSERT IGNORE INTO stock VALUES ('apple', 5), ('plum', 5)`),
			difftest.Q(0, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "update counts the rows it changed",
		Schema: stockSchema, Seed: stockSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `UPDATE stock SET n = 1`),
			difftest.S(0, `UPDATE stock SET n = 2 WHERE sku = 'apple'`),
			difftest.S(0, `INSERT INTO stock VALUES ('apple', 9) ON DUPLICATE KEY UPDATE n = 9`),
		},
	},
	{
		Name:   "serializable plain reads take shared locks",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, ser),
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "skip locked",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.Q(0, `SELECT sku FROM stock ORDER BY sku LIMIT 1 FOR UPDATE SKIP LOCKED`),
			difftest.Q(1, `SELECT sku FROM stock ORDER BY sku LIMIT 1 FOR UPDATE SKIP LOCKED`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		Name:   "nowait",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'apple' FOR UPDATE NOWAIT`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "a limited scan locks up to where it stops",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `UPDATE items SET v = 1 WHERE k >= 0 ORDER BY k LIMIT 1`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 30`),
			difftest.S(1, `INSERT INTO items (id, k) VALUES (25, 25)`),
			difftest.S(0, `COMMIT`),
		},
	},
}

func TestDiff(t *testing.T) {
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
