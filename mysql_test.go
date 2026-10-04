package detest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
	"github.com/k1LoW/detest/mysql"
	"github.com/k1LoW/detest/postgres"
)

func TestMySQLStatements(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE `items` (`id` BIGINT AUTO_INCREMENT PRIMARY KEY, `sku` VARCHAR(20) NOT NULL, `qty` TINYINT UNSIGNED NOT NULL DEFAULT 0, UNIQUE KEY `uk_sku` (`sku`), KEY `idx_qty` (`qty`)) ENGINE=InnoDB")

	res, err := db.Exec("INSERT INTO items (sku, qty) VALUES (?, ?), (?, ?)", "a", 1, "b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := res.LastInsertId(); err != nil || id != 1 {
		t.Fatalf("LastInsertId %d, %v, want the first generated id", id, err)
	}
	mustExec(t, db, "INSERT INTO items (id, sku) VALUES (10, 'c')")
	res, err = db.Exec("INSERT INTO items (sku) VALUES ('d')")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 11 {
		t.Fatalf("id after an explicit 10: %d, want 11", id)
	}
	var last int64
	if err := db.QueryRow("SELECT LAST_INSERT_ID()").Scan(&last); err != nil || last != 11 {
		t.Fatalf("LAST_INSERT_ID() = %d, %v", last, err)
	}

	mustExec(t, db, "INSERT INTO items (sku, qty) VALUES ('a', 5) ON DUPLICATE KEY UPDATE qty = qty + VALUES(qty)")
	mustExec(t, db, "INSERT IGNORE INTO items (sku, qty) VALUES ('b', 9)")
	if got := peekRows(store, "items", "sku", "qty"); got != "a:6 b:2 c:0 d:0" {
		t.Fatalf("after upserts: %s", got)
	}
	mustExec(t, db, "UPDATE items SET qty = 7 WHERE qty = 0 ORDER BY id DESC LIMIT 1")
	if got := peekRows(store, "items", "sku", "qty"); got != "a:6 b:2 c:0 d:7" {
		t.Fatalf("UPDATE ... ORDER BY ... LIMIT: %s", got)
	}

	var se *DBError
	if _, err := db.Exec("INSERT INTO items (sku) VALUES ('a')"); !errors.As(err, &se) || se.Number != 1062 || se.Code != "23000" {
		t.Fatalf("duplicate: %#v", err)
	}
	if _, err := db.Exec("INSERT INTO items (sku, qty) VALUES ('e', 300)"); !errors.As(err, &se) || se.Number != 1264 {
		t.Fatalf("out of range: %#v", err)
	}
	if _, err := db.Exec("SELEC 1"); !errors.As(err, &se) || se.Number != 1064 {
		t.Fatalf("syntax error: %#v", err)
	}

	// MySQL sorts NULL first when ascending.
	mustExec(t, db, "CREATE TABLE notes (id INT PRIMARY KEY, body TEXT)")
	mustExec(t, db, "INSERT INTO notes VALUES (1, 'x'), (2, NULL)")
	var first int
	if err := db.QueryRow("SELECT id FROM notes ORDER BY body LIMIT 1").Scan(&first); err != nil || first != 2 {
		t.Fatalf("first by body: %d, %v", first, err)
	}
}

// mysqlBin is a MySQL server whose default collation is utf8mb4_bin, so the
// tests of other behaviors compare strings as detest does; the collation
// tests use mysql.New's default.
func mysqlBin(opts ...mysql.Option) Server {
	return mysql.New(append([]mysql.Option{mysql.Collation("utf8mb4_bin")}, opts...)...)
}

func peekRows(store *DB, table, key, col string) string {
	var out []string
	for _, r := range store.Peek(table) {
		out = append(out, fmt.Sprintf("%v:%v", r[key], r[col]))
	}
	sortStrs(out)
	return strings.Join(out, " ")
}

func sortStrs(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// A failed statement rolls back only itself; the transaction goes on.
func TestMySQLStatementRollback(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO t VALUES (1), (2)"); !errors.Is(err, ErrUniqueViolation) {
		t.Fatalf("got %v", err)
	}
	if _, err := tx.Exec("INSERT INTO t VALUES (3)"); err != nil {
		t.Fatalf("after a failed statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := peekRows(store, "t", "id", "id"); got != "1:1 3:3" {
		t.Fatalf("committed %s", got)
	}
}

// mysqlStockModel declares two buyers that read the stock and write back what is
// left, and the invariant that the stock went down by what was sold. A buyer
// that fails, as a deadlock victim does, sells nothing.
func mysqlStockModel(iso IsolationLevel, update string) func(t *testing.T, s *Sim) {
	return func(t *testing.T, s *Sim) {
		db, store := s.DB("shop", mysqlBin(mysql.Isolation(iso)))
		mustExec(t, db, "CREATE TABLE stock (sku VARCHAR(10) PRIMARY KEY, n INT NOT NULL)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO stock VALUES ('apple', 2)") })
		sold := 0
		s.Seed(func() { sold = 0 })
		for _, buyer := range []string{"alice", "bob"} {
			s.Manual(buyer, 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				var n int
				if err := tx.QueryRow("SELECT n FROM stock WHERE sku = 'apple'").Scan(&n); err != nil {
					return err
				}
				if _, err := tx.Exec(update, n-1); errors.Is(err, ErrDeadlock) {
					return nil
				} else if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				sold++
				return nil
			})
		}
		s.AtQuiescence(func(st *State) error {
			if row, _ := st.Row(store, "stock", "apple"); row.Int64("n") != int64(2-sold) {
				return fmt.Errorf("lost update: sold %d, %d left", sold, row.Int64("n"))
			}
			return nil
		})
	}
}

// A consistent read takes its value from the snapshot, so writing it back
// loses a concurrent update even at Repeatable Read, while an UPDATE that
// computes from the row reads the latest version. At Serializable the read
// takes a shared lock, and the two buyers deadlock instead.
func TestMySQLLostUpdate(t *testing.T) {
	writeBack := "UPDATE stock SET n = ? WHERE sku = 'apple'"
	Explore(t, func(t *testing.T, s *Sim) {
		mysqlStockModel(RepeatableRead, writeBack)(t, s)
		s.ExpectViolation("lost update")
	})
	Explore(t, mysqlStockModel(RepeatableRead, "UPDATE stock SET n = n - 1 WHERE sku = 'apple' AND ? IS NOT NULL"))
	Explore(t, mysqlStockModel(Serializable, writeBack))
}

// At Repeatable Read a transaction reads the same rows twice whatever
// commits in between; at Read Committed it may not.
func TestMySQLSnapshot(t *testing.T) {
	model := func(iso IsolationLevel) func(t *testing.T, s *Sim) {
		return func(t *testing.T, s *Sim) {
			db, _ := s.DB("app", mysqlBin(mysql.Isolation(iso)))
			mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
			var first, second int
			s.Seed(func() { first, second = -1, -1 })
			s.Manual("reader", 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				if err := tx.QueryRow("SELECT COUNT(*) FROM t").Scan(&first); err != nil {
					return err
				}
				if err := tx.QueryRow("SELECT COUNT(*) FROM t").Scan(&second); err != nil {
					return err
				}
				return tx.Commit()
			})
			s.Manual("writer", 1, func(p *Proc) error {
				_, err := db.ExecContext(p.Context(), "INSERT INTO t VALUES (1)")
				return err
			})
			s.Always(func(*State) error {
				if second >= 0 && first != second {
					return fmt.Errorf("read %d then %d", first, second)
				}
				return nil
			})
		}
	}
	Explore(t, model(RepeatableRead))
	Explore(t, func(t *testing.T, s *Sim) {
		model(ReadCommitted)(t, s)
		s.ExpectViolation("read 0 then 1")
	})
}

// Two transactions check that a row is missing with a locking read, then
// insert it. Both hold the gap, so each insert waits for the other: InnoDB's
// classic deadlock, which Read Committed does not have.
func TestMySQLGapLockDeadlock(t *testing.T) {
	model := func(iso IsolationLevel) func(t *testing.T, s *Sim) {
		return func(t *testing.T, s *Sim) {
			db, _ := s.DB("app", mysqlBin(mysql.Isolation(iso)))
			mustExec(t, db, "CREATE TABLE accounts (id INT PRIMARY KEY, owner VARCHAR(10))")
			s.Seed(func() { mustExec(t, db, "INSERT INTO accounts VALUES (1, 'x'), (9, 'y')") })
			var errs []error
			s.Seed(func() { errs = nil })
			for i, name := range []string{"a", "b"} {
				id := 4 + i
				s.Manual(name, 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					var n int
					if err := tx.QueryRow("SELECT COUNT(*) FROM accounts WHERE id = ? FOR UPDATE", id).Scan(&n); err != nil {
						return err
					}
					if n == 0 {
						if _, err := tx.Exec("INSERT INTO accounts VALUES (?, ?)", id, name); err != nil {
							errs = append(errs, err)
							return nil
						}
					}
					return tx.Commit()
				})
			}
			s.AtQuiescence(func(*State) error {
				if len(errs) == 0 {
					return nil
				}
				var se *DBError
				if errors.As(errs[0], &se) && se.Number == 1213 {
					return fmt.Errorf("deadlock: %w", errs[0])
				}
				return errs[0]
			})
		}
	}
	Explore(t, func(t *testing.T, s *Sim) {
		model(RepeatableRead)(t, s)
		s.ExpectViolation("deadlock")
	})
	Explore(t, model(ReadCommitted))
}

// A locking read of a range keeps rows out of it until the transaction ends,
// so reading it again finds the same rows.
func TestMySQLNoPhantom(t *testing.T) {
	model := func(iso IsolationLevel) func(t *testing.T, s *Sim) {
		return func(t *testing.T, s *Sim) {
			db, _ := s.DB("app", mysqlBin(mysql.Isolation(iso)))
			mustExec(t, db, "CREATE TABLE events (id INT PRIMARY KEY, at INT NOT NULL, KEY idx_at (at))")
			s.Seed(func() { mustExec(t, db, "INSERT INTO events VALUES (1, 10), (2, 30)") })
			var first, second int
			s.Seed(func() { first, second = -1, -1 })
			s.Manual("reader", 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				if err := tx.QueryRow("SELECT COUNT(*) FROM events WHERE at BETWEEN 5 AND 20 FOR UPDATE").Scan(&first); err != nil {
					return err
				}
				if err := tx.QueryRow("SELECT COUNT(*) FROM events WHERE at BETWEEN 5 AND 20 FOR UPDATE").Scan(&second); err != nil {
					return err
				}
				return tx.Commit()
			})
			s.Manual("writer", 1, func(p *Proc) error {
				_, err := db.ExecContext(p.Context(), "INSERT INTO events VALUES (3, 15)")
				return err
			})
			s.Always(func(*State) error {
				if second >= 0 && first != second {
					return fmt.Errorf("phantom: %d then %d", first, second)
				}
				return nil
			})
		}
	}
	Explore(t, model(RepeatableRead))
	Explore(t, func(t *testing.T, s *Sim) {
		model(ReadCommitted)(t, s)
		s.ExpectViolation("phantom")
	})
}

// A deadlock rolls back the whole transaction in InnoDB, not just the
// statement.
func TestMySQLDeadlockRollsBackTheTransaction(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		mustExec(t, db, "CREATE TABLE log (id INT PRIMARY KEY)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 0), (2, 0)") })
		for i, order := range [][2]int{{1, 2}, {2, 1}} {
			mark := 10 + i
			s.Manual(fmt.Sprintf("tx%d", i), 1, func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				if _, err := tx.Exec("INSERT INTO log VALUES (?)", mark); err != nil {
					return err
				}
				for _, id := range order {
					if _, err := tx.Exec("UPDATE t SET v = v + 1 WHERE id = ?", id); err != nil {
						if errors.Is(err, ErrDeadlock) {
							return tx.Commit() // commits whatever is left
						}
						return err
					}
				}
				return tx.Commit()
			})
		}
		s.AtQuiescence(func(st *State) error {
			// Every logged transaction did both updates: the victim's insert
			// into log went with the rest of it.
			logged := len(st.Rows(store, "log"))
			a, _ := st.Row(store, "t", int64(1))
			if int64(logged) != a.Int64("v") {
				return fmt.Errorf("%d logged, %d applied", logged, a.Int64("v"))
			}
			return nil
		})
	})
}

// The statement shapes of MySQL applications, migrations and dumps run.
func TestMySQLStatementShapes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "/*!40101 SET NAMES utf8mb4 */;\n"+
		"DROP TABLE IF EXISTS `orders`;\n"+
		"CREATE TABLE `users` (\n"+
		"  `id` bigint unsigned NOT NULL AUTO_INCREMENT,\n"+
		"  `email` varchar(255) COLLATE utf8mb4_bin NOT NULL,\n"+
		"  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,\n"+
		"  PRIMARY KEY (`id`),\n"+
		"  UNIQUE KEY `users_email_unique` (`email`)\n"+
		") ENGINE=InnoDB AUTO_INCREMENT=3 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;\n"+
		"CREATE TABLE `orders` (\n"+
		"  `id` bigint unsigned NOT NULL AUTO_INCREMENT,\n"+
		"  `user_id` bigint unsigned NOT NULL,\n"+
		"  `total` decimal(10,2) NOT NULL,\n"+
		"  `status` enum('new','paid') NOT NULL DEFAULT 'new',\n"+
		"  PRIMARY KEY (`id`),\n"+
		"  KEY `orders_user_id_index` (`user_id`),\n"+
		"  CONSTRAINT `orders_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE\n"+
		") ENGINE=InnoDB;\n"+
		"LOCK TABLES `users` WRITE;\nUNLOCK TABLES;\n"+
		"ALTER TABLE orders ADD COLUMN note TEXT NULL AFTER total, ADD INDEX idx_status (status);\n"+
		"CREATE INDEX idx_created ON users (created_at);\n")
	for _, q := range []string{
		"INSERT INTO users (email) VALUES ('a@x'), ('b@x')",
		"INSERT INTO orders (user_id, total) SELECT id, 10.5 FROM users WHERE email = 'a@x'",
		"SELECT u.email, COUNT(o.id) AS n, SUM(o.total) FROM users u LEFT JOIN orders o ON o.user_id = u.id GROUP BY u.email HAVING n >= 0 ORDER BY n DESC",
		"SELECT * FROM users WHERE id IN (SELECT user_id FROM orders WHERE status = 'new') AND EXISTS (SELECT 1 FROM orders)",
		"SELECT id, ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY id) FROM orders",
		"WITH t AS (SELECT id FROM users) SELECT id FROM t UNION ALL SELECT id FROM orders",
		"SELECT IF(status = 'paid', 1, 0), IFNULL(note, ''), CAST(total AS SIGNED), NOW() FROM orders",
		"SELECT * FROM users WHERE email LIKE 'a%' AND id BETWEEN 1 AND 10 AND created_at IS NOT NULL LIMIT 5 OFFSET 0",
		"UPDATE orders o JOIN users u ON u.id = o.user_id SET o.status = 'paid' WHERE u.email = 'a@x'",
		"DELETE FROM orders WHERE status = 'paid' ORDER BY id LIMIT 10",
		"SELECT * FROM users WHERE id = 1 FOR UPDATE SKIP LOCKED",
		"SELECT * FROM users WHERE id = 1 LOCK IN SHARE MODE",
		"SET SESSION innodb_lock_wait_timeout = 5",
		"SELECT COUNT(*) FROM users WHERE email <=> NULL",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{"SAVEPOINT sp1", "INSERT INTO users (email) VALUES ('c@x')", "ROLLBACK TO SAVEPOINT sp1", "RELEASE SAVEPOINT sp1"} {
		if _, err := tx.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// MySQL tests numbers and strings as conditions, and XOR compares truth
// values.
func TestMySQLTruthValues(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, flag TINYINT, CHECK (flag))")
	mustExec(t, db, "INSERT INTO t VALUES (1, 1), (2, NULL)")
	if _, err := db.Exec("INSERT INTO t VALUES (3, 0)"); !errors.Is(err, ErrCheckViolation) {
		t.Errorf("CHECK (flag) with 0: %v", err)
	}
	for q, want := range map[string]int{
		"SELECT COUNT(*) FROM t WHERE 1":              2,
		"SELECT COUNT(*) FROM t WHERE 0":              0,
		"SELECT COUNT(*) FROM t WHERE flag":           1,
		"SELECT COUNT(*) FROM t WHERE NOT flag":       0,
		"SELECT COUNT(*) FROM t WHERE '2abc' AND id":  2,
		"SELECT COUNT(*) FROM t WHERE IF(flag, 1, 0)": 1,
		"SELECT COUNT(*) FROM t WHERE 2 XOR 3":        0,
		"SELECT COUNT(*) FROM t WHERE 2 XOR 0":        2,
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d, %v, want %d", q, n, err, want)
		}
	}
}

func TestMySQLSchemaAndSessionState(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())

	// A table dropped and created again does not read the old one's rows
	// from a snapshot.
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO t VALUES (1)")
	mustExec(t, db, "DROP TABLE t")
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); err != nil || n != 0 {
		t.Errorf("rows of the dropped table: %d, %v", n, err)
	}

	// MODIFY without AUTO_INCREMENT removes it; ALTER COLUMN of the default
	// leaves it.
	mustExec(t, db, "CREATE TABLE a (id BIGINT AUTO_INCREMENT PRIMARY KEY, v INT)")
	mustExec(t, db, "ALTER TABLE a ALTER COLUMN id DROP DEFAULT")
	mustExec(t, db, "INSERT INTO a (v) VALUES (1)")
	mustExec(t, db, "ALTER TABLE a MODIFY id BIGINT NOT NULL")
	if _, err := db.Exec("INSERT INTO a (v) VALUES (2)"); !errors.Is(err, ErrNotNullViolation) {
		t.Errorf("insert after AUTO_INCREMENT was removed: %v", err)
	}

	// Each statement of a multi-statement query is its own: the first
	// commits though the second fails.
	mustExec(t, db, "CREATE TABLE m (id INT PRIMARY KEY)")
	if _, err := db.Exec("INSERT INTO m VALUES (1); INSERT INTO m VALUES (1)"); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("second statement: %v", err)
	}
	if got := peekRows(store, "m", "id", "id"); got != "1:1" {
		t.Errorf("after the multi-statement query: %s", got)
	}
	// The one result set is the SELECT's, whatever runs after it.
	var mid int
	if err := db.QueryRow("SELECT id FROM m; SET NAMES utf8mb4").Scan(&mid); err != nil || mid != 1 {
		t.Errorf("SELECT then SET: %d, %v", mid, err)
	}

	// A row alias names the proposed row only in its ON DUPLICATE KEY
	// UPDATE; a later statement may use the name for a table.
	mustExec(t, db, "CREATE TABLE kv (k INT PRIMARY KEY, v VARCHAR(10))")
	mustExec(t, db, "INSERT INTO kv VALUES (1, 'a') AS new ON DUPLICATE KEY UPDATE v = new.v; UPDATE kv AS new SET v = 'z' WHERE new.k = 1")
	if got := peekRows(store, "kv", "k", "v"); got != "1:z" {
		t.Errorf("after the alias was reused: %s", got)
	}

	// An insert that adds no row reports no generated id.
	mustExec(t, db, "CREATE TABLE u (id BIGINT AUTO_INCREMENT PRIMARY KEY, email VARCHAR(20), UNIQUE KEY uk (email))")
	mustExec(t, db, "INSERT INTO u (email) VALUES ('a')")
	res, err := db.Exec("INSERT IGNORE INTO u (email) VALUES ('a')")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 0 {
		t.Errorf("LastInsertId of an ignored row: %d", id)
	}
	res, err = db.Exec("INSERT IGNORE INTO u (email) VALUES ('a'), ('b')")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 4 {
		t.Errorf("LastInsertId of the row inserted after an ignored one: %d, want 4", id)
	}
}

