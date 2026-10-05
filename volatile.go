package detest

import (
	"reflect"

	"github.com/k1LoW/detest/internal/sqlir"
)

// sequenceFuncs are the functions whose every evaluation changes what the
// next one returns.
var sequenceFuncs = map[string]bool{"nextval": true, "setval": true}

// checkSequenceCalls refuses nextval and setval where detest evaluates them
// another number of times than Postgres: in the WHERE of an UPDATE or a
// DELETE, which detest also evaluates to validate the statement before it
// yields, and in a subquery, which Postgres evaluates once when it is
// uncorrelated and detest for every row. Elsewhere both evaluate them as
// often.
func (x *sqlExec) checkSequenceCalls(stmt sqlir.Statement) error {
	switch st := stmt.(type) {
	case *sqlir.SelectStmt:
		return x.topSequenceCalls(st)
	case *sqlir.InsertStmt:
		for _, row := range st.Rows {
			if err := x.rowSequenceCalls(row); err != nil {
				return err
			}
		}
		if st.Select != nil {
			if err := x.topSequenceCalls(st.Select); err != nil {
				return err
			}
		}
		if oc := st.OnConflict; oc != nil {
			if err := x.rowSequenceCalls(assignedValues(oc.Set)); err != nil {
				return err
			}
			if err := x.noSequenceCalls(oc.Where, oc.Elems, oc.InferWhere); err != nil {
				return err
			}
		}
		return x.rowSequenceCalls(st.Returning)
	case *sqlir.UpdateStmt:
		if err := x.rowSequenceCalls(assignedValues(st.Set)); err != nil {
			return err
		}
		if err := x.rowSequenceCalls([]any{st.From, st.OrderBy, st.Limit}); err != nil {
			return err
		}
		if err := x.noSequenceCalls(st.Where); err != nil {
			return err
		}
		return x.rowSequenceCalls(st.Returning)
	case *sqlir.DeleteStmt:
		if err := x.rowSequenceCalls([]any{st.Using, st.OrderBy, st.Limit}); err != nil {
			return err
		}
		if err := x.noSequenceCalls(st.Where); err != nil {
			return err
		}
		return x.rowSequenceCalls(st.Returning)
	}
	return nil
}

// topSequenceCalls checks a query, whose clauses detest evaluates as
// often as Postgres does, but not its subqueries.
func (x *sqlExec) topSequenceCalls(sel *sqlir.SelectStmt) error {
	for _, cte := range sel.With {
		if err := x.topSequenceCalls(cte.Select); err != nil {
			return err
		}
	}
	if sel.SetOp != "" {
		if err := x.topSequenceCalls(sel.Larg); err != nil {
			return err
		}
		if err := x.topSequenceCalls(sel.Rarg); err != nil {
			return err
		}
	}
	for _, n := range []any{sel.Values, sel.Targets, sel.From, sel.Joins, sel.Where, sel.GroupBy, sel.Having, sel.OrderBy, sel.Limit, sel.Offset, sel.DistinctOn} {
		if err := x.rowSequenceCalls(n); err != nil {
			return err
		}
	}
	return nil
}

// rowSequenceCalls checks expressions detest evaluates as often as Postgres,
// which may call the functions, but not in a subquery other than a derived
// table that is not LATERAL.
func (x *sqlExec) rowSequenceCalls(n any) error {
	var err error
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		if err != nil {
			return
		}
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			if sub, ok := reflect.TypeAssert[*sqlir.SelectStmt](v); ok {
				err = x.noSequenceCalls(sub)
				return
			}
			walk(v.Elem())
		case reflect.Struct:
			if t, ok := reflect.TypeAssert[sqlir.TableRef](v); ok && t.Sub != nil && !t.Lateral {
				// A derived table runs once for the statement, here as there.
				err = x.topSequenceCalls(t.Sub)
				return
			}
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(n))
	return err
}

// noSequenceCalls refuses a call of the functions anywhere under ns.
func (x *sqlExec) noSequenceCalls(ns ...any) error {
	for _, n := range ns {
		var found string
		var walk func(v reflect.Value)
		walk = func(v reflect.Value) {
			if found != "" {
				return
			}
			switch v.Kind() {
			case reflect.Interface, reflect.Pointer:
				if v.IsNil() {
					return
				}
				if f, ok := reflect.TypeAssert[*sqlir.FuncCall](v); ok && sequenceFuncs[f.Name] {
					found = f.Name
					return
				}
				walk(v.Elem())
			case reflect.Struct:
				for i := range v.NumField() {
					if v.Type().Field(i).IsExported() {
						walk(v.Field(i))
					}
				}
			case reflect.Slice, reflect.Array:
				for i := range v.Len() {
					walk(v.Index(i))
				}
			}
		}
		walk(reflect.ValueOf(n))
		if found != "" {
			return x.unsupported(found + " in a subquery or in the WHERE of an UPDATE or a DELETE, which detest evaluates another number of times than Postgres")
		}
	}
	return nil
}
