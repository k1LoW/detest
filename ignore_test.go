package detest

import (
	"testing"

	"github.com/k1LoW/detest/postgres"
)

func TestIgnoredTable(t *testing.T) {
	s := newSim(t)
	db, store := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE orders (id text PRIMARY KEY, audit_id bigint REFERENCES audit_logs (id))`)
	store.Ignore("audit_logs")

	// No CREATE TABLE is needed; the insert succeeds and returns what it
	// would have written.
	var id int64
	if err := db.QueryRow(`INSERT INTO audit_logs (id, what) VALUES (7, 'created') RETURNING id`).Scan(&id); err != nil || id != 7 {
		t.Fatalf("insert: %d, %v", id, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM audit_logs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the ignored table holds %d rows, %v", n, err)
	}
	for _, q := range []string{`UPDATE audit_logs SET what = 'x'`, `DELETE FROM audit_logs`} {
		res, err := db.Exec(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if k, _ := res.RowsAffected(); k != 0 {
			t.Fatalf("%s affected %d rows", q, k)
		}
	}
	// A foreign key into the ignored table goes unchecked.
	mustExec(t, db, `INSERT INTO orders VALUES ('o1', 7)`)
}

// The writes to an ignored table are no scheduling points, so the schedules
// that differ only in where they fall are not explored.
func TestIgnoredTableNarrowsTheExploration(t *testing.T) {
	runs := func(ignore bool) int {
		res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
			db, store := s.DB("app", postgres.New())
			mustExec(t, db, `
CREATE TABLE orders (id text PRIMARY KEY);
CREATE TABLE audit_logs (id text PRIMARY KEY);
`)
			if ignore {
				store.Ignore("audit_logs")
			}
			for _, name := range []string{"a", "b"} {
				s.Manual(name, 1, func(p *Proc) error {
					if _, err := db.ExecContext(p.Context(), `INSERT INTO audit_logs VALUES ($1)`, name); err != nil {
						return err
					}
					_, err := db.ExecContext(p.Context(), `INSERT INTO orders VALUES ($1)`, name)
					return err
				})
			}
		}, nil, nil, 0)
		return res.Runs
	}
	with, without := runs(true), runs(false)
	if with >= without {
		t.Fatalf("explored %d runs with the table ignored, %d without", with, without)
	}
}
