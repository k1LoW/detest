package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

var outputNameCases = []difftest.Case{
	{
		Name:   "order by a name an output column takes from its expression",
		Schema: []string{`CREATE TABLE q (id int PRIMARY KEY, n int NOT NULL)`},
		Seed:   []string{`INSERT INTO q VALUES (9, 3), (10, 2), (100, 1)`},
		Conns:  1,
		Steps: []difftest.Step{
			difftest.Q(0, `SELECT id::text FROM q ORDER BY id`),
			difftest.Q(0, `SELECT id::text FROM q ORDER BY id LIMIT 1`),
			difftest.Q(0, `SELECT CASE WHEN id < 0 THEN 'x' ELSE id::text END FROM q ORDER BY id`),
			difftest.Q(0, `SELECT (id)::text COLLATE "C" FROM q ORDER BY id`),
			difftest.Q(0, `SELECT n::text AS id FROM q ORDER BY id`),
			difftest.Q(0, `SELECT id + 0 FROM q ORDER BY id`),
			difftest.Q(0, `SELECT CASE WHEN id < 0 THEN 'x' ELSE 'y' END, id FROM q ORDER BY id`),
			difftest.Q(0, `SELECT q.id::text FROM q ORDER BY q.id`),
			difftest.Q(0, `SELECT id::text, count(*) FROM q GROUP BY id ORDER BY id`),
		},
	},
}

func TestDiffOutputNames(t *testing.T) {
	for _, c := range outputNameCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
