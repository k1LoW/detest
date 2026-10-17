package detest

import (
	"cmp"
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
	// unnamed is set when a column is an expression without an alias,
	// which MySQL names by its text and so never by a plain identifier.
	unnamed bool
	// unread are the names MySQL gives columns detest names otherwise, such
	// as a string literal's, named by its value.
	unread []string
	// literals are the names MySQL gives literals without an alias, by
	// their value, which are among unread too.
	literals []string
	// exprs are the select list's expressions without an alias other than
	// strings, NULL and booleans, which MySQL names by their spelling.
	exprs []sqlir.Expr
	// unknown is set for an item whose columns detest does not know, such
	// as an ignored table, which resolves any name.
	unknown bool
}

// mayHave reports whether MySQL may give a column of c the name, which
// detest would not read.
func (c myCols) mayHave(name string) bool {
	if slices.ContainsFunc(c.unread, func(n string) bool { return strings.EqualFold(n, name) }) {
		return true
	}
	return c.unnamed && !identLike(name)
}

// identLike reports whether name is a plain identifier, which the text of
// an expression never is.
func identLike(name string) bool {
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_', r == '$':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return name != ""
}

// niladic are the functions MySQL calls without parentheses, which name a
// select list item by an identifier.
var niladic = []string{"current_timestamp", "current_date", "current_time", "current_user", "localtime", "localtimestamp", "utc_date", "utc_time", "utc_timestamp"}

// literalName is the name MySQL gives a literal in a select list.
func literalName(e sqlir.Expr) (string, bool) {
	c, ok := e.(*sqlir.Const)
	if !ok {
		return "", false
	}
	switch v := c.Value.(type) {
	case nil:
		return "NULL", true
	case bool:
		return strings.ToUpper(fmt.Sprint(v)), true
	case string:
		return v, true
	}
	// A number is named by its spelling, which the value does not keep,
	// and which a plain identifier never matches.
	return "", false
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
	ctes [][]*myCTE
	// views are the views being named, against a view that reads itself.
	views map[string]bool
}

