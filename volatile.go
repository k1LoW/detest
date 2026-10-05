package detest

import (
	"reflect"

	"github.com/k1LoW/detest/internal/sqlir"
)

// effectFuncs are the functions whose every call returns another value or
// changes state, a sequence's, the generated values detest counts, or the
// advisory locks held, so that calling them another number of times than
// Postgres shows in the results.
var effectFuncs = map[string]bool{
	"nextval": true, "setval": true, "gen_random_uuid": true, "uuid_generate_v4": true, "random": true,
	"clock_timestamp": true, "pg_advisory_xact_lock": true, "pg_try_advisory_xact_lock": true,
}

// checkSequenceCalls refuses effectFuncs where detest evaluates them
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
	used, shadowed := reachableCTEs(sel), nestedCTEs(sel)
	for _, cte := range sel.With {
		check := x.topSequenceCalls
		// A nested WITH of the same name hides the CTE from the parts it
		// covers, which reachableCTEs does not tell apart, so such a CTE
		// counts as unread.
		if !used[cte.Name] || shadowed[cte.Name] {
			// detest runs every CTE, where Postgres skips one the query
			// does not read.
			check = func(s *sqlir.SelectStmt) error { return x.noSequenceCalls(s) }
		}
		if err := check(cte.Select); err != nil {
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
	if len(sel.GroupBy) > 0 {
		// A GROUP BY name or position evaluates its select list item to
		// group the rows and again to give the output.
		if err := x.noSequenceCalls(sel.Targets); err != nil {
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

// reachableCTEs are the CTEs of sel the query reads, directly or through
// another CTE it reads, the ones Postgres runs.
func reachableCTEs(sel *sqlir.SelectStmt) map[string]bool {
	body := *sel
	body.With = nil
	reached := map[string]bool{}
	queue := sqlir.TableNames(&body)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if reached[name] {
			continue
		}
		for _, cte := range sel.With {
			if cte.Name == name {
				reached[name] = true
				queue = append(queue, sqlir.TableNames(cte.Select)...)
			}
		}
	}
	return reached
}

// nestedCTEs are the names the WITH clauses inside sel's query and CTEs
// declare.
func nestedCTEs(sel *sqlir.SelectStmt) map[string]bool {
	names := map[string]bool{}
	var walk func(v reflect.Value, top bool)
	walk = func(v reflect.Value, top bool) {
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if v.IsNil() {
				return
			}
			if s, ok := reflect.TypeAssert[*sqlir.SelectStmt](v); ok && !top {
				for _, cte := range s.With {
					names[cte.Name] = true
				}
			}
			walk(v.Elem(), false)
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i), false)
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i), false)
			}
		}
	}
	walk(reflect.ValueOf(sel), true)
	return names
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
				if f, ok := reflect.TypeAssert[*sqlir.FuncCall](v); ok && effectFuncs[f.Name] {
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
			return x.unsupported(found + " where detest evaluates it another number of times than Postgres, such as a subquery or the WHERE of an UPDATE or a DELETE")
		}
	}
	return nil
}

// hasEffects reports whether evaluating e changes state, by such a function
// or by a subquery, which may lock rows.
func hasEffects(e sqlir.Expr) bool {
	for _, x := range sqlir.Exprs(e) {
		switch v := x.(type) {
		case *sqlir.FuncCall:
			if effectFuncs[v.Name] {
				return true
			}
		case *sqlir.SubQuery, *sqlir.Exists:
			return true
		case *sqlir.InExpr:
			if v.Sub != nil {
				return true
			}
		}
	}
	return false
}
