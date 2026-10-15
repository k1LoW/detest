package detest

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// foldConstants raises the errors Postgres raises when it plans stmt and
// folds its expressions of constants (eval_const_expressions), such as the
// 22012 of 1 / 0, before any row is read. The evaluators reach an
// expression only when a row does, so an untaken CASE branch, a COALESCE
// argument after the first value or a statement over no rows would
// otherwise pass. Only errors are kept, as the folded values are what the
// evaluators compute anyway.
func (x *sqlExec) foldConstants(stmt sqlir.Statement) error {
	if x.tx.db.kind.InnoDB() {
		return nil // MySQL gives NULL for a division by zero in a query
	}
	f := &folder{x: x, views: map[string]bool{}}
	x.foldValues = map[sqlir.Expr]any{}
	defer func() { x.foldValues = nil }()
	switch st := stmt.(type) {
	case *sqlir.SelectStmt:
		return f.query(st)
	case *sqlir.InsertStmt:
		if err := f.node(reflect.ValueOf(st)); err != nil {
			return err
		}
		return f.insertDefaults(st)
	case *sqlir.CreateTableAsStmt:
		if st.NoData || st.IfNotExists && x.relationExists(st.Table) {
			return nil // Postgres does not plan the query
		}
		return f.query(st.Select)
	case *sqlir.UpdateStmt:
		body := *st
		body.With = nil
		if err := f.with(st.With, &body, func() error { return f.node(reflect.ValueOf(&body)) }); err != nil {
			return err
		}
		return f.setDefaults(st.Table, st.Set)
	case *sqlir.DeleteStmt:
		body := *st
		body.With = nil
		return f.with(st.With, &body, func() error { return f.node(reflect.ValueOf(&body)) })
	}
	return nil
}

// insertDefaults folds the defaults Postgres writes into the plan of an
// INSERT, those of the columns it leaves out and those DEFAULT names.
func (f *folder) insertDefaults(st *sqlir.InsertStmt) error {
	def := f.x.tx.db.defs[f.x.tx.db.resolve(st.Table)]
	if def == nil {
		return nil
	}
	cols := st.Columns
	if len(cols) == 0 {
		n := len(def.columns)
		switch {
		case len(st.Rows) > 0:
			n = len(st.Rows[0])
		case st.Select != nil:
			n = width(st.Select)
		}
		cols = def.columns[:min(n, len(def.columns))]
	}
	for _, c := range def.columns {
		if !slices.Contains(cols, c) {
			if err := f.columnDefault(def, c); err != nil {
				return err
			}
		}
	}
	for _, row := range st.Rows {
		for i, e := range row {
			if _, ok := e.(*sqlir.Default); ok && i < len(cols) {
				if err := f.columnDefault(def, cols[i]); err != nil {
					return err
				}
			}
		}
	}
	if st.OnConflict != nil {
		return f.setDefaults(st.Table, st.OnConflict.Set)
	}
	return nil
}

// setDefaults folds the defaults DEFAULT names in SET.
func (f *folder) setDefaults(table string, set []sqlir.Assignment) error {
	def := f.x.tx.db.defs[f.x.tx.db.resolve(table)]
	if def == nil {
		return nil
	}
	for _, a := range set {
		if _, ok := a.Value.(*sqlir.Default); ok {
			if err := f.columnDefault(def, a.Column); err != nil {
				return err
			}
		}
	}
	return nil
}

// columnDefault folds the default of col. A default has not been through
// the column check, so detest does not know the types Postgres resolves in
// it, such as a numeric division (#121), and an error from folding it is
// refused rather than raised. A default detest cannot evaluate is written
// as the Unknown marker rather than refused (columnDefault in schema.go),
// so a refusal for that is dropped, or every INSERT that leaves the column
// out would be unsupported.
func (f *folder) columnDefault(def *tableDef, col string) error {
	d, ok := def.defaults[col]
	if !ok || def.generated[col] != nil {
		return nil
	}
	f.doubted = false
	_, err := f.expr(d)
	if _, ok := errors.AsType[*ErrUnsupportedSQL](err); ok && !f.doubted || err == nil {
		return nil
	}
	if f.doubted {
		return err
	}
	return f.x.unsupported(foldDoubt)
}

// width is the number of columns sel gives, or, for a * whose columns
// depend on the scope it is read in, the number of the other items, which
// takes the columns after them as left out. Every arm of a set operation
// gives as many, so one without a * tells.
func width(sel *sqlir.SelectStmt) int {
	if n, ok := knownWidth(sel); ok {
		return n
	}
	for sel.SetOp != "" && sel.Larg != nil {
		sel = sel.Larg
	}
	n := 0
	for _, t := range sel.Targets {
		if !t.Star {
			n++
		}
	}
	return n
}

