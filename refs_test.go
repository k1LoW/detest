package detest

import (
	"testing"

	"github.com/k1LoW/detest/internal/sqlir"
)

// A node held in an interface is found once.
func TestColumnRefsFindsEachOnce(t *testing.T) {
	e := &sqlir.BinaryExpr{Op: "=", L: &sqlir.ColumnRef{Column: "a"}, R: &sqlir.FuncCall{Name: "lower", Args: []sqlir.Expr{&sqlir.ColumnRef{Column: "b"}}}}
	if got := len(sqlir.ColumnRefs(e)); got != 2 {
		t.Errorf("ColumnRefs found %d, want 2", got)
	}
	if got := len(sqlir.FuncCalls(e)); got != 1 {
		t.Errorf("FuncCalls found %d, want 1", got)
	}
}
