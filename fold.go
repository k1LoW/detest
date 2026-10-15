package detest

import (
	"errors"
	"maps"
	"reflect"
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
	switch st := stmt.(type) {
	case *sqlir.SelectStmt:
		return f.query(st)
	case *sqlir.InsertStmt, *sqlir.CreateTableAsStmt:
		return f.node(reflect.ValueOf(st))
	case *sqlir.UpdateStmt:
		body := *st
		body.With = nil
		return f.with(st.With, &body, func() error { return f.node(reflect.ValueOf(&body)) })
	case *sqlir.DeleteStmt:
		body := *st
		body.With = nil
		return f.with(st.With, &body, func() error { return f.node(reflect.ValueOf(&body)) })
	}
	return nil
}

type folder struct {
	x     *sqlExec
	views map[string]bool // the views folded already
	// ctes are the names of the CTEs in scope, which hide a view of the
	// same name.
	ctes map[string]bool
	// doubt counts the branches being folded that Postgres keeps or drops
	// depending on whether it folds a constant detest cannot tell it folds.
	doubt int
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
	// which is stable, against timestamp + interval, which is immutable.
	maybe
)

// folded is an expression after folding. e is an expression of constants
// with its value, which a reduced AND, OR, CASE or COALESCE stands in for
// with the part Postgres reduces it to, so that evaluating it never runs a
// part Postgres drops, such as a nextval. It is nil for varies, and for a
// maybe whose value detest cannot compute.
type folded struct {
	c      constness
	e      sqlir.Expr
	val    any
	hasVal bool
}

const foldDoubt = "an expression of constants that may fail when Postgres plans the statement, depending on whether it folds a constant detest cannot tell it folds"

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
		return f.query(t.Sub)
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
	return f.query(vw.View)
}

// query folds a query.
func (f *folder) query(sel *sqlir.SelectStmt) error {
	body := *sel
	body.With = nil
	return f.with(sel.With, &body, func() error { return f.node(reflect.ValueOf(&body).Elem()) })
}

// with folds the CTEs of with that body reads, leaving out the others,
// which Postgres drops before it plans the statement, and then body, in
// the scope of with.
func (f *folder) with(with []sqlir.CTE, body any, fold func() error) error {
	read := reachableWith(with, body)
	outer := f.ctes
	defer func() { f.ctes = outer }()
	scope := func(n int) map[string]bool {
		m := maps.Clone(outer)
		if m == nil {
			m = map[string]bool{}
		}
		for _, c := range with[:n] {
			m[c.Name] = true
		}
		return m
	}
	for i, c := range with {
		if read[c.Name] {
			// A CTE sees the ones declared before it.
			f.ctes = scope(i)
			if err := f.query(c.Select); err != nil {
				return err
			}
		}
	}
	f.ctes = scope(len(with))
	return fold()
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
	// that is not a constant or is not positive.
	body := *sel
	body.With = nil
	return f.with(sel.With, &body, func() error {
		r, err := f.expr(sel.Limit)
		if err != nil {
			return err
		}
		if r.c == varies {
			return f.node(reflect.ValueOf(&body).Elem())
		}
		s.With = nil
		if err := f.node(reflect.ValueOf(&s).Elem()); err != nil {
			return err
		}
		n, isInt := derefValue(r.val).(int64)
		if r.c == fixed && r.hasVal && (r.val == nil || isInt && n > 0) {
			return nil
		}
		if r.c == maybe || !isInt && r.val != nil {
			f.doubt++
			defer func() { f.doubt-- }()
		}
		return f.node(reflect.ValueOf(&body).Elem())
	})
}

