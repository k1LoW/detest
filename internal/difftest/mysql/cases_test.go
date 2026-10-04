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
		// A lookup through a unique secondary index locks that index's record
		// and the primary key's, two lock structs, which make the weights
		// equal here, so the transaction that closed the cycle is the victim.
		Name:   "deadlock victim weighs a unique secondary index lookup",
		Schema: []string{`CREATE TABLE u (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary},
		Seed:   []string{`INSERT INTO u (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.Q(0, `SELECT id FROM u WHERE code = 'a' FOR UPDATE`),
			difftest.S(1, `UPDATE u SET v = 1 WHERE id = 2`),
			difftest.S(0, `UPDATE u SET v = 1 WHERE id = 2`),
			difftest.S(1, `UPDATE u SET v = 1 WHERE id = 1`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// A shared next-key range and an exclusive record lock are lock
		// structs of their own, as are the shared and exclusive table locks.
		Name:   "deadlock victim weighs shared and exclusive locks apart",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.Q(0, `SELECT id FROM items WHERE id BETWEEN 1 AND 15 FOR SHARE`),
			difftest.S(0, `UPDATE items SET v = 1 WHERE id = 10`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 30`),
			difftest.S(1, `UPDATE items SET v = 3 WHERE id = 30`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 10`),
			difftest.S(0, `UPDATE items SET v = 1 WHERE id = 30`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// A foreign key check on a unique secondary index locks that index's
		// record, not the parent row, so a write of the row's other columns
		// does not wait, and a write of the key does.
		Name: "a foreign key check locks the parent's index record",
		Schema: []string{
			`CREATE TABLE p (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary,
			`CREATE TABLE c (id INT PRIMARY KEY, code VARCHAR(5), FOREIGN KEY (code) REFERENCES p (code))` + binary,
		},
		Seed:  []string{`INSERT INTO p (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `INSERT INTO c VALUES (1, 'a')`),
			difftest.S(1, `UPDATE p SET v = 1 WHERE id = 1`),
			difftest.S(1, `UPDATE p SET code = 'z' WHERE id = 1`),
			difftest.S(0, `ROLLBACK`),
			difftest.Q(1, `SELECT id, code, v FROM p ORDER BY id`),
		},
	},
	{
		// The deadlock closes on the parent's index record, which the
		// foreign key check holds in share mode.
		Name: "deadlock victim weighs a foreign key check",
		Schema: []string{
			`CREATE TABLE p (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary,
			`CREATE TABLE c (id INT PRIMARY KEY, code VARCHAR(5), FOREIGN KEY (code) REFERENCES p (code))` + binary,
		},
		Seed:  []string{`INSERT INTO p (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(0, `INSERT INTO c VALUES (1, 'a')`),
			difftest.S(1, `UPDATE p SET v = 1 WHERE id = 2`),
			difftest.S(0, `UPDATE p SET v = 1 WHERE id = 2`),
			difftest.S(1, `UPDATE p SET code = 'z' WHERE id = 1`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// A duplicate key is checked under a shared lock on the existing
		// row, which weighs the failed insert's transaction.
		Name:   "deadlock victim weighs a duplicate key check",
		Schema: []string{`CREATE TABLE u (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary},
		Seed:   []string{`INSERT INTO u (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(0, `INSERT INTO u (id, code) VALUES (3, 'a')`),
			difftest.S(0, `UPDATE u SET v = 1 WHERE id = 1`),
			difftest.S(1, `UPDATE u SET v = 1 WHERE id = 2`),
			difftest.S(1, `UPDATE u SET v = 2 WHERE id = 2`),
			difftest.S(1, `UPDATE u SET v = 1 WHERE id = 1`),
			difftest.S(0, `UPDATE u SET v = 1 WHERE id = 2`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// At Read Committed an UPDATE by a secondary index locks the index
		// record and the primary key's, with no gaps.
		Name:   "deadlock victim at read committed",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rc),
			difftest.S(1, rc),
			difftest.S(0, `UPDATE items SET v = 1 WHERE k = 10`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 30`),
			difftest.S(1, `UPDATE items SET v = 3 WHERE id = 30`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 10`),
			difftest.S(0, `UPDATE items SET v = 1 WHERE id = 30`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// A locking read keeps the table's intention lock even when SKIP
		// LOCKED skips every row, and the lock struct weighs it.
		Name:   "deadlock victim weighs the intention lock of a skipping read",
		Schema: stockSchema, Seed: []string{`INSERT INTO stock VALUES ('apple', 1), ('pear', 1), ('plum', 1)`}, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'apple' FOR SHARE SKIP LOCKED`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// A lock that waited keeps a lock struct of its own once granted,
		// so the transaction that waited earlier weighs more.
		Name:   "deadlock victim weighs an earlier wait",
		Schema: stockSchema, Seed: []string{`INSERT INTO stock VALUES ('apple', 1), ('pear', 1), ('plum', 1)`}, Conns: 3,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(2, rr),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(1, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.S(2, `UPDATE stock SET n = 2 WHERE sku = 'plum'`),
			difftest.S(2, `UPDATE stock SET n = 3 WHERE sku = 'plum'`),
			difftest.S(2, `UPDATE stock SET n = 2 WHERE sku = 'pear'`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'plum'`),
			difftest.S(1, `ROLLBACK`),
			difftest.S(2, `ROLLBACK`),
		},
	},
	{
		// A row inserted holds no lock struct until another transaction
		// waits for it, when its implicit lock turns explicit and weighs the
		// inserter.
		Name:   "deadlock victim weighs an inserted row another waits for",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(0, `INSERT INTO stock VALUES ('kiwi', 1)`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'kiwi'`),
			difftest.Q(0, `SELECT sku FROM stock WHERE sku = 'pear' FOR SHARE`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// An insert puts the row into the primary key, with its undo record,
		// before it checks a unique secondary index, so a wait in that check
		// weighs the row already.
		Name:   "deadlock victim weighs an insert waiting on a unique index",
		Schema: []string{`CREATE TABLE u (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary},
		Seed:   []string{`INSERT INTO u (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(0, `INSERT INTO u (id, code) VALUES (3, 'c'), (5, 'e')`),
			difftest.S(1, `UPDATE u SET v = 1 WHERE id = 2`),
			difftest.S(1, `INSERT INTO u (id, code) VALUES (4, 'c')`),
			difftest.S(0, `UPDATE u SET v = 1 WHERE id = 2`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// A locking read joining a table it looks up by a unique key first
		// searches the joined table's index with each row found, and locks
		// the gaps it reads there, as a search of its own would.
		Name: "a locking join locks the joined table's gaps",
		Schema: []string{
			`CREATE TABLE a (id INT PRIMARY KEY, v INT NOT NULL DEFAULT 0)` + binary,
			`CREATE TABLE b (id INT PRIMARY KEY, a_id INT NOT NULL, KEY (a_id))` + binary,
		},
		Seed:  []string{`INSERT INTO a (id) VALUES (1), (2)`, `INSERT INTO b VALUES (1, 1), (5, 5)`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.Q(0, `SELECT a.id, b.id FROM a JOIN b ON b.a_id = a.id WHERE a.id = 1 FOR UPDATE`),
			difftest.S(1, `INSERT INTO b VALUES (6, 6)`),
			difftest.S(1, `INSERT INTO b VALUES (2, 2)`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id, a_id FROM b ORDER BY id`),
		},
	},
	{
		// The const row failing the conditions on its table ends the join
		// before MySQL reads the joined table, which it then leaves unlocked.
		Name: "a locking join whose first row fails its conditions locks no joined row",
		Schema: []string{
			`CREATE TABLE a (id INT PRIMARY KEY, v INT NOT NULL DEFAULT 0)` + binary,
			`CREATE TABLE b (id INT PRIMARY KEY, a_id INT NOT NULL, KEY (a_id))` + binary,
		},
		Seed:  []string{`INSERT INTO a (id) VALUES (1), (2)`, `INSERT INTO b VALUES (1, 1), (5, 5)`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.Q(0, `SELECT a.id, b.id FROM a JOIN b ON b.a_id = a.id WHERE a.id = 1 AND a.v = 5 FOR UPDATE`),
			difftest.S(1, `INSERT INTO b VALUES (2, 1)`),
			difftest.S(1, `UPDATE b SET a_id = 1 WHERE id = 1`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		// A locking read over a join locks the joined table's index too.
		Name: "deadlock victim weighs a locking join",
		Schema: []string{
			`CREATE TABLE a (id INT PRIMARY KEY, v INT NOT NULL DEFAULT 0)` + binary,
			`CREATE TABLE b (id INT PRIMARY KEY, a_id INT NOT NULL, KEY (a_id))` + binary,
		},
		Seed:  []string{`INSERT INTO a (id) VALUES (1), (2)`, `INSERT INTO b VALUES (1, 1)`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.Q(0, `SELECT a.id FROM a JOIN b ON b.a_id = a.id WHERE a.id = 1 FOR UPDATE`),
			difftest.S(1, `UPDATE a SET v = 1 WHERE id = 2`),
			difftest.S(1, `UPDATE a SET v = 2 WHERE id = 2`),
			difftest.S(1, `UPDATE a SET v = 1 WHERE id = 1`),
			difftest.S(0, `UPDATE a SET v = 1 WHERE id = 2`),
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
		// A failed insert rolls back the row it put into the primary key, and
		// the row's lock stays as a gap lock where the row was.
		Name:   "a failed insert leaves a gap lock",
		Schema: []string{`CREATE TABLE u (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary},
		Seed:   []string{`INSERT INTO u (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `INSERT INTO u (id, code) VALUES (3, 'a')`),
			difftest.S(1, `INSERT INTO u (id, code) VALUES (0, 'y')`),
			difftest.S(1, `INSERT INTO u (id, code) VALUES (4, 'z')`),
			difftest.S(0, `ROLLBACK`),
			difftest.Q(1, `SELECT id, code FROM u ORDER BY id`),
		},
	},
	{
		// Rolling back to a savepoint takes the rows inserted since out of
		// every index, and their locks stay as gap locks there.
		Name:   "rollback to a savepoint leaves gap locks where the rows were",
		Schema: []string{`CREATE TABLE u (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary},
		Seed:   []string{`INSERT INTO u (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `UPDATE u SET v = 1 WHERE id = 1`),
			difftest.S(0, `SAVEPOINT s`),
			difftest.S(0, `INSERT INTO u (id, code) VALUES (3, 'c')`),
			difftest.S(0, `ROLLBACK TO SAVEPOINT s`),
			difftest.S(1, `INSERT INTO u (id, code) VALUES (0, '0')`),
			difftest.S(1, `INSERT INTO u (id, code) VALUES (4, '1')`),
			difftest.S(0, `ROLLBACK`),
			difftest.Q(1, `SELECT id, code FROM u ORDER BY id`),
		},
	},
	{
		// The duplicate check's shared lock on the unique index record stays
		// after the insert fails, so a write of that key waits.
		Name:   "a failed insert keeps its duplicate check's lock",
		Schema: []string{`CREATE TABLE u (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary},
		Seed:   []string{`INSERT INTO u (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `INSERT INTO u (id, code) VALUES (3, 'a')`),
			difftest.S(1, `UPDATE u SET v = 1 WHERE id = 1`),
			difftest.S(1, `UPDATE u SET code = 'q' WHERE id = 1`),
			difftest.S(0, `ROLLBACK`),
			difftest.Q(1, `SELECT id, code, v FROM u ORDER BY id`),
		},
	},
	{
		// The foreign key check of a row the failed statement inserted keeps
		// its lock on the parent's index record.
		Name: "a failed insert keeps its foreign key check's lock",
		Schema: []string{
			`CREATE TABLE p (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary,
			`CREATE TABLE c (id INT PRIMARY KEY, code VARCHAR(5), FOREIGN KEY (code) REFERENCES p (code))` + binary,
		},
		Seed:  []string{`INSERT INTO p (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `INSERT INTO c VALUES (1, 'a'), (1, 'b')`),
			difftest.S(1, `UPDATE p SET code = 'z' WHERE id = 1`),
			difftest.S(0, `ROLLBACK`),
			difftest.Q(1, `SELECT id, code FROM p ORDER BY id`),
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
