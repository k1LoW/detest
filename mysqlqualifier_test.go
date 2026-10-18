package detest

import (
	"errors"
	"testing"
)

func TestMySQLQualifierCase(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", mysqlBin())
	mustExec(t, db, `CREATE TABLE t (id INT PRIMARY KEY, n INT)`)
	mustExec(t, db, `CREATE TABLE Mixed (id INT PRIMARY KEY, n INT)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 2)`)
	mustExec(t, db, `INSERT INTO Mixed VALUES (1, 2)`)
	for _, q := range []string{
		`SELECT T.id FROM t`,
		`SELECT A.id FROM t AS a`,
		`SELECT a.id FROM t AS A`,
		`SELECT mixed.id FROM Mixed`,
		`SELECT A.id FROM t AS a WHERE FALSE`,
		`SELECT id FROM t AS a WHERE A.id = 1`,
		`SELECT id FROM t AS a ORDER BY A.id`,
		`SELECT id FROM t AS a GROUP BY A.id`,
		`SELECT a.id FROM t AS a JOIN Mixed AS b ON A.id = b.id`,
		`SELECT D.id FROM (SELECT id FROM t) AS d`,
		`WITH C AS (SELECT id FROM t) SELECT c.id FROM C`,
		`UPDATE t AS a SET n = A.n WHERE FALSE`,
		`UPDATE t AS A SET a.n = 3 WHERE FALSE`,
		`DELETE FROM t AS a WHERE A.id = 1`,
		`INSERT INTO Mixed VALUES (1, 3) ON DUPLICATE KEY UPDATE n = mixed.n`,
		`INSERT INTO t VALUES (1, 3) AS NewRow ON DUPLICATE KEY UPDATE n = newrow.n`,
	} {
		_, err := db.Exec(q)
		var dbErr *DBError
		if !errors.As(err, &dbErr) || dbErr.Number != 1054 {
			t.Errorf("%s: error = %v, want MySQL 1054", q, err)
		}
	}
	for _, q := range []string{
		`SELECT t.ID FROM t`,
		`SELECT A.ID FROM t AS A`,
		`SELECT Mixed.ID FROM Mixed`,
		`SELECT A.* FROM t AS A`,
		`SELECT Mixed.* FROM Mixed`,
		`SELECT D.id FROM (SELECT id FROM t) AS D`,
		`WITH C AS (SELECT id FROM t) SELECT C.id FROM C`,
		`SELECT n AS value FROM t ORDER BY VALUE`,
		`UPDATE t AS A SET A.n = A.n`,
		`SELECT a.id FROM t AS a WHERE EXISTS (SELECT 1 FROM (SELECT 3 AS n) AS A WHERE A.n = a.n)`,
		`INSERT INTO t VALUES (1, 5) AS NewRow ON DUPLICATE KEY UPDATE n = NewRow.n`,
	} {
		mustExec(t, db, q)
	}
}
