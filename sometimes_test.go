package detest

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/k1LoW/detest/postgres"
)

// stockModel declares two buyers of the last item in stock, with a reachable
// and an unreachable Sometimes condition.
func stockModel(t *testing.T, s *Sim) {
	db, store := s.DB("shop", postgres.New())
	if _, err := db.Exec(`CREATE TABLE stock (sku text PRIMARY KEY, n int NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	s.Seed(func() {
		_, _ = db.Exec(`INSERT INTO stock (sku, n) VALUES ('apple', 1)`)
	})
	for _, name := range []string{"alice", "bob"} {
		s.Manual(name, 1, func(p *Proc) error {
			_, err := db.Exec(`UPDATE stock SET n = n - 1 WHERE sku = 'apple' AND n > 0`)
			return err
		})
	}
	s.Sometimes("sold out", func(st *State) bool {
		row, _ := st.Row(store, "stock", "apple")
		return row.Int64("n") == 0
	})
	s.Sometimes("oversold", func(st *State) bool {
		row, _ := st.Row(store, "stock", "apple")
		return row.Int64("n") < 0
	})
}

func TestSometimesReportsUnreachedConditions(t *testing.T) {
	res, _ := exploreBubble(t, stockModel, nil, nil, 0)
	if !res.Complete || res.Violated {
		t.Fatalf("want a complete exploration without violation, got %s", res.report())
	}
	if want := []string{"oversold"}; !slices.Equal(res.Unreached, want) {
		t.Fatalf("unreached %v, want %v", res.Unreached, want)
	}
}

// A condition one worker meets counts for all of them.
func TestSometimesIsSharedByWorkers(t *testing.T) {
	res, _ := exploreWorkers(t, stockModel, nil, newFrontier(4, 200000), 4)
	if want := []string{"oversold"}; !slices.Equal(res.Unreached, want) {
		t.Fatalf("unreached %v, want %v", res.Unreached, want)
	}
}

// An exploration cut short by MaxRuns may not have got there yet: it logs
// the condition rather than failing.
func TestSometimesDoesNotFailAnIncompleteExploration(t *testing.T) {
	Explore(t, func(t *testing.T, s *Sim) {
		stockModel(t, s)
	}, MaxRuns(1))
}

// A complete exploration in which a condition never held fails the test. It
// runs in a child process because the test is meant to fail.
func TestSometimesFailsACompleteExploration(t *testing.T) {
	t.Parallel()
	if os.Getenv("DETEST_SOMETIMES_CHILD") == "1" {
		Explore(t, stockModel)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSometimesFailsACompleteExploration$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), "DETEST_SOMETIMES_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the child to fail:\n%s", out)
	}
	if !strings.Contains(string(out), `Sometimes "oversold" held in no run`) || strings.Contains(string(out), "sold out\"") {
		t.Fatalf("expected a failure naming only the unreached condition:\n%s", out)
	}
}
