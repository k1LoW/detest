package mysql_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

// The cancel cases compare a transaction whose context ends while it is
// idle, which database/sql rolls back from a goroutine of its own: the locks
// it held go, and its later statements find it done.
var cancelCases = []difftest.Case{
	{
		Name:   "a transaction whose context ends while idle rolls back",
		Schema: []string{`CREATE TABLE r (id int PRIMARY KEY, n int NOT NULL)` + binary},
		Seed:   []string{`INSERT INTO r VALUES (1, 0), (2, 0)`},
		Conns:  2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE r SET n = 10 WHERE id = 1`),
			difftest.S(0, `UPDATE r SET n = 10 WHERE id = 2`),
			difftest.S(1, `UPDATE r SET n = 21 WHERE id = 1`),
			difftest.CT(0),
			difftest.S(0, `UPDATE r SET n = 11 WHERE id = 2`),
			difftest.S(1, `UPDATE r SET n = 22 WHERE id = 2`),
			difftest.Q(1, `SELECT id, n FROM r ORDER BY id`),
		},
	},
}

func TestDiffCancel(t *testing.T) {
	for _, c := range cancelCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