// Conditions on the same column narrow each other, so a locking read of
// id IN (1, 2) AND id = 2 leaves row 1 to others.
func TestMySQLLockedRangeIntersects(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 0), (2, 0)") })
		open, wrote := false, false
		s.Seed(func() { open, wrote = false, false })
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.Query("SELECT id FROM t WHERE id IN (1, 2) AND id = 2 FOR UPDATE")
			if err != nil {
				return err
			}
			_ = rows.Close()
			open = true
			p.Step("holds its locks")
			open = false
			return tx.Commit()
		})
		s.Manual("writer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "UPDATE t SET v = 1 WHERE id = 1")
			wrote = err == nil
			return err
		})
		s.Sometimes("row 1 written while the reader holds its locks", func(*State) bool { return open && wrote })
	})
}

// ROLLBACK TO SAVEPOINT keeps InnoDB's locks, so a writer still waits for
// the transaction to end.
func TestMySQLSavepointKeepsLocks(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 0)") })
		open, broke := false, false
		s.Seed(func() { open, broke = false, false })
		s.Manual("locker", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			for _, q := range []string{"SAVEPOINT sp", "SELECT id FROM t WHERE id = 1 FOR UPDATE", "ROLLBACK TO SAVEPOINT sp"} {
				if _, err := tx.Exec(q); err != nil {
					return err
				}
			}
			open = true
			p.Step("goes on after the rollback to the savepoint")
			open = false
			return tx.Commit()
		})
		s.Manual("writer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "UPDATE t SET v = 1 WHERE id = 1")
			broke = broke || err == nil && open
			return err
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("the row was written while its lock was held")
			}
			return nil
		})
	})
}

// SET SESSION innodb_lock_wait_timeout lasts for the connection, so a later
// transaction on it can time out waiting for a lock.
func TestMySQLSessionLockTimeout(t *testing.T) {
	timedOut := false
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 0)") })
		s.Manual("holder", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("UPDATE t SET v = 1 WHERE id = 1"); err != nil {
				return err
			}
			p.Step("holds the row")
			return tx.Commit()
		})
		s.Manual("waiter", 1, func(p *Proc) error {
			conn, err := db.Conn(p.Context())
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.ExecContext(p.Context(), "SET SESSION innodb_lock_wait_timeout = 1"); err != nil {
				return err
			}
			_, err = conn.ExecContext(p.Context(), "UPDATE t SET v = 2 WHERE id = 1")
			if errors.Is(err, ErrLockNotAvailable) {
				timedOut = true
				return nil
			}
			return err
		})
	})
	if !timedOut {
		t.Fatal("no run where the session's lock timeout fired")
	}
}

func TestMySQLSecondReviewFixes(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())

	// AUTO_INCREMENT counters follow their table through a rename, start
	// over for a table created again, and count apart across databases.
	mustExec(t, db, "CREATE TABLE a (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO a (v) VALUES (1), (2)")
	mustExec(t, db, "RENAME TABLE a TO b")
	res, err := db.Exec("INSERT INTO b (v) VALUES (3)")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 3 {
		t.Errorf("id after a rename: %d, want 3", id)
	}
	mustExec(t, db, "DROP TABLE b")
	mustExec(t, db, "CREATE TABLE b (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	mustExec(t, db, "CREATE TABLE other.b (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	for _, q := range []string{"INSERT INTO b (v) VALUES (1)", "INSERT INTO other.b (v) VALUES (1)"} {
		res, err := db.Exec(q)
		if err != nil {
			t.Fatal(err)
		}
		if id, _ := res.LastInsertId(); id != 1 {
			t.Errorf("%s: id %d, want 1", q, id)
		}
	}

	// A prefix part of a unique index compares the prefix.
	mustExec(t, db, "CREATE TABLE names (id INT PRIMARY KEY, name VARCHAR(10), UNIQUE KEY uk (name(3)))")
	mustExec(t, db, "INSERT INTO names VALUES (1, 'abc1')")
	if _, err := db.Exec("INSERT INTO names VALUES (2, 'abc2')"); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("same prefix: %v", err)
	}

	// DROP FOREIGN KEY drops the foreign key, not the index of its name.
	mustExec(t, db, "CREATE TABLE parents (id INT PRIMARY KEY)")
	mustExec(t, db, "CREATE TABLE kids (id INT PRIMARY KEY, parent_id INT, KEY fk (parent_id), CONSTRAINT fk FOREIGN KEY (parent_id) REFERENCES parents (id))")
	mustExec(t, db, "ALTER TABLE kids DROP FOREIGN KEY fk")
	mustExec(t, db, "INSERT INTO kids VALUES (1, 99)")

	// Integer columns take fractions rounded, and check the rounded range.
	mustExec(t, db, "CREATE TABLE nums (id INT PRIMARY KEY, small TINYINT UNSIGNED, n INT)")
	if _, err := db.Exec("INSERT INTO nums VALUES (1, 300.0, 0)"); !errors.Is(err, ErrNumericValueOutOfRange) {
		t.Errorf("300.0 into TINYINT UNSIGNED: %v", err)
	}
	mustExec(t, db, "INSERT INTO nums VALUES (2, 1, 1.5)")
	if got := peekRows(store, "nums", "id", "n"); got != "2:2" {
		t.Errorf("1.5 into INT: %s", got)
	}

	// A LIMIT that is not a count is refused rather than read as none.
	if _, err := db.Exec("UPDATE nums SET n = 0 LIMIT ?", -1); !errors.Is(err, ErrInvalidParameterValue) {
		t.Errorf("negative LIMIT: %v", err)
	}

	// Leading whitespace before a number, and IS FALSE of a number.
	for q, want := range map[string]int{
		"SELECT COUNT(*) FROM nums WHERE ' 1'":       1,
		"SELECT COUNT(*) FROM nums WHERE 0 IS FALSE": 1,
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d, %v, want %d", q, n, err, want)
		}
	}
}

// A locking read over a join locks the rows it read from each table, on
// either server.
func TestLockingJoin(t *testing.T) {
	for _, srv := range []struct {
		name string
		s    func() Server
	}{
		{"mysql", func() Server { return mysqlBin() }},
		{"postgres", func() Server { return postgres.New() }},
	} {
		t.Run(srv.name, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", srv.s())
				mustExec(t, db, "CREATE TABLE a (id INT PRIMARY KEY)")
				mustExec(t, db, "CREATE TABLE b (id INT PRIMARY KEY, a_id INT, v INT)")
				s.Seed(func() { mustExec(t, db, "INSERT INTO a VALUES (1)") })
				s.Seed(func() { mustExec(t, db, "INSERT INTO b VALUES (1, 1, 0)") })
				open, broke := false, false
				s.Seed(func() { open, broke = false, false })
				s.Manual("reader", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					rows, err := tx.Query("SELECT a.id FROM a JOIN b ON b.a_id = a.id FOR UPDATE")
					if err != nil {
						return err
					}
					_ = rows.Close()
					open = true
					p.Step("holds the joined rows")
					open = false
					return tx.Commit()
				})
				s.Manual("writer", 1, func(p *Proc) error {
					_, err := db.ExecContext(p.Context(), "UPDATE b SET v = 1 WHERE id = 1")
					broke = broke || err == nil && open
					return err
				})
				s.AtQuiescence(func(*State) error {
					if broke {
						return fmt.Errorf("b's row was written while the join held it")
					}
					return nil
				})
			})
		})
	}
}

func TestMySQLThirdReviewFixes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())

	// CHANGE renames the column and gives it the new definition, leaving one
	// column.
	mustExec(t, db, "CREATE TABLE c (id INT PRIMARY KEY, a INT)")
	mustExec(t, db, "ALTER TABLE c CHANGE a b INT NOT NULL")
	if _, err := db.Exec("INSERT INTO c (id, b) VALUES (1, NULL)"); !errors.Is(err, ErrNotNullViolation) {
		t.Errorf("NULL into the changed column: %v", err)
	}
	if _, err := db.Exec("INSERT INTO c (id, a) VALUES (3, 1)"); err == nil {
		t.Error("the old column is still there")
	}

	// A string that is not an integer is refused by an integer column.
	if _, err := db.Exec("INSERT INTO c VALUES (2, 'abc')"); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("'abc' into INT: %v", err)
	}
}

// NOWAIT and SKIP LOCKED change how a locking read waits for rows, not the
// gaps it locks, so it still sees no phantom.
func TestMySQLWaitPolicyKeepsGaps(t *testing.T) {
	for _, policy := range []string{"NOWAIT", "SKIP LOCKED"} {
		t.Run(policy, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", mysqlBin())
				mustExec(t, db, "CREATE TABLE events (id INT PRIMARY KEY, at INT NOT NULL, KEY idx_at (at))")
				s.Seed(func() { mustExec(t, db, "INSERT INTO events VALUES (1, 10), (2, 30)") })
				var first, second int
				s.Seed(func() { first, second = -1, -1 })
				q := "SELECT COUNT(*) FROM events WHERE at BETWEEN 5 AND 20 FOR UPDATE " + policy
				s.Manual("reader", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if err := tx.QueryRow(q).Scan(&first); err != nil {
						return err
					}
					if err := tx.QueryRow(q).Scan(&second); err != nil {
						return err
					}
					return tx.Commit()
				})
				s.Manual("writer", 1, func(p *Proc) error {
					_, err := db.ExecContext(p.Context(), "INSERT INTO events VALUES (3, 15)")
					return err
				})
				s.Always(func(*State) error {
					if second >= 0 && first != second {
						return fmt.Errorf("phantom: %d then %d", first, second)
					}
					return nil
				})
			})
		})
	}
}

// FOR UPDATE OF locks the rows of the tables it names only.
func TestLockingJoinOf(t *testing.T) {
	for _, srv := range []struct {
		name string
		s    func() Server
	}{
		{"mysql", func() Server { return mysqlBin() }},
		{"postgres", func() Server { return postgres.New() }},
	} {
		t.Run(srv.name, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", srv.s())
				mustExec(t, db, "CREATE TABLE a (id INT PRIMARY KEY)")
				mustExec(t, db, "CREATE TABLE b (id INT PRIMARY KEY, a_id INT, v INT)")
				s.Seed(func() { mustExec(t, db, "INSERT INTO a VALUES (1)") })
				s.Seed(func() { mustExec(t, db, "INSERT INTO b VALUES (1, 1, 0)") })
				open, wrote := false, false
				s.Seed(func() { open, wrote = false, false })
				s.Manual("reader", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					rows, err := tx.Query("SELECT a.id FROM a JOIN b ON b.a_id = a.id FOR UPDATE OF a")
					if err != nil {
						return err
					}
					_ = rows.Close()
					open = true
					p.Step("holds a's rows")
					open = false
					return tx.Commit()
				})
				s.Manual("writer", 1, func(p *Proc) error {
					_, err := db.ExecContext(p.Context(), "UPDATE b SET v = 1 WHERE id = 1")
					wrote = wrote || err == nil && open
					return err
				})
				s.Sometimes("b written while the reader holds a's rows", func(*State) bool { return wrote })
			})
		})
	}
}

// An aggregate under a locking read follows the clause's wait policy: SKIP
// LOCKED leaves a held row out, NOWAIT fails on it.
func TestMySQLLockingAggregateWaitPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy string
		want   func(n int, err error) bool
	}{
		{"SKIP LOCKED", func(n int, err error) bool { return err == nil && n == 1 }},
		{"NOWAIT", func(_ int, err error) bool { return errors.Is(err, ErrLockNotAvailable) }},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", mysqlBin())
				mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
				s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1), (2)") })
				open, held := false, false
				s.Seed(func() { open, held = false, false })
				s.Manual("holder", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if _, err := tx.Exec("SELECT id FROM t WHERE id = 1 FOR UPDATE"); err != nil {
						return err
					}
					open = true
					p.Step("holds row 1")
					open = false
					return tx.Commit()
				})
				s.Manual("counter", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					var n int
					err = tx.QueryRow("SELECT COUNT(*) FROM t FOR UPDATE " + tc.policy).Scan(&n)
					held = held || open && tc.want(n, err)
					return nil
				})
				s.Sometimes(tc.policy+" while row 1 is held", func(*State) bool { return held })
			})
		})
	}
}

// MySQL's UPDATE assigns left to right, Postgres's against the old row.
func TestUpdateAssignmentOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Server
		want string
	}{
		{"mysql", mysqlBin(), "1:1"},
		{"postgres", postgres.New(), "1:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(t)
			db, store := s.DB("app", tc.s)
			mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, a INT, b INT)")
			mustExec(t, db, "INSERT INTO t VALUES (1, 0, 0)")
			mustExec(t, db, "UPDATE t SET a = a + 1, b = a")
			if got := peekRows(store, "t", "id", "b"); got != tc.want {
				t.Errorf("b: %s, want %s", got, tc.want)
			}
			if tc.name == "mysql" {
				mustExec(t, db, "INSERT INTO t VALUES (1, 0, 0) ON DUPLICATE KEY UPDATE a = a + 1, b = a")
				if got := peekRows(store, "t", "id", "b"); got != "1:2" {
					t.Errorf("b after ON DUPLICATE KEY UPDATE: %s, want 1:2", got)
				}
			}
		})
	}
}

// A locking read that waits for a row returns the row as the lock holder
// left it: a deleted row or one that no longer matches is left out.
func TestLockingReadRereadsAfterWait(t *testing.T) {
	for _, tc := range []struct {
		name  string
		s     func() Server
		query string
	}{
		{"postgres", func() Server { return postgres.New() }, "SELECT b.id FROM b WHERE b.v = 0 FOR UPDATE"},
		{"postgres join", func() Server { return postgres.New() }, "SELECT b.id FROM a JOIN b ON b.a_id = a.id WHERE b.v = 0 FOR UPDATE"},
		{"mysql join", func() Server { return mysqlBin() }, "SELECT b.id FROM a JOIN b ON b.a_id = a.id WHERE b.v = 0 FOR UPDATE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", tc.s())
				mustExec(t, db, "CREATE TABLE a (id INT PRIMARY KEY)")
				mustExec(t, db, "CREATE TABLE b (id INT PRIMARY KEY, a_id INT, v INT, w INT)")
				s.Seed(func() { mustExec(t, db, "INSERT INTO a VALUES (1)") })
				s.Seed(func() { mustExec(t, db, "INSERT INTO b VALUES (1, 1, 0, 0), (2, 1, 0, 0), (3, 1, 0, 0)") })
				broke := false
				s.Seed(func() { broke = false })
				s.Manual("writer", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if _, err := tx.Exec("UPDATE b SET v = 1 WHERE id = 1"); err != nil {
						return err
					}
					if _, err := tx.Exec("DELETE FROM b WHERE id = 2"); err != nil {
						return err
					}
					p.Step("holds b's rows")
					return tx.Commit()
				})
				s.Manual("reader", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					rows, err := tx.Query(tc.query)
					if err != nil {
						return err
					}
					var ids []int
					for rows.Next() {
						var id int
						if err := rows.Scan(&id); err != nil {
							return err
						}
						ids = append(ids, id)
					}
					_ = rows.Close()
					for _, id := range ids {
						res, err := tx.Exec(fmt.Sprintf("UPDATE b SET w = 1 WHERE id = %d AND v = 0", id))
						if err != nil {
							return err
						}
						if n, _ := res.RowsAffected(); n != 1 {
							broke = true
						}
					}
					return tx.Commit()
				})
				s.AtQuiescence(func(*State) error {
					if broke {
						return fmt.Errorf("the locking read returned a row the writer had deleted or changed")
					}
					return nil
				})
			})
		})
	}
}

func TestMySQLSixthReviewFixes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())

	// RENAME INDEX renames the index, not a foreign key of the same name.
	mustExec(t, db, "CREATE TABLE parents (id INT PRIMARY KEY)")
	mustExec(t, db, "CREATE TABLE kids (id INT PRIMARY KEY, parent_id INT, UNIQUE KEY fk (parent_id), CONSTRAINT fk FOREIGN KEY (parent_id) REFERENCES parents (id))")
	mustExec(t, db, "ALTER TABLE kids RENAME INDEX fk TO uk")
	mustExec(t, db, "ALTER TABLE kids DROP FOREIGN KEY fk")
	mustExec(t, db, "INSERT INTO kids VALUES (1, 99)")
	if _, err := db.Exec("INSERT INTO kids VALUES (2, 99)"); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("the renamed unique index: %v", err)
	}

	// A string compared with a number is compared as a number.
	for q, want := range map[string]bool{
		"SELECT '10' < 2":    false,
		"SELECT '02' = 2":    true,
		"SELECT 2 IN ('02')": true,
		"SELECT ' 3abc' > 2": true,
	} {
		var got bool
		if err := db.QueryRow(q).Scan(&got); err != nil || got != want {
			t.Errorf("%s: %v, %v, want %v", q, got, err, want)
		}
	}

	// USE of the current database does nothing; another is refused rather
	// than ignored.
	mustExec(t, db, "USE app")
	if _, err := db.Exec("USE other"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("USE other: %v", err)
	}

	// A locking subquery reads the latest rows inside a plain read that
	// reads a snapshot.
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO t VALUES (1)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); err != nil || n != 1 {
		t.Fatalf("snapshot: %d, %v", n, err)
	}
	mustExec(t, db, "INSERT INTO t VALUES (2)")
	if err := tx.QueryRow("SELECT (SELECT COUNT(*) FROM t FOR UPDATE)").Scan(&n); err != nil || n != 2 {
		t.Errorf("locking subquery: %d, %v, want 2", n, err)
	}
}

func TestMySQLSeventhReviewFixes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())

	// A simple CASE compares as MySQL compares.
	var got string
	if err := db.QueryRow("SELECT CASE 2 WHEN '02' THEN 'y' ELSE 'n' END").Scan(&got); err != nil || got != "y" {
		t.Errorf("CASE 2 WHEN '02': %q, %v", got, err)
	}

	// Renaming a table into another database is refused rather than done
	// within its own; a qualified name in the same database is a rename.
	mustExec(t, db, "CREATE TABLE a (id INT PRIMARY KEY)")
	for _, q := range []string{"RENAME TABLE a TO archive.b", "ALTER TABLE a RENAME TO archive.b"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	mustExec(t, db, "RENAME TABLE a TO app.b")
	mustExec(t, db, "ALTER TABLE app.b RENAME TO c")
	mustExec(t, db, "SELECT * FROM c")

	// TRUNCATE starts AUTO_INCREMENT over, and is refused in a transaction,
	// where MySQL would commit first.
	mustExec(t, db, "CREATE TABLE seq (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO seq (v) VALUES (1), (2)")
	mustExec(t, db, "TRUNCATE TABLE seq")
	res, err := db.Exec("INSERT INTO seq (v) VALUES (3)")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 1 {
		t.Errorf("id after TRUNCATE: %d, want 1", id)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("TRUNCATE TABLE seq"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("TRUNCATE in a transaction: %v", err)
	}
}

// The next-key locks of a search by strings on an integer index lock the
// range MySQL compares them as.
func TestMySQLGapLockCoercesStrings(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE events (id INT PRIMARY KEY, at INT NOT NULL, KEY idx_at (at))")
		s.Seed(func() { mustExec(t, db, "INSERT INTO events VALUES (1, 10), (2, 30)") })
		var first, second int
		s.Seed(func() { first, second = -1, -1 })
		q := "SELECT COUNT(*) FROM events WHERE at BETWEEN '5' AND '20' FOR UPDATE"
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if err := tx.QueryRow(q).Scan(&first); err != nil {
				return err
			}
			if err := tx.QueryRow(q).Scan(&second); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.Manual("writer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "INSERT INTO events VALUES (3, 15)")
			return err
		})
		s.Always(func(*State) error {
			if second >= 0 && first != second {
				return fmt.Errorf("phantom: %d then %d", first, second)
			}
			return nil
		})
		s.Sometimes("the reader counts", func(*State) bool { return first == 1 })
	})
}