// checkMySQLColumns resolves the column names of a MySQL statement.
func (x *sqlExec) checkMySQLColumns(stmt sqlir.Statement) error {
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

// myCTE is a CTE of a WITH, whose query is checked the first time the
// statement refers to it: MySQL leaves a CTE nothing refers to unresolved.
type myCTE struct {
	name  string
	sel   *sqlir.SelectStmt
	outer *myScope
	done  bool
	cols  myCols
	err   error
}

// cteCols are the columns of the i-th CTE of the level-th WITH in scope,
// whose query sees the CTEs before it only.
func (w *myWalk) cteCols(level, i int) (myCols, error) {
	e := w.ctes[level][i]
	if !e.done {
		e.done = true
		saved := w.ctes
		w.ctes = append(slices.Clone(saved[:level]), saved[level][:i])
		e.cols, _, e.err = w.query(e.sel, e.outer)
		w.ctes = saved
	}
	return e.cols, e.err
}

// tableCols are the columns of the table, view or CTE name refers to.
func (w *myWalk) tableCols(name string) (myCols, error) {
	for level, ctes := range slices.Backward(w.ctes) {
		for i, e := range slices.Backward(ctes) {
			if e.name == name {
				return w.cteCols(level, i)
			}
		}
	}
	db := w.x.tx.db
	table := db.resolve(name)
	if def := db.defs[table]; def != nil {
		return myCols{names: def.columns}, nil
	}
	if v := db.views[table]; v != nil && !w.views[table] {
		w.views[table] = true
		defer delete(w.views, table)
		// A read of the view runs its query, which is checked as the
		// statement's own.
		saved := w.ctes
		w.ctes = nil
		cols, _, err := w.query(v.View, nil)
		w.ctes = saved
		if err != nil {
			return myCols{}, err
		}
		if len(v.ViewColumns) > 0 {
			return myCols{names: v.ViewColumns}, nil
		}
		return cols, nil
	}
	return myCols{unknown: true}, nil
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
		cols, err := w.tableCols(t.Name)
		if err != nil {
			return it, err
		}
		it.cols = cols
	}
	if len(t.Columns) > 0 {
		it.cols = myCols{names: t.Columns}
	}
	named := append(slices.Clone(it.cols.names), it.cols.literals...)
	for i, n := range named {
		if slices.ContainsFunc(named[i+1:], func(m string) bool { return strings.EqualFold(m, n) }) {
			// MySQL fails it with 1060 before it runs the statement.
			return it, w.x.unsupported(fmt.Sprintf("FROM item %s with two columns named %q", it.alias, n))
		}
	}
	if len(t.Columns) == 0 {
		for i, e := range it.cols.exprs {
			if slices.ContainsFunc(it.cols.exprs[i+1:], func(f sqlir.Expr) bool { return reflect.DeepEqual(e, f) }) {
				// MySQL fails it with 1060 when the two are spelled alike,
				// which the parser does not keep.
				return it, w.x.unsupported(fmt.Sprintf("FROM item %s with an expression twice without an alias", it.alias))
			}
		}
		for _, n := range it.cols.names {
			if slices.ContainsFunc(it.cols.unread, func(m string) bool { return strings.EqualFold(m, n) }) {
				// Another column may have the name on MySQL, which fails
				// it with 1060.
				return it, w.x.unsupported(fmt.Sprintf("FROM item %s with the column name %q, which an expression's column may have", it.alias, n))
			}
		}
		if len(it.cols.exprs) > 0 {
			for _, n := range it.cols.names {
				if !identLike(strings.ToLower(n)) {
					// The name may be an expression's spelling, which
					// MySQL fails with 1060.
					return it, w.x.unsupported(fmt.Sprintf("FROM item %s with the column name %q beside an expression without an alias", it.alias, n))
				}
			}
		}
	}
	return it, nil
}

