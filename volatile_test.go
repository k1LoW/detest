package detest

import (
	"testing"

	"github.com/k1LoW/detest/internal/sqlir"
)

// A function that returns another value each time is no bound of a locked
// range, as the WHERE tests another value.
func TestVolatileIsNoBound(t *testing.T) {
	for e, want := range map[sqlir.Expr]bool{
		&sqlir.FuncCall{Name: "gen_random_uuid"}:                                                     true,
		&sqlir.BinaryExpr{Op: "||", L: &sqlir.Const{Value: "a"}, R: &sqlir.FuncCall{Name: "random"}}: true,
		&sqlir.FuncCall{Name: "now"}:                                                                 false,
		&sqlir.Const{Value: 1}:                                                                       false,
	} {
		if got := volatile(e); got != want {
			t.Errorf("volatile(%#v) = %v, want %v", e, got, want)
		}
	}
}
