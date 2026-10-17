package detest

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// myCols are the names of a MySQL FROM item's columns, in order.
type myCols struct {
	names []string
	// unnamed is set when a column's name is not known, as MySQL names an
	// expression without an alias by its text.
	unnamed bool
	// unknown is set for an item whose columns detest does not know, such
	// as an ignored table, which resolves any name.
	unknown bool
}

// myItem is a FROM item a MySQL query block sees.
type myItem struct {
	alias string
	cols  myCols
	// refuse is why a reference that resolves to the item is refused,
	// where detest would read another value than MySQL does.
	refuse string
	// qualified is set for the row ON DUPLICATE KEY UPDATE reads through
	// VALUES() or the row alias, which a bare name does not reach.
	qualified bool
}

// myScope is the FROM items of one MySQL query block, inside those of the
// blocks around it.
type myScope struct {
	items []myItem
	outer *myScope
	// aliases are the block's select list aliases, which a subquery of the
	// block may read on MySQL in some clauses only.
	aliases []string
	// having is set on the scope a HAVING clause resolves in.
	having *myHaving
}

// myHaving are the columns MySQL lets a HAVING clause read outside an
// aggregate: the grouped ones and those the select list names.
type myHaving struct {
	cols map[[2]string]bool // by item alias and column
	star bool
}

// myWalk resolves the column names of a MySQL statement as MySQL does
// before it runs one: a name no item in scope has fails with 1054 and one
// two items have with 1052, whatever rows the statement reads. It checks
// names only, unlike columnChecker, whose types are Postgres's.
type myWalk struct {
	x    *sqlExec
	ctes []map[string]myCols
	// views are the views being named, against a view that reads itself.
	views map[string]bool
}

// checkMySQLColumns resolves the column names of a MySQL statement.
func (x *sqlExec) checkMySQLColumns(stmt sqlir.Statement) error {
	if len(x.tx.db.defs) == 0 {
		// Without a declared schema, every table takes any column.
		return nil
	}
	w := &myWalk{x: x, views: map[string]bool{}}
	switch st := stmt.(type) {
	case *sqlir.SelectStmt:
		_, _, err := w.query(st, nil)
		return err
	case *sqlir.InsertStmt:
		return w.insert(st)
	case *sqlir.UpdateStmt:
		return w.update(st)
	case *sqlir.DeleteStmt:
		return w.delete(st)
	}
	return nil
}

func (w *myWalk) unknownColumn(name, clause string) error {
	return w.x.tx.db.kind.Error(sqlir.UndefinedColumn, fmt.Sprintf("Unknown column '%s' in '%s'", name, clause), "", name, "")
}

func (w *myWalk) ambiguous(name, clause string) error {
	return w.x.tx.db.kind.Error(sqlir.AmbiguousColumn, fmt.Sprintf("Column '%s' in %s is ambiguous", name, clause), "", name, "")
}

// tableCols are the columns of the table, view or CTE name refers to.
func (w *myWalk) tableCols(name string) myCols {
	for _, m := range slices.Backward(w.ctes) {
		if c, ok := m[name]; ok {
			return c
		}
	}
	db := w.x.tx.db
	table := db.resolve(name)
	if def := db.defs[table]; def != nil {
		return myCols{names: def.columns}
	}
	if v := db.views[table]; v != nil && !w.views[table] {
		if len(v.ViewColumns) > 0 {
			return myCols{names: v.ViewColumns}
		}
		w.views[table] = true
		defer delete(w.views, table)
		// The view's own query is checked where it is created; here only
		// the names of its columns matter.
		saved := w.ctes
		w.ctes = nil
		cols, _, err := w.query(v.View, nil)
		w.ctes = saved
		if err == nil {
			return cols
		}
	}
	return myCols{unknown: true}
}

// itemAlias is the name a FROM item is referred to by, lower case as the
// parser writes a column's qualifier.
func itemAlias(t sqlir.TableRef) string {
	if t.Alias != "" {
		return t.Alias
	}
	return strings.ToLower(relname(t.Name))
}

// item returns the FROM item t, checking the query it is made of, which
// reads outer.
func (w *myWalk) item(t sqlir.TableRef, outer *myScope) (myItem, error) {
	it := myItem{alias: itemAlias(t)}
	switch {
	case t.Sub != nil:
		cols, _, err := w.query(t.Sub, outer)
		if err != nil {
			return it, err
		}
		it.cols = cols
	case t.Func != nil:
		it.cols = myCols{unknown: true}
	default:
		it.cols = w.tableCols(t.Name)
	}
	if len(t.Columns) > 0 {
		it.cols = myCols{names: t.Columns}
	}
	return it, nil
}