// query checks sel, whose correlated references reach outer, and returns
// the names of its columns and the scope of its block, nil for a set
// operation.
func (w *myWalk) query(sel *sqlir.SelectStmt, outer *myScope) (myCols, *myScope, error) {
	if len(sel.With) > 0 {
		var level []*myCTE
		for i, cte := range sel.With {
			if slices.ContainsFunc(sel.With[:i], func(o sqlir.CTE) bool { return strings.EqualFold(o.Name, cte.Name) }) {
				// MySQL fails it with 1066, and detest reads the last.
				return myCols{}, nil, w.x.unsupported(fmt.Sprintf("two CTEs named %s in one WITH", cte.Name))
			}
			level = append(level, &myCTE{name: cte.Name, sel: cte.Select, outer: outer})
		}
		w.ctes = append(w.ctes, level)
		defer func() { w.ctes = w.ctes[:len(w.ctes)-1] }()
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
		var out myCols
		for i := range sel.Values[0] {
			out.unread = append(out.unread, fmt.Sprintf("column_%d", i), fmt.Sprintf("column%d", i+1))
		}
		return out, nil, nil
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
			if err := w.add(sc, it); err != nil {
				return myCols{}, nil, err
			}
			if i > 0 && sel.Joins[i-1].On != nil {
				// ON sees the items joined so far only.
				on := &myScope{items: slices.Clone(sc.items), outer: outer}
				if err := w.expr(sel.Joins[i-1].On, myCtx{sc: on, clause: "on clause"}); err != nil {
					return myCols{}, nil, err
				}
			}
		}
	}
	for _, t := range sel.Targets {
		if t.Star && t.Table != "" && !slices.ContainsFunc(sc.items, func(it myItem) bool { return it.alias == t.Table }) {
			// MySQL fails it with 1051, and detest gives no columns.
			return myCols{}, nil, w.x.unsupported(fmt.Sprintf("%s.* of a table not in FROM", t.Table))
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
	var grouped [][2]string
	for _, g := range sel.GroupBy {
		if alias, ok := substituted[g]; ok {
			// The parser put the select list's expression for its alias,
			// which the select list's walk checked.
			if err := w.aliasColumn(sc, g, alias); err != nil {
				return myCols{}, nil, err
			}
		} else {
			if r, ok := g.(*sqlir.ColumnRef); ok && r.Table == "" && w.selectsColumn(sel, r.Column) {
				if n := slices.IndexFunc(sc.items, func(it myItem) bool { return slices.Contains(it.cols.names, r.Column) }); n >= 0 &&
					slices.ContainsFunc(sc.items[n+1:], func(it myItem) bool { return slices.Contains(it.cols.names, r.Column) }) {
					// MySQL groups by the select list's column, where
					// detest groups by the first item's.
					return myCols{}, nil, w.x.unsupported(fmt.Sprintf("GROUP BY name %q, which two FROM items have and the select list names", r.Column))
				}
			}
			if err := w.expr(g, myCtx{sc: sc, clause: "group statement", aliases: sc.aliases}); err != nil {
				return myCols{}, nil, err
			}
		}
		if r, ok := g.(*sqlir.ColumnRef); ok {
			if alias, ok := w.owner(r, sc); ok {
				having.cols[[2]string{alias, r.Column}] = true
				grouped = append(grouped, [2]string{alias, r.Column})
			}
		}
	}
	if sel.Having != nil {
		for _, t := range sel.Targets {
			if t.Star && t.Table == "" {
				having.star = true
			}
			if t.Star && t.Table != "" {
				for _, it := range sc.items {
					if it.alias != t.Table {
						continue
					}
					for _, c := range it.cols.names {
						having.cols[[2]string{it.alias, c}] = true
					}
				}
			}
			if r, ok := t.Expr.(*sqlir.ColumnRef); ok {
				if alias, ok := w.owner(r, sc); ok {
					having.cols[[2]string{alias, r.Column}] = true
				}
			}
		}
		hs := &myScope{items: sc.items, outer: sc.outer, aliases: sc.aliases, having: having}
		if err := w.expr(sel.Having, myCtx{sc: hs, block: sc, clause: "having clause", substituted: substituted, grouped: grouped}); err != nil {
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
			case cols.mayHave(r.Column):
				return myCols{}, nil, w.x.unsupported("an ORDER BY name of a set operation detest cannot match to its columns")
			case !cols.unknown && len(slices.DeleteFunc(slices.Clone(cols.names), func(n string) bool { return !strings.EqualFold(n, r.Column) })) > 1:
				return myCols{}, nil, w.ambiguous(r.Column, "order clause")
			case cols.unknown || slices.Contains(cols.names, r.Column):
			case cols.mayHave(r.Column) || slices.ContainsFunc(cols.names, func(n string) bool { return strings.EqualFold(n, r.Column) }):
				// The executor keys the column by the name the first
				// query gives it.
				return myCols{}, nil, w.x.unsupported("an ORDER BY name of a set operation detest cannot match to its columns")
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
	if len(slices.DeleteFunc(slices.Clone(sc.aliases), func(a string) bool { return !strings.EqualFold(a, alias) })) > 1 {
		// MySQL fails it with 1052 where the items differ, and the parser
		// puts one of them.
		return w.x.unsupported(fmt.Sprintf("GROUP BY or HAVING name %q, which two select list aliases give", alias))
	}
	if r, ok := e.(*sqlir.ColumnRef); ok && strings.EqualFold(r.Column, alias) {
		return nil
	}
	for _, it := range sc.items {
		if it.cols.mayHave(alias) || slices.ContainsFunc(it.cols.names, func(n string) bool { return strings.EqualFold(n, alias) }) {
			return w.x.unsupported(fmt.Sprintf("GROUP BY or HAVING name %q, which is both a select list alias and a column", alias))
		}
	}
	return nil
}

// orderKey checks an ORDER BY key of a query block, where a bare name reads
// the select list before the FROM items.
func (w *myWalk) orderKey(sel *sqlir.SelectStmt, sc *myScope, e sqlir.Expr) error {
	if r, ok := e.(*sqlir.ColumnRef); ok && r.Table == "" {
		if w.outputs(sel, sc).mayHave(r.Column) {
			// detest keys an output by a name MySQL gives it otherwise,
			// or the reverse, and may sort by another item.
			return w.x.unsupported("an ORDER BY name a select list item detest names otherwise may give")
		}
		exprs := 0
		var matched [][2]string // the columns the matching items are
		for _, t := range sel.Targets {
			if t.Star {
				for _, it := range sc.items {
					if t.Table != "" && it.alias != t.Table {
						continue
					}
					for _, c := range it.cols.names {
						if c != r.Column && strings.EqualFold(c, r.Column) {
							// The executor keys the column by the name
							// its query gives it.
							return w.x.unsupported(fmt.Sprintf("ORDER BY name %q, which a * gives in another case", r.Column))
						}
						if c == r.Column {
							matched = append(matched, [2]string{it.alias, c})
						}
					}
				}
				continue
			}
			name := t.Alias
			if c, ok := t.Expr.(*sqlir.ColumnRef); ok && name == "" {
				name = c.Column
			}
			if name == "" || !strings.EqualFold(name, r.Column) {
				continue
			}
			c, ok := t.Expr.(*sqlir.ColumnRef)
			alias := ""
			if ok {
				alias, ok = w.owner(c, sc)
			}
			if !ok {
				exprs++
				continue
			}
			matched = append(matched, [2]string{alias, c.Column})
		}
		n := len(matched) + exprs
		switch {
		case n > 1 && exprs > 0:
			// MySQL takes one of two items of the name when one is an
			// expression, by rules detest does not follow.
			return w.x.unsupported(fmt.Sprintf("ORDER BY name %q, which two select list items give", r.Column))
		case n > 1 && slices.ContainsFunc(matched, func(m [2]string) bool { return m != matched[0] }):
			return w.ambiguous(r.Column, "order clause")
		case n > 0:
			return nil
		}
		return w.expr(e, myCtx{sc: sc, clause: "order clause"})
	}
	return w.expr(e, myCtx{sc: sc, clause: "order clause", aliases: sc.aliases})
}

// selectsColumn reports whether a select list item of sel is a column of
// the name.
func (w *myWalk) selectsColumn(sel *sqlir.SelectStmt, name string) bool {
	return slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool {
		c, ok := t.Expr.(*sqlir.ColumnRef)
		return ok && c.Column == name
	})
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
				out.unread = append(out.unread, it.cols.unread...)
				out.literals = append(out.literals, it.cols.literals...)
				out.exprs = append(out.exprs, it.cols.exprs...)
				out.unnamed = out.unnamed || it.cols.unnamed
				out.unknown = out.unknown || it.cols.unknown
			}
		case t.Alias != "":
			out.names = append(out.names, t.Alias)
		default:
			if r, ok := t.Expr.(*sqlir.ColumnRef); ok {
				out.names = append(out.names, r.Column)
			} else {
				// detest names the column by targetName, which MySQL
				// does not, so neither name reads it the same on both.
				out.unread = append(out.unread, targetName(t))
				if f, ok := t.Expr.(*sqlir.FuncCall); ok && len(f.Args) == 0 {
					// MySQL names CURRENT_TIMESTAMP and the like as
					// written, which the parser folds into another call.
					out.unread = append(out.unread, niladic...)
				}
				if c, ok := t.Expr.(*sqlir.Const); ok {
					if v, ok := c.Value.(int64); ok {
						// Only for the duplicate check: an integer's
						// spelling is never a plain identifier.
						out.literals = append(out.literals, fmt.Sprint(v))
						// TRUE and FALSE convert to 1 and 0, and MySQL
						// names them as written.
						switch v {
						case 1:
							out.unread = append(out.unread, "TRUE")
						case 0:
							out.unread = append(out.unread, "FALSE")
						}
					}
				}
				if name, ok := literalName(t.Expr); ok {
					out.unread = append(out.unread, name)
					out.literals = append(out.literals, name)
				} else {
					out.unnamed = true
					out.exprs = append(out.exprs, t.Expr)
				}
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
	// grouped are the GROUP BY columns of the block, by item alias and
	// column, which a HAVING name reads before the select list's alias.
	grouped [][2]string
	// skip are the expressions checked already.
	skip map[sqlir.Expr]bool
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
			if x, ok := reflect.TypeAssert[sqlir.Expr](v); ok && ctx.skip[x] {
				return
			}
			if x, ok := reflect.TypeAssert[sqlir.Expr](v); ok && ctx.substituted != nil {
				if alias, ok := ctx.substituted[x]; ok {
					// Inside an aggregate the lookup scope is the block's.
					block := cmp.Or(ctx.block, ctx.sc)
					if r, ok := x.(*sqlir.ColumnRef); ok && strings.EqualFold(r.Column, alias) {
						own, _ := w.owner(r, block)
						if slices.ContainsFunc(ctx.grouped, func(g [2]string) bool { return strings.EqualFold(g[1], alias) && g != [2]string{own, r.Column} }) {
							// MySQL reads the grouped column, where the
							// parser put the select list's.
							err = w.x.unsupported(fmt.Sprintf("HAVING name %q, which both a select list alias and another item's GROUP BY column give", alias))
							return
						}
					}
					err = w.aliasColumn(block, x, alias)
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
		folded, unknown := false, false
		refuse := ""
		for _, it := range s.items {
			if r.Table != "" && it.alias != r.Table || r.Table == "" && it.qualified {
				continue
			}
			if it.cols.unknown {
				unknown = true
				refuse = cmp.Or(refuse, it.refuse)
				continue
			}
			if it.cols.mayHave(r.Column) {
				return w.x.unsupported(fmt.Sprintf("column %s beside a column detest names otherwise than MySQL", name))
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
				missed = missed || r.Table != ""
			}
		}
		switch {
		case unknown && refuse != "" && len(hits) <= 1:
			return w.x.unsupported(refuse)
		case unknown && len(hits) <= 1:
			// An item whose columns detest does not know may have it too.
			return nil
		case len(hits) > 1:
			return w.ambiguous(name, clause)
		case len(hits) == 1 && folded:
			// MySQL takes the two names for one and fails with 1052,
			// where detest reads the one of the exact case.
			return w.x.unsupported(fmt.Sprintf("column %s beside a column of the name in another case", name))
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
		}
		if r.Table == "" && s != sc && slices.ContainsFunc(s.aliases, func(a string) bool { return strings.EqualFold(a, r.Column) }) {
			// MySQL reads an outer select list alias in some clauses of a
			// subquery, before the blocks farther out, and fails it with
			// 1054 in others.
			return w.x.unsupported(fmt.Sprintf("subquery reading the outer select list alias %q", r.Column))
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
	// MySQL matches a joined item's column in any case.
	in := func(it myItem) bool {
		return slices.ContainsFunc(it.cols.names, func(n string) bool { return strings.EqualFold(n, col) })
	}
	uncertain := func(it myItem) bool { return it.cols.unknown || it.cols.mayHave(col) }
	switch {
	case in(target):
		if slices.ContainsFunc(others, in) {
			return w.ambiguous(col, "field list")
		}
		if slices.ContainsFunc(others, uncertain) {
			return w.x.unsupported(fmt.Sprintf("SET of column %q beside a joined column detest names otherwise", col))
		}
		return nil
	case slices.ContainsFunc(others, func(it myItem) bool { return in(it) || uncertain(it) }):
		// MySQL writes the joined table's column, detest the target's.
		return w.x.unsupported(fmt.Sprintf("SET of column %q of a joined table", col))
	}
	return w.unknownColumn(col, "field list")
}

func (w *myWalk) insert(ins *sqlir.InsertStmt) error {
	t := w.target(ins.Table, ins.Alias)
	for i, c := range ins.Columns {
		if err := w.setColumn(c, t, nil); err != nil {
			return err
		}
		if slices.Contains(ins.Columns[:i], c) {
			return w.x.tx.db.kind.Error(sqlir.DuplicateColumn, fmt.Sprintf("Column '%s' specified twice", c), relname(ins.Table), c, "")
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
			selected = &myScope{items: []myItem{{cols: myCols{unknown: true}}}}
		}
	}
	oc := ins.OnConflict
	if oc == nil || oc.DoNothing {
		return nil
	}
	if t.alias == "excluded" {
		// The parser writes VALUES(col) as excluded.col, which the
		// table's own name would read too.
		return w.x.unsupported("ON DUPLICATE KEY UPDATE of a table named excluded")
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
		if a.Table != "" && a.Table != t.alias {
			return w.unknownColumn(a.Table+"."+a.Column, "field list")
		}
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
		if err := w.add(sc, it); err != nil {
			return nil, err
		}
	}
	return sc, nil
}

// add adds a FROM item to the block sc.
func (w *myWalk) add(sc *myScope, it myItem) error {
	if slices.ContainsFunc(sc.items, func(o myItem) bool { return o.alias == it.alias }) {
		// MySQL fails it with 1066, and detest's row keeps one item of the
		// name. The parser writes aliases in lower case, so two that
		// differ in case only are refused too.
		return w.x.unsupported(fmt.Sprintf("two FROM items named %s", it.alias))
	}
	sc.items = append(sc.items, it)
	return nil
}

func (w *myWalk) update(up *sqlir.UpdateStmt) error {
	t := w.target(up.Table, up.Alias)
	sc, err := w.joined(t, up.From)
	if err != nil {
		return err
	}
	for i, on := range up.JoinOn {
		if on == nil {
			continue
		}
		// ON sees the tables joined so far only.
		if err := w.expr(on, myCtx{sc: &myScope{items: sc.items[:i+2]}, clause: "on clause"}); err != nil {
			return err
		}
	}
	for _, a := range up.Set {
		others := sc.items[1:]
		if a.Table != "" {
			if a.Table != t.alias {
				// The parser refuses the name of another table, so this
				// is one an alias hides.
				return w.unknownColumn(a.Table+"."+a.Column, "field list")
			}
			others = nil
		}
		if err := w.setColumn(a.Column, t, others); err != nil {
			return err
		}
		if err := w.expr(a.Value, myCtx{sc: sc, clause: "field list"}); err != nil {
			return err
		}
	}
	// The parser joins the ON conditions to WHERE, and they resolve in
	// their own scopes above.
	skip := map[sqlir.Expr]bool{}
	for _, on := range up.JoinOn {
		if on != nil {
			skip[on] = true
		}
	}
	return w.writeRest(sc, up.Where, skip, up.OrderBy, up.Limit)
}


func (w *myWalk) delete(del *sqlir.DeleteStmt) error {
	sc, err := w.joined(w.target(del.Table, del.Alias), del.Using)
	if err != nil {
		return err
	}
	return w.writeRest(sc, del.Where, nil, del.OrderBy, del.Limit)
}

func (w *myWalk) writeRest(sc *myScope, where sqlir.Expr, skip map[sqlir.Expr]bool, order []sqlir.OrderKey, limit sqlir.Expr) error {
	if where != nil {
		if err := w.expr(where, myCtx{sc: sc, clause: "where clause", skip: skip}); err != nil {
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
