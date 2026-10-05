package postgres_test

import (
	"testing"
	"time"

	"github.com/k1LoW/detest/internal/difftest"
)

var (
	stockSchema = []string{`CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`}
	stockSeed   = []string{`INSERT INTO stock VALUES ('apple', 1), ('pear', 1)`}
	ordSchema   = []string{
		`CREATE TABLE cust (id int PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE ord (id int PRIMARY KEY, cust_id int NOT NULL REFERENCES cust (id), qty int NOT NULL)`,
	}
	ordSeed  = []string{`INSERT INTO cust VALUES (1, 'a'), (2, 'b')`, `INSERT INTO ord VALUES (10, 1, 1), (11, 1, 2), (12, 2, 3)`}
	fkSchema = []string{
		`CREATE TABLE parent (id int PRIMARY KEY, name text)`,
		`CREATE TABLE child (id int PRIMARY KEY, parent_id int REFERENCES parent (id))`,
	}
	fkSeed = []string{`INSERT INTO parent VALUES (1, 'a'), (2, 'b')`, `INSERT INTO child VALUES (10, 1)`}
)

var cases = []difftest.Case{
	{
		Name:   "reset all in a script clears the session lock timeout",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(1, `SET lock_timeout = '1ms'`),
			difftest.S(1, `RESET ALL; SELECT 1`),
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = 2 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "update waits for the row lock",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = n - 1 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = n - 1 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
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
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
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
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		// Run at once, the first waiter's deadlock check runs after the cycle
		// closed, and it aborts itself. With a pause longer than
		// deadlock_timeout (1s by default) before the cycle closes, its check
		// finds nothing, and the session closing the cycle aborts itself. The
		// default is kept, as a shorter one would leave the run without the
		// pause too little time to close the cycle under load.
		Name:   "deadlock victim",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(1, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(1, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `ROLLBACK`),
		},
		Pauses: []map[int]time.Duration{{5: 1500 * time.Millisecond}},
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
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
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
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "failed statement after a savepoint undoes a lock upgrade",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR KEY SHARE`),
			difftest.S(0, `SAVEPOINT s`),
			difftest.Q(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR UPDATE`),
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
			difftest.Q(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR KEY SHARE`),
			difftest.S(0, `SAVEPOINT s`),
			difftest.Q(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR UPDATE`),
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
			difftest.Q(0, `SELECT sku FROM stock ORDER BY sku LIMIT 1 FOR UPDATE SKIP LOCKED`),
			difftest.Q(1, `SELECT sku FROM stock ORDER BY sku LIMIT 1 FOR UPDATE SKIP LOCKED`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		Name:   "lock strengths: key share vs no key update",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR KEY SHARE`),
			difftest.S(1, `UPDATE stock SET n = 9 WHERE sku = 'apple'`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'apple' FOR NO KEY UPDATE NOWAIT`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'apple' FOR UPDATE NOWAIT`),
			difftest.S(1, `DELETE FROM stock WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(0, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "lock strengths: share vs share and no key update",
		Schema: stockSchema, Seed: stockSeed, Conns: 3,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(1, `BEGIN`),
			difftest.Q(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR SHARE`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'apple' FOR SHARE`),
			difftest.Q(2, `SELECT sku FROM stock WHERE sku = 'apple' FOR KEY SHARE NOWAIT`),
			difftest.Q(2, `SELECT sku FROM stock WHERE sku = 'apple' FOR NO KEY UPDATE NOWAIT`),
			difftest.S(2, `UPDATE stock SET n = 3 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
			difftest.Q(2, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "lock strengths: no key update vs key share and share",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 7 WHERE sku = 'apple'`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'apple' FOR KEY SHARE NOWAIT`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'apple' FOR SHARE NOWAIT`),
			difftest.Q(1, `SELECT sku, n FROM stock WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "update of the key takes for update",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET sku = 'banana' WHERE sku = 'apple'`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'apple' FOR KEY SHARE NOWAIT`),
			difftest.S(0, `ROLLBACK`),
		},
	},
	{
		Name:   "update waits and finds the row deleted",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `DELETE FROM stock WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = n + 1 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "update waits and follows the key change",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET sku = 'banana' WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = n + 10 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "update waits and the new version now matches no longer",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 5 WHERE sku = 'pear'`),
			difftest.S(1, `UPDATE stock SET n = n * 2 WHERE n = 1`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "read committed does not see a row inserted during the wait",
		Schema: stockSchema, Seed: stockSeed, Conns: 3,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 2 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = n + 100 WHERE n >= 1`),
			difftest.S(2, `INSERT INTO stock VALUES ('kiwi', 1)`),
			difftest.S(0, `COMMIT`),
			difftest.Q(2, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "select for update waits and rechecks",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(1, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.Q(1, `SELECT sku, n FROM stock WHERE n > 0 ORDER BY sku FOR UPDATE`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		Name:   "select for update with limit after the wait",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(1, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.Q(1, `SELECT sku FROM stock WHERE n > 0 ORDER BY sku LIMIT 1 FOR UPDATE`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		Name:   "select for update of rows deleted while waiting",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `DELETE FROM stock WHERE sku = 'apple'`),
			difftest.S(1, `BEGIN`),
			difftest.Q(1, `SELECT sku FROM stock ORDER BY sku FOR UPDATE`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		Name:   "select for share waits for an update and rechecks",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.S(1, `BEGIN`),
			difftest.Q(1, `SELECT sku, n FROM stock WHERE n < 3 ORDER BY sku FOR SHARE`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		Name:   "nowait on a locked row",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR UPDATE`),
			difftest.S(1, `BEGIN`),
			difftest.Q(1, `SELECT sku FROM stock ORDER BY sku FOR UPDATE NOWAIT`),
			difftest.Q(1, `SELECT 1`),
			difftest.S(1, `ROLLBACK`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "nowait on a row with only a key share lock",
		Schema: fkSchema, Seed: fkSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `INSERT INTO child VALUES (11, 1)`),
			difftest.Q(1, `SELECT id FROM parent WHERE id = 1 FOR NO KEY UPDATE NOWAIT`),
			difftest.Q(1, `SELECT id FROM parent WHERE id = 1 FOR UPDATE NOWAIT`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "skip locked over several rows",
		Schema: stockSchema, Seed: []string{`INSERT INTO stock VALUES ('a', 1), ('b', 1), ('c', 1)`}, Conns: 3,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(1, `BEGIN`),
			difftest.Q(0, `SELECT sku FROM stock ORDER BY sku LIMIT 2 FOR UPDATE SKIP LOCKED`),
			difftest.Q(1, `SELECT sku FROM stock ORDER BY sku FOR UPDATE SKIP LOCKED`),
			difftest.Q(2, `SELECT sku FROM stock ORDER BY sku FOR SHARE SKIP LOCKED`),
			difftest.Q(2, `SELECT sku FROM stock ORDER BY sku FOR KEY SHARE SKIP LOCKED`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		Name:   "for update over a join locks both tables",
		Schema: ordSchema, Seed: ordSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `SELECT o.id FROM ord o JOIN cust c ON c.id = o.cust_id WHERE o.id = 10 FOR UPDATE`),
			difftest.Q(1, `SELECT id FROM cust WHERE id = 1 FOR UPDATE NOWAIT`),
			difftest.Q(1, `SELECT id FROM cust WHERE id = 2 FOR UPDATE NOWAIT`),
			difftest.Q(1, `SELECT id FROM ord WHERE id = 10 FOR UPDATE NOWAIT`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "for update of one table in a join",
		Schema: ordSchema, Seed: ordSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `SELECT o.id FROM ord o JOIN cust c ON c.id = o.cust_id WHERE o.id = 10 FOR UPDATE OF o`),
			difftest.Q(1, `SELECT id FROM cust WHERE id = 1 FOR UPDATE NOWAIT`),
			difftest.Q(1, `SELECT id FROM ord WHERE id = 10 FOR UPDATE NOWAIT`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "join for update waits and rechecks the joined row",
		Schema: ordSchema, Seed: ordSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE cust SET name = 'z' WHERE id = 1`),
			difftest.S(1, `BEGIN`),
			difftest.Q(1, `SELECT o.id FROM ord o JOIN cust c ON c.id = o.cust_id WHERE c.name = 'a' ORDER BY o.id FOR UPDATE`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		Name:   "update with a subquery waits",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = n + 1 WHERE sku IN (SELECT sku FROM stock WHERE n > 0)`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name: "update from a join waits",
		Schema: []string{
			`CREATE TABLE acct (id int PRIMARY KEY, bal int NOT NULL)`,
			`CREATE TABLE tx (id int PRIMARY KEY, acct int, amt int)`,
		},
		Seed:  []string{`INSERT INTO acct VALUES (1, 10), (2, 10)`, `INSERT INTO tx VALUES (1, 1, 5), (2, 2, 7)`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE acct SET bal = 0 WHERE id = 1`),
			difftest.S(1, `UPDATE acct SET bal = acct.bal - tx.amt FROM tx WHERE tx.acct = acct.id AND acct.bal >= tx.amt`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id, bal FROM acct ORDER BY id`),
		},
	},
	{
		Name:   "update from waits on the target and keeps the joined row",
		Schema: ordSchema, Seed: ordSeed, Conns: 3,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE ord SET qty = 100 WHERE id = 10`),
			difftest.S(2, `UPDATE cust SET name = 'z' WHERE id = 1`),
			difftest.S(1, `UPDATE ord SET qty = ord.qty + 1 FROM cust WHERE cust.id = ord.cust_id AND cust.name = 'a'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id, qty FROM ord ORDER BY id`),
		},
	},
	{
		Name:   "delete returning after a wait",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.Q(1, `DELETE FROM stock WHERE n > 0 RETURNING sku`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "delete waits on delete",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `DELETE FROM stock WHERE sku = 'apple'`),
			difftest.S(1, `DELETE FROM stock WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "insert select reads committed rows only",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `INSERT INTO stock VALUES ('kiwi', 1)`),
			difftest.S(1, `INSERT INTO stock SELECT sku || '2', n FROM stock`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "read committed sees commits between statements",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = 42 WHERE sku = 'apple'`),
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple'`),
			difftest.S(0, `UPDATE stock SET n = n + 1 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT n FROM stock WHERE sku = 'apple'`),
		},
	},
	{
		Name:   "uncommitted changes are invisible",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `INSERT INTO stock VALUES ('kiwi', 3)`),
			difftest.S(0, `DELETE FROM stock WHERE sku = 'pear'`),
			difftest.S(0, `UPDATE stock SET n = 9 WHERE sku = 'apple'`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
			difftest.Q(0, `SELECT sku, n FROM stock ORDER BY sku`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "update in a transaction sees its own writes",
		Schema: stockSchema, Seed: stockSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = n + 1`),
			difftest.S(0, `UPDATE stock SET n = n * 10 WHERE n = 2`),
			difftest.S(0, `DELETE FROM stock WHERE n = 20 AND sku = 'pear'`),
			difftest.S(0, `INSERT INTO stock VALUES ('pear', 0)`),
			difftest.Q(0, `SELECT sku, n FROM stock ORDER BY sku`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "optimistic locking by version",
		Schema: []string{`CREATE TABLE doc (id int PRIMARY KEY, ver int NOT NULL, body text)`},
		Seed:   []string{`INSERT INTO doc VALUES (1, 1, 'a')`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(1, `BEGIN`),
			difftest.S(0, `UPDATE doc SET body = 'x', ver = ver + 1 WHERE id = 1 AND ver = 1`),
			difftest.S(1, `UPDATE doc SET body = 'y', ver = ver + 1 WHERE id = 1 AND ver = 1`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
			difftest.S(0, `UPDATE doc SET body = body WHERE id = 1`),
			difftest.Q(0, `SELECT ver, body FROM doc`),
		},
	},
	{
		Name: "check constraint violation",
		Schema: []string{
			`CREATE TABLE acct (id int PRIMARY KEY, bal int NOT NULL CHECK (bal >= 0))`,
		},
		Seed:  []string{`INSERT INTO acct VALUES (1, 10)`},
		Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE acct SET bal = bal - 6 WHERE id = 1`),
			difftest.S(1, `UPDATE acct SET bal = bal - 6 WHERE id = 1`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `UPDATE acct SET bal = NULL WHERE id = 1`),
			difftest.Q(1, `SELECT bal FROM acct`),
		},
	},
	{
		Name:   "savepoint rollback releases a row lock",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `SAVEPOINT s`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.S(0, `ROLLBACK TO SAVEPOINT s`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "release savepoint keeps the changes",
		Schema: stockSchema, Seed: stockSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `SAVEPOINT a`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `SAVEPOINT b`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'pear'`),
			difftest.S(0, `RELEASE SAVEPOINT b`),
			difftest.S(0, `ROLLBACK TO SAVEPOINT b`),
			difftest.S(0, `ROLLBACK TO SAVEPOINT a`),
			difftest.Q(0, `SELECT sku, n FROM stock ORDER BY sku`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "savepoint with the same name twice",
		Schema: stockSchema, Seed: stockSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `SAVEPOINT a`),
			difftest.S(0, `UPDATE stock SET n = 7 WHERE sku = 'apple'`),
			difftest.S(0, `SAVEPOINT a`),
			difftest.S(0, `UPDATE stock SET n = 8 WHERE sku = 'apple'`),
			difftest.S(0, `ROLLBACK TO SAVEPOINT a`),
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple'`),
			difftest.S(0, `RELEASE SAVEPOINT a`),
			difftest.S(0, `ROLLBACK TO SAVEPOINT a`),
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "statement in an aborted transaction",
		Schema: stockSchema, Seed: stockSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `SELECT 1 / 0`),
			difftest.S(0, `UPDATE stock SET n = 0`),
			difftest.S(0, `SAVEPOINT s`),
			difftest.S(0, `ROLLBACK`),
			difftest.S(0, `BEGIN`),
			difftest.S(0, `SAVEPOINT s`),
			difftest.Q(0, `SELECT 1 / 0`),
			difftest.S(0, `ROLLBACK TO SAVEPOINT s`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(0, `SELECT sku, n FROM stock ORDER BY sku`),
			difftest.S(0, `BEGIN`),
			difftest.S(0, `ROLLBACK TO SAVEPOINT nope`),
			difftest.S(0, `ROLLBACK`),
		},
	},
	{
		Name:   "advisory xact lock",
		Schema: stockSchema, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `SELECT 1 FROM (SELECT pg_advisory_xact_lock(1)) s`),
			difftest.S(1, `BEGIN`),
			difftest.Q(1, `SELECT pg_try_advisory_xact_lock(1)`),
			difftest.Q(1, `SELECT pg_try_advisory_xact_lock(2)`),
			difftest.Q(1, `SELECT 1 FROM (SELECT pg_advisory_xact_lock(1)) s`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
		},
	},
	{
		Name:   "shared locks upgraded deadlock",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(1, `BEGIN`),
			difftest.Q(0, `SELECT sku FROM stock WHERE sku = 'apple' FOR SHARE`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'apple' FOR SHARE`),
			difftest.S(0, `UPDATE stock SET n = 2 WHERE sku = 'apple'`),
			difftest.S(1, `UPDATE stock SET n = 3 WHERE sku = 'apple'`),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
			difftest.Q(0, `SELECT n FROM stock WHERE sku = 'apple'`),
		},
		Pauses: []map[int]time.Duration{{5: 1500 * time.Millisecond}},
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