// query checks sel, whose correlated references reach outer, and returns
// the names of its columns and the scope of its block, nil for a set
// operation.
func (w *myWalk) query(sel *sqlir.SelectStmt, outer *myScope) (myCols, *myScope, error) {
	if len(sel.With) > 0 {
		m := map[string]myCols{}
		w.ctes = append(w.ctes, m)
		defer func() { w.ctes = w.ctes[:len(w.ctes)-1] }()
		for _, cte := range sel.With {
			cols, _, err := w.query(cte.Select, outer)
			if err != nil {
				return myCols{}, nil, err
			}
			m[cte.Name] = cols
		}
	}
	if sel.SetOp != "" {
		return w.setOp(sel, outer)
	}
	if len(sel.Values) > 0 {
		for _, row := range sel.Values {
			for _, e := range row {
				if err := w.expr(e, myCtx{sc: outer, clause: "field list"}); err != nil {
					return myCols{}, nil, err
				}
			}
		}
		// MySQL names the columns column_0 and on, detest column1.
		return myCols{unnamed: true}, nil, nil
	}
	sc := &myScope{outer: outer}
	if sel.From != nil {
		refs := []sqlir.TableRef{*sel.From}
		for _, j := range sel.Joins {
			refs = append(refs, j.Table)
		}
		for i, t := range refs {
			itemOuter := outer
			if t.Lateral {
				itemOuter = &myScope{items: slices.Clone(sc.items), outer: outer}
			}
			it, err := w.item(t, itemOuter)
			if err != nil {
				return myCols{}, nil, err
			}
			sc.items = append(sc.items, it)
			if i > 0 && sel.Joins[i-1].On != nil {
				// ON sees the items joined so far only.
				on := &myScope{items: slices.Clone(sc.items), outer: outer}
				if err := w.expr(sel.Joins[i-1].On, myCtx{sc: on, clause: "on clause"}); err != nil {
					return myCols{}, nil, err
				}
			}
		}
	}
	substituted := map[sqlir.Expr]string{}
	for _, t := range sel.Targets {
		if t.Alias != "" && t.Expr != nil {
			sc.aliases = append(sc.aliases, t.Alias)
			substituted[t.Expr] = t.Alias
		}
	}
	for _, t := range sel.Targets {
		if t.Expr != nil {
			if err := w.expr(t.Expr, myCtx{sc: sc, clause: "field list"}); err != nil {
				return myCols{}, nil, err
			}
		}
	}
	if sel.Where != nil {
		if err := w.expr(sel.Where, myCtx{sc: sc, clause: "where clause"}); err != nil {
			return myCols{}, nil, err
		}
	}
	having := &myHaving{cols: map[[2]string]bool{}}
	for _, g := range sel.GroupBy {
		if alias, ok := substituted[g]; ok {
			// The parser put the select list's expression for its alias,
			// which the select list's walk checked.
			if err := w.aliasColumn(sc, g, alias); err != nil {
				return myCols{}, nil, err
			}
		} else if err := w.expr(g, myCtx{sc: sc, clause: "group statement", aliases: sc.aliases}); err != nil {
			return myCols{}, nil, err
		}
		if r, ok := g.(*sqlir.ColumnRef); ok {
			if alias, ok := w.owner(r, sc); ok {
				having.cols[[2]string{alias, r.Column}] = true
			}
		}
	}
	if sel.Having != nil {
		for _, t := range sel.Targets {
			if t.Star {
				having.star = true
			}
			if r, ok := t.Expr.(*sqlir.ColumnRef); ok {
				if alias, ok := w.owner(r, sc); ok {
					having.cols[[2]string{alias, r.Column}] = true
				}
			}
		}
		hs := &myScope{items: sc.items, outer: sc.outer, aliases: sc.aliases, having: having}
		if err := w.expr(sel.Having, myCtx{sc: hs, block: sc, clause: "having clause", substituted: substituted}); err != nil {
			return myCols{}, nil, err
		}
	}
	for _, k := range sel.OrderBy {
		if err := w.orderKey(sel, sc, k.Expr); err != nil {
			return myCols{}, nil, err
		}
	}
	for _, e := range []sqlir.Expr{sel.Limit, sel.Offset} {
		if e != nil {
			if err := w.expr(e, myCtx{sc: sc, clause: "field list"}); err != nil {
				return myCols{}, nil, err
			}
		}
	}
	return w.outputs(sel, sc), sc, nil
}

