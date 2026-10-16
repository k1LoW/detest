package detest

import (
	"cmp"
	"fmt"
	"maps"
	"reflect"
	"slices"

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

// queryHasEffects reports whether running sel changes state: a locking
// clause or one of effectFuncs anywhere in it.
func queryHasEffects(sel *sqlir.SelectStmt) bool {
	for _, s := range sqlir.Selects(sel) {
		if s.Lock != nil {
			return true
		}
	}
	for _, f := range sqlir.FuncCallsIn(sel) {
		if effectFuncs[f.Name] {
			return true
		}
	}
	return false
}

// checkSequenceCalls refuses effectFuncs where detest evaluates them
// another number of times than Postgres: in the WHERE of an UPDATE or a
// DELETE, which detest also evaluates to validate the statement before it
// yields, and in a subquery, which Postgres evaluates once when it is
// uncorrelated and detest for every row. Elsewhere both evaluate them as
// often.
func (x *sqlExec) checkSequenceCalls(stmt sqlir.Statement) error {
	switch stmt.(type) {
	case *sqlir.SelectStmt, *sqlir.InsertStmt, *sqlir.UpdateStmt, *sqlir.DeleteStmt, *sqlir.CreateTableAsStmt:
		// Creating a view runs no query, so its views are checked when a
		// statement that runs it reads them.
		if err := x.viewSequenceChecks(stmt, map[string]bool{}); err != nil {
			return err
		}
		if err := x.partlyRead(stmt, true); err != nil {
			return err
		}
	}
	switch st := stmt.(type) {
	case *sqlir.CreateTableAsStmt:
		return x.topSequenceCalls(st.Select)
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
		body := *st
		body.With = nil
		if err := x.cteSequenceCalls(st.With, &body); err != nil {
			return err
		}
		if err := x.rowSequenceCalls(assignedValues(st.Set)); err != nil {
			return err
		}
		rest := []any{st.Set, st.Where, st.Returning}
		if def := x.tx.db.defs[x.tx.db.resolve(st.Table)]; def != nil {
			// SET col = DEFAULT computes the column's default per row.
			for _, a := range st.Set {
				if _, ok := a.Value.(*sqlir.Default); ok {
					rest = append(rest, def.defaults[a.Column])
				}
			}
		}
		if err := x.writeItems(st.With, st.From, st.Table, st.Alias, st.Where, rest); err != nil {
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
		body := *st
		body.With = nil
		if err := x.cteSequenceCalls(st.With, &body); err != nil {
			return err
		}
		if err := x.writeItems(st.With, st.Using, st.Table, st.Alias, st.Where, []any{st.Where, st.Returning}); err != nil {
			return err
		}
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

// partlyRead refuses a FROM item that locks rows or has effects, a CTE,
// a derived table or a view, where Postgres may run only part of it, or
// none, or where its rows interleave with effects of the query reading
// it. Postgres reads such an item's rows on demand, so a LIMIT, an EXISTS,
// a scalar subquery, a join or a query around the reader that stops early
// leaves the rest of its rows unread, and their locks and calls never
// happen. A WHERE or HAVING condition that reads no column of the block's
// own rows is a filter Postgres tests once, before it reads any row, so
// when it is false the reader asks for none. And Postgres computes a row
// of the reader as soon as it pulls the row from the item, so a sequence
// call of the reader's select list comes between two of the item's.
// detest runs every row of an item before the query reading it computes
// any of its own.
//
// An item is read in full when the statement reads all of its query's
// rows, as those of a SELECT, an INSERT ... SELECT or a CREATE TABLE AS
// when consumed is set, and each read of the item is the only FROM item of
// a block that reads all of its rows, which is the query itself or a later
// CTE or an item read in full, with no LIMIT, OFFSET or condition of no
// column of its own. These blocks have no query around them, so a column of the
// block is one of its own rows. A CTE of one row read by a subquery is
// refused too, as whether Postgres reaches the subquery at all depends on
// more than the subquery. The select list of an EXISTS is never computed,
// and a join or a LIMIT around it may stop first. The CTEs of a write are
// checked by writeWith.
func (x *sqlExec) partlyRead(stmt sqlir.Statement, consumed bool) error {
	readsAll := func(b *sqlir.SelectStmt) bool {
		if b.Limit != nil || b.Offset != nil {
			return false
		}
		for _, e := range append(splitAnd(b.Where), splitAnd(b.Having)...) {
			if e != nil && len(sqlir.BlockColumnRefs(e)) == 0 {
				return false
			}
		}
		return true
	}
	// readers are the blocks that read the i-th CTE of q by its name.
	readers := func(q *sqlir.SelectStmt, i int) []*sqlir.SelectStmt {
		body := *q
		body.With = nil
		in := []any{&body}
		for _, later := range q.With[i+1:] {
			in = append(in, later.Select)
		}
		var out []*sqlir.SelectStmt
		name := q.With[i].Name
		for _, b := range sqlir.Selects(in) {
			reads := b.From != nil && b.From.Name == name
			for _, j := range b.Joins {
				reads = reads || j.Table.Name == name
			}
			if !reads {
				continue
			}
			if b == &body {
				b = q
			}
			out = append(out, b)
		}
		return out
	}
	ctes := map[string]*sqlir.SelectStmt{}
	for _, q := range sqlir.Selects(stmt) {
		for _, cte := range q.With {
			ctes[cte.Name] = cte.Select
		}
	}
	// own reports a block with effects of its own, beside its FROM items.
	own := func(b *sqlir.SelectStmt) bool {
		rest := *b
		// The block's own locking clause takes no lock between the
		// item's rows, as planLocking refuses it over a derived table or
		// a view and Postgres over a CTE.
		rest.From, rest.Joins, rest.With, rest.Lock = nil, nil, nil, nil
		return x.effectfulQuery(&rest, ctes, map[string]bool{})
	}
	// view is the query of the view t names, nil for another item.
	view := func(t *sqlir.TableRef) *sqlir.SelectStmt {
		if t == nil || t.Name == "" || t.Sub != nil {
			return nil
		}
		if v := x.tx.db.views[x.tx.db.resolve(t.Name)]; v != nil {
			return v.View
		}
		return nil
	}
	// sole reports a read that is the only FROM item of a block reading
	// all of its rows.
	sole := func(b *sqlir.SelectStmt) bool { return len(b.Joins) == 0 && readsAll(b) }
	full := map[*sqlir.SelectStmt]bool{} // blocks whose rows are all read
	whole := map[any]bool{}              // items whose rows are all read
	paced := map[any]bool{}              // items read by a block with effects
	var visit func(q *sqlir.SelectStmt, effects bool)
	visit = func(q *sqlir.SelectStmt, effects bool) {
		if q == nil || !readsAll(q) {
			return
		}
		full[q] = true
		if q.SetOp != "" {
			// Its queries are read one after the other, so the effects of
			// one do not fall between the rows of another.
			visit(q.Larg, effects)
			visit(q.Rarg, effects)
		} else {
			effects = effects || own(q)
		}
		if f := q.From; q.SetOp == "" && f != nil && len(q.Joins) == 0 && !f.Lateral {
			// A view's query is shared by every reference to it, so it is
			// the reference that is read in full.
			var key any
			item := f.Sub
			if item != nil {
				key = item
			} else if ctes[f.Name] == nil {
				item, key = view(f), f
			}
			if item != nil {
				whole[key] = true
				paced[key] = paced[key] || effects
				visit(item, effects)
			}
		}
		read := reachableCTEs(q)
		for i, cte := range slices.Backward(q.With) {
			if !read[cte.Name] {
				continue
			}
			rs := readers(q, i)
			if !slices.ContainsFunc(rs, func(b *sqlir.SelectStmt) bool { return !full[b] || !sole(b) }) {
				whole[cte.Select] = true
				paced[cte.Select] = paced[cte.Select] || effects || slices.ContainsFunc(rs, own)
				visit(cte.Select, effects)
			}
		}
	}
	switch st := stmt.(type) {
	case *sqlir.SelectStmt:
		if consumed {
			visit(st, false)
		}
	case *sqlir.InsertStmt:
		visit(st.Select, false)
	case *sqlir.CreateTableAsStmt:
		visit(st.Select, false)
	case *sqlir.UpdateStmt:
		for _, cte := range st.With {
			visit(cte.Select, false)
		}
	case *sqlir.DeleteStmt:
		for _, cte := range st.With {
			visit(cte.Select, false)
		}
	}
	check := func(what string, def *sqlir.SelectStmt, key any) error {
		if (whole[key] && !paced[key]) || !x.effectfulQuery(def, ctes, map[string]bool{}) {
			return nil
		}
		if whole[key] {
			return x.unsupported(fmt.Sprintf("%s, which locks rows or has effects, read by a query with effects of its own, which Postgres computes between its rows", what))
		}
		return x.unsupported(fmt.Sprintf("%s, which locks rows or has effects, read where Postgres may run only part of it", what))
	}
	// A name some WITH declares reads its CTE where the WITH is in scope.
	// One the statement also reads where none is may read the view.
	free := map[string]bool{}
	for _, name := range freeNames(stmt) {
		free[name] = true
	}
	// The query of a CTE no one reads never runs.
	unread := map[*sqlir.SelectStmt]bool{}
	for _, q := range sqlir.Selects(stmt) {
		read := reachableCTEs(q)
		for _, cte := range q.With {
			if !read[cte.Name] {
				for _, in := range sqlir.Selects(cte.Select) {
					unread[in] = true
				}
			}
		}
	}
	for _, q := range sqlir.Selects(stmt) {
		if unread[q] {
			continue
		}
		read := reachableCTEs(q)
		for _, cte := range q.With {
			if !read[cte.Name] {
				continue
			}
			if err := check(fmt.Sprintf("CTE %q", cte.Name), cte.Select, cte.Select); err != nil {
				return err
			}
		}
		items := []*sqlir.TableRef{q.From}
		for i := range q.Joins {
			items = append(items, &q.Joins[i].Table)
		}
		for _, t := range items {
			// A LATERAL subquery is never read in full, as it runs again
			// for each row and stops where the query reading it does.
			if t == nil {
				continue
			}
			if t.Sub != nil {
				if err := check("a derived table", t.Sub, t.Sub); err != nil {
					return err
				}
			} else if v := view(t); v != nil && (ctes[t.Name] == nil || free[t.Name]) {
				if err := check(fmt.Sprintf("view %q", t.Name), v, t); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// writeItems refuses a derived table or a view of a write's FROM or USING
// that locks rows or has effects, unless it has the shape partlyRunItem
// lets through, as Postgres reads it on demand like a CTE there, which
// writeWith checks. A LATERAL one runs again for each row, so it is never
// read in full. And as Postgres computes SET and RETURNING for a row as
// soon as the join gives it, an item with effects, a CTE too, is refused
// beside effects of the write's own in rest.
func (x *sqlExec) writeItems(with []sqlir.CTE, items []sqlir.TableRef, target, alias string, where sqlir.Expr, rest any) error {
	ctes := map[string]*sqlir.SelectStmt{}
	for _, c := range with {
		ctes[c.Name] = c.Select
	}
	isCTE := func(name string) bool { return ctes[name] != nil }
	own := slices.ContainsFunc(sqlir.FuncCallsIn(rest), func(f *sqlir.FuncCall) bool { return effectFuncs[f.Name] }) ||
		slices.ContainsFunc(sqlir.Selects(rest), func(q *sqlir.SelectStmt) bool { return x.effectfulQuery(q, ctes, map[string]bool{}) })
	for _, t := range items {
		if t.Func != nil {
			continue
		}
		sel, cols, what := t.Sub, t.Columns, "a derived table"
		if sel == nil {
			if def := ctes[t.Name]; def != nil {
				if own && x.effectfulQuery(def, ctes, map[string]bool{}) {
					return x.unsupported(fmt.Sprintf("CTE %q, which locks rows or has effects, read by a write with effects of its own", t.Name))
				}
				continue
			}
			v := x.tx.db.views[x.tx.db.resolve(t.Name)]
			if v == nil || v.View == nil {
				continue
			}
			sel, cols, what = v.View, v.ViewColumns, fmt.Sprintf("view %q", t.Name)
		}
		if !x.effectfulQuery(sel, ctes, map[string]bool{}) {
			continue
		}
		switch {
		case t.Lateral:
			return x.unsupported(fmt.Sprintf("%s, which locks rows or has effects, LATERAL in a write", what))
		case own:
			return x.unsupported(fmt.Sprintf("%s, which locks rows or has effects, read by a write with effects of its own", what))
		}
		names := outputNames(sel)
		if len(cols) > 0 {
			names = renamedCols(names, cols)
			if len(names) < len(cols) {
				names = cols
			}
		}
		itemAlias := cmp.Or(t.Alias, t.Name)
		if why := x.partlyRunItem(sel, isCTE, itemAlias, namesSet(names), items, target, targetAlias(target, alias), where); why != "" {
			return x.unsupported(fmt.Sprintf("%s, which locks rows or has effects, %s", what, why))
		}
	}
	return nil
}

// cteSequenceCalls checks the CTEs of a write that body reads, which run
// once in both.
func (x *sqlExec) cteSequenceCalls(with []sqlir.CTE, body any) error {
	used := reachableWith(with, body)
	for _, cte := range with {
		if !used[cte.Name] {
			continue
		}
		if err := x.topSequenceCalls(cte.Select); err != nil {
			return err
		}
	}
	return nil
}

// topSequenceCalls checks a query, whose clauses detest evaluates as
// often as Postgres does, but not its subqueries.
func (x *sqlExec) topSequenceCalls(sel *sqlir.SelectStmt) error {
	used := reachableCTEs(sel)
	for _, cte := range sel.With {
		if !used[cte.Name] {
			continue // an unread CTE runs in neither
		}
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
		if sel.Limit != nil || sel.Offset != nil {
			// Postgres pulls the queries' rows on demand and stops at
			// LIMIT, where detest runs them in full.
			ctes := map[string]*sqlir.SelectStmt{}
			for _, c := range sel.With {
				ctes[c.Name] = c.Select
			}
			if x.effectfulQuery(sel.Larg, ctes, map[string]bool{}) || x.effectfulQuery(sel.Rarg, ctes, map[string]bool{}) {
				return x.unsupported("a set operation under LIMIT or OFFSET over a query that locks rows or has effects, which Postgres runs only as far as LIMIT reads")
			}
		}
	}
	if len(sel.DistinctOn) > 0 {
		// Postgres computes a select list item no DISTINCT ON key reads
		// for the first row of each key only, where detest computes it
		// for every row before it keeps the first.
		for _, t := range sel.Targets {
			if slices.ContainsFunc(sel.DistinctOn, func(e sqlir.Expr) bool { return reflect.DeepEqual(e, t.Expr) }) {
				continue
			}
			for _, f := range sqlir.FuncCalls(t.Expr) {
				if effectFuncs[f.Name] {
					return x.unsupported(f.Name + " in the select list of a query with DISTINCT ON, outside its keys")
				}
			}
		}
	}
	if sel.Values != nil && (sel.Limit != nil || sel.Offset != nil) {
		// Postgres reads the rows of VALUES as far as LIMIT asks, where
		// detest computes every row first.
		if err := x.noSequenceCalls(sel.Values); err != nil {
			return err
		}
		for _, q := range sqlir.Selects(sel.Values) {
			if x.effectfulQuery(q, nil, map[string]bool{}) {
				return x.unsupported("a subquery that locks rows in VALUES under LIMIT or OFFSET")
			}
		}
	}
	if len(windowsOf(sel)) > 0 && (len(sel.OrderBy) > 0 || sel.Limit != nil || sel.Offset != nil) {
		// Postgres computes the select list after the window functions,
		// row by row as ORDER BY and LIMIT pull them, where detest
		// computes it for every row before.
		ctes := map[string]*sqlir.SelectStmt{}
		for _, c := range sel.With {
			ctes[c.Name] = c.Select
		}
		for _, t := range sel.Targets {
			for _, f := range sqlir.FuncCalls(t.Expr) {
				if effectFuncs[f.Name] {
					return x.unsupported(f.Name + " in the select list of a query with window functions and ORDER BY, LIMIT or OFFSET")
				}
			}
			for _, q := range sqlir.Selects(t.Expr) {
				if x.effectfulQuery(q, ctes, map[string]bool{}) {
					return x.unsupported("a subquery that locks rows in the select list of a query with window functions and ORDER BY, LIMIT or OFFSET")
				}
			}
		}
	}
	if sel.Lock != nil {
		// Postgres computes the select list below the row locks, for a
		// row SKIP LOCKED skips too and again for a row it re-checks after
		// a lock wait, where detest computes it for the rows it locked.
		ctes := map[string]*sqlir.SelectStmt{}
		for _, c := range sel.With {
			ctes[c.Name] = c.Select
		}
		for _, t := range sel.Targets {
			for _, f := range sqlir.FuncCalls(t.Expr) {
				if effectFuncs[f.Name] {
					return x.unsupported(f.Name + " in the select list of a locking read")
				}
			}
			for _, q := range sqlir.Selects(t.Expr) {
				if x.effectfulQuery(q, ctes, map[string]bool{}) {
					return x.unsupported("a subquery that locks rows in the select list of a locking read")
				}
			}
		}
		// Its WHERE runs for the rows it pulls, which LIMIT stops, where
		// detest tests every row before it locks any.
		if sel.Limit != nil || sel.Offset != nil {
			for _, f := range sqlir.FuncCalls(sel.Where) {
				if effectFuncs[f.Name] {
					return x.unsupported(f.Name + " in the WHERE of a locking read with LIMIT or OFFSET")
				}
			}
		}
	}
	if len(sel.OrderBy) > 0 && (sel.Limit != nil || sel.Offset != nil) {
		// Whether Postgres computes a subquery of the select list for a
		// row past LIMIT depends on its plan. Its cost may postpone the
		// subquery past the sort, and an index may stand in for the sort.
		// Without LIMIT it computes the subquery for every row before the
		// sort, and without ORDER BY for the rows it pulls, as pull does.
		ctes := map[string]*sqlir.SelectStmt{}
		for _, c := range sel.With {
			ctes[c.Name] = c.Select
		}
		for _, t := range sel.Targets {
			for _, q := range sqlir.Selects(t.Expr) {
				if x.effectfulQuery(q, ctes, map[string]bool{}) {
					return x.unsupported("a subquery that locks rows in the select list of a query with ORDER BY and LIMIT or OFFSET")
				}
			}
		}
	}
	if len(sel.GroupBy) > 0 {
		// A GROUP BY name or position evaluates its select list item to
		// group the rows and again to give the output.
		if err := x.noSequenceCalls(sel.Targets); err != nil {
			return err
		}
	}
	if err := x.aggregateEffects(sel); err != nil {
		return err
	}
	for _, n := range []any{sel.Values, sel.Targets, sel.From, sel.Joins, sel.Where, sel.GroupBy, sel.Having, sel.OrderBy, sel.Limit, sel.Offset, sel.DistinctOn} {
		if err := x.rowSequenceCalls(n); err != nil {
			return err
		}
	}
	return nil
}

// reachableCTEs are the CTEs of sel the query reads, directly or through
// another CTE it reads, the ones Postgres runs. A CTE's body sees only the
// CTEs declared before it, so a name it shares with a later one reads
// whatever that name means outside.
func reachableCTEs(sel *sqlir.SelectStmt) map[string]bool {
	body := *sel
	body.With = nil
	return reachableWith(sel.With, &body)
}

// reachableWith is reachableCTEs for the CTEs of any statement, body being
// the statement without them.
func reachableWith(with []sqlir.CTE, body any) map[string]bool {
	index := map[string]int{}
	for i, cte := range with {
		index[cte.Name] = i
	}
	reached := map[string]bool{}
	var visit func(names []string, before int)
	visit = func(names []string, before int) {
		for _, name := range names {
			i, ok := index[name]
			if !ok || i >= before || reached[name] {
				continue
			}
			reached[name] = true
			visit(freeNames(with[i].Select), i)
		}
	}
	visit(freeNames(body), len(with))
	return reached
}

// freeNames are the names the FROM items under n read that no WITH inside
// n declares where they are read: a WITH's CTE sees the ones declared
// before it, and its query all of them, as in Postgres.
func freeNames(n any) []string {
	var out []string
	var walk func(v reflect.Value, hidden map[string]bool)
	walk = func(v reflect.Value, hidden map[string]bool) {
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if v.IsNil() {
				return
			}
			if s, ok := reflect.TypeAssert[*sqlir.SelectStmt](v); ok && len(s.With) > 0 {
				inner := maps.Clone(hidden)
				for _, cte := range s.With {
					walk(reflect.ValueOf(cte.Select), inner)
					inner = maps.Clone(inner)
					inner[cte.Name] = true
				}
				body := *s
				body.With = nil
				walk(reflect.ValueOf(body), inner)
				return
			}
			walk(v.Elem(), hidden)
		case reflect.Struct:
			if t, ok := reflect.TypeAssert[sqlir.TableRef](v); ok && t.Name != "" && !hidden[t.Name] {
				out = append(out, t.Name)
			}
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i), hidden)
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i), hidden)
			}
		}
	}
	walk(reflect.ValueOf(n), map[string]bool{})
	return out
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
				if err = x.noSequenceCalls(sub); err != nil {
					return
				}
				for _, q := range sqlir.Selects(sub) {
					if err = x.aggregateEffects(q); err != nil {
						return
					}
				}
				return
			}
			if f, ok := reflect.TypeAssert[*sqlir.FuncCall](v); ok && isAggregateCall(f) {
				// Postgres computes each aggregate once, before any
				// expression of the row, where detest computes it at
				// each reference in the order it evaluates them, so an
				// effect in one, or in a subquery or the view it reads,
				// happens another number of times or in another order.
				if len(sqlir.Selects(f.Args)) > 0 {
					err = x.unsupported("a subquery in an aggregate's argument")
					return
				}
				err = x.noSequenceCalls(f.Args)
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

// aggregateEffects refuses what runs beside the aggregates of sel when it
// aggregates.
func (x *sqlExec) aggregateEffects(sel *sqlir.SelectStmt) error {
	if !isAggregate(sel) {
		return nil
	}
	// Postgres computes the aggregates before any other expression
	// of the select list, HAVING, ORDER BY or DISTINCT ON, so one that fails does
	// so before a sequence call or a subquery's lock beside it, where
	// detest evaluates them in their written order.
	if err := x.noSequenceCalls(sel.Targets, sel.Having, sel.OrderBy, sel.DistinctOn); err != nil {
		return err
	}
	ctes := map[string]*sqlir.SelectStmt{}
	for _, c := range sel.With {
		ctes[c.Name] = c.Select
	}
	for _, n := range []any{sel.Targets, sel.Having, sel.OrderBy, sel.DistinctOn} {
		for _, q := range sqlir.Selects(n) {
			if x.effectfulQuery(q, ctes, map[string]bool{}) {
				return x.unsupported("a subquery with an effect, or reading a view or a CTE with one, beside an aggregate")
			}
		}
	}
	return nil
}

// viewSequenceChecks checks the query of every view n reads, through other
// views too, as a derived table of the statement, since Postgres expands
// a view into the statement before it plans it. seen guards against a view
// checked twice.
func (x *sqlExec) viewSequenceChecks(n any, seen map[string]bool) error {
	var refs []*sqlir.TableRef
	// The FROM of an UPDATE and the USING of a DELETE name tables outside
	// any query.
	switch st := n.(type) {
	case *sqlir.UpdateStmt:
		for i := range st.From {
			refs = append(refs, &st.From[i])
		}
	case *sqlir.DeleteStmt:
		for i := range st.Using {
			refs = append(refs, &st.Using[i])
		}
	}
	for _, q := range sqlir.Selects(n) {
		refs = append(refs, q.From)
		for i := range q.Joins {
			refs = append(refs, &q.Joins[i].Table)
		}
	}
	for _, t := range refs {
		if t == nil || t.Name == "" {
			continue
		}
		name := x.tx.db.resolve(t.Name)
		v := x.tx.db.views[name]
		if v == nil || v.View == nil || seen[name] {
			continue
		}
		seen[name] = true
		if err := x.topSequenceCalls(v.View); err != nil {
			return err
		}
		// Whether a statement reads all of a view's rows is not tracked,
		// so its query counts as one read in part.
		if err := x.partlyRead(v.View, false); err != nil {
			return err
		}
		if err := x.viewSequenceChecks(v.View, seen); err != nil {
			return err
		}
	}
	return nil
}

// viewSequenceCalls reports a node reading a view, directly or through
// another, whose query calls one of effectFuncs. seen guards against a view
// read twice.
func (x *sqlExec) viewSequenceCalls(sel any, seen map[string]bool) bool {
	for _, q := range sqlir.Selects(sel) {
		refs := []*sqlir.TableRef{q.From}
		for i := range q.Joins {
			refs = append(refs, &q.Joins[i].Table)
		}
		for _, t := range refs {
			if t == nil || t.Name == "" {
				continue
			}
			name := x.tx.db.resolve(t.Name)
			v := x.tx.db.views[name]
			if v == nil || v.View == nil || seen[name] {
				continue
			}
			seen[name] = true
			if slices.ContainsFunc(sqlir.FuncCallsIn(v.View), func(f *sqlir.FuncCall) bool { return effectFuncs[f.Name] }) || x.viewSequenceCalls(v.View, seen) {
				return true
			}
		}
	}
	return false
}

// effectfulQuery reports a query with a locking clause or one of
// effectFuncs, in it or in a view or one of ctes it reads. seen guards
// against a view or a CTE read twice.
func (x *sqlExec) effectfulQuery(sel *sqlir.SelectStmt, ctes map[string]*sqlir.SelectStmt, seen map[string]bool) bool {
	if queryHasEffects(sel) {
		return true
	}
	for _, q := range sqlir.Selects(sel) {
		refs := []*sqlir.TableRef{q.From}
		for i := range q.Joins {
			refs = append(refs, &q.Joins[i].Table)
		}
		for _, t := range refs {
			if t == nil || t.Name == "" || seen[t.Name] {
				continue
			}
			seen[t.Name] = true
			// A CTE of the name and a view of it are both followed, as
			// which one the name reads depends on scopes not tracked here.
			if c := ctes[t.Name]; c != nil && x.effectfulQuery(c, ctes, seen) {
				return true
			}
			if v := x.tx.db.views[x.tx.db.resolve(t.Name)]; v != nil && v.View != nil && x.effectfulQuery(v.View, ctes, seen) {
				return true
			}
		}
	}
	return false
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
		if x.viewSequenceCalls(n, map[string]bool{}) {
			return x.unsupported("a view with a sequence call where detest evaluates it another number of times than Postgres, such as a subquery or the WHERE of an UPDATE or a DELETE")
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