func TestMySQLEighthReviewFixes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())

	// A column rename reaches the versions a snapshot reads.
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, a INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 5)")
	mustExec(t, db, "UPDATE t SET a = 6 WHERE id = 1")
	mustExec(t, db, "ALTER TABLE t CHANGE a b INT")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var b sql.NullInt64
	if err := tx.QueryRow("SELECT b FROM t WHERE id = 1").Scan(&b); err != nil || b.Int64 != 6 {
		t.Errorf("renamed column in a snapshot: %v, %v", b, err)
	}
	_ = tx.Rollback()

	// BIGINT refuses what is not an integer, as the other integer types do.
	mustExec(t, db, "CREATE TABLE big (id INT PRIMARY KEY, n BIGINT)")
	if _, err := db.Exec("INSERT INTO big VALUES (1, 'abc')"); !errors.Is(err, ErrInvalidTextRepresentation) {
		t.Errorf("'abc' into BIGINT: %v", err)
	}

	// AUTO_INCREMENT=n sets the next value, and never below one used.
	mustExec(t, db, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT) ENGINE=InnoDB AUTO_INCREMENT=100")
	for _, step := range []struct {
		alter string
		want  int64
	}{{"", 100}, {"ALTER TABLE ai AUTO_INCREMENT = 200", 200}, {"ALTER TABLE ai AUTO_INCREMENT = 5", 201}} {
		if step.alter != "" {
			mustExec(t, db, step.alter)
		}
		res, err := db.Exec("INSERT INTO ai (v) VALUES (0)")
		if err != nil {
			t.Fatal(err)
		}
		if id, _ := res.LastInsertId(); id != step.want {
			t.Errorf("after %q: id %d, want %d", step.alter, id, step.want)
		}
	}

	// FIRST and AFTER place a column for INSERT without a column list.
	mustExec(t, db, "CREATE TABLE p (a INT, b INT)")
	mustExec(t, db, "ALTER TABLE p ADD COLUMN c INT FIRST")
	mustExec(t, db, "ALTER TABLE p MODIFY b INT AFTER c")
	mustExec(t, db, "ALTER TABLE p CHANGE a z INT FIRST")
	mustExec(t, db, "INSERT INTO p VALUES (1, 2, 3)")
	var z, c, bb int
	if err := db.QueryRow("SELECT z, c, b FROM p").Scan(&z, &c, &bb); err != nil || z != 1 || c != 2 || bb != 3 {
		t.Errorf("column order: z=%d c=%d b=%d, %v", z, c, bb, err)
	}

	// BEGIN and COMMIT are refused in a script as alone.
	if _, err := db.Exec("BEGIN; INSERT INTO p VALUES (4, 5, 6); COMMIT"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("BEGIN in a script: %v", err)
	}
}

func TestMySQLNinthReviewFixes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())

	// An engine other than InnoDB is refused rather than run as InnoDB.
	for _, q := range []string{"CREATE TABLE m (id INT PRIMARY KEY) ENGINE=MyISAM", "CREATE TABLE m (id INT PRIMARY KEY) ENGINE=InnoDB; ALTER TABLE m ENGINE=MEMORY"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}

	// LOCK TABLES is refused alone, where code would rely on it, and passed
	// over in a dump.
	mustExec(t, db, "CREATE TABLE d (id INT PRIMARY KEY)")
	if _, err := db.Exec("LOCK TABLES d WRITE"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("LOCK TABLES: %v", err)
	}
	mustExec(t, db, "LOCK TABLES `d` WRITE; INSERT INTO `d` VALUES (1); UNLOCK TABLES;")
}

// A locking read of an empty CTE locks nothing of the table it shadows.
func TestMySQLEmptyCTELocksNothing(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1)") })
		open, wrote := false, false
		s.Seed(func() { open, wrote = false, false })
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.Query("WITH t AS (SELECT 1 AS id FROM DUAL WHERE 1 = 0 UNION SELECT 2 FROM DUAL WHERE 1 = 0) SELECT id FROM t FOR UPDATE")
			if err != nil {
				return err
			}
			_ = rows.Close()
			open = true
			p.Step("holds its locks")
			open = false
			return tx.Commit()
		})
		s.Manual("writer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "INSERT INTO t VALUES (2)")
			wrote = wrote || err == nil && open
			return err
		})
		s.Sometimes("t written while the reader holds its locks", func(*State) bool { return wrote })
	})
}

func TestMySQLEleventhReviewFixes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())

	// RowsAffected counts the rows changed, and an ON DUPLICATE KEY UPDATE
	// that changed its row twice.
	mustExec(t, db, "CREATE TABLE r (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO r VALUES (1, 0), (2, 0)")
	for _, step := range []struct {
		q    string
		want int64
	}{
		{"UPDATE r SET v = v", 0},
		{"UPDATE r SET v = 1 WHERE id = 1", 1},
		{"INSERT INTO r VALUES (1, 1) ON DUPLICATE KEY UPDATE v = 1", 0},
		{"INSERT INTO r VALUES (2, 5) ON DUPLICATE KEY UPDATE v = 5", 2},
		{"INSERT INTO r VALUES (3, 0) ON DUPLICATE KEY UPDATE v = 9", 1},
		{"UPDATE r SET v = 7 WHERE id IN (1, 2, 3) ORDER BY id LIMIT 2", 2},
	} {
		res, err := db.Exec(step.q)
		if err != nil {
			t.Fatalf("%s: %v", step.q, err)
		}
		if n, _ := res.RowsAffected(); n != step.want {
			t.Errorf("%s: %d rows affected, want %d", step.q, n, step.want)
		}
	}

	// CREATE TABLE ... SELECT with declarations of its own is refused.
	for _, q := range []string{"CREATE TABLE copy (id INT PRIMARY KEY) SELECT id FROM r", "CREATE TABLE copy AUTO_INCREMENT=5 SELECT id FROM r"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	mustExec(t, db, "CREATE TABLE copy SELECT id FROM r")

	// An explicit AUTO_INCREMENT value as a string moves the counter, and a
	// column made AUTO_INCREMENT starts past the values it holds.
	mustExec(t, db, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO ai VALUES ('10', 0)")
	mustExec(t, db, "CREATE TABLE later (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO later VALUES (20, 0)")
	mustExec(t, db, "ALTER TABLE later MODIFY id INT AUTO_INCREMENT")
	for q, want := range map[string]int64{"INSERT INTO ai (v) VALUES (1)": 11, "INSERT INTO later (v) VALUES (1)": 21} {
		res, err := db.Exec(q)
		if err != nil {
			t.Fatal(err)
		}
		if id, _ := res.LastInsertId(); id != want {
			t.Errorf("%s: id %d, want %d", q, id, want)
		}
	}

	// A timeout from a value bound later is refused.
	if _, err := db.Exec("SET SESSION innodb_lock_wait_timeout = ?", 1); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("SET innodb_lock_wait_timeout = ?: %v", err)
	}

	// CheckSQL runs TRUNCATE as autocommit code would.
	if err := CheckSQL(mysqlBin(), "TRUNCATE TABLE t"); err != nil && errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("CheckSQL TRUNCATE: %v", err)
	}
}

// MySQL's functions are MySQL's only.
func TestMySQLFunctionsStayMySQL(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	var n int64
	if err := db.QueryRow("SELECT last_insert_id()").Scan(&n); err == nil {
		t.Error("last_insert_id() ran on PostgreSQL")
	}
}

// A lock wait that times out reports MySQL's 1205, not NOWAIT's 3572.
func TestMySQLLockWaitTimeoutCode(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 0)") })
		var got int
		s.Seed(func() { got = 0 })
		s.Manual("holder", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("UPDATE t SET v = 1 WHERE id = 1"); err != nil {
				return err
			}
			p.Step("holds the row")
			return tx.Commit()
		})
		s.Manual("waiter", 1, func(p *Proc) error {
			conn, err := db.Conn(p.Context())
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.ExecContext(p.Context(), "SET SESSION innodb_lock_wait_timeout = 1"); err != nil {
				return err
			}
			_, err = conn.ExecContext(p.Context(), "UPDATE t SET v = 2 WHERE id = 1")
			if de, ok := errors.AsType[*DBError](err); ok {
				got = de.Number
			}
			return nil
		})
		s.Always(func(*State) error {
			if got != 0 && got != 1205 {
				return fmt.Errorf("lock wait timeout reported %d", got)
			}
			return nil
		})
		s.Sometimes("the wait times out", func(*State) bool { return got == 1205 })
	})
}

// Equal bounds keep the open one, so the row on it is not locked.
func TestMySQLEqualBoundsKeepTheOpenOne(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (5, 0), (10, 0)") })
		open, wrote := false, false
		s.Seed(func() { open, wrote = false, false })
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.Query("SELECT id FROM t WHERE id >= 5 AND id > 5 FOR UPDATE")
			if err != nil {
				return err
			}
			_ = rows.Close()
			open = true
			p.Step("holds its locks")
			open = false
			return tx.Commit()
		})
		s.Manual("writer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "UPDATE t SET v = 1 WHERE id = 5")
			wrote = wrote || err == nil && open
			return err
		})
		s.Sometimes("row 5 written while the reader holds its locks", func(*State) bool { return wrote })
	})
}

func TestMySQLTwelfthReviewFixes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO t VALUES (1), (2)")

	// Functions MySQL does not have are refused, and CONCAT, GREATEST and
	// LEAST are NULL when an argument is.
	for _, q := range []string{"SELECT hashtext('s')", "SELECT pg_advisory_xact_lock(1)"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	for q, want := range map[string]sql.NullString{
		"SELECT CONCAT('a', 'b')":  {String: "ab", Valid: true},
		"SELECT CONCAT('a', NULL)": {},
		"SELECT GREATEST(1, NULL)": {},
		"SELECT LEAST(NULL, 1)":    {},
		"SELECT GREATEST(1, 3, 2)": {String: "3", Valid: true},
	} {
		var got sql.NullString
		if err := db.QueryRow(q).Scan(&got); err != nil || got != want {
			t.Errorf("%s: %v, %v, want %v", q, got, err, want)
		}
	}

	// Settings that change what a transaction is are refused; the others,
	// and autocommit left on, do nothing.
	for _, q := range []string{"SET autocommit = 0", "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED", "SET TRANSACTION READ ONLY"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	mustExec(t, db, "SET autocommit = 1")
	mustExec(t, db, "SET NAMES utf8mb4")
}

// A LIMIT or OFFSET bound to a negative number is refused on either server,
// with MySQL's 1210 and as unsupported on Postgres.
func TestNegativeLimit(t *testing.T) {
	for _, srv := range []struct {
		name string
		s    Server
		q    []string
		want func(error) bool
	}{
		{"mysql", mysqlBin(), []string{"SELECT id FROM t LIMIT ?", "SELECT id FROM t LIMIT 1 OFFSET ?", "SELECT id FROM t LIMIT ? FOR UPDATE"}, func(err error) bool { return errors.Is(err, ErrInvalidParameterValue) }},
		{"postgres", postgres.New(), []string{"SELECT id FROM t LIMIT $1", "SELECT id FROM t OFFSET $1", "SELECT id FROM t LIMIT $1 FOR UPDATE"}, func(err error) bool { return errors.As(err, new(*ErrUnsupportedSQL)) }},
	} {
		t.Run(srv.name, func(t *testing.T) {
			s := newSim(t)
			db, _ := s.DB("app", srv.s)
			mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
			mustExec(t, db, "INSERT INTO t VALUES (1), (2)")
			for _, q := range srv.q {
				if _, err := db.Query(q, -1); !srv.want(err) {
					t.Errorf("%s with -1: %v", q, err)
				}
			}
		})
	}
}

func TestFourteenthReviewFixes(t *testing.T) {
	s := newSim(t)
	my, _ := s.DB("my", mysqlBin())
	// A generated column is refused rather than made writable.
	if _, err := my.Exec("CREATE TABLE g (a INT, b INT AS (a + 1))"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("generated column: %v", err)
	}
	// A LIMIT too large for an int is no limit, not a negative one.
	mustExec(t, my, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, my, "INSERT INTO t VALUES (1), (2)")
	rows, err := my.Query("SELECT id FROM t LIMIT ?", 1e300)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	_ = rows.Close()
	if n != 2 {
		t.Errorf("LIMIT 1e300: %d rows, want 2", n)
	}
}

// InnoDB's Serializable locks the rows a subquery reads, as it does the
// query's own.
func TestMySQLSerializableLocksSubqueries(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin(mysql.Isolation(Serializable)))
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 0)") })
		open, broke := false, false
		s.Seed(func() { open, broke = false, false })
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var v int
			if err := tx.QueryRow("SELECT (SELECT v FROM t WHERE id = 1)").Scan(&v); err != nil {
				return err
			}
			open = true
			p.Step("holds what it read")
			open = false
			return tx.Commit()
		})
		s.Manual("writer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "UPDATE t SET v = 1 WHERE id = 1")
			broke = broke || err == nil && open
			return err
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("the row was written while a Serializable reader held it")
			}
			return nil
		})
	})
}

// A multi-table UPDATE holds shared locks on the joined rows it read, and
// index hints are refused.
func TestMySQLUpdateJoinLocksJoinedRows(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE a (id INT PRIMARY KEY, v INT)")
		mustExec(t, db, "CREATE TABLE b (id INT PRIMARY KEY, a_id INT, v INT)")
		if _, err := db.Exec("SELECT id FROM a FORCE INDEX (PRIMARY)"); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("FORCE INDEX: %v", err)
		}
		s.Seed(func() { mustExec(t, db, "INSERT INTO a VALUES (1, 0)") })
		s.Seed(func() { mustExec(t, db, "INSERT INTO b VALUES (1, 1, 5)") })
		open, broke := false, false
		s.Seed(func() { open, broke = false, false })
		s.Manual("updater", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("UPDATE a JOIN b ON b.a_id = a.id SET a.v = b.v"); err != nil {
				return err
			}
			open = true
			p.Step("holds its locks")
			open = false
			return tx.Commit()
		})
		s.Manual("writer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "UPDATE b SET v = 9 WHERE id = 1")
			broke = broke || err == nil && open
			return err
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("the joined row was written while the UPDATE held it")
			}
			return nil
		})
	})
}

// A multi-table UPDATE that waited for a joined row uses the row as the lock
// holder left it.
func TestMySQLUpdateJoinRereadsJoinedRows(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE a (id INT PRIMARY KEY, v INT)")
		mustExec(t, db, "CREATE TABLE b (id INT PRIMARY KEY, a_id INT, v INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO a VALUES (1, 0)") })
		s.Seed(func() { mustExec(t, db, "INSERT INTO b VALUES (1, 1, 5)") })
		committed, broke := false, false
		s.Seed(func() { committed, broke = false, false })
		s.Manual("writer", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("UPDATE b SET v = 9 WHERE id = 1"); err != nil {
				return err
			}
			p.Step("holds b")
			if err := tx.Commit(); err != nil {
				return err
			}
			committed = true
			return nil
		})
		s.Manual("updater", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("UPDATE a JOIN b ON b.a_id = a.id SET a.v = b.v"); err != nil {
				return err
			}
			var v int
			if err := tx.QueryRow("SELECT v FROM a WHERE id = 1").Scan(&v); err != nil {
				return err
			}
			broke = broke || committed && v != 9
			return tx.Commit()
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("the UPDATE wrote b's value from before the wait")
			}
			return nil
		})
		s.Sometimes("the UPDATE waits for the writer", func(*State) bool { return committed })
	})
}

// An ON DUPLICATE KEY UPDATE whose conflicting row is deleted while it waits
// inserts its row instead of updating the deleted one.
func TestMySQLUpsertRechecksTheConflictAfterWaiting(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE u (id INT PRIMARY KEY, email VARCHAR(20) NOT NULL, n INT NOT NULL, UNIQUE KEY uk_email (email))")
		s.Seed(func() { mustExec(t, db, "INSERT INTO u VALUES (1, 'x', 0)") })
		broke := ""
		s.Seed(func() { broke = "" })
		s.Manual("deleter", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("DELETE FROM u WHERE id = 1"); err != nil {
				return err
			}
			p.Step("holds the deleted row")
			return tx.Commit()
		})
		s.Manual("upserter", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("INSERT INTO u VALUES (2, 'x', 1) ON DUPLICATE KEY UPDATE n = n + 1"); err != nil {
				broke = err.Error()
				return err
			}
			var n int
			if err := tx.QueryRow("SELECT COUNT(*) FROM u WHERE email = 'x' FOR UPDATE").Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				broke = fmt.Sprintf("%d rows with the email", n)
			}
			return tx.Commit()
		})
		s.AtQuiescence(func(*State) error {
			if broke != "" {
				return fmt.Errorf("the upsert went wrong: %s", broke)
			}
			return nil
		})
	})
}

// An UPDATE or an upsert that moves an indexed value into a locked gap waits
// as an insert does, so the locking reader sees no phantom.
func TestMySQLMovedValueWaitsForTheGap(t *testing.T) {
	for _, write := range []string{"UPDATE events SET at = 15 WHERE id = 3", "INSERT INTO events VALUES (3, 15) ON DUPLICATE KEY UPDATE at = 15"} {
		t.Run(write, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", mysqlBin())
				mustExec(t, db, "CREATE TABLE events (id INT PRIMARY KEY, at INT NOT NULL, KEY idx_at (at))")
				// Row 3 is past the next record (30) the reader locks, so only
				// the gap keeps the writer out.
				s.Seed(func() { mustExec(t, db, "INSERT INTO events VALUES (1, 10), (2, 30), (3, 40)") })
				var first, second int
				s.Seed(func() { first, second = -1, -1 })
				q := "SELECT COUNT(*) FROM events WHERE at BETWEEN 5 AND 20 FOR UPDATE"
				s.Manual("reader", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if err := tx.QueryRow(q).Scan(&first); err != nil {
						return err
					}
					if err := tx.QueryRow(q).Scan(&second); err != nil {
						return err
					}
					return tx.Commit()
				})
				s.Manual("writer", 1, func(p *Proc) error {
					_, err := db.ExecContext(p.Context(), write)
					return err
				})
				s.Always(func(*State) error {
					if second >= 0 && first != second {
						return fmt.Errorf("phantom: %d then %d", first, second)
					}
					return nil
				})
			})
		})
	}
}

// LOCK TABLES passes in a dump only around the rows it loads.
func TestMySQLLockTablesOnlyAroundInserts(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE d (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "LOCK TABLES d WRITE; INSERT INTO d VALUES (1, 0); UNLOCK TABLES;")
	// So does mysqldump's own form, which disables the keys around the rows.
	mustExec(t, db, "LOCK TABLES `d` WRITE;\n/*!40000 ALTER TABLE `d` DISABLE KEYS */;\nINSERT INTO `d` VALUES (2, 0);\n/*!40000 ALTER TABLE `d` ENABLE KEYS */;\nUNLOCK TABLES;")
	// A dump of an empty table locks it around nothing.
	mustExec(t, db, "LOCK TABLES `d` WRITE; UNLOCK TABLES;")
	mustExec(t, db, "CREATE TABLE e (id INT PRIMARY KEY)")
	for _, q := range []string{
		"LOCK TABLES d WRITE; UPDATE d SET v = 1; UNLOCK TABLES;",
		"LOCK TABLES d READ; INSERT INTO d VALUES (3, 0); UNLOCK TABLES;",
		"LOCK TABLES d WRITE; INSERT INTO e VALUES (1); UNLOCK TABLES;",
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
}

// A unique point search whose row is deleted while it waits locks the gap
// the row leaves, so an insert into it waits. The search is by a secondary
// unique key, as the lock on a primary key would hold the insert anyway.
func TestMySQLPointSearchRereadsAfterWaiting(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, email VARCHAR(10) NOT NULL, UNIQUE KEY uk (email))")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 'a'), (2, 'm'), (3, 'z')") })
		var first, second int
		s.Seed(func() { first, second = -1, -1 })
		s.Manual("deleter", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("DELETE FROM t WHERE id = 2"); err != nil {
				return err
			}
			p.Step("holds row 2")
			return tx.Commit()
		})
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			q := "SELECT COUNT(*) FROM t WHERE email = 'm' FOR UPDATE"
			if err := tx.QueryRow(q).Scan(&first); err != nil {
				return err
			}
			if err := tx.QueryRow(q).Scan(&second); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.Manual("inserter", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "INSERT INTO t VALUES (9, 'm')")
			return err
		})
		s.Always(func(*State) error {
			if second >= 0 && first != second {
				return fmt.Errorf("phantom: %d then %d", first, second)
			}
			return nil
		})
	})
}