func knownWidth(sel *sqlir.SelectStmt) (int, bool) {
	if sel.SetOp != "" {
		if n, ok := knownWidth(sel.Larg); ok {
			return n, true
		}
		return knownWidth(sel.Rarg)
	}
	if names := outputNames(sel); names != nil {
		return len(names), true
	}
	return 0, false
}

type folder struct {
	x     *sqlExec
	views map[string]bool // the views folded already
	// ctes are the names of the CTEs in scope, which hide a view of the
	// same name.
	ctes map[string]bool
	// doubt counts the parts being folded that Postgres may drop before it
	// folds them, by what detest does not model.
	doubt int
	// doubted is set by a refusal for what detest does not model, which a
	// default keeps, unlike one for an expression detest cannot evaluate.
	doubted bool
	// depth is how deep in an expression of the query being folded expr
	// is, 1 at its root.
	depth int
}

// stand records that Postgres reduces e to v without evaluating its
// operands, so that the expressions around it are evaluated with v for it.
func (f *folder) stand(e sqlir.Expr, v any) folded {
	f.x.foldValues[e] = v
	return folded{c: fixed, e: e, val: v, hasVal: true}
}

func (f *folder) doubtful() error {
	f.doubted = true
	return f.x.unsupported(foldDoubt)
}

// constness is whether Postgres folds an expression when it plans the
// statement.
type constness int

const (
	varies constness = iota // it does not
	fixed                   // it does
	// maybe is a constant whose folding depends on what detest does not
	// model, the volatility of an operator, a cast or a function that
	// Postgres picks by the operand types, such as timestamptz + interval,
	// which is stable, against timestamp + interval, which is immutable,
	// or the strictness of a function detest does not list.
	maybe
)

// folded is an expression after folding, with the value Postgres folds it
// to when hasVal is set. e is the expression itself when detest can
// evaluate it for that value, which it cannot when the expression holds
// anything but constants, even in a part Postgres drops, as evaluating it
// would run that part, a nextval say. A rebuilt expression would not do
// instead, as the column check keys the types it resolves, such as a
// division's or a CASE's, by the expression.
type folded struct {
	c      constness
	e      sqlir.Expr
	val    any
	hasVal bool
}

func (r folded) null() bool { return r.c != varies && r.hasVal && derefValue(r.val) == nil }

