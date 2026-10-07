package detest

import (
	"strings"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// ticketModel declares two processes that each take a ticket from a counter
// kept outside the simulated resources. The first ticket ever taken is
// inserted and every later one only read, so without reset the counter
// carries over from run to run and a replayed prefix issues other
// statements than the run that recorded it.
func ticketModel(reset bool) func(t *testing.T, s *Sim) {
	return func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE tickets (no bigint PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		next := int64(0)
		if reset {
			s.Seed(func() { next = 0 })
		}
		for _, name := range []string{"a", "b"} {
			s.Manual(name, 1, func(p *Proc) error {
				next++
				if next == 1 {
					_, err := db.Exec(`INSERT INTO tickets (no) VALUES ($1)`, next)
					return err
				}
				rows, err := db.Query(`SELECT no FROM tickets`)
				if err != nil {
					return err
				}
				return rows.Close()
			})
		}
	}
}

func TestReplayDetectsNondeterminism(t *testing.T) {
	res, _ := exploreBubble(t, ticketModel(false), nil, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "the operations before choice") {
		t.Fatalf("got %v, want a nondeterminism error", res.Fatal)
	}
}

func TestReplayAcceptsDeterministicRuns(t *testing.T) {
	res, _ := exploreBubble(t, ticketModel(true), nil, nil, 0)
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	if res.Runs < 2 {
		t.Fatalf("explored %d runs, want several", res.Runs)
	}
}

// Values that change from run to run, such as generated ids and wall-clock
// times, are no nondeterminism: the operations stay the same.
func TestReplayIgnoresChangingValues(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		if _, err := db.Exec(`CREATE TABLE tickets (no bigint PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		next := int64(0) // never reset, like an id generator
		for _, name := range []string{"a", "b"} {
			s.Manual(name, 1, func(p *Proc) error {
				next++
				_, err := db.Exec(`INSERT INTO tickets (no) VALUES ($1)`, next)
				return err
			})
		}
	}, nil, nil, 0)
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
}