func TestMySQLNineteenthReviewFixes(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	for _, q := range []string{"SELECT 5 DIV 2", "SELECT SYSDATE()"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// Postgres's now() is the transaction's start throughout it.
func TestPostgresNowIsTheTransactionTime(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var a, b time.Time
	if err := tx.QueryRow("SELECT now()").Scan(&a); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := tx.QueryRow("SELECT now()").Scan(&b); err != nil {
		t.Fatal(err)
	}
	if !a.Equal(b) {
		t.Errorf("now() changed within a transaction: %v then %v", a, b)
	}
}

// An insert that waited for a row lock checks the gaps again, as a gap lock
// taken while it waited covers the row too.
func TestMySQLInsertRechecksGapsAfterItsRowLock(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1), (5), (10)") })
		var first, second int
		s.Seed(func() { first, second = -1, -1 })
		s.Manual("deleter", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("DELETE FROM t WHERE id = 5"); err != nil {
				return err
			}
			p.Step("holds row 5")
			return tx.Commit()
		})
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			q := "SELECT COUNT(*) FROM t WHERE id = 5 FOR UPDATE"
			if err := tx.QueryRow(q).Scan(&first); err != nil {
				return err
			}
			if err := tx.QueryRow(q).Scan(&second); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.Manual("inserter", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "INSERT INTO t VALUES (5)")
			return err
		})
		s.Always(func(*State) error {
			if second >= 0 && first != second {
				return fmt.Errorf("phantom: %d then %d", first, second)
			}
			return nil
		})
	})
}

func TestMySQLPreviouslyMissedFindings(t *testing.T) {
	s := newSim(t)
	my, _ := s.DB("my", mysqlBin())
	pg, _ := s.DB("pg", postgres.New())
	for _, c := range []struct {
		db   *sql.DB
		q    string
		want string
	}{
		// A backslash escapes % and _ in LIKE, on either server.
		{my, `SELECT 'a_c' LIKE 'a\\_c'`, "true"},
		{my, `SELECT 'abc' LIKE 'a\\_c'`, "false"},
		{pg, `SELECT 'abc' LIKE 'a\_c'`, "false"},
		{pg, `SELECT '50%' LIKE '50\%'`, "true"},
		// MySQL's LENGTH counts bytes, CHAR_LENGTH characters.
		{my, "SELECT LENGTH('é')", "2"},
		{my, "SELECT CHAR_LENGTH('é')", "1"},
	} {
		var got string
		if err := c.db.QueryRow(c.q).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s: %q, %v, want %q", c.q, got, err, c.want)
		}
	}
}

// INSERT ... SELECT reads its source with shared locks at Repeatable Read.
func TestMySQLInsertSelectLocksItsSource(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE src (id INT PRIMARY KEY, v INT)")
		mustExec(t, db, "CREATE TABLE dst (id INT PRIMARY KEY, v INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO src VALUES (1, 0)") })
		open, broke := false, false
		s.Seed(func() { open, broke = false, false })
		s.Manual("copier", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("INSERT INTO dst SELECT id, v FROM src WHERE id = 1"); err != nil {
				return err
			}
			open = true
			p.Step("holds what it copied")
			open = false
			return tx.Commit()
		})
		s.Manual("writer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "UPDATE src SET v = 1 WHERE id = 1")
			broke = broke || err == nil && open
			return err
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("the source row was written while INSERT ... SELECT held it")
			}
			return nil
		})
	})
}

// A pooled connection brings no session state from one run into the next.
func TestSessionStateStaysInItsRun(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		db.SetMaxOpenConns(1)
		mustExec(t, db, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
		inserted, broke := false, false
		s.Seed(func() { inserted, broke = false, false })
		s.Manual("inserter", 1, func(p *Proc) error {
			if _, err := db.ExecContext(p.Context(), "INSERT INTO ai (v) VALUES (1)"); err != nil {
				return err
			}
			inserted = true
			return nil
		})
		s.Manual("reader", 1, func(p *Proc) error {
			var id int64
			if err := db.QueryRowContext(p.Context(), "SELECT LAST_INSERT_ID()").Scan(&id); err != nil {
				return err
			}
			broke = broke || !inserted && id != 0
			return nil
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("LAST_INSERT_ID() came from another run")
			}
			return nil
		})
	})
}

func TestMySQLTwentiethReviewFindings(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 0)")
	for _, c := range []struct {
		q    string
		want sql.NullString
	}{
		{"SELECT CAST('12' AS SIGNED) + 1", sql.NullString{String: "13", Valid: true}},
		{"SELECT CAST(12.7 AS SIGNED)", sql.NullString{String: "13", Valid: true}},
		{"SELECT CAST(' 12abc' AS SIGNED)", sql.NullString{String: "12", Valid: true}},
		{"SELECT NULL <=> NULL", sql.NullString{String: "true", Valid: true}},
		{"SELECT 1 <=> NULL", sql.NullString{String: "false", Valid: true}},
		{"SELECT LOWER(NULL)", sql.NullString{}},
		{"SELECT LENGTH(NULL)", sql.NullString{}},
		{"SELECT ABS(NULL)", sql.NullString{}},
		// A mixed-case alias names the table its columns are read from.
		{"SELECT U.id FROM t AS U", sql.NullString{String: "1", Valid: true}},
	} {
		var got sql.NullString
		if err := db.QueryRow(c.q).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s: %v, %v, want %v", c.q, got, err, c.want)
		}
	}
	for _, q := range []string{"SELECT CAST(-1 AS UNSIGNED)", "SET innodb_lock_wait_timeout = 1, autocommit = 0"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}

	// A row SeedRowNow changes is a commit a snapshot taken before does not see.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM t WHERE v = 0").Scan(&n); err != nil || n != 1 {
		t.Fatalf("snapshot: %d, %v", n, err)
	}
	store.SeedRowNow("t", Row{"id": int64(1), "v": int64(5)})
	store.SeedRowNow("t", Row{"id": int64(2), "v": int64(0)})
	if err := tx.QueryRow("SELECT COUNT(*) FROM t WHERE v = 0").Scan(&n); err != nil || n != 1 {
		t.Errorf("after SeedRowNow: %d, %v, want 1", n, err)
	}
}

func TestMySQLTwentyFirstReviewFindings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	var same bool
	if err := db.QueryRow("SELECT '02' <=> 2").Scan(&same); err != nil || !same {
		t.Errorf("'02' <=> 2: %v, %v", same, err)
	}
	// InnoDB checks a foreign key declared MATCH FULL as MATCH SIMPLE.
	mustExec(t, db, "CREATE TABLE parents (a INT, b INT, PRIMARY KEY (a, b))")
	mustExec(t, db, "CREATE TABLE kids (id INT PRIMARY KEY, a INT, b INT, FOREIGN KEY (a, b) REFERENCES parents (a, b) MATCH FULL)")
	mustExec(t, db, "INSERT INTO kids VALUES (1, 1, NULL)")
	for _, q := range []string{
		"CREATE TABLE sd (id INT PRIMARY KEY, a INT, b INT, FOREIGN KEY (a, b) REFERENCES parents (a, b) ON DELETE SET DEFAULT)",
		"SELECT 'a#_c' LIKE 'a#_c' ESCAPE '#'",
		"UPDATE IGNORE kids SET a = 2",
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	if _, err := db.Query("SELECT id FROM kids LIMIT ?", 1.5); !errors.Is(err, ErrInvalidParameterValue) {
		t.Errorf("LIMIT 1.5: %v", err)
	}
	// An UPDATE that sets an omitted nullable column to NULL changes nothing.
	mustExec(t, db, "CREATE TABLE n (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO n (id) VALUES (1)")
	res, err := db.Exec("UPDATE n SET v = NULL WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := res.RowsAffected(); got != 0 {
		t.Errorf("UPDATE to the same NULL: %d rows affected, want 0", got)
	}
}

// A locking read over a LEFT JOIN keeps the left row when the joined row it
// waited for is deleted.
func TestMySQLLockingLeftJoinKeepsTheLeftRow(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE a (id INT PRIMARY KEY)")
		mustExec(t, db, "CREATE TABLE b (id INT PRIMARY KEY, a_id INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO a VALUES (1)") })
		s.Seed(func() { mustExec(t, db, "INSERT INTO b VALUES (1, 1)") })
		broke := false
		s.Seed(func() { broke = false })
		s.Manual("deleter", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("DELETE FROM b WHERE id = 1"); err != nil {
				return err
			}
			p.Step("holds b's row")
			return tx.Commit()
		})
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.Query("SELECT a.id FROM a LEFT JOIN b ON b.a_id = a.id FOR UPDATE")
			if err != nil {
				return err
			}
			n := 0
			for rows.Next() {
				n++
			}
			_ = rows.Close()
			broke = broke || n != 1
			return tx.Commit()
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("the LEFT JOIN lost a's row")
			}
			return nil
		})
	})
}

func TestMySQLTwentySecondReviewFindings(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT DEFAULT 7)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 0)")
	for _, c := range []struct {
		q    string
		want sql.NullString
	}{
		{"SELECT NULL IN (SELECT NULL)", sql.NullString{}},
		{"SELECT 1 IN (SELECT NULL)", sql.NullString{}},
		{"SELECT 2 NOT IN (SELECT NULL)", sql.NullString{}},
		{"SELECT 1 IN (SELECT 1)", sql.NullString{String: "true", Valid: true}},
		{"SELECT CASE NULL WHEN NULL THEN 1 ELSE 2 END", sql.NullString{String: "2", Valid: true}},
	} {
		var got sql.NullString
		if err := db.QueryRow(c.q).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s: %v, %v, want %v", c.q, got, err, c.want)
		}
	}
	for _, q := range []string{
		"SELECT id FROM t FOR UPDATE WAIT 5",
		"UPDATE t SET v = DEFAULT",
		"INSERT INTO t VALUES (1, 0) ON DUPLICATE KEY UPDATE v = DEFAULT",
		"LOCK TABLES t WRITE; INSERT INTO t VALUES (2, 0)",
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	// MODIFY replaces the column's default with the one it gives, or none.
	mustExec(t, db, "ALTER TABLE t MODIFY v INT")
	mustExec(t, db, "INSERT INTO t (id) VALUES (3)")
	if got := peekRows(store, "t", "id", "v"); !strings.Contains(got, "3:<nil>") {
		t.Errorf("default after MODIFY: %s", got)
	}
}

func TestMySQLTwentyThirdReviewFindings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE p (a INT, b INT)")
	// A failed AFTER leaves the column where it was.
	if _, err := db.Exec("ALTER TABLE p MODIFY b INT AFTER missing"); err == nil {
		t.Error("AFTER a missing column succeeded")
	}
	mustExec(t, db, "INSERT INTO p VALUES (1, 2)")
	var b int
	if err := db.QueryRow("SELECT b FROM p WHERE a = 1").Scan(&b); err != nil || b != 2 {
		t.Errorf("column after a failed MODIFY: %d, %v", b, err)
	}
	mustExec(t, db, "CREATE TABLE q (id INT PRIMARY KEY, a INT)")
	mustExec(t, db, "INSERT INTO q VALUES (1, 1)")
	for _, q := range []string{"SELECT +'2'", "UPDATE p JOIN q ON q.a = p.a SET p.b = 3 ORDER BY q.id LIMIT 1"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	mustExec(t, db, "UPDATE q SET a = 2 LIMIT 18446744073709551615")
	for _, c := range []struct {
		q    string
		want sql.NullString
	}{
		{"SELECT NULL IN (NULL)", sql.NullString{}},
		{"SELECT NULL NOT IN (NULL)", sql.NullString{}},
		{"SELECT 2 IN (NULL, 1)", sql.NullString{}},
		{"SELECT 1 IN (NULL, 1)", sql.NullString{String: "true", Valid: true}},
		{"SELECT +2", sql.NullString{String: "2", Valid: true}},
	} {
		var got sql.NullString
		if err := db.QueryRow(c.q).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s: %v, %v, want %v", c.q, got, err, c.want)
		}
	}
}

// Postgres's now() is the time the transaction began, not when first asked.
func TestPostgresNowIsTakenAtBegin(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	begun := time.Now()
	time.Sleep(2 * time.Millisecond)
	var now time.Time
	if err := tx.QueryRow("SELECT now()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	if now.After(begun) {
		t.Errorf("now() %v is after the transaction began at %v", now, begun)
	}
}

// A failed foreign key check keeps a shared gap lock where the missing parent
// would be, so inserting that parent waits for the checking transaction.
func TestMySQLMissingParentKeepsAGapLock(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		// The key referenced is a unique secondary one: a primary key's
		// lock on the missing value would hold the insert anyway.
		mustExec(t, db, "CREATE TABLE parents (id INT PRIMARY KEY, code VARCHAR(5) NOT NULL, UNIQUE KEY uk_code (code))")
		mustExec(t, db, "CREATE TABLE kids (id INT PRIMARY KEY, code VARCHAR(5), FOREIGN KEY (code) REFERENCES parents (code))")
		s.Seed(func() { mustExec(t, db, "INSERT INTO parents VALUES (1, 'a'), (2, 'z')") })
		open, broke := false, false
		s.Seed(func() { open, broke = false, false })
		s.Manual("child", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("INSERT INTO kids VALUES (1, 'm')"); !errors.Is(err, ErrForeignKeyViolation) {
				return err
			}
			open = true
			p.Step("keeps its locks")
			open = false
			return tx.Commit()
		})
		s.Manual("parent", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "INSERT INTO parents VALUES (3, 'm')")
			broke = broke || err == nil && open
			return err
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("the parent was inserted into the gap the failed check locked")
			}
			return nil
		})
	})
}

// After a deadlock MySQL has ended the transaction, so a later statement
// through the same Tx commits on its own and a Rollback does not undo it.
func TestMySQLDeadlockEndsTheTransaction(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		mustExec(t, db, "CREATE TABLE log (id INT PRIMARY KEY)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 0), (2, 0)") })
		deadlocked := 0
		s.Seed(func() { deadlocked = 0 })
		worker := func(first, second, logID int) func(p *Proc) error {
			return func(p *Proc) error {
				tx, err := db.BeginTx(p.Context(), nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				if _, err := tx.Exec("UPDATE t SET v = v + 1 WHERE id = ?", first); err != nil {
					return err
				}
				if _, err := tx.Exec("UPDATE t SET v = v + 1 WHERE id = ?", second); err != nil {
					if !errors.Is(err, ErrDeadlock) {
						return err
					}
					deadlocked = logID
					if _, err := tx.Exec("INSERT INTO log VALUES (?)", logID); err != nil {
						return err
					}
					return tx.Rollback()
				}
				return tx.Commit()
			}
		}
		s.Manual("a", 1, worker(1, 2, 1))
		s.Manual("b", 1, worker(2, 1, 2))
		s.AtQuiescence(func(st *State) error {
			if deadlocked == 0 {
				return nil
			}
			if got := peekRows(store, "log", "id", "id"); got != fmt.Sprintf("%d:%d", deadlocked, deadlocked) {
				return fmt.Errorf("the statement after the deadlock was undone by Rollback: log %q", got)
			}
			return nil
		})
		s.Sometimes("a deadlock", func(*State) bool { return deadlocked != 0 })
	})
}

// A seed sees no session state from the run before.
func TestSeedSessionStateStaysInItsRun(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		db.SetMaxOpenConns(1)
		mustExec(t, db, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
		broke := false
		s.Seed(func() { broke = false })
		s.Seed(func() {
			var id int64
			if err := db.QueryRow("SELECT LAST_INSERT_ID()").Scan(&id); err != nil {
				t.Fatal(err)
			}
			broke = id != 0
		})
		s.Manual("inserter", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "INSERT INTO ai (v) VALUES (1)")
			return err
		})
		s.Manual("other", 1, func(p *Proc) error { p.Step("runs"); return nil })
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("a seed saw LAST_INSERT_ID() from another run")
			}
			return nil
		})
	})
}

func TestMySQLTwentyFifthReviewFindings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 0), (2, 0)")
	// ADD COLUMN gives the rows already there the column's default.
	mustExec(t, db, "ALTER TABLE t ADD COLUMN status INT NOT NULL DEFAULT 5")
	var status int
	if err := db.QueryRow("SELECT status FROM t WHERE id = 1").Scan(&status); err != nil || status != 5 {
		t.Errorf("status of an existing row: %d, %v", status, err)
	}
	// A NULL count bound to LIMIT is 0 on MySQL.
	rows, err := db.Query("SELECT id FROM t LIMIT ?", nil)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	_ = rows.Close()
	if n != 0 {
		t.Errorf("LIMIT NULL: %d rows, want 0", n)
	}
	res, err := db.Exec("UPDATE t SET v = 1 LIMIT ?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := res.RowsAffected(); got != 0 {
		t.Errorf("UPDATE ... LIMIT NULL: %d rows, want 0", got)
	}
}

// A search by a foreign key's column uses the index InnoDB creates for it,
// and <=> bounds a range as = does, so neither locks the whole table.
func TestMySQLRangesThroughImplicitIndexesAndNullSafeEquality(t *testing.T) {
	for _, c := range []struct{ query, write string }{
		{"SELECT id FROM kids WHERE parent_id = 1 FOR UPDATE", "INSERT INTO kids VALUES (9, 5)"},
		{"SELECT id FROM kids WHERE id <=> 1 FOR UPDATE", "UPDATE kids SET parent_id = 1 WHERE id = 2"},
	} {
		t.Run(c.query, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", mysqlBin())
				mustExec(t, db, "CREATE TABLE parents (id INT PRIMARY KEY)")
				mustExec(t, db, "CREATE TABLE kids (id INT PRIMARY KEY, parent_id INT, FOREIGN KEY (parent_id) REFERENCES parents (id))")
				s.Seed(func() { mustExec(t, db, "INSERT INTO parents VALUES (1), (3), (5)") })
				s.Seed(func() { mustExec(t, db, "INSERT INTO kids VALUES (1, 1), (2, 3)") })
				open, wrote := false, false
				s.Seed(func() { open, wrote = false, false })
				s.Manual("reader", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					rows, err := tx.Query(c.query)
					if err != nil {
						return err
					}
					_ = rows.Close()
					open = true
					p.Step("holds its locks")
					open = false
					return tx.Commit()
				})
				s.Manual("writer", 1, func(p *Proc) error {
					_, err := db.ExecContext(p.Context(), c.write)
					wrote = wrote || err == nil && open
					return err
				})
				s.Sometimes("written while the reader holds its locks", func(*State) bool { return wrote })
			})
		})
	}
}

