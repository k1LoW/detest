package detest

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The trace lists the operations in the order they ran, not in the order the
// processes reached them: a process parked at an operation while another
// process runs has its line after that process's.
func TestTraceListsOperationsInTheOrderTheyRan(t *testing.T) {
	var ran []string
	model := func(t *testing.T, s *Sim) {
		s.Seed(func() { ran = nil })
		for _, name := range []string{"a", "b"} {
			s.Manual(name, 1, func(p *Proc) error {
				for _, op := range []string{name + "1", name + "2"} {
					p.Step("%s", op)
					ran = append(ran, op)
				}
				return nil
			})
		}
		s.AtQuiescence(func(st *State) error {
			if got := strings.Join(ran, ","); got == "a1,b1,a2,b2" {
				return fmt.Errorf("interleaved: %s", got)
			}
			return nil
		})
	}
	res, _ := exploreBubble(t, model, nil, nil, 0)
	if !res.Violated {
		t.Fatal("expected a violation")
	}
	var traced []string
	for line := range strings.Lines(res.Trace) {
		f := strings.Fields(line)
		if len(f) == 3 && slices.Contains([]string{"a1", "a2", "b1", "b2"}, f[2]) {
			traced = append(traced, f[2])
		}
	}
	if want := []string{"a1", "b1", "a2", "b2"}; !slices.Equal(traced, want) {
		t.Errorf("traced %v, want %v\n%s", traced, want, res.Trace)
	}
}
