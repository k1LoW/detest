package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

var foldCases = []difftest.Case{
	{
		Name:   "an expression of constants that fails, folded when the statement is planned",
		Schema: []string{`CREATE TABLE t (id int PRIMARY KEY, n int)`},
		Seed:   []string{`INSERT INTO t VALUES (1, 0)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT CASE WHEN n = 0 THEN 1 ELSE 1 / 0 END FROM t`),
			difftest.Q(0, `SELECT n + 1 / 0 FROM t WHERE false`),
			difftest.Q(0, `SELECT coalesce(n, 1 / 0) FROM t`),
			difftest.Q(0, `SELECT coalesce(n, -1, 1 / 0) FROM t`),
			difftest.Q(0, `SELECT CASE WHEN false THEN 1 / 0 ELSE n END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN true THEN n ELSE 1 / 0 END FROM t`),
			difftest.Q(0, `SELECT 1 FROM t WHERE false AND 1 / 0 = 1`),
			difftest.Q(0, `SELECT 1 FROM t WHERE 1 / 0 = 1 AND false`),
			difftest.Q(0, `SELECT sum(1 / 0) FROM t WHERE false`),
			difftest.Q(0, `SELECT CASE 1 WHEN 2 THEN 1 / 0 ELSE 0 END`),
			difftest.Q(0, `SELECT coalesce(n, extract(year FROM now()), 1 / 0) FROM t`),
			difftest.Q(0, `SELECT n + (SELECT 1 / 0) FROM t WHERE false`),
			difftest.Q(0, `SELECT abs(-1) / 0 FROM t WHERE false`),
			difftest.Q(0, `SELECT 1 / (CASE WHEN n > 0 THEN 0 END) FROM t WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1 % 0 END FROM t`),
			difftest.S(0, `UPDATE t SET n = 1 / 0 WHERE false`),
			difftest.Q(0, `SELECT 1 FROM t WHERE n = 1 OR 1 / 0 = 1`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 'a' || 1 / 0 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN 1 = 1 THEN 0 ELSE 1 / 0 END`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN nullif(1, 1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 'x'::int + 1 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1 / 0 END FROM t WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1.0 / 0 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1 / 0 > 1 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1 / 0 IS NULL END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN 1 IN (1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN -(1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN (1 / 0)::text END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN greatest(1, 1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN lower('A' || (1 / 0)::text) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN date_trunc('day', now()) + interval '1 day' * (1 / 0) END FROM t`),
			difftest.Q(0, `SELECT n FROM t ORDER BY 1 / 0 LIMIT 0`),
			difftest.Q(0, `SELECT n FROM t LIMIT 1 / 0`),
			difftest.Q(0, `WITH c AS (SELECT 1 / 0 AS v) SELECT n FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN coalesce(1, 1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n > 0 THEN coalesce(NULL, 1 / 0) END FROM t`),
			difftest.Q(0, `SELECT CASE n WHEN 1 THEN 1 / 0 END FROM t`),
		},
	},
	{
		Name: "what Postgres drops before it folds, the CTEs no statement reads and the select list of an EXISTS",
		Schema: []string{
			`CREATE TABLE t (id int PRIMARY KEY, n int)`,
			`CREATE VIEW fv AS SELECT CASE WHEN n > 0 THEN 1 / 0 END AS v FROM t`,
		},
		Seed:  []string{`INSERT INTO t VALUES (1, 0)`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.S(0, `WITH c AS (SELECT 1 / 0) UPDATE t SET n = 1 WHERE false`),
			difftest.S(0, `WITH c AS (SELECT 1 / 0) DELETE FROM t WHERE false`),
			difftest.Q(0, `WITH c AS (SELECT 1 / 0) INSERT INTO t SELECT 2, 2 WHERE false`),
			difftest.Q(0, `WITH fv AS (SELECT 1 AS v) SELECT v FROM fv`),
			difftest.Q(0, `WITH fv AS (SELECT 1 AS v) SELECT (SELECT v FROM fv)`),
			difftest.Q(0, `SELECT EXISTS (SELECT 1 / 0 FROM t WHERE false)`),
			difftest.Q(0, `SELECT 1 FROM t WHERE EXISTS (SELECT 1 / 0 FROM t WHERE false)`),
			difftest.Q(0, `SELECT EXISTS (SELECT 1 / 0 FROM t WHERE false LIMIT 1)`),
			difftest.Q(0, `SELECT EXISTS (SELECT 1 / 0 FROM t WHERE false LIMIT 0)`),
			difftest.Q(0, `SELECT EXISTS (SELECT 1 / 0 FROM t WHERE false OFFSET 1)`),
			difftest.Q(0, `SELECT EXISTS (SELECT sum(1 / 0) FROM t WHERE false)`),
			difftest.Q(0, `SELECT EXISTS (SELECT 1 / 0 FROM t WHERE false HAVING true)`),
			difftest.Q(0, `SELECT EXISTS (SELECT 1 / 0 FROM t WHERE false UNION SELECT 1)`),
			difftest.Q(0, `SELECT EXISTS (SELECT 1 FROM t WHERE false ORDER BY 1 / 0)`),
			difftest.Q(0, `SELECT EXISTS (SELECT n FROM t WHERE false GROUP BY n, 1 / 0)`),
			difftest.Q(0, `SELECT EXISTS (SELECT 1 / 0 WHERE false ORDER BY row_number() OVER ())`),
			difftest.Q(0, `SELECT EXISTS (SELECT (SELECT row_number() OVER () + 1 / 0) WHERE false)`),
			difftest.Q(0, `SELECT EXISTS (SELECT DISTINCT ON (sum(n)) 1 / 0 FROM t WHERE false)`),
		},
	},
	{
		Name: "an operand that is a constant NULL, which folds a strict operator or function to NULL",
		Schema: []string{
			`CREATE TABLE t (id int PRIMARY KEY, n int)`,
			`CREATE TABLE tn (n numeric)`,
		},
		Seed:  []string{`INSERT INTO t VALUES (1, 0)`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT CASE WHEN n = NULL THEN 1 / 0 ELSE 0 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN NULL IN (n) THEN 1 / 0 ELSE 0 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN NULL IN (n, 2) THEN 1 / 0 ELSE 0 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN n IN (NULL, 2) THEN 1 / 0 ELSE 0 END FROM t WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN abs(NULL::int + n) > 0 THEN 1 / 0 ELSE 0 END FROM t`),
			difftest.Q(0, `SELECT CASE WHEN greatest(NULL, n) > 0 THEN 1 / 0 ELSE 0 END FROM t WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN coalesce(1, n) / 2 = 0 THEN 1 / 0 ELSE 1 END FROM tn WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN (CASE WHEN true THEN 1 ELSE n END) / 2 = 0 THEN 1 / 0 ELSE 1 END FROM tn WHERE false`),
			difftest.Q(0, `SELECT CASE n WHEN NULL THEN 1 / 0 ELSE 0 END FROM t WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN nextval(NULL) IS NULL THEN 0 ELSE 1 / 0 END FROM t WHERE false`),
		},
	},
	{
		Name:   "a write that gives the column whose default fails folding",
		Schema: []string{`CREATE TABLE d (id int, n int DEFAULT (1 / 0))`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `INSERT INTO d (id, n) SELECT 1, 2 WHERE false`),
			difftest.Q(0, `INSERT INTO d SELECT 1, 2 WHERE false`),
			difftest.Q(0, `WITH src AS (SELECT 1 AS id, 2 AS n) INSERT INTO d SELECT * FROM src WHERE false UNION ALL SELECT 3, 4 WHERE false`),
			difftest.S(0, `UPDATE d SET n = 1 WHERE false`),
		},
	},
	{
		Name: "a function with effects in a branch folding drops, called no more often than Postgres calls it",
		Schema: []string{
			`CREATE SEQUENCE s`,
		},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT coalesce(nextval('s') > 0 OR true, false) WHERE false`),
			difftest.Q(0, `SELECT CASE WHEN nextval('s') > 0 AND false THEN 1 END WHERE false`),
			difftest.Q(0, `SELECT (CASE WHEN true THEN 1 ELSE nextval('s') END) / 2 = 0.5 WHERE false`),
			difftest.Q(0, `SELECT nextval('s')`),
		},
	},
}

func TestDiffFold(t *testing.T) {
	for _, c := range foldCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
