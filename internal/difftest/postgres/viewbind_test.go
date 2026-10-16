package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

var viewBindCases = []difftest.Case{
	{
		Name: "a view's columns after its tables change",
		Schema: []string{
			`CREATE TABLE t (id int PRIMARY KEY, s text, n int)`,
			`CREATE TABLE u (id int PRIMARY KEY, m int)`,
			`CREATE VIEW tv AS SELECT * FROM t`,
			`CREATE VIEW ev AS SELECT s FROM t`,
			`CREATE VIEW jv AS SELECT m FROM t JOIN u ON t.id = u.id`,
			`CREATE VIEW xv AS SELECT * FROM t WHERE EXISTS (SELECT * FROM u)`,
			`CREATE VIEW dv AS SELECT * FROM (SELECT * FROM t) y`,
			`CREATE VIEW cv AS WITH c AS (SELECT * FROM t) SELECT * FROM c`,
			`CREATE VIEW ov AS SELECT n AS s, s AS k FROM t ORDER BY s`,
			`CREATE VIEW sv AS SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE m = n)`,
			`CREATE VIEW lv(a) AS SELECT * FROM t JOIN (SELECT 1 AS k) z ON true`,
			`CREATE VIEW rv AS SELECT * FROM tv`,
			`ALTER TABLE t ADD COLUMN x int`,
			`ALTER TABLE t ADD COLUMN m int`,
			`ALTER TABLE u ADD COLUMN n int`,
			`ALTER TABLE t RENAME COLUMN s TO s2`,
			`ALTER TABLE t RENAME TO t2`,
			`ALTER VIEW tv RENAME TO tv2`,
		},
		Seed:  []string{`INSERT INTO t2 (id, s2, n, m) VALUES (1, 'a', 2, 7), (2, 'b', 3, 8)`, `INSERT INTO u VALUES (1, 5, NULL), (2, 2, 9)`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT x FROM tv2`),
			difftest.Q(0, `SELECT * FROM tv2 ORDER BY id`),
			difftest.Q(0, `SELECT s FROM tv2 ORDER BY id`),
			difftest.Q(0, `SELECT s FROM ev ORDER BY 1`),
			difftest.Q(0, `SELECT m FROM jv ORDER BY 1`),
			difftest.Q(0, `SELECT * FROM xv ORDER BY id`),
			difftest.Q(0, `SELECT * FROM dv ORDER BY id`),
			difftest.Q(0, `SELECT * FROM cv ORDER BY id`),
			difftest.Q(0, `SELECT * FROM ov`),
			difftest.Q(0, `SELECT id FROM sv ORDER BY 1`),
			difftest.Q(0, `SELECT a, s, n, k FROM lv ORDER BY a`),
			difftest.Q(0, `SELECT * FROM rv ORDER BY id`),
		},
	},
	{
		Name: "a view's column list shorter than a * over a join that repeats a name",
		Schema: []string{
			`CREATE TABLE t (id int PRIMARY KEY, s text, n int)`,
			`CREATE TABLE u (id int PRIMARY KEY)`,
			`CREATE VIEW dv(a, b) AS SELECT * FROM t JOIN u ON true`,
		},
		Seed:  []string{`INSERT INTO t VALUES (1, 'a', 2)`, `INSERT INTO u VALUES (5)`},
		Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT a, b, n, id FROM dv`),
			difftest.Q(0, `SELECT * FROM dv`),
		},
	},
}

func TestDiffViewBind(t *testing.T) {
	for _, c := range viewBindCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
