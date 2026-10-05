package postgres_test

import (
	"testing"

	"github.com/k1LoW/detest/internal/difftest"
)

// The CTE write cases compare WITH ... UPDATE and WITH ... DELETE with
// Postgres: the CTE that claims a job with FOR UPDATE SKIP LOCKED, the waits
// its locks take, the snapshot it reads, and a CTE the statement never reads,
// which Postgres does not run.
var (
	jobSchema = []string{`CREATE TABLE jobs (id int PRIMARY KEY, status text NOT NULL, worker int)`}
	jobSeed   = []string{`INSERT INTO jobs VALUES (1, 'queued', NULL), (2, 'queued', NULL), (3, 'done', NULL)`}
	claimJob  = func(worker string) string {
		return `WITH c AS (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
			UPDATE jobs SET status = 'running', worker = ` + worker + ` FROM c WHERE jobs.id = c.id RETURNING jobs.id`
	}
)

var cteWriteCases = []difftest.Case{
	{
		Name:   "two workers claim jobs with a CTE and skip locked",
		Schema: jobSchema, Seed: jobSeed, Conns: 3,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(1, `BEGIN`),
			difftest.Q(0, claimJob("10")),
			difftest.Q(1, claimJob("11")),
			difftest.Q(2, claimJob("12")),
			difftest.S(0, `COMMIT`),
			difftest.S(1, `COMMIT`),
			difftest.Q(2, `SELECT id, status, worker FROM jobs ORDER BY id`),
		},
	},
	{
		Name:   "a CTE that locks waits and takes the next row after the recheck",
		Schema: jobSchema, Seed: jobSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `WITH c AS (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE)
				UPDATE jobs SET status = 'running' FROM c WHERE jobs.id = c.id RETURNING jobs.id`),
			difftest.Q(1, `WITH c AS (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE)
				UPDATE jobs SET status = 'running' FROM c WHERE jobs.id = c.id RETURNING jobs.id`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id, status FROM jobs ORDER BY id`),
		},
	},
	{
		// Without a lock the CTE reads job 1 for both, and the second UPDATE
		// waits for the first and updates the row again after the recheck.
		Name:   "a CTE without a lock claims the same job twice",
		Schema: jobSchema, Seed: jobSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `WITH c AS (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1)
				UPDATE jobs SET status = 'running', worker = 10 FROM c WHERE jobs.id = c.id RETURNING jobs.id`),
			difftest.Q(1, `WITH c AS (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1)
				UPDATE jobs SET status = 'running', worker = 11 FROM c WHERE jobs.id = c.id RETURNING jobs.id`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id, status, worker FROM jobs ORDER BY id`),
		},
	},
	{
		Name:   "the CTE of an UPDATE in WHERE, SET and RETURNING",
		Schema: jobSchema, Seed: jobSeed, Conns: 1,
		Steps: []difftest.Step{
			difftest.Q(0, `WITH q AS (SELECT id FROM jobs WHERE status = 'queued')
				UPDATE jobs SET worker = (SELECT count(*) FROM q) WHERE id IN (SELECT id FROM q) RETURNING id, worker`),
			difftest.Q(0, `WITH d AS (SELECT 3 AS id), w AS (SELECT id + 1 AS id FROM d)
				UPDATE jobs SET status = 'archived' WHERE id = (SELECT id FROM d) RETURNING id, (SELECT id FROM w)`),
			difftest.S(0, `WITH n AS (SELECT 9 AS id) UPDATE jobs SET worker = 0 FROM n WHERE jobs.id = n.id`),
			difftest.Q(0, `WITH c AS (SELECT 1 AS id, 70 AS v) UPDATE jobs SET worker = c.v FROM c WHERE jobs.id = c.id RETURNING jobs.id, c.v, v`),
			difftest.Q(0, `UPDATE jobs SET worker = j.id FROM jobs j WHERE jobs.id = 2 AND j.id = 3 RETURNING jobs.id, j.id, j.status = 'archived'`),
			difftest.Q(0, `WITH c AS (SELECT 3 AS id, 'gone' AS why) DELETE FROM jobs USING c WHERE jobs.id = c.id RETURNING jobs.id, why = 'gone'`),
			difftest.Q(0, `SELECT id, status, worker FROM jobs ORDER BY id`),
		},
	},
	{
		// The CTE waits for job 1 while job 4 is inserted and job 2 is
		// finished. The UPDATE chooses its rows from the snapshot it began
		// with, so job 4 is not among them, and job 2 fails the recheck of
		// its latest version.
		Name:   "an UPDATE chooses its rows from its snapshot after its CTE waited",
		Schema: jobSchema, Seed: jobSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE`),
			difftest.Q(1, `WITH c AS (SELECT id FROM jobs WHERE id = 1 FOR UPDATE)
				UPDATE jobs SET worker = 7 FROM c WHERE jobs.status = 'queued' RETURNING jobs.id`),
			difftest.S(0, `INSERT INTO jobs VALUES (4, 'queued', NULL)`),
			difftest.S(0, `UPDATE jobs SET status = 'done' WHERE id = 2`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id, status, worker FROM jobs ORDER BY id`),
		},
	},
	{
		Name:   "the CTE of an UPDATE reads the statement's snapshot",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.Q(0, `WITH t AS (SELECT sum(n) AS s FROM stock) UPDATE stock SET n = t.s FROM t RETURNING sku, n`),
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 5 WHERE sku = 'apple'`),
			difftest.S(1, `WITH a AS (SELECT n FROM stock WHERE sku = 'apple') UPDATE stock SET n = a.n + 1 FROM a WHERE sku = 'pear'`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "an UPDATE over a CTE waits for the target row and rechecks it",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `UPDATE stock SET n = 0 WHERE sku = 'apple'`),
			difftest.S(1, `WITH c AS (SELECT 'apple' AS sku UNION ALL SELECT 'pear')
				UPDATE stock SET n = n - 1 FROM c WHERE stock.sku = c.sku AND n > 0`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT sku, n FROM stock ORDER BY sku`),
		},
	},
	{
		Name:   "a CTE the write does not read takes no locks",
		Schema: stockSchema, Seed: stockSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.S(0, `WITH all_rows AS (SELECT sku FROM stock FOR UPDATE) UPDATE stock SET n = n + 1 WHERE sku = 'apple'`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'pear' FOR UPDATE NOWAIT`),
			difftest.S(0, `WITH all_rows AS (SELECT sku FROM stock FOR UPDATE) DELETE FROM stock WHERE sku = 'none'`),
			difftest.Q(1, `SELECT sku FROM stock WHERE sku = 'pear' FOR UPDATE NOWAIT`),
			difftest.S(0, `COMMIT`),
		},
	},
	{
		Name:   "WITH ... DELETE with USING, WHERE and RETURNING",
		Schema: jobSchema, Seed: jobSeed, Conns: 2,
		Steps: []difftest.Step{
			difftest.S(0, `BEGIN`),
			difftest.Q(0, `WITH done AS (SELECT id FROM jobs WHERE status = 'done' FOR UPDATE)
				DELETE FROM jobs USING done WHERE jobs.id = done.id RETURNING jobs.id`),
			difftest.Q(1, `WITH q AS (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1)
				DELETE FROM jobs WHERE id IN (SELECT id FROM q) RETURNING id`),
			difftest.Q(1, `WITH q AS (SELECT id FROM jobs ORDER BY id FOR UPDATE SKIP LOCKED)
				DELETE FROM jobs USING q WHERE jobs.id = q.id RETURNING jobs.id`),
			difftest.S(0, `COMMIT`),
			difftest.Q(1, `SELECT id FROM jobs ORDER BY id`),
		},
	},
}

func TestDiffCTEWrites(t *testing.T) {
	for _, c := range cteWriteCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			difftest.Run(t, backend{}, c)
		})
	}
}
