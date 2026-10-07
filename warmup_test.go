package detest

import (
	"os"
	"os/exec"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// The first statement a process parses compiles the PostgreSQL parser, which
// takes seconds under -race. It is no stall: a fresh process explores a
// Postgres simulation under a watchdog shorter than the compile.
func TestParserCompileIsNoStall(t *testing.T) {
	t.Parallel()
	if os.Getenv("DETEST_WARMUP_CHILD") == "1" {
		Explore(t, func(t *testing.T, s *Sim) {
			db, _ := s.DB("app", postgres.New())
			if _, err := db.Exec(`CREATE TABLE counters (id text PRIMARY KEY, n int NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			s.Seed(func() {
				if _, err := db.Exec(`INSERT INTO counters VALUES ('c', 0)`); err != nil {
					t.Fatal(err)
				}
			})
			s.Manual("inc", 1, func(p *Proc) error {
				_, err := db.Exec(`UPDATE counters SET n = n + 1 WHERE id = 'c'`)
				return err
			})
		})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestParserCompileIsNoStall$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), "DETEST_WARMUP_CHILD=1", "DETEST_STALL=200ms")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the child stalled or failed:\n%s", out)
	}
}