// existsNeedsTargets reports an aggregate, a window function or a
// set-returning function in the select list of an EXISTS, which keeps
// Postgres from dropping the list.
func existsNeedsTargets(sel *sqlir.SelectStmt) bool {
	for _, t := range sel.Targets {
		if hasAggregate(t.Expr) || len(sqlir.WindowFuncs(t.Expr)) > 0 {
			return true
		}
		for _, c := range sqlir.FuncCalls(t.Expr) {
			if c.Name == "generate_series" || c.Name == "unnest" {
				return true
			}
		}
	}
	return orderOnlyAggregate(sel)
}

// expr folds e. A branch Postgres drops because of a constant before it,
// in AND, OR, CASE or COALESCE, is not folded, as it is not there.
func (f *folder) expr(e sqlir.Expr) (folded, error) {
	switch v := e.(type) {
	case nil:
		return folded{c: fixed, e: &sqlir.Const{}, hasVal: true}, nil
	case *sqlir.Const:
		return folded{c: fixed, e: v, val: v.Value, hasVal: true}, nil
	case *sqlir.ColumnRef, *sqlir.Param, *sqlir.Default, *sqlir.Unconverted:
		return folded{}, nil
	case *sqlir.SubQuery:
		return folded{}, f.query(v.Select)
	case *sqlir.Exists:
		return folded{}, f.exists(v.Select)
	case *sqlir.BinaryExpr:
		if v.Op == "AND" || v.Op == "OR" {
			return f.logic(v)
		}
		kids, err := f.exprs(v.L, v.R)
		if err != nil {
			return folded{}, err
		}
		c := plainOperands(kids...)
		if v.Op == "||" && !sameKind(kids[0], kids[1]) {
			// || with an operand that is not text is anytextcat, which is
			// stable, as it calls the operand's output function.
			c = maybe
		}
		return f.apply(kids, c, func(es []sqlir.Expr) sqlir.Expr {
			return &sqlir.BinaryExpr{Op: v.Op, L: es[0], R: es[1]}
		})
	case *sqlir.UnaryExpr:
		kids, err := f.exprs(v.X)
		if err != nil {
			return folded{}, err
		}
		return f.apply(kids, plainOperands(kids...), func(es []sqlir.Expr) sqlir.Expr {
			return &sqlir.UnaryExpr{Op: v.Op, X: es[0]}
		})
	case *sqlir.IsNull:
		kids, err := f.exprs(v.X)
		if err != nil {
			return folded{}, err
		}
		return f.apply(kids, fixed, func(es []sqlir.Expr) sqlir.Expr {
			return &sqlir.IsNull{X: es[0], Not: v.Not}
		})
	case *sqlir.Collate:
		kids, err := f.exprs(v.X)
		if err != nil {
			return folded{}, err
		}
		return f.apply(kids, fixed, func(es []sqlir.Expr) sqlir.Expr {
			return &sqlir.Collate{X: es[0], Name: v.Name}
		})
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
		return f.apply(kids, c, func(es []sqlir.Expr) sqlir.Expr {
			return &sqlir.Cast{X: es[0], Type: v.Type, Len: v.Len}
		})
	case *sqlir.InExpr:
		if v.Sub != nil {
			if _, err := f.expr(v.X); err != nil {
				return folded{}, err
			}
			return folded{}, f.query(v.Sub)
		}
		kids, err := f.exprs(append([]sqlir.Expr{v.X}, v.List...)...)
		if err != nil {
			return folded{}, err
		}
		return f.apply(kids, plainOperands(kids...), func(es []sqlir.Expr) sqlir.Expr {
			return &sqlir.InExpr{X: es[0], List: es[1:], Not: v.Not}
		})
	case *sqlir.ArrayCmp:
		kids, err := f.exprs(v.X, v.Array)
		if err != nil {
			return folded{}, err
		}
		return f.apply(kids, plainOperands(kids...), func(es []sqlir.Expr) sqlir.Expr {
			return &sqlir.ArrayCmp{X: es[0], Array: es[1], ElemType: v.ElemType, All: v.All}
		})
	case *sqlir.RowExpr:
		kids, err := f.exprs(v.Items...)
		if err != nil {
			return folded{}, err
		}
		for _, k := range kids {
			if k.c == varies {
				return folded{}, nil
			}
			if k.e == nil {
				return folded{c: maybe}, nil
			}
		}
		// A row has no value of its own to evaluate; the comparison
		// around it is evaluated instead.
		items := make([]sqlir.Expr, len(kids))
		for i, k := range kids {
			items[i] = k.e
		}
		return folded{c: maybe, e: &sqlir.RowExpr{Items: items}}, nil
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

// apply folds an expression whose operands are kids. When every one is a
// constant, it evaluates the expression build makes of their expressions,
// as c, or as maybe when an operand is one.
func (f *folder) apply(kids []folded, c constness, build func([]sqlir.Expr) sqlir.Expr) (folded, error) {
	for _, k := range kids {
		if k.c == varies {
			return folded{}, nil
		}
	}
	es := make([]sqlir.Expr, len(kids))
	for i, k := range kids {
		if k.c == maybe {
			c = maybe
		}
		if k.e == nil {
			// The operand may be a constant whose value detest does not
			// know, so whether this one fails folding is not known either.
			return folded{}, f.x.unsupported(foldDoubt)
		}
		es[i] = k.e
	}
	return f.eval(build(es), c)
}

// eval evaluates e, an expression of constants, for the error Postgres
// raises folding it. One detest does not evaluate is refused, as whether
// Postgres fails folding it is not known, and so is any error from a
// constant Postgres may not fold or in a branch it may drop.
func (f *folder) eval(e sqlir.Expr, c constness) (folded, error) {
	// evalRaw, as a quotient that repeats is a range here that the
	// expression around it may still compare exactly.
	v, err := f.x.evalRaw(e, nil)
	if err != nil {
		if _, ok := errors.AsType[errUnknownExpr](err); ok {
			return folded{}, f.x.unsupportedExpr(err, "of constants")
		}
		if _, ok := errors.AsType[*ErrUnsupportedSQL](err); !ok && (c == maybe || f.doubt > 0) {
			return folded{}, f.x.unsupported(foldDoubt)
		}
		return folded{}, err
	}
	return folded{c: c, e: e, val: v, hasVal: true}, nil
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

// reduced is r, the part Postgres reduces e to, given as e itself when e
// is pure. The expression around it then computes e's type as the
// evaluators do, CASE WHEN true THEN '5' ELSE 1.0 END being a number and
// not the text '5'.
func (f *folder) reduced(e sqlir.Expr, r folded) (folded, error) {
	if r.c == varies || r.e == nil || !pure(e) {
		return r, nil
	}
	return f.eval(e, r.c)
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
	if isAggregateCall(v) || effectFuncs[v.Name] || sessionFuncs[v.Name] {
		return folded{}, nil
	}
	c := plainOperands(kids...)
	if !immutableFuncs[v.Name] {
		c = maybe
	}
	return f.apply(kids, c, func(es []sqlir.Expr) sqlir.Expr {
		return &sqlir.FuncCall{Name: v.Name, Args: es}
	})
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
			return folded{c: fixed, e: &sqlir.Const{Value: decides}, val: decides, hasVal: true}, nil
		}
		uncertain = uncertain || c.uncertain()
		if c.take {
			// Reduced if Postgres folds r, which its value would decide.
			reduced = reduced || !c.drop
			defer f.enter()()
		}
	}
	if !uncertain {
		return f.apply(kids, plainOperands(kids...), func(es []sqlir.Expr) sqlir.Expr {
			return &sqlir.BinaryExpr{Op: v.Op, L: es[0], R: es[1]}
		})
	}
	for _, k := range kids {
		if k.c == varies || k.e == nil {
			if reduced {
				return folded{c: maybe, e: &sqlir.Const{Value: decides}, val: decides, hasVal: true}, nil
			}
			return folded{c: maybe}, nil
		}
	}
	return f.apply(kids, maybe, func(es []sqlir.Expr) sqlir.Expr {
		return &sqlir.BinaryExpr{Op: v.Op, L: es[0], R: es[1]}
	})
}

// caseExpr folds a CASE as Postgres does. A branch whose condition is a
// constant false or NULL is dropped unfolded, and one whose condition is a
// constant true ends the CASE, dropping the branches after it and ELSE.
func (f *folder) caseExpr(v *sqlir.CaseExpr) (folded, error) {
	arg, err := f.expr(v.Arg)
	if err != nil {
		return folded{}, err
	}
	var kids []folded
	var argExpr sqlir.Expr
	if v.Arg != nil {
		kids = append(kids, arg)
		argExpr = arg.e
	}
	var whens []sqlir.CaseWhen
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
		test := when
		if v.Arg != nil {
			pair := []folded{arg, when}
			test, err = f.apply(pair, plainOperands(pair...), func(es []sqlir.Expr) sqlir.Expr {
				return &sqlir.BinaryExpr{Op: "=", L: es[0], R: es[1]}
			})
			if err != nil {
				return folded{}, err
			}
		}
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
			return f.reduced(v, then) // the CASE is reduced to this branch
		}
		certain = false
		if !mayEnd && (test.c == varies || then.c == varies && !c.drop) {
			varying = true
		}
		uncertain = uncertain || c.uncertain()
		kids = append(kids, when, then)
		whens = append(whens, sqlir.CaseWhen{When: when.e, Then: then.e})
		if c.take && !c.uncertain() {
			ended = true // ELSE is dropped
			break
		}
		if c.take {
			mayEnd = true
			defer f.enter()()
		}
	}
	var elseExpr sqlir.Expr
	if !ended {
		r, err := f.expr(v.Else)
		if err != nil {
			return folded{}, err
		}
		if certain {
			return f.reduced(v, r) // every branch is dropped
		}
		if !mayEnd && r.c == varies {
			varying = true
		}
		kids = append(kids, r)
		elseExpr = r.e
	}
	if varying {
		return folded{}, nil
	}
	c := fixed
	if uncertain {
		c = maybe
		for _, k := range kids {
			if k.c == varies || k.e == nil {
				return folded{c: maybe}, nil
			}
		}
	}
	return f.apply(kids, c, func([]sqlir.Expr) sqlir.Expr {
		return &sqlir.CaseExpr{Arg: argExpr, Whens: whens, Else: elseExpr}
	})
}

