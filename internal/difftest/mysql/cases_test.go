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
		// A range of the primary key starting at a row, closed, locks that
		// row alone, and the row past its end as a gap alone, so neither
		// the gap below nor the row past it waits.
		Name:   "a primary key range locks the gap before the next row only",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.Q(0, `SELECT v FROM items WHERE id BETWEEN 10 AND 15 FOR UPDATE`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 20`),
			difftest.S(1, `INSERT INTO items (id, k) VALUES (5, 5)`),
			difftest.S(1, `INSERT INTO items (id, k) VALUES (15, 15)`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id, v FROM items ORDER BY id`),
		},
	},
	{
		// An equality on a plain index locks the entry past it as a gap
		// alone.
		Name:   "an index equality locks the gap before the next entry only",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.Q(0, `SELECT v FROM items WHERE k = 20 FOR UPDATE`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 30`),
			difftest.S(1, `INSERT INTO items (id, k) VALUES (25, 25)`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id, v FROM items ORDER BY id`),
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
		// An update of a row a range read already locked next-key needs no
		// lock of its own, so it adds no lock struct.
		Name:   "deadlock victim weighs a lock a held one covers as none",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.Q(0, `SELECT id FROM items WHERE id BETWEEN 10 AND 15 FOR UPDATE`),
			difftest.S(0, `UPDATE items SET v = 1 WHERE id = 10`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 30`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 10`),
			difftest.S(0, `UPDATE items SET v = 1 WHERE id = 30`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// An insert takes the table's IX before its duplicate check, whose
		// shared lock then adds no IS.
		Name:   "deadlock victim weighs an insert's intention lock as IX",
		Schema: stockSchema, Seed: []string{`INSERT INTO stock VALUES ('apple', 1), ('pear', 1), ('plum', 1)`}, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(0, `INSERT IGNORE INTO stock VALUES ('apple', 1)`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(1, `UPDATE stock SET n = 2 WHERE sku = 'plum'`),
			difftest.S(1, `UPDATE stock SET n = 3 WHERE sku = 'plum'`),
			difftest.S(1, `UPDATE stock SET n = 2 WHERE sku = 'pear'`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'plum'`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// An update of an indexed column holds the old index entry
		// implicitly, which turns explicit, a lock struct, when a search
		// along the index waits for it.
		Name:   "deadlock victim weighs an index entry another waits for",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(1, `UPDATE items SET k = 25 WHERE id = 20`),
			difftest.S(0, `UPDATE items SET v = 1 WHERE id = 30`),
			difftest.Q(0, `SELECT v FROM items WHERE k BETWEEN 15 AND 22 FOR UPDATE`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 30`),
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
			difftest.Q(0, `SELECT a.id, b.id FROM a JOIN b ON a.v = 5 AND b.a_id = a.id WHERE a.id = 1 FOR UPDATE`),
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
		// A locking search meets the index entry another transaction's
		// uncommitted update moved into its range, waits for it, and reads
		// the row once the update commits.
		Name:   "a locking search waits for an update moving a row into its range",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(1, rr),
			difftest.S(1, `UPDATE items SET k = 15 WHERE id = 30`),
			difftest.S(0, rr),
			difftest.Q(0, `SELECT id FROM items WHERE k BETWEEN 11 AND 19 FOR UPDATE`),
			difftest.S(1, `COMMIT`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		// SKIP LOCKED passes over a row whose unique index record a foreign
		// key check holds.
		Name: "skip locked passes over a unique index record a check holds",
		Schema: []string{
			`CREATE TABLE p (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary,
			`CREATE TABLE c (id INT PRIMARY KEY, code VARCHAR(5), FOREIGN KEY (code) REFERENCES p (code))` + binary,
		},
		Seed:  []string{`INSERT INTO p (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `INSERT INTO c VALUES (1, 'a')`),
			difftest.S(1, rr),
			difftest.Q(1, `SELECT id FROM p WHERE code = 'a' FOR UPDATE SKIP LOCKED`),
			difftest.S(1, `COMMIT`),
			difftest.S(0, `ROLLBACK`),
		},
	},
	{
		// A limited scan along a unique secondary index locks its records,
		// so it waits for a foreign key check holding one.
		Name: "a limited scan on a unique index waits for a check's lock",
		Schema: []string{
			`CREATE TABLE p (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary,
			`CREATE TABLE c (id INT PRIMARY KEY, code VARCHAR(5), FOREIGN KEY (code) REFERENCES p (code))` + binary,
		},
		Seed:  []string{`INSERT INTO p (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `INSERT INTO c VALUES (1, 'a')`),
			difftest.S(1, rr),
			difftest.Q(1, `SELECT id FROM p WHERE code >= 'a' ORDER BY code LIMIT 1 FOR UPDATE`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		// Read Committed takes no gap locks, but a search through a unique
		// secondary index still locks its records, so it waits for a
		// foreign key check holding one.
		Name: "read committed locks the unique index records it searches",
		Schema: []string{
			`CREATE TABLE p (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary,
			`CREATE TABLE c (id INT PRIMARY KEY, code VARCHAR(5), FOREIGN KEY (code) REFERENCES p (code))` + binary,
		},
		Seed:  []string{`INSERT INTO p (id, code) VALUES (1, 'a'), (2, 'b')`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rc),
			difftest.S(0, `INSERT INTO c VALUES (1, 'a')`),
			difftest.S(1, rc),
			difftest.S(1, `UPDATE p SET v = 1 WHERE code = 'a'`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		// At Repeatable Read too, a range through an index locks its rows in
		// the index's order, the reverse of the primary key's here.
		Name:   "a range locks along the index it searches",
		Schema: itemsSchema,
		Seed:   []string{`INSERT INTO items (id, k) VALUES (10, 30), (30, 10), (50, 100), (60, 110), (70, 120), (80, 130)`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 10`),
			difftest.S(1, `UPDATE items SET v = 3 WHERE id = 10`),
			difftest.S(0, `UPDATE items SET v = 1 WHERE k BETWEEN 5 AND 40`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 30`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// A search through an index locks its rows in the index's order,
		// here the reverse of the primary key's, which closes the cycle,
		// and changes each row as it locks it, so the undo record of the
		// first weighs the wait for the second.
		Name:   "read committed locks along the index it searches",
		Schema: itemsSchema,
		Seed:   []string{`INSERT INTO items (id, k) VALUES (10, 30), (30, 10), (50, 100), (60, 110), (70, 120), (80, 130)`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, rc),
			difftest.S(1, rc),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 10`),
			difftest.S(1, `UPDATE items SET v = 3 WHERE id = 10`),
			difftest.S(0, `UPDATE items SET v = 1 WHERE k BETWEEN 5 AND 40`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 30`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
	},
	{
		// At Read Committed, the locks of a row the search reads but WHERE
		// does not take are released.
		Name:   "read committed releases the rows its search does not take",
		Schema: itemsSchema, Seed: itemsSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rc),
			difftest.S(0, `UPDATE items SET v = 1 WHERE k = 10 AND v = 99`),
			difftest.S(1, `UPDATE items SET v = 2 WHERE id = 10`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		// An insert failing on a unique index has put its entries into the
		// indexes InnoDB writes before that one, whose gaps it then keeps,
		// and holds the gap before the duplicate's entry with its check.
		Name: "a failed insert keeps the gaps of the indexes it reached",
		Schema: []string{
			`CREATE TABLE o (id INT PRIMARY KEY, a INT NULL, b INT NOT NULL, UNIQUE KEY ua (a), UNIQUE KEY ub (b))` + binary,
		},
		Seed:  []string{`INSERT INTO o VALUES (1, 1, 10), (2, 2, 20)`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(0, `INSERT INTO o VALUES (3, 1, 15)`),
			difftest.S(1, `INSERT INTO o VALUES (-2, 6, 30)`),
			difftest.S(1, `INSERT INTO o VALUES (-1, 5, 12)`),
			difftest.S(0, `ROLLBACK`),
			difftest.Q(1, `SELECT id, a, b FROM o ORDER BY id`),
		},
	},
	{
		// The duplicate check's shared lock on a unique secondary index is
		// next-key, at Read Committed too, so an insert into the gap before
		// the duplicate waits.
		Name: "a duplicate check locks the gap before the duplicate",
		Schema: []string{
			`CREATE TABLE o (id INT PRIMARY KEY, a INT NULL, b INT NOT NULL, UNIQUE KEY ua (a), UNIQUE KEY ub (b))` + binary,
		},
		Seed:  []string{`INSERT INTO o VALUES (1, 1, 10), (2, 5, 20)`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rc),
			difftest.S(0, `INSERT INTO o VALUES (3, 5, 15)`),
			difftest.S(1, rc),
			difftest.S(1, `INSERT INTO o VALUES (4, 9, 30)`),
			difftest.S(1, `INSERT INTO o VALUES (5, 3, 31)`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		// An update writes each row as its search locks it, so a row failing
		// a unique check ends the statement before it waits for a later row.
		Name:   "a multi-row update fails on a row before it waits for a later one",
		Schema: []string{`CREATE TABLE u (id INT PRIMARY KEY, code VARCHAR(5), v INT NOT NULL DEFAULT 0, UNIQUE KEY (code))` + binary},
		Seed:   []string{`INSERT INTO u (id, code) VALUES (1, 'a'), (2, 'b'), (3, 'c')`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(1, `UPDATE u SET v = 9 WHERE id = 3`),
			difftest.S(0, `UPDATE u SET code = 'b' WHERE id >= 1`),
			difftest.S(1, `COMMIT`),
			difftest.S(0, `ROLLBACK`),
		},
	},
	{
		// A delete does so too, failing on a row a foreign key references.
		Name: "a multi-row delete fails on a row before it waits for a later one",
		Schema: []string{
			`CREATE TABLE p (id INT PRIMARY KEY, v INT NOT NULL DEFAULT 0)` + binary,
			`CREATE TABLE c (id INT PRIMARY KEY, p_id INT, FOREIGN KEY (p_id) REFERENCES p (id))` + binary,
		},
		Seed:  []string{`INSERT INTO p (id) VALUES (1), (2), (3)`, `INSERT INTO c VALUES (1, 1)`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, rr),
			difftest.S(1, rr),
			difftest.S(1, `UPDATE p SET v = 9 WHERE id = 3`),
			difftest.S(0, `DELETE FROM p WHERE id >= 1`),
			difftest.S(1, `COMMIT`),
			difftest.S(0, `ROLLBACK`),
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