const foldDoubt = "an expression of constants that may fail when Postgres plans the statement, where whether it does depends on what detest does not model"

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
		if t, ok := reflect.TypeAssert[sqlir.TableRef](v); ok {
			return f.tableRef(t)
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

func (f *folder) tableRef(t sqlir.TableRef) error {
	if t.Func != nil {
		// A function in FROM is not folded into a constant, but its
		// arguments are.
		for _, a := range t.Func.Args {
			if _, err := f.expr(a); err != nil {
				return err
			}
		}
		return nil
	}
	if t.Sub != nil {
		return f.derived(t.Sub)
	}
	if t.Name == "" || (!strings.Contains(t.Name, ".") && f.ctes[t.Name]) {
		return nil
	}
	// Postgres expands a view into the statement before it plans it, so
	// the view's expressions are folded too, in the view's own scope.
	name := f.x.tx.db.resolve(t.Name)
	vw := f.x.tx.db.views[name]
	if vw == nil || vw.View == nil || f.views[name] {
		return nil
	}
	f.views[name] = true
	outer := f.ctes
	f.ctes = nil
	defer func() { f.ctes = outer }()
	return f.derived(vw.View)
}

// query folds a query.
func (f *folder) query(sel *sqlir.SelectStmt) error {
	body := *sel
	body.With = nil
	return f.with(sel.With, &body, func() error { return f.node(reflect.ValueOf(&body).Elem()) })
}

// derived folds the query of a table in FROM, a view or a CTE in doubt.
// Postgres removes the outputs the query around it does not read before
// it plans it, and plans none of it when a constant false condition of
// the query around it empties it, as for one it does not pull up into
// that query. Which it does depends on how the outer query reads it,
// whether it inlines it and how many times a CTE is read, which detest
// does not model.
func (f *folder) derived(sel *sqlir.SelectStmt) error {
	defer f.enter()()
	return f.query(sel)
}

// with folds the CTEs of with that body reads, leaving out the others,
// which Postgres drops before it plans the statement, and then body, in
// the scope of with.
func (f *folder) with(with []sqlir.CTE, body any, fold func() error) error {
	read := reachableWith(with, body)
	outer := f.ctes
	defer func() { f.ctes = outer }()
	for i, c := range with {
		if read[c.Name] {
			// A CTE sees the ones declared before it.
			f.ctes = scopeWith(outer, with[:i])
			if err := f.derived(c.Select); err != nil {
				return err
			}
		}
	}
	f.ctes = scopeWith(outer, with)
	return fold()
}

func scopeWith(outer map[string]bool, with []sqlir.CTE) map[string]bool {
	m := maps.Clone(outer)
	if m == nil {
		m = map[string]bool{}
	}
	for _, c := range with {
		m[c.Name] = true
	}
	return m
}

// exists folds the query of an EXISTS, whose select list, GROUP BY,
// DISTINCT and ORDER BY Postgres drops unplanned when nothing in the query
// needs them (simplify_EXISTS_query).
func (f *folder) exists(sel *sqlir.SelectStmt) error {
	if sel.SetOp != "" || sel.Values != nil || sel.Having != nil || sel.Offset != nil || sel.Lock != nil ||
		existsNeedsTargets(sel) {
		return f.query(sel)
	}
	s := *sel
	s.Targets, s.GroupBy, s.OrderBy, s.Distinct, s.DistinctOn, s.Limit = nil, nil, nil, false, nil, nil
	if sel.Limit == nil {
		return f.query(&s)
	}
	// Postgres folds the LIMIT to tell, keeping the query whole for one
	// that is not a constant or is not positive. The CTEs are folded after
	// that, as the select list it drops may be what reads one.
	outer := f.ctes
	f.ctes = scopeWith(outer, sel.With)
	r, err := f.expr(sel.Limit)
	f.ctes = outer
	if err != nil {
		return err
	}
	n, isInt := derefValue(r.val).(int64)
	switch {
	case r.c == varies:
		return f.query(sel)
	case r.c == fixed && r.null():
		return f.query(&s)
	case r.c == fixed && isInt:
		if n > 0 {
			return f.query(&s)
		}
		return f.query(sel)
	}
	if err := f.query(&s); err != nil {
		return err
	}
	defer f.enter()()
	return f.query(sel)
}

// existsNeedsTargets reports an aggregate, a window function or a
// set-returning function in the query block of an EXISTS, which keeps
// Postgres from dropping its select list.
func existsNeedsTargets(sel *sqlir.SelectStmt) bool {
	if len(windowsOf(sel)) > 0 || orderOnlyAggregate(sel) || slices.ContainsFunc(sel.DistinctOn, hasAggregate) {
		return true
	}
	for _, t := range sel.Targets {
		if hasAggregate(t.Expr) {
			return true
		}
		for _, c := range sqlir.BlockFuncCalls(t.Expr) {
			if c.Name == "generate_series" || c.Name == "unnest" {
				return true
			}
		}
	}
	return false
}

// expr folds e. A branch Postgres drops because of a constant before it,
// in AND, OR, CASE or COALESCE, is not folded, as it is not there.
func (f *folder) expr(e sqlir.Expr) (folded, error) {
	f.depth++
	defer func() { f.depth-- }()
	return f.fold(e)
}

// subquery folds sel, a subquery of an expression. Postgres plans it only
// when it is still there after it folds the expression around it, which
// may drop it by more rules than detest models, such as NULL + (SELECT
// ...) or (SELECT ...) = 1 AND false, so one that is not the whole
// expression is folded in doubt.
func (f *folder) subquery(sel *sqlir.SelectStmt, exists bool) error {
	if f.depth > 1 {
		defer f.enter()()
	}
	depth := f.depth
	f.depth = 0
	defer func() { f.depth = depth }()
	if exists {
		return f.exists(sel)
	}
	return f.query(sel)
}

func (f *folder) fold(e sqlir.Expr) (folded, error) {
	switch v := e.(type) {
	case nil:
		return folded{c: fixed, e: &sqlir.Const{}, hasVal: true}, nil
	case *sqlir.Const:
		return folded{c: fixed, e: v, val: v.Value, hasVal: true}, nil
	case *sqlir.ColumnRef, *sqlir.Param, *sqlir.Default, *sqlir.Unconverted:
		return folded{}, nil
	case *sqlir.SubQuery:
		return folded{}, f.subquery(v.Select, false)
	case *sqlir.Exists:
		return folded{}, f.subquery(v.Select, true)
	case *sqlir.BinaryExpr:
		if v.Op == "AND" || v.Op == "OR" {
			return f.logic(v)
		}
		kids, err := f.rowExprs(v.L, v.R)
		if err != nil {
			return folded{}, err
		}
		c := plainOperands(kids...)
		if v.Op == "||" && !sameKind(kids[0], kids[1]) {
			// || with an operand that is not text is anytextcat, which is
			// stable, as it calls the operand's output function.
			c = maybe
		}
		return f.apply(v, kids, c, strict)
	case *sqlir.UnaryExpr:
		kids, err := f.exprs(v.X)
		if err != nil {
			return folded{}, err
		}
		if k := kids[0]; v.Op == "NOT" && k.c != varies {
			if !boolOrNull(k) {
				return folded{c: k.c}, nil // see logic
			}
			if b, ok := derefValue(k.val).(bool); ok && k.e == nil {
				// A reduced AND or OR detest cannot evaluate, whose value
				// it knows.
				return folded{c: k.c, val: !b, hasVal: true}, nil
			}
		}
		return f.apply(v, kids, plainOperands(kids...), strict)
	case *sqlir.IsNull:
		kids, err := f.exprs(v.X)
		if err != nil {
			return folded{}, err
		}
		if _, ok := v.X.(*sqlir.RowExpr); ok {
			// Postgres does not fold a NULL test of a row, whatever its
			// items are.
			return folded{}, nil
		}
		if k := kids[0]; k.c != varies && k.e == nil && k.hasVal {
			// An operand detest cannot evaluate, whose value it knows to
			// be NULL or a boolean.
			return folded{c: k.c, val: k.null() != v.Not, hasVal: true}, nil
		}
		return f.apply(v, kids, fixed, notStrict)
	case *sqlir.Collate:
		kids, err := f.exprs(v.X)
		if err != nil {
			return folded{}, err
		}
		return f.apply(v, kids, fixed, strict)
	case *sqlir.Cast:
		kids, err := f.exprs(v.X)
		if err != nil {
			return folded{}, err
		}
		c := fixed
		if k, ok := v.X.(*sqlir.Const); !ok || !isString(k.Value) {
			// A string literal is read by the type's input function when
			// Postgres parses the statement. Any other cast is a function
			// whose volatility depends on both types, such as timestamp to
			// date, immutable, against timestamptz to date, stable.
			if !plainCastTypes[v.Type] || !kids[0].hasVal || !plainValue(kids[0].val) {
				c = maybe
			}
		}
		return f.apply(v, kids, c, strict)
	case *sqlir.InExpr:
		if v.Sub != nil {
			if _, err := f.expr(v.X); err != nil {
				return folded{}, err
			}
			return folded{}, f.subquery(v.Sub, false)
		}
		kids, err := f.rowExprs(append([]sqlir.Expr{v.X}, v.List...)...)
		if err != nil {
			return folded{}, err
		}
		s, err := f.inStrictness(v, kids)
		if err != nil {
			return folded{}, err
		}
		if s == maybeStrict {
			return folded{c: maybe}, nil
		}
		return f.apply(v, kids, plainOperands(kids...), s)
	case *sqlir.ArrayCmp:
		kids, err := f.exprs(v.X, v.Array)
		if err != nil {
			return folded{}, err
		}
		return f.apply(v, kids, plainOperands(kids...), notStrict)
	case *sqlir.RowExpr:
		kids, err := f.exprs(v.Items...)
		if err != nil {
			return folded{}, err
		}
		for _, k := range kids {
			if k.c == varies {
				return folded{}, nil
			}
		}
		// A row has no value of its own to evaluate, so the comparison
		// around it is evaluated instead.
		r := folded{c: maybe, e: v}
		for _, k := range kids {
			if k.e == nil {
				// An item whose value detest does not know, so the
				// row's is not known either.
				r.e = nil
			}
		}
		return r, nil
	case *sqlir.CaseExpr:
		return f.caseExpr(v)
	case *sqlir.FuncCall:
		return f.call(v)
	case *sqlir.WindowFunc:
		for _, a := range v.Func.Args {
			if _, err := f.expr(a); err != nil {
				return folded{}, err
			}
		}
		if err := f.node(reflect.ValueOf(v.Partition)); err != nil {
			return folded{}, err
		}
		return folded{}, f.node(reflect.ValueOf(v.Order))
	}
	// Any other expression folds what it holds, and is not a constant.
	return folded{}, f.node(reflect.ValueOf(e).Elem())
}

// inStrictness is whether a NULL folds v, an IN list whose operands are
// kids, to NULL. Postgres's parser compares X with the items that read no
// column as one array when there are two or more of them, and with each
// other item by = (transformAExprIn). A NULL X reduces each = to NULL,
// and the array comparison only when every item in it is a constant, as
// Postgres folds that comparison only then. A NULL item reduces the =
// of a list of one item. Which items a subquery in the list reads is not
// modeled, so a NULL with one is maybeStrict.
func (f *folder) inStrictness(v *sqlir.InExpr, kids []folded) (strictness, error) {
	if v.Array {
		return notStrict, nil
	}
	if len(v.List) == 1 && kids[1].null() {
		return strict, nil
	}
	if !kids[0].null() {
		return notStrict, nil
	}
	var array []folded
	for i, item := range v.List {
		if len(sqlir.Selects(item)) > 0 {
			return maybeStrict, nil
		}
		if len(sqlir.ColumnRefs(item)) == 0 {
			array = append(array, kids[i+1])
		}
	}
	if len(array) >= 2 {
		for _, k := range array {
			if k.c == varies || k.e == nil {
				return notStrict, nil
			}
		}
	}
	return strict, nil
}

// rowExprs is exprs for the operands of a comparison, which Postgres
// expands into comparisons of the items when they are rows, with pruning
// of its own (1 = 2 AND 1 / 0 = 0 for (1, 1 / 0) = (2, 0)), so the
// operands are folded in doubt then.
func (f *folder) rowExprs(es ...sqlir.Expr) ([]folded, error) {
	for _, e := range es {
		if _, ok := e.(*sqlir.RowExpr); ok {
			defer f.enter()()
			break
		}
	}
	return f.exprs(es...)
}

func (f *folder) exprs(es ...sqlir.Expr) ([]folded, error) {
	out := make([]folded, len(es))
	for i, e := range es {
		r, err := f.expr(e)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

// strictness is whether an expression is NULL whenever an operand is,
// which Postgres folds to NULL without the other operands being constants.
type strictness int

const (
	notStrict strictness = iota
	strict
	maybeStrict // a function detest does not list
)

// apply folds e, whose operands are kids. When every one is a constant, it
// evaluates e as c, or as maybe when an operand is one.
func (f *folder) apply(e sqlir.Expr, kids []folded, c constness, s strictness) (folded, error) {
	if s != notStrict {
		// Postgres returns NULL without calling a strict operator or
		// function, so nothing about the other operands, nor what detest
		// can evaluate, matters.
		for _, k := range kids {
			if k.null() {
				r := f.stand(e, nil)
				r.c = k.c
				if s == maybeStrict {
					r.c = maybe
				}
				return r, nil
			}
		}
	}
	for _, k := range kids {
		if k.c == varies {
			return folded{}, nil
		}
	}
	for _, k := range kids {
		if k.c == maybe {
			c = maybe
		}
		if k.e == nil {
			// An operand is a constant detest cannot evaluate, so whether
			// this one fails folding is not known.
			return folded{}, f.doubtful()
		}
	}
	return f.eval(e, c)
}

// eval evaluates e, an expression of constants, for the error Postgres
// raises folding it. One detest does not evaluate is refused, as whether
// Postgres fails folding it is not known, and so is any error from a
// constant Postgres may not fold or in a part it may drop.
func (f *folder) eval(e sqlir.Expr, c constness) (folded, error) {
	// evalRaw, as a quotient that repeats is a range here that the
	// expression around it may still compare exactly.
	v, err := f.x.evalRaw(e, nil)
	if err != nil {
		if _, ok := errors.AsType[errUnknownExpr](err); ok {
			return folded{}, f.x.unsupportedExpr(err, "of constants")
		}
		if _, ok := errors.AsType[*ErrUnsupportedSQL](err); !ok && (c == maybe || f.doubt > 0) {
			return folded{}, f.doubtful()
		}
		return folded{}, err
	}
	return folded{c: c, e: e, val: v, hasVal: true}, nil
}

// reduced is r, the part Postgres reduces e to, as e, path being the
// parts of e the evaluators read to reach that part, in order. e is
// evaluated as a whole, so that the type it resolves, such as numeric for
// CASE WHEN true THEN 1 ELSE 1.5 END, is kept. The evaluators stop where
// Postgres does, so what follows path is not run, a random() or a nextval
// in it included. A part of path detest cannot evaluate leaves e unknown
// but for whether it is NULL or a boolean.
func (f *folder) reduced(e sqlir.Expr, r folded, path ...folded) (folded, error) {
	if r.c == varies {
		return r, nil
	}
	for _, p := range path {
		if p.e == nil {
			r.e = nil
			return r, nil
		}
	}
	return f.eval(e, r.c)
}

// plainOperands is fixed when every operand has a value of a type whose
// operators and functions detest knows are immutable, and maybe otherwise.
// An interval is one, as its arithmetic, comparison, extract and
// date_trunc read no setting, unlike a timestamptz's.
func plainOperands(kids ...folded) constness {
	for _, k := range kids {
		if !k.hasVal {
			return maybe
		}
		if _, ok := derefValue(k.val).(pgInterval); !ok && !plainValue(k.val) {
			return maybe
		}
	}
	return fixed
}

// plainValue reports a value of a type whose casts to plainCastTypes
// Postgres folds. An interval is not one, as its output reads
// IntervalStyle.
func plainValue(v any) bool {
	switch derefValue(v).(type) {
	case nil, bool, int64, int, float64, string, []byte, numRange:
		return true
	}
	return false
}

// pure reports that e holds nothing but constants and the operators and
// functions over them, even in a branch Postgres drops, so that evaluating
// it runs nothing Postgres does not.
func pure(e sqlir.Expr) bool {
	if len(sqlir.ColumnRefs(e)) > 0 || len(sqlir.Params(e)) > 0 || len(sqlir.Selects(e)) > 0 || len(sqlir.WindowFuncs(e)) > 0 {
		return false
	}
	for _, c := range sqlir.FuncCalls(e) {
		if isAggregateCall(c) || effectFuncs[c.Name] || sessionFuncs[c.Name] {
			return false
		}
	}
	return true
}

func sameKind(a, b folded) bool {
	if derefValue(a.val) == nil || derefValue(b.val) == nil {
		return true
	}
	return reflect.TypeOf(derefValue(a.val)) == reflect.TypeOf(derefValue(b.val))
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

// plainCastTypes are the types a cast to which from a plain value
// Postgres folds.
var plainCastTypes = map[string]bool{
	"int": true, "int2": true, "int4": true, "int8": true, "bigint": true, "integer": true, "smallint": true,
	"numeric": true, "float8": true, "float4": true, "double precision": true, "real": true,
	"text": true, "varchar": true, "bpchar": true, "bool": true, "boolean": true, "bytea": true, "uuid": true,
}

// immutableFuncs are the functions detest computes that Postgres folds
// over constant arguments of the types plainOperands accepts.
var immutableFuncs = map[string]bool{
	"abs": true, "floor": true, "ceil": true, "ceiling": true, "round": true, "power": true, "pow": true,
	"lower": true, "upper": true, "length": true, "char_length": true, "octet_length": true, "left": true,
	"hashtext": true, "greatest": true, "least": true, "nullif": true,
	"extract": true, "date_part": true, "date_trunc": true, "make_interval": true,
}

// nonStrictFuncs are the functions detest lists that a NULL argument does
// not make NULL.
var nonStrictFuncs = map[string]bool{"greatest": true, "least": true, "nullif": true, "concat": true}

// sessionFuncs are the functions Postgres never folds, as they read the
// transaction or the session, besides effectFuncs, which change state and
// so are never evaluated here.
var sessionFuncs = map[string]bool{
	"now": true, "current_timestamp": true, "transaction_timestamp": true, "statement_timestamp": true,
	"localtimestamp": true, "current_date": true, "current_time": true, "localtime": true, "timeofday": true,
}

func (f *folder) call(v *sqlir.FuncCall) (folded, error) {
	if v.Name == "coalesce" {
		return f.coalesce(v)
	}
	kids, err := f.exprs(v.Args...)
	if err != nil {
		return folded{}, err
	}
	if isAggregateCall(v) {
		return folded{}, nil
	}
	if effectFuncs[v.Name] || sessionFuncs[v.Name] {
		// Strict, so Postgres reduces one with a NULL argument to NULL
		// without calling it, its volatility notwithstanding.
		for _, k := range kids {
			if k.null() {
				r := f.stand(v, nil)
				r.c = k.c
				return r, nil
			}
		}
		return folded{}, nil
	}
	if v.Name == "nullif" && len(kids) == 2 && kids[0].null() && (kids[1].c == varies || kids[1].e == nil) {
		// Postgres reduces a NULLIF of a NULL to NULL whatever the other
		// argument is.
		r := f.stand(v, nil)
		r.c = kids[0].c
		return r, nil
	}
	c, s := plainOperands(kids...), strict
	if nonStrictFuncs[v.Name] {
		s = notStrict
	}
	if !immutableFuncs[v.Name] {
		c = maybe
		if s == strict {
			s = maybeStrict
		}
	}
	return f.apply(v, kids, c, s)
}

// cond is what Postgres may do with a branch behind a condition when it
// plans the statement. It keeps the branch when the condition is not a
// constant, takes it when the condition is true and drops it when the
// condition is false or NULL. More than one is set when detest cannot tell
// which.
type cond struct{ keep, take, drop bool }

// condOf reads r as the condition of a branch, or as an operand of AND or
// OR, take standing for the value that decides it.
func condOf(r folded, take bool) cond {
	var out cond
	switch r.c {
	case varies:
		return cond{keep: true}
	case maybe:
		out.keep = true
	}
	v := derefValue(r.val)
	b, isBool := v.(bool)
	switch {
	case !r.hasVal || !isBool && v != nil:
		// A string literal Postgres reads as a boolean when it parses the
		// statement, which detest does not.
		out.take, out.drop = true, true
	case isBool && b == take:
		out.take = true
	default:
		out.drop = true
	}
	return out
}

// boolean is r read as a boolean. The converter casts a string literal
// where a boolean is wanted to bool (#118), so a constant whose value is
// not one is a string that reached here otherwise, whose value as a
// boolean detest does not know.
func boolean(r folded) folded {
	if r.c != varies && r.hasVal && !boolOrNull(r) {
		return folded{c: r.c}
	}
	return r
}

// boolOrNull reports a constant whose value detest knows to be a boolean
// or NULL.
func boolOrNull(r folded) bool {
	if !r.hasVal {
		return false
	}
	switch derefValue(r.val).(type) {
	case nil, bool:
		return true
	}
	return false
}

func (c cond) uncertain() bool {
	n := 0
	for _, b := range []bool{c.keep, c.take, c.drop} {
		if b {
			n++
		}
	}
	return n > 1
}

// enter folds what follows in doubt, until the returned func is called.
func (f *folder) enter() func() {
	f.doubt++
	return func() { f.doubt-- }
}

// logic folds AND and OR as Postgres does. An operand that is a constant
// that decides the whole reduces it to that, and the operands after it are
// not folded.
func (f *folder) logic(v *sqlir.BinaryExpr) (folded, error) {
	decides := v.Op == "OR"
	var kids []folded
	uncertain, reduced := false, false
	for _, a := range []sqlir.Expr{v.L, v.R} {
		r, err := f.expr(a)
		if err != nil {
			return folded{}, err
		}
		kids = append(kids, r)
		c := condOf(r, decides)
		if c.take && !c.uncertain() {
			// Postgres reduces the whole AND to false, or the OR to true.
			return f.stand(v, decides), nil
		}
		uncertain = uncertain || c.uncertain()
		if c.take {
			// Reduced if Postgres folds r, which its value would decide.
			reduced = reduced || !c.drop
			defer f.enter()()
		}
	}
	for _, k := range kids {
		if k.c != varies && !boolOrNull(k) {
			// A string the converter did not cast to bool (#118), whose
			// value as a boolean is not known.
			c := fixed
			for _, k := range kids {
				c = max(c, k.c)
			}
			if c == varies {
				c = maybe
			}
			return folded{c: c}, nil
		}
	}
	if !uncertain {
		return f.apply(v, kids, plainOperands(kids...), notStrict)
	}
	for _, k := range kids {
		if k.c == varies || k.e == nil {
			if reduced {
				return folded{c: maybe, val: decides, hasVal: true}, nil
			}
			return folded{c: maybe}, nil
		}
	}
	return f.apply(v, kids, maybe, notStrict)
}

// caseExpr folds a CASE as Postgres does. A branch whose condition is a
// constant false or NULL is dropped unfolded, and one whose condition is a
// constant true ends the CASE, dropping the branches after it and ELSE.
func (f *folder) caseExpr(v *sqlir.CaseExpr) (folded, error) {
	arg, err := f.expr(v.Arg)
	if err != nil {
		return folded{}, err
	}
	var kids, path []folded
	if v.Arg != nil {
		kids = append(kids, arg)
		path = append(path, arg)
	}
	certain := true    // every branch so far is dropped for sure
	uncertain := false // a branch so far may be kept, taken or dropped
	mayEnd := false    // a branch so far may be taken
	varying := false   // the CASE is no constant whatever detest cannot tell
	ended := false
	for _, w := range v.Whens {
		when, err := f.expr(w.When)
		if err != nil {
			return folded{}, err
		}
		if v.Arg == nil {
			when = boolean(when)
		}
		test := when
		if v.Arg != nil {
			test, err = f.caseTest(arg, when)
			if err != nil {
				return folded{}, err
			}
		}
		path = append(path, when)
		c := condOf(test, true)
		if !c.keep && !c.take {
			continue // dropped, false or NULL
		}
		leave := func() {}
		if c.drop {
			leave = f.enter()
		}
		then, err := f.expr(w.Then)
		leave()
		if err != nil {
			return folded{}, err
		}
		if c.take && !c.uncertain() && certain {
			return f.reduced(v, then, append(path, then)...) // the CASE is reduced to this branch
		}
		certain = false
		if !mayEnd && (test.c == varies || then.c == varies && !c.drop) {
			varying = true
		}
		uncertain = uncertain || c.uncertain()
		kids = append(kids, when, then)
		if c.take && !c.uncertain() {
			ended = true // ELSE is dropped
			break
		}
		if c.take {
			mayEnd = true
			defer f.enter()()
		}
	}
	if !ended {
		r, err := f.expr(v.Else)
		if err != nil {
			return folded{}, err
		}
		if certain {
			return f.reduced(v, r, append(path, r)...) // every branch is dropped
		}
		if !mayEnd && r.c == varies {
			varying = true
		}
		kids = append(kids, r)
	}
	if varying {
		return folded{}, nil
	}
	return f.whole(v, kids, uncertain)
}

// caseTest folds the comparison by = of a simple CASE's operand with the
// value of one of its branches. detest has no expression of its own for
// it, so it is evaluated from a new one, whose operands are constants of
// the types they are written with.
func (f *folder) caseTest(arg, when folded) (folded, error) {
	// = is strict, so a NULL on either side reduces it to NULL whatever
	// the other is.
	if arg.null() {
		return folded{c: arg.c, hasVal: true}, nil
	}
	if when.null() {
		return folded{c: when.c, hasVal: true}, nil
	}
	if arg.c == varies || when.c == varies {
		return folded{}, nil
	}
	c := max(arg.c, when.c)
	if arg.e == nil || when.e == nil {
		return folded{c: maybe}, nil
	}
	return f.eval(&sqlir.BinaryExpr{Op: "=", L: arg.e, R: when.e}, max(c, plainOperands(arg, when)))
}

// whole folds e, a CASE or a COALESCE that no constant reduced, from kids,
// the parts of it Postgres keeps.
func (f *folder) whole(e sqlir.Expr, kids []folded, uncertain bool) (folded, error) {
	c := fixed
	if uncertain {
		c = maybe
	}
	known := true
	for _, k := range kids {
		if k.c == varies {
			if !uncertain {
				return folded{}, nil
			}
			return folded{c: maybe}, nil
		}
		c = max(c, k.c)
		// A part whose value detest does not know, such as a string
		// literal under NOT, which the evaluators would read as NULL.
		known = known && k.e != nil
	}
	if !known || !pure(e) {
		return folded{c: c}, nil
	}
	return f.eval(e, c)
}

// coalesce folds a COALESCE as Postgres does. It drops a NULL argument and
// stops at the first constant that is not NULL, folding none after it.
func (f *folder) coalesce(v *sqlir.FuncCall) (folded, error) {
	var kids, path []folded
	uncertain, kept := false, false
	mayStop := false // an argument so far may end the COALESCE
	for _, a := range v.Args {
		r, err := f.expr(a)
		if err != nil {
			return folded{}, err
		}
		path = append(path, r)
		if r.c == varies {
			kept = true
			kids = append(kids, r)
			if !mayStop {
				// No constant, whatever detest cannot tell about the
				// arguments after it.
				uncertain = false
			}
			continue
		}
		if r.c == fixed && r.null() {
			continue // dropped
		}
		if r.c == fixed && r.hasVal {
			if !kept && !uncertain {
				return f.reduced(v, r, path...) // the COALESCE is reduced to this argument
			}
			kids = append(kids, r)
			break
		}
		if !kept || mayStop {
			uncertain = true
		}
		kids = append(kids, r)
		if !r.null() {
			// Postgres stops here if it folds this argument.
			mayStop = true
			defer f.enter()()
		}
	}
	if len(kids) == 0 {
		return f.reduced(v, folded{c: fixed, hasVal: true}, path...)
	}
	return f.whole(v, kids, uncertain)
}