// coalesce folds a COALESCE as Postgres does. It drops a NULL argument and
// stops at the first constant that is not NULL, folding none after it.
func (f *folder) coalesce(v *sqlir.FuncCall) (folded, error) {
	var kids []folded
	uncertain, kept := false, false
	mayStop := false // an argument so far may end the COALESCE
	for _, a := range v.Args {
		r, err := f.expr(a)
		if err != nil {
			return folded{}, err
		}
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
		null := r.hasVal && derefValue(r.val) == nil
		if r.c == fixed && null {
			continue // dropped
		}
		if r.c == fixed {
			if !kept && !uncertain {
				return f.reduced(v, r) // the COALESCE is reduced to this argument
			}
			kids = append(kids, r)
			break
		}
		if !kept || mayStop {
			uncertain = true
		}
		kids = append(kids, r)
		if !null {
			// Postgres stops here if it folds this argument.
			mayStop = true
			defer f.enter()()
		}
	}
	if len(kids) == 0 {
		return folded{c: fixed, e: &sqlir.Const{}, hasVal: true}, nil
	}
	c := fixed
	if uncertain {
		c = maybe
		for _, k := range kids {
			if k.c == varies || k.e == nil {
				return folded{c: maybe}, nil
			}
		}
	}
	return f.apply(kids, c, func(es []sqlir.Expr) sqlir.Expr {
		return &sqlir.FuncCall{Name: "coalesce", Args: es}
	})
}