func TestTwentySixthReviewFindings(t *testing.T) {
	s := newSim(t)
	my, _ := s.DB("my", mysqlBin())
	pg, _ := s.DB("pg", postgres.New())
	mustExec(t, my, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, my, "INSERT INTO t VALUES (1, 0)")
	// A qualifier naming the relation of a schema-qualified target is its own.
	mustExec(t, my, "UPDATE app.t SET t.v = 1")
	// GLOBAL changes later sessions only, which detest does not model.
	for _, q := range []string{
		"SET GLOBAL innodb_lock_wait_timeout = 1", "SET GLOBAL FOREIGN_KEY_CHECKS = 0",
		"SET GLOBAL sql_mode = 'NO_AUTO_VALUE_ON_ZERO'", "SET @@GLOBAL.foreign_key_checks = 0",
		"SET GLOBAL autocommit = 1",
	} {
		if _, err := my.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	// A trailing escape is a literal backslash in MySQL and refused in Postgres.
	for q, want := range map[string]bool{`SELECT 'abc\\' LIKE 'abc\\'`: true, `SELECT 'abc' LIKE 'abc\\'`: false} {
		var got bool
		if err := my.QueryRow(q).Scan(&got); err != nil || got != want {
			t.Errorf("%s: %v, %v, want %v", q, got, err, want)
		}
	}
	if _, err := pg.Exec(`SELECT 'abc' LIKE 'abc\'`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("Postgres trailing escape: %v", err)
	}
}

func TestMySQLTwentySeventhReviewFindings(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE p (id INT PRIMARY KEY, s VARCHAR(10), UNIQUE KEY uk (s))")
	mustExec(t, db, "CREATE TABLE src (id INT PRIMARY KEY)")
	for _, q := range []string{
		"CREATE TABLE k (id INT PRIMARY KEY, s VARCHAR(10), FOREIGN KEY (s(3)) REFERENCES p (s))",
		"SELECT ROUND(1.25, 1)",
		"INSERT INTO src SELECT 1 UNION SELECT 2",
	} {
		if _, err := db.Exec(q); err == nil || !errors.As(err, new(*ErrUnsupportedSQL)) && !strings.Contains(err.Error(), "syntax") {
			t.Errorf("%s: %v", q, err)
		}
	}
	// NO_AUTO_VALUE_ON_ZERO, as a dump sets it, keeps an explicit 0.
	mustExec(t, db, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, q := range []string{
		"SET SQL_MODE='NO_AUTO_VALUE_ON_ZERO'",
		"INSERT INTO ai VALUES (0, 1)",
		"SET SQL_MODE='STRICT_TRANS_TABLES'",
		"INSERT INTO ai VALUES (0, 2)",
	} {
		if _, err := conn.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if got := peekRows(store, "ai", "id", "v"); got != "0:1 1:2" {
		t.Errorf("ids: %s, want 0:1 1:2", got)
	}
}

// When the next record a range locks is deleted while the range waits for
// it, the gap reaches the record after it, as InnoDB's purge leaves it.
func TestMySQLGapFollowsADeletedNextRecord(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE e (id INT PRIMARY KEY, at INT NOT NULL, KEY idx_at (at))")
		s.Seed(func() { mustExec(t, db, "INSERT INTO e VALUES (1, 10), (2, 30), (3, 50)") })
		open, broke, deleted, after := false, false, false, false
		s.Seed(func() { open, broke, deleted, after = false, false, false, false })
		s.Manual("deleter", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("DELETE FROM e WHERE id = 2"); err != nil {
				return err
			}
			p.Step("holds row 2")
			if err := tx.Commit(); err != nil {
				return err
			}
			deleted = true
			return nil
		})
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			rows, err := tx.Query("SELECT id FROM e WHERE at BETWEEN 5 AND 20 FOR UPDATE")
			if err != nil {
				return err
			}
			_ = rows.Close()
			after = deleted // the reader waited for the deleted record
			open = true
			p.Step("holds its locks")
			open = false
			return tx.Commit()
		})
		s.Manual("inserter", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "INSERT INTO e VALUES (9, 40)")
			broke = broke || err == nil && open && after
			return err
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("an insert went into the gap the deleted next record left")
			}
			return nil
		})
	})
}

func TestMySQLTwentyEighthReviewFindings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	if _, err := db.Exec("SET sql_mode = 'NO_AUTO_VALUE_ON_ZERO', innodb_lock_wait_timeout = 1"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("SET of two settings: %v", err)
	}
	// A failed ADD COLUMN ... AFTER adds nothing.
	mustExec(t, db, "CREATE TABLE p (a INT, b INT)")
	if _, err := db.Exec("ALTER TABLE p ADD COLUMN c INT AFTER missing"); err == nil {
		t.Error("AFTER a missing column succeeded")
	}
	mustExec(t, db, "INSERT INTO p VALUES (1, 2)")
	// AUTO_INCREMENT=n starts an emptied table over.
	mustExec(t, db, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO ai (v) VALUES (1), (2), (3)")
	mustExec(t, db, "DELETE FROM ai")
	mustExec(t, db, "ALTER TABLE ai AUTO_INCREMENT = 1")
	res, err := db.Exec("INSERT INTO ai (v) VALUES (4)")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 1 {
		t.Errorf("id after the reset: %d, want 1", id)
	}
	// A column dropped and added again starts from its new default.
	mustExec(t, db, "CREATE TABLE d (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO d VALUES (1, 9)")
	mustExec(t, db, "ALTER TABLE d DROP COLUMN v")
	mustExec(t, db, "ALTER TABLE d ADD COLUMN v INT DEFAULT 5")
	var v int
	if err := db.QueryRow("SELECT v FROM d WHERE id = 1").Scan(&v); err != nil || v != 5 {
		t.Errorf("re-added column: %d, %v, want 5", v, err)
	}
	// String functions take []byte arguments as strings.
	var n int
	var lower string
	if err := db.QueryRow("SELECT LENGTH(?), LOWER(?)", []byte("é"), []byte("AB")).Scan(&n, &lower); err != nil || n != 2 || lower != "ab" {
		t.Errorf("[]byte arguments: %d %q, %v", n, lower, err)
	}
	// An expression that overflows is 1690, not a column's 1264.
	_, err = db.Exec("SELECT 9223372036854775807 + 1")
	if de, ok := errors.AsType[*DBError](err); !ok || de.Number != 1690 {
		t.Errorf("overflow: %v", err)
	}
}

func TestMySQLTwentyNinthReviewFindings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	var n float64
	if err := db.QueryRow("SELECT '1' + 2").Scan(&n); err != nil || n != 3 {
		t.Errorf("'1' + 2: %v, %v", n, err)
	}
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, a INT, b INT)")
	for _, q := range []string{"SELECT COUNT(DISTINCT a, b) FROM t", "SET sql_mode = CONCAT('NO_AUTO_VALUE_ON_', 'ZERO')"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	// A dump's restore from the variable it saved the mode in still passes.
	mustExec(t, db, "SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO'; SET SQL_MODE=@OLD_SQL_MODE")
}

func TestMySQLThirtiethReviewFindings(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	// INSERT ... SET inserts its row: the parser turns it into columns and
	// one row of values.
	mustExec(t, db, "INSERT INTO t SET id = 1, v = 7")
	if got := peekRows(store, "t", "id", "v"); got != "1:7" {
		t.Errorf("INSERT ... SET: %s", got)
	}
	if _, err := db.Exec("SELECT id FROM t HAVING id > 0"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("HAVING without grouping: %v", err)
	}
	// HAVING naming a select alias filters on what the alias names.
	for q, want := range map[string]int{
		"SELECT COUNT(*) AS n FROM t HAVING n > 0":                 1,
		"SELECT COUNT(*) FROM t HAVING COUNT(*) > 0":               1,
		"SELECT COUNT(*) AS n FROM (SELECT 1 AS x) d HAVING n > 5": 0,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		got := 0
		for rows.Next() {
			got++
		}
		_ = rows.Close()
		if got != want {
			t.Errorf("%s: %d rows, want %d", q, got, want)
		}
	}
}

// A locking read with LIMIT stops where InnoDB's index scan stops, so the
// rows past it stay free, and a locking read of a view is refused.
func TestMySQLLimitStopsTheLockingScan(t *testing.T) {
	for _, c := range []struct{ query, write string }{
		{"SELECT id FROM t WHERE id >= 1 ORDER BY id LIMIT 1 FOR UPDATE", "UPDATE t SET v = 1 WHERE id = 3"},
		{"SELECT id FROM t WHERE id >= 1 ORDER BY id DESC LIMIT 1 FOR UPDATE", "UPDATE t SET v = 1 WHERE id = 1"},
		{"SELECT id FROM t WHERE id >= 1 LIMIT 1 FOR UPDATE SKIP LOCKED", "UPDATE t SET v = 1 WHERE id = 3"},
		{"UPDATE t SET v = 2 WHERE id >= 1 ORDER BY id LIMIT 1", "UPDATE t SET v = 1 WHERE id = 3"},
	} {
		t.Run(c.query, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", mysqlBin())
				mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
				s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 0), (2, 0), (3, 0)") })
				open, wrote := false, false
				s.Seed(func() { open, wrote = false, false })
				s.Manual("reader", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if _, err := tx.Exec(c.query); err != nil {
						return err
					}
					open = true
					p.Step("holds its locks")
					open = false
					return tx.Commit()
				})
				s.Manual("writer", 1, func(p *Proc) error {
					_, err := db.ExecContext(p.Context(), c.write)
					wrote = wrote || err == nil && open
					return err
				})
				s.Sometimes("a row past the scan written while the reader holds its locks", func(*State) bool { return wrote })
			})
		})
	}
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, db, "CREATE VIEW vt AS SELECT id FROM t")
	if _, err := db.Exec("SELECT id FROM vt FOR UPDATE"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("FOR UPDATE of a view: %v", err)
	}
}

func TestMySQLThirtySecondReviewFindings(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	for _, c := range []struct {
		q    string
		want sql.NullString
	}{
		{"SELECT (1, NULL) = (1, NULL)", sql.NullString{}},
		{"SELECT (1, 2) = (1, 2)", sql.NullString{String: "true", Valid: true}},
		{"SELECT (1, NULL) IN ((1, NULL))", sql.NullString{}},
		{"SELECT (1, 2) IN ((1, 2))", sql.NullString{String: "true", Valid: true}},
	} {
		var got sql.NullString
		if err := db.QueryRow(c.q).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s: %v, %v, want %v", c.q, got, err, c.want)
		}
	}
	// FOREIGN_KEY_CHECKS=0, as a dump sets it, lets a child in before its
	// parent, and the checks come back with the setting.
	mustExec(t, db, "CREATE TABLE parents (id INT PRIMARY KEY)")
	mustExec(t, db, "CREATE TABLE kids (id INT PRIMARY KEY, parent_id INT, FOREIGN KEY (parent_id) REFERENCES parents (id))")
	mustExec(t, db, "SET @OLD_FOREIGN_KEY_CHECKS=@@FOREIGN_KEY_CHECKS, FOREIGN_KEY_CHECKS=0; INSERT INTO kids VALUES (1, 99); SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS")
	if _, err := db.Exec("INSERT INTO kids VALUES (2, 98)"); !errors.Is(err, ErrForeignKeyViolation) {
		t.Errorf("after the checks come back: %v", err)
	}
	// A transaction begun outside a process keeps the session's settings.
	mustExec(t, db, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(t.Context(), "SET SQL_MODE='NO_AUTO_VALUE_ON_ZERO'"); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO ai VALUES (0, 1)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := peekRows(store, "ai", "id", "v"); got != "0:1" {
		t.Errorf("id in a direct transaction: %s, want 0:1", got)
	}
}

// ORDER BY naming a select alias does not stop the locking scan at the
// index's order.
func TestMySQLOrderByAliasLocksTheRange(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 0), (2, 0), (3, 0)") })
		open, broke := false, false
		s.Seed(func() { open, broke = false, false })
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("SELECT -id AS id FROM t WHERE id >= 1 ORDER BY id LIMIT 1 FOR UPDATE"); err != nil {
				return err
			}
			open = true
			p.Step("holds its locks")
			open = false
			return tx.Commit()
		})
		s.Manual("writer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "UPDATE t SET v = 1 WHERE id = 3")
			broke = broke || err == nil && open
			return err
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("the row the alias ordered first was written while locked")
			}
			return nil
		})
	})
}

// An ON UPDATE CASCADE that moves a child's key into a locked gap waits, as
// an UPDATE of the child does.
func TestMySQLCascadeWaitsForTheGap(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE parents (id INT PRIMARY KEY)")
		mustExec(t, db, "CREATE TABLE kids (id INT PRIMARY KEY, pid INT, FOREIGN KEY (pid) REFERENCES parents (id) ON UPDATE CASCADE)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO parents VALUES (1), (30), (50)") })
		s.Seed(func() { mustExec(t, db, "INSERT INTO kids VALUES (1, 1), (2, 30), (3, 50)") })
		var first, second int
		s.Seed(func() { first, second = -1, -1 })
		q := "SELECT COUNT(*) FROM kids WHERE pid BETWEEN 5 AND 20 FOR UPDATE"
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if err := tx.QueryRow(q).Scan(&first); err != nil {
				return err
			}
			if err := tx.QueryRow(q).Scan(&second); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.Manual("mover", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "UPDATE parents SET id = 15 WHERE id = 50")
			return err
		})
		s.Always(func(*State) error {
			if second >= 0 && first != second {
				return fmt.Errorf("phantom: %d then %d", first, second)
			}
			return nil
		})
	})
}

// A LIMIT scan whose first row is deleted while it waits goes on to the next
// one, and its gap reaches that row.
func TestMySQLLimitScanRereadsAfterWaiting(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (10, 0), (20, 0), (30, 0)") })
		open, broke, deleted, after := false, false, false, false
		s.Seed(func() { open, broke, deleted, after = false, false, false, false })
		s.Manual("deleter", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("DELETE FROM t WHERE id = 10"); err != nil {
				return err
			}
			p.Step("holds row 10")
			if err := tx.Commit(); err != nil {
				return err
			}
			deleted = true
			return nil
		})
		s.Manual("reader", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("SELECT id FROM t WHERE id >= 1 ORDER BY id LIMIT 1 FOR UPDATE"); err != nil {
				return err
			}
			after = deleted
			open = true
			p.Step("holds its locks")
			open = false
			return tx.Commit()
		})
		s.Manual("writer", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "INSERT INTO t VALUES (15, 0)")
			broke = broke || err == nil && open && after
			return err
		})
		s.AtQuiescence(func(*State) error {
			if broke {
				return fmt.Errorf("an insert went into the gap before the row the scan went on to")
			}
			return nil
		})
	})
}

// IS NULL on an index searches the NULL range, not the whole table.
func TestMySQLIsNullSearchesTheNullRange(t *testing.T) {
	for _, q := range []string{"SELECT id FROM kids WHERE pid IS NULL FOR UPDATE", "SELECT id FROM kids WHERE pid <=> NULL FOR UPDATE"} {
		t.Run(q, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", mysqlBin())
				mustExec(t, db, "CREATE TABLE kids (id INT PRIMARY KEY, pid INT, v INT, KEY idx_pid (pid))")
				s.Seed(func() { mustExec(t, db, "INSERT INTO kids VALUES (1, NULL, 0), (2, 10, 0), (3, 50, 0)") })
				open, wrote := false, false
				s.Seed(func() { open, wrote = false, false })
				s.Manual("reader", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if _, err := tx.Exec(q); err != nil {
						return err
					}
					open = true
					p.Step("holds its locks")
					open = false
					return tx.Commit()
				})
				s.Manual("writer", 1, func(p *Proc) error {
					_, err := db.ExecContext(p.Context(), "UPDATE kids SET v = 1 WHERE id = 3")
					wrote = wrote || err == nil && open
					return err
				})
				s.Sometimes("a row outside the NULL range written while the reader holds its locks", func(*State) bool { return wrote })
			})
		})
	}
}

func TestMySQLThirtyFourthReviewFindings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	for _, q := range []string{"SELECT (2, 0) < (10, 0)", "SET sql_mode = 'NO_BACKSLASH_ESCAPES'", "SET sql_mode = 'ANSI_QUOTES,STRICT_TRANS_TABLES'"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	mustExec(t, db, "SET sql_mode = 'STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION'")
	if _, err := db.Query("SELECT id FROM t LIMIT ?", math.Inf(1)); !errors.Is(err, ErrInvalidParameterValue) {
		t.Errorf("LIMIT +Inf: %v", err)
	}
}

func TestMySQLThirtyFifthReviewFindings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 1), (2, 1), (3, 2)")
	if _, err := db.Exec("ALTER TABLE t DROP PARTITION p0"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("DROP PARTITION: %v", err)
	}
	count := func(q string) int {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer func() { _ = rows.Close() }()
		n := 0
		for rows.Next() {
			n++
		}
		return n
	}
	// Aliases resolve in GROUP BY and inside expressions of HAVING.
	if n := count("SELECT v + 0 AS x, COUNT(*) FROM t GROUP BY x"); n != 2 {
		t.Errorf("GROUP BY alias: %d groups, want 2", n)
	}
	if n := count("SELECT v AS x, COUNT(*) FROM t GROUP BY v HAVING CASE WHEN x > 1 THEN 1 ELSE 0 END = 1"); n != 1 {
		t.Errorf("HAVING alias in CASE: %d groups, want 1", n)
	}
	// A plain subquery of a locking read reads the snapshot.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); err != nil || n != 3 {
		t.Fatalf("snapshot: %d, %v", n, err)
	}
	mustExec(t, db, "INSERT INTO t VALUES (4, 3)")
	rows, err := tx.Query("SELECT id FROM t WHERE id = 1 AND (SELECT COUNT(*) FROM t) = 3 FOR UPDATE")
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for rows.Next() {
		got++
	}
	_ = rows.Close()
	if got != 1 {
		t.Errorf("locking read with a plain subquery: %d rows, want 1", got)
	}
}

// A WHERE no row can match locks nothing.
func TestMySQLContradictionLocksNothing(t *testing.T) {
	for _, q := range []string{"SELECT id FROM t WHERE id > 5 AND id < 3 FOR UPDATE", "SELECT id FROM t WHERE k IS NULL AND k = 1 FOR UPDATE"} {
		t.Run(q, func(t *testing.T) {
			Explore(t, func(t *testing.T, s *Sim) {
				db, _ := s.DB("app", mysqlBin())
				mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, k INT, KEY idx_k (k))")
				s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 1), (10, 10)") })
				open, wrote := false, false
				s.Seed(func() { open, wrote = false, false })
				s.Manual("reader", 1, func(p *Proc) error {
					tx, err := db.BeginTx(p.Context(), nil)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback() }()
					if _, err := tx.Exec(q); err != nil {
						return err
					}
					open = true
					p.Step("holds its locks")
					open = false
					return tx.Commit()
				})
				s.Manual("writer", 1, func(p *Proc) error {
					_, err := db.ExecContext(p.Context(), "INSERT INTO t VALUES (4, NULL)")
					wrote = wrote || err == nil && open
					return err
				})
				s.Sometimes("inserted while the reader holds its locks", func(*State) bool { return wrote })
			})
		})
	}
}

// A column rename reaches the plain indexes on it, which the next-key locks
// search by.
func TestMySQLRenameReachesPlainIndexes(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, a INT, KEY idx_a (a))")
	mustExec(t, db, "ALTER TABLE t RENAME COLUMN a TO b")
	if def := store.defs[store.resolve("t")]; !slices.ContainsFunc(def.indexes, func(ix sqlir.IndexDef) bool { return slices.Equal(ix.Columns, []string{"b"}) }) {
		t.Errorf("indexes after the rename: %v", def.indexes)
	}
}

func TestMySQLThirtySeventhReviewFindings(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, flag TINYINT, body TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 1, 'x'), (2, 0, 'y')")
	// A boolean argument is 1 or 0, as go-sql-driver sends it.
	var id int
	if err := db.QueryRow("SELECT id FROM t WHERE flag = ?", true).Scan(&id); err != nil || id != 1 {
		t.Errorf("flag = true: %d, %v", id, err)
	}
	var neg float64
	if err := db.QueryRow("SELECT -'2'").Scan(&neg); err != nil || neg != -2 {
		t.Errorf("-'2': %v, %v", neg, err)
	}
	// A FULLTEXT index is no index a range search goes along.
	mustExec(t, db, "CREATE FULLTEXT INDEX ft_body ON t (body)")
	if def := store.defs[store.resolve("t")]; len(def.indexes) != 0 {
		t.Errorf("indexes after CREATE FULLTEXT INDEX: %v", def.indexes)
	}
}

