package detest

import (
	"reflect"

	"github.com/k1LoW/detest/internal/sqlir"
)

// foldConstants raises the errors Postgres raises when it plans stmt and
// folds its expressions of constants (eval_const_expressions), such as the
// 22012 of 1 / 0, before any row is read. The evaluators reach an
// expression only when a row does, so an untaken CASE branch, a COALESCE
// argument after the first value or a statement over no rows would
// otherwise pass. Only errors are kept: the folded values are what the
// evaluators compute anyway.
func (x *sqlExec) foldConstants(stmt sqlir.Statement) error {
	if x.tx.db.kind.InnoDB() {
		return nil // MySQL gives NULL for a division by zero in a query
	}
	switch stmt.(type) {
	case *sqlir.SelectStmt, *sqlir.InsertStmt, *sqlir.UpdateStmt, *sqlir.DeleteStmt, *sqlir.CreateTableAsStmt:
	default:
		return nil
	}
	f := &folder{x: x, views: map[string]bool{}}
	return f.node(reflect.ValueOf(stmt))
}

type folder struct {
	x     *sqlExec
	views map[string]bool // the views folded already
}

// node folds the expressions under v, a statement or a part of one.
func (f *folder) node(v reflect.Value) error {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return f.node(v.Elem())
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		if e, ok := reflect.TypeAssert[sqlir.Expr](v); ok {
			_, err := f.expr(e)
			return err
		}
		if sel, ok := reflect.TypeAssert[*sqlir.SelectStmt](v); ok {
			return f.query(sel)
		}
		return f.node(v.Elem())
	case reflect.Struct:
		if t, ok := reflect.TypeAssert[sqlir.TableRef](v); ok && t.Sub == nil && t.Func == nil && t.Name != "" {
			// Postgres expands a view into the statement before it plans
			// it, so the view's expressions are folded too.
			name := f.x.tx.db.resolve(t.Name)
			if vw := f.x.tx.db.views[name]; vw != nil && vw.View != nil && !f.views[name] {
				f.views[name] = true
				if err := f.query(vw.View); err != nil {
					return err
				}
			}
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				if err := f.node(v.Field(i)); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if err := f.node(v.Index(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// query folds a query, leaving out the CTEs it does not read, which
// Postgres drops before it plans the query.
func (f *folder) query(sel *sqlir.SelectStmt) error {
	body := *sel
	body.With = nil
	read := reachableCTEs(sel)
	for _, c := range sel.With {
		if read[c.Name] {
			if err := f.query(c.Select); err != nil {
				return err
			}
		}
	}
	return f.node(reflect.ValueOf(&body).Elem())
}

// expr folds e and reports whether it is a constant Postgres folds, so
// that the expression around it may fold too. A branch Postgres drops
// because of a constant before it, in AND, OR, CASE or COALESCE, is not
// folded, as it is not there.
func (f *folder) expr(e sqlir.Expr) (bool, error) {
	switch v := e.(type) {
	case nil, *sqlir.Const:
		return true, nil
	case *sqlir.ColumnRef, *sqlir.Param, *sqlir.Default:
		return false, nil
	case *sqlir.SubQuery:
		return false, f.query(v.Select)
	case *sqlir.Exists:
		return false, f.query(v.Select)
	case *sqlir.BinaryExpr:
		if v.Op == "AND" || v.Op == "OR" {
			decides := v.Op == "OR"
			all := true
			for _, a := range []sqlir.Expr{v.L, v.R} {
				c, err := f.expr(a)
				if err != nil {
					return false, err
				}
				if !c {
					all = false
					continue
				}
				val, ok, err := f.value(a)
				if err != nil {
					return false, err
				}
				if !ok {
					all = false
					continue
				}
				if b, isBool := derefValue(val).(bool); isBool && b == decides {
					// Postgres reduces the whole AND to false, or the OR
					// to true, and folds no argument after it.
					return true, nil
				}
			}
			if !all {
				return false, nil
			}
			return f.folds(v)
		}
		return f.all(v, v.L, v.R)
	case *sqlir.UnaryExpr:
		return f.all(v, v.X)
	case *sqlir.IsNull:
		return f.all(v, v.X)
	case *sqlir.Collate:
		return f.all(v, v.X)
	case *sqlir.Cast:
		c, err := f.expr(v.X)
		if err != nil || !c {
			return false, err
		}
		// The input functions of the time types read the session's
		// settings, so Postgres does not fold a cast to one, apart from a
		// string literal's, which it reads as the type's input when it
		// parses the statement.
		if k, ok := v.X.(*sqlir.Const); ok {
			if _, isText := k.Value.(string); isText {
				return f.folds(v)
			}
		}
		switch v.Type {
		case "date", "time", "timetz", "timestamp", "timestamptz", "interval":
			return false, nil
		}
		return f.folds(v)
	case *sqlir.InExpr:
		if v.Sub != nil {
			if _, err := f.expr(v.X); err != nil {
				return false, err
			}
			return false, f.query(v.Sub)
		}
		return f.all(v, append([]sqlir.Expr{v.X}, v.List...)...)
	case *sqlir.CaseExpr:
		return f.caseExpr(v)
	case *sqlir.FuncCall:
		return f.call(v)
	case *sqlir.WindowFunc:
		for _, a := range v.Func.Args {
			if _, err := f.expr(a); err != nil {
				return false, err
			}
		}
		if err := f.node(reflect.ValueOf(v.Partition)); err != nil {
			return false, err
		}
		return false, f.node(reflect.ValueOf(v.Order))
	}
	// Any other expression folds what it holds, and is not a constant.
	return false, f.node(reflect.ValueOf(e).Elem())
}

// all folds operands and, when every one is a constant, folds e.
func (f *folder) all(e sqlir.Expr, operands ...sqlir.Expr) (bool, error) {
	all := true
	for _, o := range operands {
		c, err := f.expr(o)
		if err != nil {
			return false, err
		}
		all = all && c
	}
	if !all {
		return false, nil
	}
	return f.folds(e)
}

// immutableFuncs are the functions detest computes that Postgres folds
// over constant arguments. now() and the time functions over a
// timestamptz read the session and are left alone.
var immutableFuncs = map[string]bool{
	"abs": true, "floor": true, "ceil": true, "ceiling": true, "round": true, "power": true, "pow": true,
	"lower": true, "upper": true, "length": true, "char_length": true, "octet_length": true, "left": true,
	"hashtext": true, "greatest": true, "least": true, "nullif": true,
}

func (f *folder) call(v *sqlir.FuncCall) (bool, error) {
	if v.Name == "coalesce" {
		// Postgres drops a NULL argument and stops at the first constant
		// that is not NULL, folding none after it.
		all := true
		for _, a := range v.Args {
			c, err := f.expr(a)
			if err != nil {
				return false, err
			}
			if !c {
				all = false
				continue
			}
			val, ok, err := f.value(a)
			if err != nil {
				return false, err
			}
			if !ok {
				all = false
				continue
			}
			if derefValue(val) != nil {
				break
			}
		}
		if !all {
			return false, nil
		}
		return f.folds(v)
	}
	all := true
	for _, a := range v.Args {
		c, err := f.expr(a)
		if err != nil {
			return false, err
		}
		all = all && c
	}
	if !all || !immutableFuncs[v.Name] {
		return false, nil
	}
	return f.folds(v)
}

// caseExpr folds a CASE as Postgres does: a branch whose condition is a
// constant false or NULL is dropped unfolded, and one whose condition is a
// constant true ends the CASE, dropping the branches after it and ELSE.
func (f *folder) caseExpr(v *sqlir.CaseExpr) (bool, error) {
	argConst, err := f.expr(v.Arg)
	if err != nil {
		return false, err
	}
	all := argConst
	for _, w := range v.Whens {
		c, err := f.expr(w.When)
		if err != nil {
			return false, err
		}
		cond := w.When
		if v.Arg != nil {
			cond = &sqlir.BinaryExpr{Op: "=", L: v.Arg, R: w.When}
			c = c && argConst
		}
		if c {
			val, ok, err := f.value(cond)
			if err != nil {
				return false, err
			}
			if ok {
				b, _ := derefValue(val).(bool)
				if !b {
					continue // dropped, false or NULL
				}
				then, err := f.expr(w.Then)
				if err != nil {
					return false, err
				}
				return all && then && f.must(v), nil
			}
		}
		all = false
		if _, err := f.expr(w.Then); err != nil {
			return false, err
		}
	}
	elseConst, err := f.expr(v.Else)
	if err != nil {
		return false, err
	}
	if !all || !elseConst {
		return false, nil
	}
	return f.folds(v)
}

// must is folds for a caller that has the error from it already.
func (f *folder) must(e sqlir.Expr) bool {
	c, _ := f.folds(e)
	return c
}

// folds evaluates e, a constant expression, for the error Postgres raises
// folding it. One detest does not evaluate is refused, as whether Postgres
// fails folding it is not known.
func (f *folder) folds(e sqlir.Expr) (bool, error) {
	_, ok, err := f.value(e)
	return ok, err
}

func (f *folder) value(e sqlir.Expr) (any, bool, error) {
	// evalRaw, as a quotient that repeats is a range here that the
	// expression around it may still compare exactly.
	v, err := f.x.evalRaw(e, nil)
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}