// setOp checks a set operation, whose ORDER BY reads the names of its
// first query's columns only.
func (w *myWalk) setOp(sel *sqlir.SelectStmt, outer *myScope) (myCols, *myScope, error) {
	cols, _, err := w.query(sel.Larg, outer)
	if err != nil {
		return myCols{}, nil, err
	}
	if _, _, err := w.query(sel.Rarg, outer); err != nil {
		return myCols{}, nil, err
	}
	for _, k := range sel.OrderBy {
		if len(sqlir.Selects(k.Expr)) > 0 {
			return myCols{}, nil, w.x.unsupported("a subquery in the ORDER BY of a set operation")
		}
		for _, r := range sqlir.ColumnRefs(k.Expr) {
			switch {
			case r.Table != "":
				// MySQL fails it with 1250 rather than 1054.
				return myCols{}, nil, w.x.unsupported("a qualified column in the ORDER BY of a set operation")
			case cols.unknown || slices.ContainsFunc(cols.names, func(n string) bool { return strings.EqualFold(n, r.Column) }):
			case cols.unnamed:
				return myCols{}, nil, w.x.unsupported("an ORDER BY name of a set operation whose columns detest cannot name")
			default:
				return myCols{}, nil, w.unknownColumn(r.Column, "order clause")
			}
		}
	}
	return cols, nil, nil
}

// aliasColumn refuses a GROUP BY or HAVING name the parser read as the
// select list's alias while a FROM item has a column of the name, which
// MySQL reads instead.
func (w *myWalk) aliasColumn(sc *myScope, e sqlir.Expr, alias string) error {
	if r, ok := e.(*sqlir.ColumnRef); ok && strings.EqualFold(r.Column, alias) {
		return nil
	}
	for _, it := range sc.items {
		if slices.ContainsFunc(it.cols.names, func(n string) bool { return strings.EqualFold(n, alias) }) {
			return w.x.unsupported(fmt.Sprintf("GROUP BY or HAVING name %q, which is both a select list alias and a column", alias))
		}
	}
	return nil
}

// orderKey checks an ORDER BY key of a query block, where a bare name reads
// the select list before the FROM items.
func (w *myWalk) orderKey(sel *sqlir.SelectStmt, sc *myScope, e sqlir.Expr) error {
	if r, ok := e.(*sqlir.ColumnRef); ok && r.Table == "" {
		unnamed := false
		for _, t := range sel.Targets {
			name := t.Alias
			if c, ok := t.Expr.(*sqlir.ColumnRef); ok && name == "" {
				name = c.Column
			}
			if t.Expr != nil && name == "" {
				unnamed = true
			}
			if name != "" && strings.EqualFold(name, r.Column) {
				return nil
			}
		}
		err := w.expr(e, myCtx{sc: sc, clause: "order clause"})
		if unnamed && errors.Is(err, sqlir.ErrUndefinedColumn) {
			// MySQL names the item by its text, which ORDER BY may name.
			return w.x.unsupported("an ORDER BY name beside a select list item detest cannot name")
		}
		return err
	}
	return w.expr(e, myCtx{sc: sc, clause: "order clause", aliases: sc.aliases})
}