func TestMySQLThirtyEighthReviewFindings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	var got string
	if err := db.QueryRow("SELECT LEFT('abc', '2')").Scan(&got); err != nil || got != "ab" {
		t.Errorf("LEFT('abc', '2'): %q, %v", got, err)
	}
	// Deleting a parent its children reference is MySQL's 1451.
	mustExec(t, db, "CREATE TABLE parents (id INT PRIMARY KEY)")
	mustExec(t, db, "CREATE TABLE kids (id INT PRIMARY KEY, pid INT, FOREIGN KEY (pid) REFERENCES parents (id))")
	mustExec(t, db, "INSERT INTO parents VALUES (1)")
	mustExec(t, db, "INSERT INTO kids VALUES (1, 1)")
	_, err := db.Exec("DELETE FROM parents WHERE id = 1")
	if de, ok := errors.AsType[*DBError](err); !ok || de.Number != 1451 || !errors.Is(err, ErrForeignKeyViolation) {
		t.Errorf("parent delete: %v", err)
	}
}

// SKIP LOCKED leaves the rows others hold out before OFFSET counts.
func TestSkipLockedBeforeOffset(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1), (2), (3)") })
		held, got := false, 0
		var seen bool
		s.Seed(func() { held, got, seen = false, 0, false })
		s.Manual("holder", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("SELECT id FROM t WHERE id = 1 FOR UPDATE"); err != nil {
				return err
			}
			held = true
			p.Step("holds row 1")
			held = false
			return tx.Commit()
		})
		s.Manual("claimer", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			h := held
			if err := tx.QueryRow("SELECT id FROM t ORDER BY id LIMIT 1 OFFSET 1 FOR UPDATE SKIP LOCKED").Scan(&got); err != nil {
				return err
			}
			seen = h && held
			return tx.Commit()
		})
		s.AtQuiescence(func(*State) error {
			if seen && got != 3 {
				return fmt.Errorf("OFFSET 1 past held row 1 claimed %d, want 3", got)
			}
			return nil
		})
		s.Sometimes("claimed while row 1 is held", func(*State) bool { return seen })
	})
}

// INSERT IGNORE waits for a transaction deleting the row it collides with,
// and inserts once the delete commits.
func TestMySQLInsertIgnoreWaitsForADelete(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v VARCHAR(5))")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1, 'old')") })
		deleting, started := false, false
		s.Seed(func() { deleting, started = false, false })
		s.Manual("deleter", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("DELETE FROM t WHERE id = 1"); err != nil {
				return err
			}
			deleting = true
			p.Step("holds the deleted row")
			return tx.Commit()
		})
		s.Manual("inserter", 1, func(p *Proc) error {
			started = deleting
			_, err := db.ExecContext(p.Context(), "INSERT IGNORE INTO t VALUES (1, 'new')")
			return err
		})
		s.AtQuiescence(func(*State) error {
			if started {
				if got := peekRows(store, "t", "id", "v"); got != "1:new" {
					return fmt.Errorf("INSERT IGNORE after the delete began left %q", got)
				}
			}
			return nil
		})
		s.Sometimes("the insert starts while the delete is open", func(*State) bool { return started })
	})
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE a (v INT)")
	mustExec(t, db, "INSERT INTO a VALUES (1)")
	if _, err := db.Exec("ALTER TABLE a ADD COLUMN id INT AUTO_INCREMENT UNIQUE"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("AUTO_INCREMENT added to a table with rows: %v", err)
	}
	mustExec(t, db, "CREATE TABLE b (v INT)")
	mustExec(t, db, "ALTER TABLE b ADD COLUMN id INT AUTO_INCREMENT UNIQUE")
}

// BIGINT values above 2^53 compare exactly.
func TestBigintComparesExactly(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id BIGINT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO t VALUES (9007199254740992), (9007199254740993)")
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM t WHERE id <= 9007199254740992").Scan(&n); err != nil || n != 1 {
		t.Errorf("id <= 2^53: %d, %v, want 1", n, err)
	}
}

func TestFortiethReviewFindings(t *testing.T) {
	s := newSim(t)
	my, store := s.DB("my", mysqlBin())
	pg, _ := s.DB("pg", postgres.New())
	// A numeric string goes into a BIGINT and the AUTO_INCREMENT counter
	// exactly.
	mustExec(t, my, "CREATE TABLE b (id BIGINT AUTO_INCREMENT PRIMARY KEY, v INT)")
	mustExec(t, my, "INSERT INTO b VALUES (?, 1)", "9007199254740993")
	res, err := my.Exec("INSERT INTO b (v) VALUES (2)")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 9007199254740994 {
		t.Errorf("id after 9007199254740993: %d", id)
	}
	if got := peekRows(store, "b", "id", "v"); got != "9007199254740993:1 9007199254740994:2" {
		t.Errorf("rows: %s", got)
	}
	var got sql.NullString
	if err := my.QueryRow("SELECT NULLIF('01', 1)").Scan(&got); err != nil || got.Valid {
		t.Errorf("NULLIF('01', 1): %v, %v", got, err)
	}
	if _, err := my.Exec("SELECT UTC_TIMESTAMP()"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("UTC_TIMESTAMP: %v", err)
	}
	// Postgres's locking clauses merge when they agree and are refused when
	// they do not.
	mustExec(t, pg, "CREATE TABLE a (id int PRIMARY KEY)")
	mustExec(t, pg, "CREATE TABLE c (id int PRIMARY KEY)")
	// Postgres refuses more than one locking clause.
	if _, err := pg.Exec("SELECT a.id FROM a, c FOR UPDATE OF a FOR SHARE OF c"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("mixed locking clauses: %v", err)
	}
}

// An integer literal above int64 is refused, except as a LIMIT, where
// MySQL's idiom for no limit uses one.
func TestMySQLHugeIntegerLiterals(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO t VALUES (1)")
	if _, err := db.Exec("SELECT 18446744073709551615 = 18446744073709551614"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("huge literal: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM (SELECT id FROM t LIMIT 0, 18446744073709551615) x").Scan(&n); err != nil || n != 1 {
		t.Errorf("LIMIT 18446744073709551615: %d, %v", n, err)
	}
}

func TestFortySecondReviewFindings(t *testing.T) {
	s := newSim(t)
	my, store := s.DB("my", mysqlBin())
	pg, _ := s.DB("pg", postgres.New())
	if _, err := my.Exec("SELECT 9007199254740993.0"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("huge DECIMAL literal: %v", err)
	}
	mustExec(t, my, "SELECT 0.1 + 0.2")
	var left string
	if err := pg.QueryRow("SELECT left('abcd', -1)").Scan(&left); err != nil || left != "abc" {
		t.Errorf("Postgres left('abcd', -1): %q, %v", left, err)
	}
	// sql_mode entries are read with the spaces around them trimmed.
	mustExec(t, my, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	conn, err := my.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, q := range []string{"SET sql_mode = 'STRICT_TRANS_TABLES, NO_AUTO_VALUE_ON_ZERO'", "INSERT INTO ai VALUES (0, 1)"} {
		if _, err := conn.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if got := peekRows(store, "ai", "id", "v"); got != "0:1" {
		t.Errorf("ids: %s, want 0:1", got)
	}
	// A plain subquery in INSERT ... SELECT reads the snapshot.
	mustExec(t, my, "CREATE TABLE src (id INT PRIMARY KEY)")
	mustExec(t, my, "CREATE TABLE dst (id INT PRIMARY KEY)")
	mustExec(t, my, "INSERT INTO src VALUES (1)")
	tx, err := my.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM src").Scan(&n); err != nil || n != 1 {
		t.Fatalf("snapshot: %d, %v", n, err)
	}
	mustExec(t, my, "INSERT INTO src VALUES (2)")
	res, err := tx.Exec("INSERT INTO dst SELECT id FROM src WHERE id = 1 AND (SELECT COUNT(*) FROM src) = 1")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := res.RowsAffected(); got != 1 {
		t.Errorf("INSERT ... SELECT with a plain subquery: %d rows, want 1", got)
	}
}

// Subqueries of the wrong shape are refused, and a scalar subquery of more
// than one row is a cardinality violation, on either server.
func TestSubqueryShapes(t *testing.T) {
	for _, srv := range []struct {
		name string
		s    Server
	}{{"mysql", mysqlBin()}, {"postgres", postgres.New()}} {
		t.Run(srv.name, func(t *testing.T) {
			s := newSim(t)
			db, _ := s.DB("app", srv.s)
			mustExec(t, db, "CREATE TABLE t (a INT PRIMARY KEY, b INT)")
			mustExec(t, db, "INSERT INTO t VALUES (1, 1), (2, 2)")
			for _, q := range []string{"SELECT 1 IN (SELECT a, b FROM t)", "SELECT (SELECT a, b FROM t WHERE a = 1)"} {
				if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
					t.Errorf("%s: %v", q, err)
				}
			}
			if _, err := db.Exec("SELECT (SELECT a FROM t)"); !errors.Is(err, ErrCardinalityViolation) {
				t.Errorf("scalar subquery of two rows: %v", err)
			}
		})
	}
}

func TestMySQLFortyFourthReviewFindings(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE ai (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO ai VALUES (?, ?)", true, true)
	res, err := db.Exec("INSERT INTO ai (v) VALUES (0)")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 2 {
		t.Errorf("id after an explicit true: %d, want 2", id)
	}
	if got := peekRows(store, "ai", "id", "v"); got != "1:1 2:0" {
		t.Errorf("rows: %s, want 1:1 2:0", got)
	}
	for _, v := range []string{"1/2", "0x10", "1_2"} {
		if _, err := db.Exec("INSERT INTO ai (v) VALUES (?)", v); !errors.Is(err, ErrInvalidTextRepresentation) {
			t.Errorf("%q into INT: %v", v, err)
		}
	}
	if _, err := db.Exec("SELECT 1 IN ((1, 2))"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("IN of a value among rows: %v", err)
	}
	var same bool
	if err := db.QueryRow("SELECT (1, '02') <=> (1, 2)").Scan(&same); err != nil || !same {
		t.Errorf("(1, '02') <=> (1, 2): %v, %v", same, err)
	}
	if err := db.QueryRow("SELECT (1, NULL) <=> (1, NULL)").Scan(&same); err != nil || !same {
		t.Errorf("(1, NULL) <=> (1, NULL): %v, %v", same, err)
	}
}

func TestMySQLFortyFifthReviewFindings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	var n int
	if err := db.QueryRow("WITH Foo AS (SELECT 1 AS x) SELECT COUNT(*) FROM foo").Scan(&n); err != nil || n != 1 {
		t.Errorf("CTE named in another case: %d, %v", n, err)
	}
	if _, err := db.Exec("CREATE TABLE two (a INT AUTO_INCREMENT, b INT AUTO_INCREMENT, PRIMARY KEY (a), UNIQUE KEY (b))"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("two AUTO_INCREMENT columns: %v", err)
	}
}

func TestMySQLFortySixthReviewFindings(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	// A refused CREATE TABLE leaves no table behind.
	if _, err := db.Exec("CREATE TABLE two (a INT AUTO_INCREMENT, b INT AUTO_INCREMENT, PRIMARY KEY (a), UNIQUE KEY (b))"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("two AUTO_INCREMENT columns: %v", err)
	}
	if store.defs[store.resolve("two")] != nil {
		t.Error("the refused table was created")
	}
	// A nested CTE shadows an outer one inside its block only.
	var n int
	if err := db.QueryRow("WITH c AS (SELECT 1 AS x UNION ALL SELECT 2) SELECT (SELECT COUNT(*) FROM (WITH c AS (SELECT 1 AS x) SELECT x FROM c) d) * 10 + (SELECT COUNT(*) FROM c)").Scan(&n); err != nil || n != 12 {
		t.Errorf("nested CTE: %d, %v, want 12", n, err)
	}
	if _, err := db.Query("SELECT 1; SELECT 2"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("two result sets: %v", err)
	}
}

func TestMySQLFortySeventhReviewFindings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 10), (2, 20)")
	count := func(q string) int {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer func() { _ = rows.Close() }()
		n := 0
		for rows.Next() {
			n++
		}
		return n
	}
	if n := count("WITH Foo AS (SELECT 1 AS x) SELECT x FROM Foo UNION ALL SELECT x FROM Foo"); n != 2 {
		t.Errorf("CTE in a set operation: %d rows, want 2", n)
	}
	var id, v int
	if err := db.QueryRow("SELECT U.* FROM t AS U WHERE U.id = 1").Scan(&id, &v); err != nil || id != 1 || v != 10 {
		t.Errorf("U.*: %d %d, %v", id, v, err)
	}
	var first int
	if err := db.QueryRow("SELECT -id AS SortKey FROM t ORDER BY sortkey LIMIT 1").Scan(&first); err != nil || first != -2 {
		t.Errorf("ORDER BY an alias in another case: %d, %v, want -2", first, err)
	}
}

// Temporary tables are the session's own, which detest does not model.
func TestMySQLTemporaryTablesRefused(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	for _, q := range []string{"CREATE TEMPORARY TABLE tmp (id INT)", "DROP TEMPORARY TABLE t"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	mustExec(t, db, "SELECT id FROM t")
}

// A statement that moved a row and then failed leaves no move behind, so a
// waiter on the row is not sent to the key the rolled back statement gave it.
func TestMySQLFailedStatementForgetsMove(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO t VALUES (1), (2), (12)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// 1 moves to 11, then 2 collides with 12 and the statement rolls back.
	if _, err := tx.Exec("UPDATE t SET id = id + 10 ORDER BY id"); !errors.Is(err, ErrUniqueViolation) {
		t.Fatalf("got %v, want a duplicate key", err)
	}
	if _, err := tx.Exec("DELETE FROM t WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "INSERT INTO t VALUES (11)")
	waiter := &Tx{db: store, writes: map[lockKey]Row{}, deleted: map[lockKey]bool{}}
	if key, _, ok := waiter.latest("t", "1"); ok {
		t.Errorf("row 1 was deleted, but its chain leads to %s", key)
	}
}

// CAST targets whose conversion the executor does not carry out as MySQL does
// are refused, and the others convert.
func TestMySQLCastTargets(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, s VARCHAR(10))")
	mustExec(t, db, "INSERT INTO t VALUES (1, '12')")
	for _, q := range []string{
		"SELECT CAST(s AS CHAR(1)) FROM t", "SELECT CAST(s AS BINARY) FROM t", "SELECT CAST(s AS DATE) FROM t",
		"SELECT CAST(s AS DATETIME) FROM t", "SELECT CAST(s AS TIME) FROM t", "SELECT CAST(s AS JSON) FROM t",
		"SELECT CAST(s AS DECIMAL) FROM t", "SELECT CAST(s AS FLOAT) FROM t", "SELECT CAST(s AS YEAR) FROM t",
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	var c string
	var d float64
	var n int64
	if err := db.QueryRow("SELECT CAST(id AS CHAR), CAST(s AS DOUBLE), CAST(s AS SIGNED) FROM t").Scan(&c, &d, &n); err != nil {
		t.Fatal(err)
	}
	if c != "1" || d != 12 || n != 12 {
		t.Errorf("got %q, %v, %v", c, d, n)
	}
}

// With LIMIT and no ORDER BY, a locking read, an UPDATE and a DELETE take the
// rows in the order of the index their scan locks along, so the rows they
// take are the ones whose records and gaps they locked.
func TestMySQLLimitFollowsTheScannedIndex(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, k INT, v INT DEFAULT 0, KEY (k))")
	mustExec(t, db, "INSERT INTO t (id, k) VALUES (1, 20), (2, 10)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var id int
	if err := tx.QueryRow("SELECT id FROM t WHERE k >= 0 LIMIT 1 FOR UPDATE").Scan(&id); err != nil || id != 2 {
		t.Errorf("FOR UPDATE: %d, %v, want 2", id, err)
	}
	if _, err := tx.Exec("UPDATE t SET v = 1 WHERE k >= 0 LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow("SELECT id FROM t WHERE v = 1").Scan(&id); err != nil || id != 2 {
		t.Errorf("UPDATE: %d, %v, want 2", id, err)
	}
	if _, err := tx.Exec("DELETE FROM t WHERE k >= 0 LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow("SELECT id FROM t").Scan(&id); err != nil || id != 1 {
		t.Errorf("DELETE left %d, %v, want 1", id, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// A numeric string bound as []byte counts as the number in an integer column:
// its range is checked, and an explicit AUTO_INCREMENT value moves the counter.
func TestMySQLBytesIntoIntegerColumns(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, n TINYINT)")
	if _, err := db.Exec("INSERT INTO t (n) VALUES (?)", []byte("300")); !errors.Is(err, ErrNumericValueOutOfRange) {
		t.Errorf("TINYINT 300: %v, want out of range", err)
	}
	mustExec(t, db, "INSERT INTO t (id, n) VALUES (?, ?)", []byte("10"), []byte("7"))
	res, err := db.Exec("INSERT INTO t (n) VALUES (1)")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 11 {
		t.Errorf("next id: %d, want 11", id)
	}
}

// An aggregate of a subquery or a window does not make the query one group,
// so HAVING without either is still refused.
func TestMySQLHavingSeesItsOwnBlock(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	for _, q := range []string{
		"SELECT id, (SELECT COUNT(*) FROM t) AS c FROM t HAVING id > 0",
		"SELECT id, SUM(v) OVER () AS s FROM t HAVING id > 0",
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	mustExec(t, db, "SELECT COUNT(*) AS n FROM t HAVING n >= 0")
}

// AUTO_INCREMENT=n of a schema made before the runs holds in every run, which
// starts with the tables empty again.
func TestMySQLAutoIncrementOptionInEveryRun(t *testing.T) {
	var ids []int64
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, v INT) AUTO_INCREMENT=100")
		s.Manual("insert", 1, func(p *Proc) error {
			res, err := db.ExecContext(p.Context(), "INSERT INTO t (v) VALUES (1)")
			if err != nil {
				return err
			}
			id, _ := res.LastInsertId()
			ids = append(ids, id)
			return nil
		})
	})
	if len(ids) == 0 {
		t.Fatal("no run")
	}
	for _, id := range ids {
		if id != 100 {
			t.Errorf("ids %v, want 100 in every run", ids)
			break
		}
	}
}

// MySQL's numeric functions and SUM and AVG take a string as the number it
// converts to. GREATEST and LEAST of numbers and strings, and ROUND of a
// string, round or compare by rules detest does not follow, and are refused.
func TestMySQLNumericFunctionsOfStrings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, s VARCHAR(10))")
	mustExec(t, db, "INSERT INTO t VALUES (1, '10'), (2, '5')")
	var abs, floor, sum, avg float64
	if err := db.QueryRow("SELECT ABS('-2'), FLOOR('2.5'), (SELECT SUM(s) FROM t), (SELECT AVG(s) FROM t)").Scan(&abs, &floor, &sum, &avg); err != nil {
		t.Fatal(err)
	}
	if abs != 2 || floor != 2 || sum != 15 || avg != 7.5 {
		t.Errorf("got %v, %v, %v, %v", abs, floor, sum, avg)
	}
	for _, q := range []string{"SELECT GREATEST(2, 10, '0')", "SELECT LEAST(s, 3) FROM t", "SELECT ROUND('2.5')"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
}

// A multi-table UPDATE that sets a column the first table lacks is refused,
// as MySQL would update the joined table that has it.
func TestMySQLJoinedUpdateOfAnotherTable(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE a (id INT PRIMARY KEY, x INT)")
	mustExec(t, db, "CREATE TABLE b (id INT PRIMARY KEY, y INT)")
	mustExec(t, db, "INSERT INTO a VALUES (1, 0)")
	mustExec(t, db, "INSERT INTO b VALUES (1, 0)")
	if _, err := db.Exec("UPDATE a JOIN b ON a.id = b.id SET y = 5"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("SET y: %v, want unsupported", err)
	}
	mustExec(t, db, "UPDATE a JOIN b ON a.id = b.id SET x = b.y + 1")
	if got := peekRows(store, "a", "id", "x"); got != "1:1" {
		t.Errorf("SET x: %s", got)
	}
}

// MySQL's arithmetic takes a boolean as 1 or 0, and a division by zero in a
// SELECT is NULL, while a write refuses it in strict mode.
func TestMySQLArithmeticOfBooleansAndZero(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	var n int64
	var q sql.NullFloat64
	if err := db.QueryRow("SELECT (1 = 1) + 1, 1 / 0").Scan(&n, &q); err != nil {
		t.Fatal(err)
	}
	if n != 2 || q.Valid {
		t.Errorf("got %d, %v", n, q)
	}
	if _, err := db.Exec("INSERT INTO t VALUES (1, 1 / 0)"); !errors.Is(err, ErrDivisionByZero) {
		t.Errorf("INSERT of 1 / 0: %v", err)
	}
	if _, err := db.Exec("INSERT INTO t SELECT 2, 1 % 0"); !errors.Is(err, ErrDivisionByZero) {
		t.Errorf("INSERT ... SELECT of 1 %% 0: %v", err)
	}
}

// The result of a multi-statement Exec reports the last statement's insert
// id, as go-sql-driver's does.
func TestMySQLMultiStatementLastInsertID(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE c (id INT AUTO_INCREMENT PRIMARY KEY)")
	res, err := db.Exec("INSERT INTO c VALUES (NULL); INSERT INTO c VALUES (NULL)")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 2 {
		t.Errorf("LastInsertId %d, want 2", id)
	}
}

// Comparisons take a []byte argument as the string it holds.
func TestMySQLBytesCompareAsStrings(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(10))")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'a'), (2, 'c')")
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM t WHERE name < ?", []byte("b")).Scan(&n); err != nil || n != 1 {
		t.Errorf("name < 'b': %d, %v", n, err)
	}
}

// A schema with a prefix index builds, and a locking search that only the
// prefix index serves is refused, as the gap locks do not model its prefixes.
func TestMySQLPrefixIndexSearch(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(20), code VARCHAR(20), KEY (name(3)), UNIQUE KEY (code(4)))")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'abcdef', 'x1')")
	for _, q := range []string{
		"SELECT id FROM t WHERE name = 'abcdef' FOR UPDATE",
		"UPDATE t SET name = 'z' WHERE name = 'abcdef'",
		"DELETE FROM t WHERE code = 'x1'",
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	mustExec(t, db, "SELECT id FROM t WHERE id = 1 AND name = 'abcdef' FOR UPDATE")
	mustExec(t, db, "SELECT id FROM t WHERE name = 'abcdef'")
}

// An insert by another transaction of the same process into a gap its open
// transaction locked can never proceed, which is reported as for a row lock.
func TestMySQLGapWaitOnOwnTransaction(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
		s.Seed(func() { mustExec(t, db, "INSERT INTO t VALUES (1)") })
		s.Manual("handler", 1, func(p *Proc) error {
			tx, err := db.BeginTx(p.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("SELECT id FROM t WHERE id > 5 FOR UPDATE"); err != nil {
				return err
			}
			// An in-process RPC inserts into the gap the caller locked.
			if _, err := db.ExecContext(p.Context(), "INSERT INTO t VALUES (10)"); err != nil {
				return err
			}
			return tx.Commit()
		})
		s.ExpectViolation("own open transaction")
	})
}

// An INSERT ... VALUES reserves its AUTO_INCREMENT values before inserting
// any row: they are consecutive whatever inserts run meanwhile, and a row an
// error stops still used its value.
func TestMySQLInsertValuesReservesIDs(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		db, store := s.DB("app", mysqlBin())
		mustExec(t, db, "CREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(1))")
		s.Manual("batch", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "INSERT INTO t (v) VALUES ('a'), ('b')")
			return err
		})
		s.Manual("single", 1, func(p *Proc) error {
			_, err := db.ExecContext(p.Context(), "INSERT INTO t (v) VALUES ('s')")
			return err
		})
		s.AtQuiescence(func(*State) error {
			if got := peekRows(store, "t", "id", "v"); got != "1:a 2:b 3:s" && got != "1:s 2:a 3:b" {
				return fmt.Errorf("rows %s, want a and b on consecutive ids", got)
			}
			return nil
		})
	})

	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE u (id INT AUTO_INCREMENT PRIMARY KEY, n TINYINT)")
	if _, err := db.Exec("INSERT INTO u (n) VALUES (300), (1)"); !errors.Is(err, ErrNumericValueOutOfRange) {
		t.Fatalf("got %v, want out of range", err)
	}
	res, err := db.Exec("INSERT INTO u (n) VALUES (1)")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 3 {
		t.Errorf("next id %d, want 3", id)
	}
}

// A failed statement's rows go with their locks, while the locks it took on
// rows that stay are kept.
func TestMySQLFailedStatementReleasesInsertLocks(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 0), (3, 0)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("UPDATE t SET v = 1 WHERE id = 3"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO t VALUES (2, 0), (1, 0)"); !errors.Is(err, ErrUniqueViolation) {
		t.Fatalf("got %v, want a duplicate key", err)
	}
	table := store.resolve("t")
	if held := store.locks[lockKey{table, "2"}]; len(held) > 0 {
		t.Error("the rolled back row 2 is still locked")
	}
	if held := store.locks[lockKey{table, "3"}]; len(held) == 0 {
		t.Error("the updated row 3 lost its lock")
	}
}

// String bounds on a number column take the numbers they convert to, so a
// locking search narrows its range as MySQL does: '20' is above '5'.
func TestMySQLStringBoundsOnNumberColumn(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO t VALUES (10), (30)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var id int
	if err := tx.QueryRow("SELECT id FROM t WHERE id > '5' AND id > '20' FOR UPDATE").Scan(&id); err != nil || id != 30 {
		t.Fatalf("got %d, %v", id, err)
	}
	if held := store.locks[lockKey{store.resolve("t"), "10"}]; len(held) > 0 {
		t.Error("row 10, below the range, is locked")
	}
}

// A row seeded with an explicit AUTO_INCREMENT value moves the counter past
// it, as an insert of it would.
func TestMySQLSeedMovesAutoIncrement(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, v INT)")
	store.SeedRow("t", Row{"id": int64(10), "v": int64(0)})
	res, err := db.Exec("INSERT INTO t (v) VALUES (1)")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 11 {
		t.Errorf("next id %d, want 11", id)
	}
}

// A number compared with a text column cannot search the column's index,
// as '01' and '1' both equal 1, so a locking search by it locks as a scan
// without that index does, gaps included.
func TestMySQLNumberSearchOnTextIndex(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, code VARCHAR(5), UNIQUE KEY (code))")
	mustExec(t, db, "INSERT INTO t VALUES (1, '1'), (2, '01')")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query("SELECT id FROM t WHERE code = 1 FOR UPDATE")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	_ = rows.Close()
	if n != 2 {
		t.Errorf("matched %d rows, want 2", n)
	}
	if len(store.gaps) == 0 {
		t.Error("no gap is locked, so an insert of '001' could slip in")
	}
}

// Window functions take the arguments MySQL does: LAG and LEAD take a
// nonnegative integer offset.
func TestMySQLWindowArguments(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO t VALUES (1), (2)")
	// MySQL's grammar refuses an offset other than a literal or a parameter.
	for _, q := range []string{"SELECT LAG(id, -1) OVER (ORDER BY id) FROM t", "SELECT LAG(id, id) OVER (ORDER BY id) FROM t"} {
		if _, err := db.Exec(q); !errors.Is(err, ErrSyntaxError) {
			t.Errorf("%s: %v, want a syntax error", q, err)
		}
	}
	if _, err := db.Exec("SELECT NTILE(2) OVER (ORDER BY id) FROM t"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("NTILE: %v, want unsupported", err)
	}
	if _, err := db.Exec("SELECT LAG(id, ?) OVER (ORDER BY id) FROM t", -1); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("bound offset -1: %v, want unsupported", err)
	}
	var v sql.NullInt64
	if err := db.QueryRow("SELECT LAG(id, ?) OVER (ORDER BY id) FROM t ORDER BY id DESC LIMIT 1", 1).Scan(&v); err != nil || v.Int64 != 1 {
		t.Errorf("bound offset 1: %v, %v", v, err)
	}
	mustExec(t, db, "SELECT COUNT(*) OVER (), ROW_NUMBER() OVER (ORDER BY id), FIRST_VALUE(id) OVER (ORDER BY id) FROM t")
}

// A text column stores a number as its text, which then compares as text.
// A float's text and a number in a temporal column are refused.
func TestMySQLNumberStoredInTextColumn(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, s VARCHAR(10), d DATETIME)")
	mustExec(t, db, "INSERT INTO t (id, s) VALUES (1, 10)")
	mustExec(t, db, "INSERT INTO t (id, s) VALUES (2, 'x')")
	mustExec(t, db, "UPDATE t SET s = 3 WHERE id = 2")
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM t WHERE s < '2'").Scan(&n); err != nil || n != 1 {
		t.Errorf("s < '2': %d, %v, want 1 ('10')", n, err)
	}
	for _, q := range []string{"INSERT INTO t (id, s) VALUES (3, 1.5)", "INSERT INTO t (id, d) VALUES (4, 20240101)"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
}

// ON UPDATE CURRENT_TIMESTAMP sets the column on an update that changes the
// row and does not set it, and NOW() has whole seconds unless asked for more.
func TestMySQLOnUpdateCurrentTimestamp(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT, at DATETIME ON UPDATE CURRENT_TIMESTAMP, at6 DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6))")
	mustExec(t, db, "INSERT INTO t (id, v, at, at6) VALUES (1, 0, NULL, NULL), (2, 0, NULL, NULL)")
	mustExec(t, db, "UPDATE t SET v = 1 WHERE id = 1")
	mustExec(t, db, "UPDATE t SET v = 0 WHERE id = 2") // no change, no touch
	got := map[string]Row{}
	for _, r := range store.Peek("t") {
		got[fmt.Sprint(r["id"])] = r
	}
	at, ok := derefValue(got["1"]["at"]).(time.Time)
	if !ok || at.Nanosecond() != 0 {
		t.Errorf("row 1 at = %v, want a whole-second time", got["1"]["at"])
	}
	if _, ok := derefValue(got["1"]["at6"]).(time.Time); !ok {
		t.Errorf("row 1 at6 = %v, want a time", got["1"]["at6"])
	}
	if got["2"]["at"] != nil {
		t.Errorf("row 2 at = %v, want NULL as the update changed nothing", got["2"]["at"])
	}
	mustExec(t, db, "ALTER TABLE t MODIFY at DATETIME")
	mustExec(t, db, "UPDATE t SET v = 5 WHERE id = 2")
	for _, r := range store.Peek("t") {
		if fmt.Sprint(r["id"]) == "2" && r["at"] != nil {
			t.Errorf("MODIFY without ON UPDATE kept it: at = %v", r["at"])
		}
	}
}

// DATE, DATETIME and TIMESTAMP columns hold a time whether written as a
// string or a time, so both compare alike, also with a string literal.
func TestMySQLTemporalValues(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, at DATETIME, d DATE, note VARCHAR(30))")
	mustExec(t, db, "INSERT INTO t (id, at, d) VALUES (1, '2024-01-01 10:00:00', '2024-01-01 23:00:00')")
	mustExec(t, db, "INSERT INTO t (id, at, d, note) VALUES (2, ?, ?, ?)", time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 5, 0, 0, 0, time.UTC), time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC))
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM t WHERE at < ?", time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC)).Scan(&n); err != nil || n != 2 {
		t.Errorf("at < a time: %d, %v", n, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM t WHERE at < '2024-01-01 09:30:00'").Scan(&n); err != nil || n != 1 {
		t.Errorf("at < a string: %d, %v", n, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM t WHERE d = '2024-01-01'").Scan(&n); err != nil || n != 1 {
		t.Errorf("d = a date: %d, %v", n, err)
	}
	if got := peekRows(store, "t", "id", "note"); got != "1:<nil> 2:2024-01-01 09:00:00" {
		t.Errorf("a time in a VARCHAR: %s", got)
	}
	if _, err := db.Exec("INSERT INTO t (id, at) VALUES (3, 'garbage')"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("garbage: %v, want unsupported", err)
	}
}

// innodb_lock_wait_timeout takes an integer, raised to the minimum of 1, so
// a lock wait may still time out.
func TestMySQLLockWaitTimeoutValues(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	for _, q := range []string{"SET innodb_lock_wait_timeout = 'x'", "SET innodb_lock_wait_timeout = 1.5"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	st, err := parseWith(sqlir.ImplOf(mysql.New()).Parser(), "SET innodb_lock_wait_timeout = 0")
	if err != nil {
		t.Fatal(err)
	}
	if set, ok := st.stmt.(*sqlir.SetStmt); !ok || set.Value != "1" {
		t.Errorf("0: %#v, want the minimum of 1", st.stmt)
	}
}

// CHAR(n) and VARCHAR(n) refuse a longer string, and ENUM and SET a value
// that names no member, as MySQL's strict mode does.
func TestMySQLStringLimits(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(3), st ENUM('NEW', 'DONE'), tags SET('a', 'b', 'c'))")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'äbc', 'new', 'c,A')")
	if got := peekRows(store, "t", "st", "tags"); got != "NEW:a,c" {
		t.Errorf("stored %s, want the members as declared", got)
	}
	if _, err := db.Exec("INSERT INTO t (id, name) VALUES (2, 'abcd')"); !errors.Is(err, ErrStringDataRightTruncation) {
		t.Errorf("VARCHAR(3) of 'abcd': %v", err)
	}
	if _, err := db.Exec("UPDATE t SET st = 'GONE' WHERE id = 1"); !errors.Is(err, ErrDataTruncated) {
		t.Errorf("ENUM of 'GONE': %v", err)
	}
	if _, err := db.Exec("UPDATE t SET tags = 'a,z' WHERE id = 1"); !errors.Is(err, ErrDataTruncated) {
		t.Errorf("SET of 'a,z': %v", err)
	}
}

// A NULL member of an indexed IN list matches no row, so the other members
// still bound the search, which locks them and not the whole table.
func TestMySQLInListWithNull(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO t VALUES (1), (5)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SELECT id FROM t WHERE id IN (1, NULL) FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	if held := store.locks[lockKey{store.resolve("t"), "5"}]; len(held) > 0 {
		t.Error("row 5, outside the IN list, is locked")
	}
}

// CAST(? AS SIGNED) of a bound boolean is 1 or 0, as go-sql-driver sends it,
// and a TIME column, which detest does not model, refuses values.
func TestMySQLBoolCastAndTime(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	var n int64
	if err := db.QueryRow("SELECT CAST(? AS SIGNED)", true).Scan(&n); err != nil || n != 1 {
		t.Errorf("CAST(true AS SIGNED): %d, %v", n, err)
	}
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, d TIME)")
	mustExec(t, db, "INSERT INTO t VALUES (1, NULL)")
	if _, err := db.Exec("INSERT INTO t VALUES (2, '10:00:00')"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("TIME value: %v, want unsupported", err)
	}
}

// An integer column refuses a bound value it does not convert, such as a
// time.Time, whose text MySQL would refuse.
func TestMySQLIntegerColumnRefusesOtherValues(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	if _, err := db.Exec("INSERT INTO t VALUES (1, ?)", time.Now()); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("time.Time into INT: %v, want unsupported", err)
	}
}

// MySQL has no ILIKE, which TiDB's parser takes, so it is refused.
func TestMySQLRefusesILike(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, s VARCHAR(5))")
	if _, err := db.Exec("SELECT id FROM t WHERE s ILIKE 'a%'"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("ILIKE: %v, want unsupported", err)
	}
}

// A Postgres SET lock_timeout in a transaction holds for the session only
// once the transaction commits; a rollback undoes it.
func TestPostgresSessionLockTimeoutFollowsTheTransaction(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	session := func() bool {
		var on bool
		_ = conn.Raw(func(dc any) error {
			if c, ok := dc.(*sqlConn); ok {
				on = c.lockTimeout
			}
			return nil
		})
		return on
	}
	for _, commit := range []bool{false, true} {
		tx, err := conn.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("SET lock_timeout = '1s'"); err != nil {
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		if session() != commit {
			t.Errorf("commit %v: the session's lock timeout is %v", commit, session())
		}
	}
}

// A search fixing every column of a composite unique index is a point
// lookup, which locks the row only; one fixing a prefix locks that prefix's
// keys and the gaps around them in key order, not every row of the leading
// column's value.
func TestMySQLCompositeIndexSearch(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (tenant INT, id INT, v INT, PRIMARY KEY (tenant, id))")
	mustExec(t, db, "INSERT INTO t VALUES (1, 1, 0), (1, 5, 0), (1, 9, 0), (2, 1, 0)")
	table := store.resolve("t")
	locked := func(key string) bool { return len(store.locks[lockKey{table, key}]) > 0 }
	keyOfRow := func(tenant, id int64) string {
		r := Row{"tenant": tenant, "id": id}
		if err := store.assignKey(table, r); err != nil {
			t.Fatal(err)
		}
		return r.Key()
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("SELECT v FROM t WHERE tenant = 1 AND id = 5 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	if !locked(keyOfRow(1, 5)) || locked(keyOfRow(1, 1)) || locked(keyOfRow(1, 9)) {
		t.Error("a point lookup on the whole key should lock (1, 5) only")
	}
	if len(store.gaps) != 0 {
		t.Errorf("a point lookup that finds its row takes no gap: %d", len(store.gaps))
	}
	_ = tx.Rollback()

	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("SELECT v FROM t WHERE tenant = 1 AND id > 4 AND id < 8 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	if !locked(keyOfRow(1, 5)) || !locked(keyOfRow(1, 9)) || locked(keyOfRow(1, 1)) || locked(keyOfRow(2, 1)) {
		t.Error("a range on the second column should lock (1, 5) and the next record (1, 9) only")
	}
	covered := func(tenant, id int64) bool {
		r := Row{"tenant": tenant, "id": id}
		return slices.ContainsFunc(store.gaps, func(g *gapLock) bool { return g.covers(r) })
	}
	if !covered(1, 3) || !covered(1, 7) || covered(1, 0) || covered(1, 10) || covered(2, 0) {
		t.Error("the gaps should run from (1, 1) to (1, 9) only")
	}
	_ = tx.Rollback()
}

// A unique key with a NULL is no point lookup, as a unique index holds any
// number of them, so the search locks the gap around it.
func TestMySQLUniqueNullKeyLocksTheGap(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, code INT, UNIQUE KEY (code))")
	mustExec(t, db, "INSERT INTO t VALUES (1, NULL), (2, 5)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SELECT id FROM t WHERE code IS NULL FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(store.gaps, func(g *gapLock) bool { return g.covers(Row{"id": int64(3), "code": nil}) }) {
		t.Error("another NULL code can be inserted past the locked search")
	}
}

// A composite foreign key uses an index that leads with all of its columns,
// which InnoDB creates when none does.
func TestMySQLCompositeForeignKeyIndex(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE p (a INT, b INT, PRIMARY KEY (a, b))")
	mustExec(t, db, "CREATE TABLE c (id INT PRIMARY KEY, a INT, b INT, x INT, KEY (a, x), FOREIGN KEY (a, b) REFERENCES p (a, b))")
	def := store.defs[store.resolve("c")]
	if !slices.ContainsFunc(def.indexes, func(ix sqlir.IndexDef) bool { return slices.Equal(ix.Columns, []string{"a", "b"}) }) {
		t.Errorf("no (a, b) index for the foreign key: %v", def.indexes)
	}
}

// A locking search that more than one index serves, none by a unique point
// lookup, is refused: MySQL's optimizer picks the index by its statistics.
func TestMySQLAmbiguousIndexSearch(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, a INT, b INT, KEY (a), KEY (b))")
	mustExec(t, db, "INSERT INTO t VALUES (1, 1, 1)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SELECT id FROM t WHERE a = 1 AND b = 1 FOR UPDATE"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("two candidate indexes: %v, want unsupported", err)
	}
	for _, q := range []string{
		"SELECT id FROM t WHERE id = 1 AND a = 1 FOR UPDATE", // the primary key is a point lookup
		"SELECT id FROM t WHERE a = 1 FOR UPDATE",
	} {
		if _, err := tx.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// DATETIME and TIMESTAMP round fractional seconds to the precision declared,
// none by default, so a time compares with what the column holds as MySQL
// compares it.
func TestMySQLTemporalPrecision(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, a DATETIME, b DATETIME(3))")
	at := time.Date(2024, 1, 1, 10, 0, 0, 123456789, time.UTC)
	mustExec(t, db, "INSERT INTO t VALUES (1, ?, ?)", at, at)
	r := store.Peek("t")[0]
	if got := derefValue(r["a"]); got != time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC) {
		t.Errorf("DATETIME holds %v, want whole seconds", got)
	}
	if got := derefValue(r["b"]); got != time.Date(2024, 1, 1, 10, 0, 0, 123000000, time.UTC) {
		t.Errorf("DATETIME(3) holds %v, want milliseconds", got)
	}
}