// owner is the alias of the item of the block sc a reference resolves to,
// false when it resolves to none of them.
func (w *myWalk) owner(r *sqlir.ColumnRef, sc *myScope) (string, bool) {
	var found []string
	for _, it := range sc.items {
		if r.Table != "" && it.alias != r.Table {
			continue
		}
		if slices.Contains(it.cols.names, r.Column) {
			found = append(found, it.alias)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// outputs are the names of the columns of the block sel, in scope sc.
func (w *myWalk) outputs(sel *sqlir.SelectStmt, sc *myScope) myCols {
	var out myCols
	for _, t := range sel.Targets {
		switch {
		case t.Star:
			for _, it := range sc.items {
				if t.Table != "" && it.alias != t.Table {
					continue
				}
				out.names = append(out.names, it.cols.names...)
				out.unnamed = out.unnamed || it.cols.unnamed
				out.unknown = out.unknown || it.cols.unknown
			}
		case t.Alias != "":
			out.names = append(out.names, t.Alias)
		default:
			if r, ok := t.Expr.(*sqlir.ColumnRef); ok {
				out.names = append(out.names, r.Column)
			} else {
				out.unnamed = true
			}
		}
	}
	return out
}

// myCtx is where an expression is walked.
type myCtx struct {
	sc     *myScope
	clause string
	// aliases are the select list aliases a bare name in the expression
	// may read on MySQL, where detest would read a column or nothing.
	aliases []string
	// block and substituted are set in HAVING: the scope of the block for
	// an aggregate's arguments, and the select list's expressions the
	// parser put for an alias.
	block       *myScope
	substituted map[sqlir.Expr]string
}

// expr checks the names e reads, its subqueries' included.
func (w *myWalk) expr(e sqlir.Expr, ctx myCtx) error {
	var err error
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		if err != nil {
			return
		}
		switch v.Kind() {
		case reflect.Interface:
			if v.IsNil() {
				return
			}
			if x, ok := reflect.TypeAssert[sqlir.Expr](v); ok && ctx.substituted != nil {
				if alias, ok := ctx.substituted[x]; ok {
					err = w.aliasColumn(ctx.block, x, alias)
					return
				}
			}
			walk(v.Elem())
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			switch n := v.Interface().(type) {
			case *sqlir.SelectStmt:
				_, _, err = w.query(n, ctx.sc)
				return
			case *sqlir.ColumnRef:
				if n.Table == "" && slices.ContainsFunc(ctx.aliases, func(a string) bool { return strings.EqualFold(a, n.Column) }) {
					// MySQL may read the alias here, which detest does
					// not evaluate.
					err = w.x.unsupported(fmt.Sprintf("select list alias %q inside an expression in %s", n.Column, ctx.clause))
					return
				}
				err = w.ref(n, ctx.sc, ctx.clause)
				return
			case *sqlir.FuncCall:
				if ctx.block != nil && isAggregateCall(n) {
					inner := ctx
					inner.sc, inner.block = ctx.block, nil
					err = w.expr(&sqlir.RowExpr{Items: n.Args}, inner)
					return
				}
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
	walk(reflect.ValueOf(&e).Elem())
	return err
}

// ref resolves a column reference in sc, innermost block first.
func (w *myWalk) ref(r *sqlir.ColumnRef, sc *myScope, clause string) error {
	name := r.Column
	if r.Table != "" {
		name = r.Table + "." + r.Column
	}
	missed := false // an item of the qualifier lacks the column
	for s := sc; s != nil; s = s.outer {
		var hits []myItem
		folded, unnamed := false, false
		for _, it := range s.items {
			if r.Table != "" && it.alias != r.Table || r.Table == "" && it.qualified {
				continue
			}
			if it.cols.unknown {
				return nil
			}
			n := 0
			for _, c := range it.cols.names {
				if c == r.Column {
					n++
				} else if strings.EqualFold(c, r.Column) {
					folded = true
				}
			}
			if n > 1 {
				return w.x.unsupported(fmt.Sprintf("column %q, which the query it comes from gives twice", r.Column))
			}
			if n == 1 {
				hits = append(hits, it)
			} else {
				unnamed = unnamed || it.cols.unnamed
				missed = missed || r.Table != ""
			}
		}
		switch {
		case len(hits) > 1:
			return w.ambiguous(name, clause)
		case len(hits) == 1:
			it := hits[0]
			if missed && r.Table != "" && s != sc {
				// MySQL looks past an inner item of the name that lacks
				// the column, where detest reads the inner item's NULL.
				return w.x.unsupported(fmt.Sprintf("column %s of an outer item whose name an inner item shadows", name))
			}
			if it.refuse != "" {
				return w.x.unsupported(it.refuse)
			}
			if s.having != nil && !s.having.star && !s.having.cols[[2]string{it.alias, r.Column}] {
				return w.unknownColumn(name, "having clause")
			}
			return nil
		case folded:
			// The executor keys the column by the name its query gives it.
			return w.x.unsupported(fmt.Sprintf("column %s named in another case than its query gives it", name))
		case unnamed:
			return w.x.unsupported(fmt.Sprintf("column %s beside a column detest cannot name", name))
		}
	}
	if r.Table == "" {
		for s := sc.outer; s != nil; s = s.outer {
			if slices.ContainsFunc(s.aliases, func(a string) bool { return strings.EqualFold(a, r.Column) }) {
				// MySQL reads an outer select list alias in some clauses
				// of a subquery and fails it with 1054 in others.
				return w.x.unsupported(fmt.Sprintf("subquery reading the outer select list alias %q", r.Column))
			}
		}
	}
	return w.unknownColumn(name, clause)
}

// target is the item of the table an INSERT, UPDATE or DELETE writes.
func (w *myWalk) target(table, alias string) myItem {
	db := w.x.tx.db
	it := myItem{alias: itemAlias(sqlir.TableRef{Name: table, Alias: alias}), cols: myCols{unknown: true}}
	if def := db.defs[db.resolve(table)]; def != nil {
		it.cols = myCols{names: def.columns}
	}
	return it
}

// setColumn checks the column an assignment or an INSERT's column list
// names, which is the target's; others are items the statement joins.
func (w *myWalk) setColumn(col string, target myItem, others []myItem) error {
	if target.cols.unknown {
		return nil
	}
	in := func(it myItem) bool { return slices.Contains(it.cols.names, col) }
	switch {
	case in(target):
		if slices.ContainsFunc(others, in) {
			return w.ambiguous(col, "field list")
		}
		return nil
	case slices.ContainsFunc(others, func(it myItem) bool { return in(it) || it.cols.unknown }):
		// MySQL writes the joined table's column, detest the target's.
		return w.x.unsupported(fmt.Sprintf("SET of column %q of a joined table", col))
	}
	return w.unknownColumn(col, "field list")
}

func (w *myWalk) insert(ins *sqlir.InsertStmt) error {
	t := w.target(ins.Table, ins.Alias)
	for _, c := range ins.Columns {
		if err := w.setColumn(c, t, nil); err != nil {
			return err
		}
	}
	// MySQL reads the row being built, detest nothing.
	row := t
	row.refuse = "a column of the row INSERT ... VALUES builds"
	vs := &myScope{items: []myItem{row}}
	for _, r := range ins.Rows {
		for _, e := range r {
			if err := w.expr(e, myCtx{sc: vs, clause: "field list"}); err != nil {
				return err
			}
		}
	}
	var selected *myScope
	if ins.Select != nil {
		_, sc, err := w.query(ins.Select, nil)
		if err != nil {
			return err
		}
		selected = sc
		if sc == nil {
			// A set operation, whose columns ON DUPLICATE KEY UPDATE
			// reads by names this walk does not follow.
			selected = &myScope{items: []myItem{{cols: myCols{unnamed: true}}}}
		}
	}
	oc := ins.OnConflict
	if oc == nil || oc.DoNothing {
		return nil
	}
	excluded := t
	excluded.alias, excluded.qualified = "excluded", true
	sc := &myScope{items: []myItem{t, excluded}}
	if selected != nil {
		// MySQL reads the SELECT's columns too, detest none of them.
		for _, it := range selected.items {
			it.refuse = "a column of the SELECT in INSERT ... SELECT ... ON DUPLICATE KEY UPDATE"
			sc.items = append(sc.items, it)
		}
	}
	for _, a := range oc.Set {
		if err := w.setColumn(a.Column, t, nil); err != nil {
			return err
		}
		if err := w.expr(a.Value, myCtx{sc: sc, clause: "field list"}); err != nil {
			if selected != nil && errors.Is(err, sqlir.ErrAmbiguousColumn) {
				return w.x.unsupported("a column of the SELECT in INSERT ... SELECT ... ON DUPLICATE KEY UPDATE")
			}
			return err
		}
	}
	return nil
}

// joined is the scope of a write's target and the items it joins.
func (w *myWalk) joined(target myItem, refs []sqlir.TableRef) (*myScope, error) {
	sc := &myScope{items: []myItem{target}}
	for _, t := range refs {
		it, err := w.item(t, nil)
		if err != nil {
			return nil, err
		}
		sc.items = append(sc.items, it)
	}
	return sc, nil
}

func (w *myWalk) update(up *sqlir.UpdateStmt) error {
	t := w.target(up.Table, up.Alias)
	sc, err := w.joined(t, up.From)
	if err != nil {
		return err
	}
	for _, a := range up.Set {
		others := sc.items[1:]
		if a.Qualified {
			// The parser refuses a qualifier other than the target's.
			others = nil
		}
		if err := w.setColumn(a.Column, t, others); err != nil {
			return err
		}
		if err := w.expr(a.Value, myCtx{sc: sc, clause: "field list"}); err != nil {
			return err
		}
	}
	return w.writeRest(sc, up.Where, up.OrderBy, up.Limit)
}

func (w *myWalk) delete(del *sqlir.DeleteStmt) error {
	sc, err := w.joined(w.target(del.Table, del.Alias), del.Using)
	if err != nil {
		return err
	}
	return w.writeRest(sc, del.Where, del.OrderBy, del.Limit)
}

func (w *myWalk) writeRest(sc *myScope, where sqlir.Expr, order []sqlir.OrderKey, limit sqlir.Expr) error {
	if where != nil {
		if err := w.expr(where, myCtx{sc: sc, clause: "where clause"}); err != nil {
			return err
		}
	}
	for _, k := range order {
		if err := w.expr(k.Expr, myCtx{sc: sc, clause: "order clause"}); err != nil {
			return err
		}
	}
	if limit != nil {
		return w.expr(limit, myCtx{sc: sc, clause: "field list"})
	}
	return nil
}