// MySQL commits a transaction before DDL, which detest does not do, so DDL
// inside a transaction is refused.
func TestMySQLDDLInsideTransaction(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{"CREATE TABLE t (id INT PRIMARY KEY)", "CREATE TABLE u AS SELECT 1 AS x"} {
		if _, err := tx.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
}

// Equal keys of a secondary index are in primary key order by value, so a
// LIMIT scan takes id 2 before id 10.
func TestMySQLSecondaryIndexTiesByPrimaryKey(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, k INT, v INT DEFAULT 0, KEY (k))")
	mustExec(t, db, "INSERT INTO t (id, k) VALUES (10, 1), (2, 1)")
	mustExec(t, db, "UPDATE t SET v = 1 WHERE k = 1 LIMIT 1")
	if got := peekRows(store, "t", "id", "v"); got != "10:0 2:1" {
		t.Errorf("updated %s, want id 2", got)
	}
}

// A locking search by a descending index is refused, as the gap locks follow
// ascending key order, and so is a descending unique key.
func TestMySQLDescendingIndex(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, k INT, KEY (k DESC))")
	if _, err := db.Exec("SELECT id FROM t WHERE k > 1 FOR UPDATE"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("search by a descending index: %v, want unsupported", err)
	}
	if _, err := db.Exec("CREATE TABLE u (id INT, PRIMARY KEY (id DESC))"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("descending primary key: %v, want unsupported", err)
	}
}

// A sql_mode without strict mode is refused, as detest keeps strict mode's
// errors; the mode mysqldump sets and a restore from a variable are not.
func TestMySQLNonStrictSQLMode(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	for _, q := range []string{"SET sql_mode = ''", "SET sql_mode = 'NO_ENGINE_SUBSTITUTION'"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	mustExec(t, db, "SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO'")
	mustExec(t, db, "SET SQL_MODE=@OLD_SQL_MODE")
	mustExec(t, db, "SET sql_mode = 'TRADITIONAL'")
}

// At Read Committed, which takes no gap locks, a LIMIT scan without ORDER BY
// still follows the index it searches by.
func TestMySQLReadCommittedLimitFollowsTheIndex(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin(mysql.Isolation(ReadCommitted)))
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, k INT, v INT DEFAULT 0, KEY (k))")
	mustExec(t, db, "INSERT INTO t (id, k) VALUES (1, 20), (2, 10)")
	mustExec(t, db, "UPDATE t SET v = 1 WHERE k >= 0 LIMIT 1")
	if got := peekRows(store, "t", "id", "v"); got != "1:0 2:1" {
		t.Errorf("updated %s, want id 2", got)
	}
}

// Crossed string bounds match no row, so the search locks nothing.
func TestMySQLCrossedStringBounds(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, code VARCHAR(5), KEY (code))")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'm')")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SELECT id FROM t WHERE code > 'z' AND code < 'a' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	if len(store.gaps) != 0 || len(store.locks) != 0 {
		t.Errorf("an empty range locked %d gaps and %d rows", len(store.gaps), len(store.locks))
	}
}

// A CHECK detest cannot convert loads with the schema, and a write it would
// guard fails as unsupported rather than pass unchecked.
func TestMySQLUnconvertedCheck(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, s VARCHAR(10) CHECK (s REGEXP '^[a-z]+$'), CONSTRAINT c2 CHECK (SOUNDEX(s) <> ''))")
	if _, err := db.Exec("INSERT INTO t VALUES (1, 'ABC')"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a write the check guards: %v, want unsupported", err)
	}
}

// A column added with a default gives existing rows the default as an
// insert would store it, converted to the column's type.
func TestMySQLBackfillConvertsTheDefault(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO t VALUES (1)")
	mustExec(t, db, "ALTER TABLE t ADD COLUMN n INT DEFAULT '5'")
	if got := derefValue(store.Peek("t")[0]["n"]); got != int64(5) {
		t.Errorf("backfilled %#v, want int64 5", got)
	}
}

// A LIMIT scan of a secondary index stops at an entry of a key with
// duplicates, and the gap up to it is bounded by the primary key that InnoDB
// appends: an insert of the same key below that entry falls in it, one
// above does not.
func TestMySQLLimitGapIncludesThePrimaryKey(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, k INT, KEY (k))")
	mustExec(t, db, "INSERT INTO t VALUES (1, 5), (3, 5), (5, 5)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SELECT id FROM t WHERE k >= 5 LIMIT 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	covered := func(id int64) bool {
		r := Row{"id": id, "k": int64(5)}
		return slices.ContainsFunc(store.gaps, func(g *gapLock) bool { return g.covers(r) })
	}
	if !covered(0) || covered(2) {
		t.Errorf("covers (5, 0): %v, want true; (5, 2): %v, want false", covered(0), covered(2))
	}
}

// A locking search with OR on an indexed column is refused, as MySQL's
// optimizer picks how to search it; IN is searched as points.
func TestMySQLLockingSearchWithOr(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 0), (2, 0)")
	if _, err := db.Exec("UPDATE t SET v = 1 WHERE id = 1 OR id = 2"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("OR on the primary key: %v, want unsupported", err)
	}
	mustExec(t, db, "UPDATE t SET v = 1 WHERE id IN (1, 2)")
	mustExec(t, db, "UPDATE t SET v = 2 WHERE id = 1 AND (v = 1 OR v = 3)")
}

// A LIMIT scan the WHERE bounds by no index follows the primary key from its
// start and stops after LIMIT rows, so it locks those, not the whole table.
func TestMySQLLimitScanOfTheClusteredIndex(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 0), (10, 0), (20, 0)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("UPDATE t SET v = 1 WHERE v = 0 LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	table := store.resolve("t")
	if len(store.locks[lockKey{table, "2"}]) == 0 || len(store.locks[lockKey{table, "10"}]) > 0 {
		t.Error("the scan should lock id 2, the first in the primary key, and stop")
	}
	covered := func(id int64) bool {
		r := Row{"id": id, "v": int64(0)}
		return slices.ContainsFunc(store.gaps, func(g *gapLock) bool { return g.covers(r) })
	}
	if !covered(1) || covered(5) {
		t.Errorf("covers 1: %v, want true; 5: %v, want false", covered(1), covered(5))
	}
}

// A locking search that reaches a prefix part after an index's whole columns
// is refused, as one by a prefix index is.
func TestMySQLPrefixPartAfterColumns(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, a INT, b VARCHAR(20), KEY (a, b(3)))")
	if _, err := db.Exec("SELECT id FROM t WHERE a = 1 AND b = 'abcdef' FOR UPDATE"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a search reaching b(3): %v, want unsupported", err)
	}
	mustExec(t, db, "SELECT id FROM t WHERE a = 1 FOR UPDATE")
}

// MODIFY of a column's type converts the values the rows hold, as MySQL does.
func TestMySQLModifyConvertsValues(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, n VARCHAR(10))")
	mustExec(t, db, "INSERT INTO t VALUES (1, '42')")
	mustExec(t, db, "ALTER TABLE t MODIFY n INT")
	if got := derefValue(store.Peek("t")[0]["n"]); got != int64(42) {
		t.Errorf("n is %#v, want int64 42", got)
	}
	mustExec(t, db, "INSERT INTO t VALUES (2, '0')")
	if _, err := db.Exec("ALTER TABLE t MODIFY n VARCHAR(10)"); err != nil {
		t.Fatal(err)
	}
	if got := peekRows(store, "t", "id", "n"); got != "1:42 2:0" {
		t.Errorf("rows %s", got)
	}
}

// A Postgres SET lock_timeout after a savepoint is undone by ROLLBACK TO it,
// so the commit leaves the session as before.
func TestPostgresSavepointUndoesSessionLockTimeout(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"SAVEPOINT s", "SET lock_timeout = '1s'", "ROLLBACK TO SAVEPOINT s"} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	on := false
	_ = conn.Raw(func(dc any) error {
		if c, ok := dc.(*sqlConn); ok {
			on = c.lockTimeout
		}
		return nil
	})
	if on {
		t.Error("the rolled back SET lock_timeout holds for the session")
	}
}

// time_zone is accepted as UTC, which detest keeps temporal values in, and
// as a dump restores it from a variable; another zone is refused.
func TestMySQLTimeZone(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "SET @OLD_TIME_ZONE=@@TIME_ZONE")
	mustExec(t, db, "SET TIME_ZONE='+00:00'")
	mustExec(t, db, "SET TIME_ZONE=@OLD_TIME_ZONE")
	mustExec(t, db, "SET time_zone = 'UTC'")
	for _, q := range []string{"SET time_zone = '+09:00'", "SET time_zone = 'Asia/Tokyo'", "SET time_zone = 'SYSTEM'"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
}

// CheckSQL refuses a multi-statement query of more than one result set, as
// running it does.
func TestMySQLCheckSQLMultipleResultSets(t *testing.T) {
	if err := CheckSQL(mysqlBin(), "SELECT 1; SELECT 2"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("CheckSQL: %v, want unsupported", err)
	}
}

// ROLLBACK TO a savepoint restores the transaction's Postgres lock timeout
// as it was there.
func TestPostgresSavepointRestoresLockTimeout(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{"SAVEPOINT s", "SET LOCAL lock_timeout = '1s'", "ROLLBACK TO SAVEPOINT s"} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	on := true
	_ = conn.Raw(func(dc any) error {
		if c, ok := dc.(*sqlConn); ok && c.tx != nil {
			on = c.tx.lockTimeout
		}
		return nil
	})
	if on {
		t.Error("the lock timeout set after the savepoint holds after ROLLBACK TO")
	}
}

// A column added with a volatile default gives each row one value, which
// the versions snapshots read share.
func TestMySQLBackfillSharesVolatileDefault(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 0)")
	mustExec(t, db, "UPDATE t SET v = 1 WHERE id = 1")
	mustExec(t, db, "ALTER TABLE t ADD COLUMN u VARCHAR(36) DEFAULT (UUID())")
	table := store.resolve("t")
	cur := store.committed[table]["1"]["u"]
	for _, v := range store.history[table]["1"] {
		if v.row != nil && v.row["u"] != cur {
			t.Errorf("a version holds %v, the row %v", v.row["u"], cur)
		}
	}
}

// ON UPDATE CURRENT_TIMESTAMP compares the row as it stores it, so a value
// that converts back to the one held changes nothing.
func TestMySQLOnUpdateComparesStoredValues(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, n INT, at DATETIME ON UPDATE CURRENT_TIMESTAMP)")
	mustExec(t, db, "INSERT INTO t (id, n) VALUES (1, 1)")
	res, err := db.Exec("UPDATE t SET n = 1.2 WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("affected %d, want 0", n)
	}
	if at := store.Peek("t")[0]["at"]; at != nil {
		t.Errorf("at = %v, want NULL as nothing changed", at)
	}
}

// INSERT IGNORE skips a duplicate key, and refuses as unsupported a row whose
// other error MySQL would turn into a warning and store coerced or skip.
func TestMySQLInsertIgnoreDowngrades(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, n TINYINT, s VARCHAR(2))")
	mustExec(t, db, "INSERT INTO t VALUES (1, 0, 'a')")
	mustExec(t, db, "INSERT IGNORE INTO t VALUES (1, 0, 'b')")
	if got := peekRows(store, "t", "id", "s"); got != "1:a" {
		t.Errorf("rows %s", got)
	}
	for _, q := range []string{"INSERT IGNORE INTO t VALUES (2, 300, 'a')", "INSERT IGNORE INTO t VALUES (3, 0, 'abc')"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	if _, err := db.Exec("INSERT INTO t VALUES (4, 300, 'a')"); !errors.Is(err, ErrNumericValueOutOfRange) {
		t.Errorf("a plain INSERT: %v, want out of range", err)
	}
}

// col <=> ? with a NULL argument searches the NULL keys, as <=> NULL does,
// and locks them and the gap around them, not the whole table.
func TestMySQLNullSafeEqualBoundNull(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, k INT, KEY (k))")
	mustExec(t, db, "INSERT INTO t VALUES (1, NULL), (2, 5), (3, 9)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SELECT id FROM t WHERE k <=> ? FOR UPDATE", nil); err != nil {
		t.Fatal(err)
	}
	if held := store.locks[lockKey{store.resolve("t"), "3"}]; len(held) > 0 {
		t.Error("row 3, past the NULL keys and the next record, is locked")
	}
}

// LIKE with a literal prefix on an indexed text column searches the range
// of strings that start with it, so the search locks those and its gaps,
// not the whole table; <>, != and NOT IN on an indexed column are refused.
func TestMySQLLikePrefixSearch(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, code VARCHAR(10), KEY (code))")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'aa'), (2, 'ab'), (3, 'ba'), (4, 'ca')")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SELECT id FROM t WHERE code LIKE 'a%' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	table := store.resolve("t")
	locked := func(k string) bool { return len(store.locks[lockKey{table, k}]) > 0 }
	if !locked("1") || !locked("2") || !locked("3") || locked("4") {
		t.Error("LIKE 'a%' should lock 'aa', 'ab' and the next record 'ba' only")
	}
	for _, q := range []string{"SELECT id FROM t WHERE code <> 'aa' FOR UPDATE", "SELECT id FROM t WHERE code NOT IN ('aa') FOR UPDATE"} {
		if _, err := tx.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	if p, wild := likePrefix(`a\%b%`); p != "a%b" || !wild {
		t.Errorf("likePrefix: %q, %v", p, wild)
	}
}

// A MODIFY that converts a row's primary key value is refused, as the row
// would be filed under its old identity; one that leaves the keys as they
// are runs.
func TestMySQLModifyOfAPrimaryKey(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE a (id VARCHAR(5) PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO a VALUES ('01')")
	if _, err := db.Exec("ALTER TABLE a MODIFY id INT"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("'01' to INT: %v, want unsupported", err)
	}
	mustExec(t, db, "CREATE TABLE b (id INT PRIMARY KEY, n INT)")
	mustExec(t, db, "INSERT INTO b VALUES (1, 2)")
	mustExec(t, db, "ALTER TABLE b MODIFY id BIGINT")
	mustExec(t, db, "ALTER TABLE b MODIFY n VARCHAR(5)")
}

// An AUTO_INCREMENT column that leads no index is refused, as MySQL refuses
// it; one leading the primary key or another index loads.
func TestMySQLAutoIncrementNeedsAnIndex(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	if _, err := db.Exec("CREATE TABLE a (id INT AUTO_INCREMENT, v INT)"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("no index: %v, want unsupported", err)
	}
	mustExec(t, db, "CREATE TABLE b (id INT AUTO_INCREMENT, v INT, KEY (id))")
	mustExec(t, db, "CREATE TABLE c (v INT, id INT AUTO_INCREMENT, PRIMARY KEY (id, v))")
}

// CAST(... AS DOUBLE) and the dividend of / convert a string to the number
// it starts with, as MySQL does.
func TestMySQLDoubleOfAString(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	var lt bool
	var q float64
	if err := db.QueryRow("SELECT CAST('2' AS DOUBLE) < '10', '10' / 4").Scan(&lt, &q); err != nil {
		t.Fatal(err)
	}
	if !lt || q != 2.5 {
		t.Errorf("got %v, %v", lt, q)
	}
}

// An ALTER that leaves NULL in a NOT NULL column of rows already there is
// refused, as MySQL refuses it or fills in a value of its own.
func TestMySQLAlterNotNullOverRows(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, NULL)")
	for _, q := range []string{"ALTER TABLE t ADD COLUMN w INT NOT NULL", "ALTER TABLE t MODIFY v INT NOT NULL"} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	mustExec(t, db, "ALTER TABLE t ADD COLUMN x INT NOT NULL DEFAULT 0")
	mustExec(t, db, "CREATE TABLE e (id INT PRIMARY KEY)")
	mustExec(t, db, "ALTER TABLE e ADD COLUMN w INT NOT NULL")
}

// A text column of a case-insensitive collation, MySQL 8's default, loads
// with the schema, and a statement whose outcome depends on how it compares
// is refused; reading and writing its values as they are, and columns of a
// _bin collation or binary strings, run.
func TestMySQLCaseInsensitiveCollation(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysql.New())
	mustExec(t, db, "CREATE TABLE u (id INT PRIMARY KEY, email VARCHAR(50), note VARCHAR(50), code VARCHAR(10) COLLATE utf8mb4_bin, raw VARBINARY(10), UNIQUE KEY (email))")
	mustExec(t, db, "CREATE TABLE b (id INT PRIMARY KEY, name VARCHAR(10), UNIQUE KEY (name)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin")
	mustExec(t, db, "CREATE TABLE k (name VARCHAR(10) PRIMARY KEY)")
	mustExec(t, db, "CREATE TABLE n (id INT PRIMARY KEY, note VARCHAR(10))")
	for _, q := range []string{
		"INSERT INTO u (id, email) VALUES (1, 'a@x')",  // the unique index compares it
		"SELECT id FROM u WHERE email = 'A@X'",         // a comparison
		"SELECT id FROM u WHERE note LIKE 'a%'",        // LIKE
		"SELECT id FROM u WHERE note IN ('a', 'b')",    // IN
		"SELECT id FROM u ORDER BY note",               // ORDER BY
		"SELECT note, COUNT(*) FROM u GROUP BY note",   // GROUP BY
		"SELECT DISTINCT note FROM u",                  // DISTINCT
		"SELECT MAX(note) FROM u",                      // MAX
		"UPDATE u SET email = 'b@x' WHERE id = 1",      // the unique index compares it
		"DELETE FROM k LIMIT 1",                        // a scan along a key that holds it
		"SELECT n.id FROM n JOIN u ON u.note = n.note", // a join on it
		"SELECT id FROM n WHERE note > 'm'",            // a range
		"SELECT note FROM n UNION SELECT note FROM u",  // UNION's DISTINCT
	} {
		if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: %v, want unsupported", q, err)
		}
	}
	for _, q := range []string{
		"INSERT INTO n VALUES (1, 'Abc')",
		"UPDATE n SET note = 'x' WHERE id = 1",
		"SELECT note FROM n WHERE id = 1",
		"SELECT id FROM u WHERE code = 'a' AND raw = 'b'",
		"INSERT INTO b VALUES (1, 'a'), (2, 'A')",
		"SELECT id FROM b WHERE name = 'a' ORDER BY name",
		"SELECT note FROM n UNION ALL SELECT note FROM u",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	// CONVERT TO gives the columns the table's new collation.
	mustExec(t, db, "ALTER TABLE b CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	if _, err := db.Exec("SELECT id FROM b WHERE name = 'a'"); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("after CONVERT TO a _ci collation: %v, want unsupported", err)
	}
	// A server whose default collation is _bin gives it to the columns that
	// declare none.
	s2 := newSim(t)
	db2, _ := s2.DB("app", mysql.New(mysql.Collation("utf8mb4_bin")))
	mustExec(t, db2, "CREATE TABLE u (id INT PRIMARY KEY, email VARCHAR(50), UNIQUE KEY (email))")
	mustExec(t, db2, "INSERT INTO u VALUES (1, 'a@x')")
	mustExec(t, db2, "SELECT id FROM u WHERE email = 'a@x'")
}
