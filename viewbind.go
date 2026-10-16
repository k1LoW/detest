package detest

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// viewBinding is what binding a view's query at CREATE VIEW left undone.
type viewBinding struct {
	// loose holds the names of the column references the query left
	// unbound, and "*" for a * left unexpanded, as a later column of such
	// a name in a relation the view reads may change what they read.
	loose map[string]bool
	// stale says why a change made after the view was created changed
	// what its query means in detest but not in the server.
	stale string
}

// viewScope is the FROM items of one query and the scopes around it.
type viewScope struct {
	items []viewItem
	outer *viewScope
}

type viewItem struct {
	alias string
	table string // the relation a table or view item reads, "" for others
	cols  []string
	ref   *sqlir.TableRef
}

// viewWalk walks a view's query with the FROM items in scope at each
// point. Binding expands each * into the columns it covers, names each
// select list item that is a bare column after it, and qualifies each
// column reference with the item it reads, as the server resolves the
// query once when it creates the view. The other modes follow a rename,
// as Postgres keeps a view bound to the relation and the column.
type viewWalk struct {
	db        *DB
	bind      bool
	renameCol [3]string // table, old name, new name
	renamed   bool      // a reference was renamed
	renameRel [2]string // relation, new name
	loose     map[string]bool
}

// bindView returns the query of a view bound as the server binds it when
// it creates the view, and whether anything was left unbound. The parsed
// statement is shared by every database that runs it, so the query is
// copied first.
func (db *DB) bindView(sel *sqlir.SelectStmt) (*sqlir.SelectStmt, *viewBinding) {
	if sel == nil {
		return nil, &viewBinding{}
	}
	sub, _ := sqlir.CloneExpr(&sqlir.SubQuery{Select: sel}).(*sqlir.SubQuery)
	sel = sub.Select
	w := &viewWalk{db: db, bind: true, loose: map[string]bool{}}
	w.query(sel, nil, nil)
	return sel, &viewBinding{loose: w.loose}
}

// viewColumnsChanged follows a change to the columns of table, the
// columns named added or renamed from old to nw, into the views that read
// it, "*" among names standing for any name. A view that left a reference
// of one of the names unbound is marked stale instead, as whether the
// change reaches it is not known.
func (db *DB) viewColumnsChanged(table string, names []string, old, nw string) {
	for name, v := range db.views {
		b := db.viewBinds[v]
		if v.View == nil || b == nil || b.stale != "" || !db.viewReads(v.View, table) {
			continue
		}
		if b.loose["*"] || (slices.Contains(names, "*") && len(b.loose) > 0) || slices.ContainsFunc(slices.Concat(names, []string{old, nw}), func(n string) bool { return n != "" && b.loose[n] }) {
			b.stale = fmt.Sprintf("view %q, which leaves a column unbound, after a change to the columns of %q", relname(name), relname(table))
			continue
		}
		switch {
		case old == "":
		case db.kind.InnoDB():
			// MySQL keeps a view's query by the names it reads, so the view
			// fails once a column it reads is renamed, which detest, having
			// no check of MySQL's column names, would read as NULL.
			// The rename walk tells which references read the column, on a
			// copy the view does not keep.
			sub, _ := sqlir.CloneExpr(&sqlir.SubQuery{Select: v.View}).(*sqlir.SubQuery)
			w := &viewWalk{db: db, renameCol: [3]string{table, old, nw}}
			if w.query(sub.Select, nil, nil); w.renamed {
				b.stale = fmt.Sprintf("view %q after a rename of column %q of %q it reads", relname(name), old, relname(table))
			}
		default:
			(&viewWalk{db: db, renameCol: [3]string{table, old, nw}}).query(v.View, nil, nil)
		}
	}
}

// viewRelationRenamed follows a rename of a table or view into the views
// that read it.
//
// MySQL keeps a view's query by the names it reads, so a rename leaves the
// view reading a relation that is gone there too.
func (db *DB) viewRelationRenamed(table, to string) {
	if db.kind.InnoDB() {
		return
	}
	for _, v := range db.views {
		if v.View != nil && db.viewReads(v.View, table) {
			(&viewWalk{db: db, renameRel: [2]string{table, to}}).query(v.View, nil, nil)
		}
	}
}

// staleView is why a statement opening name cannot read it, a view made
// stale or one reading such a view, or "". The column check finds it on
// Postgres, which MySQL does not run.
func (db *DB) staleView(name string, seen map[string]bool) string {
	v := db.views[name]
	if v == nil || seen[name] {
		return ""
	}
	seen[name] = true
	if b := db.viewBinds[v]; b != nil && b.stale != "" {
		return b.stale
	}
	for _, n := range openedTables(v.View) {
		if why := db.staleView(db.resolve(n), seen); why != "" {
			return why
		}
	}
	return ""
}

func (db *DB) viewReads(sel *sqlir.SelectStmt, table string) bool {
	return slices.ContainsFunc(sqlir.TableNames(sel), func(n string) bool { return db.resolve(n) == table })
}

// query walks sel, whose correlated references reach outer, and returns
// the names of its columns, nil where they are not known.
func (w *viewWalk) query(sel *sqlir.SelectStmt, outer *viewScope, ctes []map[string][]string) []string {
	if sel == nil {
		return nil
	}
	if len(sel.With) > 0 {
		m := map[string][]string{}
		ctes = append(slices.Clip(ctes), m)
		for _, cte := range sel.With {
			m[cte.Name] = w.query(cte.Select, outer, ctes)
		}
	}
	if sel.SetOp != "" {
		// The keys of ORDER BY can only name the output columns.
		names := w.query(sel.Larg, outer, ctes)
		w.query(sel.Rarg, outer, ctes)
		w.trailing(sel, names, &viewScope{outer: outer}, ctes)
		return names
	}
	if len(sel.Values) > 0 {
		sc := &viewScope{outer: outer}
		w.exprs(sel.Values, sc, ctes)
		var names []string
		for i := range sel.Values[0] {
			names = append(names, fmt.Sprintf("column%d", i+1))
		}
		w.trailing(sel, names, sc, ctes)
		return names
	}
	sc := &viewScope{outer: outer}
	if sel.From != nil {
		w.item(sel.From, sc, outer, ctes)
	}
	for i := range sel.Joins {
		w.item(&sel.Joins[i].Table, sc, outer, ctes)
		w.exprs(sel.Joins[i].On, sc, ctes)
	}
	if w.bind {
		sel.Targets = w.expandStars(sel.Targets, sc)
	}
	var names []string
	for i, t := range sel.Targets {
		if t.Star {
			names = nil
			break
		}
		if w.bind && t.Alias == "" && t.Name == "" {
			if r, ok := t.Expr.(*sqlir.ColumnRef); ok {
				sel.Targets[i].Alias = r.Column
			}
		}
		names = append(names, targetName(sel.Targets[i]))
	}
	for _, t := range sel.Targets {
		w.exprs(t.Expr, sc, ctes)
	}
	w.exprs(sel.Where, sc, ctes)
	for _, e := range sel.GroupBy {
		w.groupKey(e, names, sc, ctes)
	}
	w.exprs(sel.Having, sc, ctes)
	// A bare name in ORDER BY or DISTINCT ON is an output column first.
	for _, e := range sel.DistinctOn {
		w.key(e, names, sc, ctes)
	}
	w.trailing(sel, names, sc, ctes)
	return names
}

// trailing walks the ORDER BY, LIMIT and OFFSET of sel, which a set
// operation and VALUES have too.
func (w *viewWalk) trailing(sel *sqlir.SelectStmt, names []string, sc *viewScope, ctes []map[string][]string) {
	for _, k := range sel.OrderBy {
		w.key(k.Expr, names, sc, ctes)
	}
	w.exprs(sel.Limit, sc, ctes)
	w.exprs(sel.Offset, sc, ctes)
}

func (w *viewWalk) key(e sqlir.Expr, names []string, sc *viewScope, ctes []map[string][]string) {
	if r, ok := e.(*sqlir.ColumnRef); ok && r.Table == "" && (names == nil || slices.Contains(names, r.Column)) {
		if names == nil && w.bind {
			w.loose[r.Column] = true
		}
		return
	}
	w.exprs(e, sc, ctes)
}

// groupKey walks a GROUP BY key. Postgres takes a bare name for a column
// of the query's own items first, then for an output column, and only
// then for a column of a query around it.
func (w *viewWalk) groupKey(e sqlir.Expr, names []string, sc *viewScope, ctes []map[string][]string) {
	r, ok := e.(*sqlir.ColumnRef)
	if !ok || r.Table != "" || !w.bind {
		w.exprs(e, sc, ctes)
		return
	}
	local := false
	for _, it := range sc.items {
		if it.cols == nil || slices.Contains(it.cols, r.Column) {
			local = true
		}
	}
	if !local && (names == nil || slices.Contains(names, r.Column)) {
		w.loose[r.Column] = true
		return
	}
	w.qualify(r, sc)
}

// item adds a FROM item to sc.
func (w *viewWalk) item(ref *sqlir.TableRef, sc, outer *viewScope, ctes []map[string][]string) {
	alias := ref.Alias
	if alias == "" && ref.Name != "" {
		alias = relname(ref.Name)
	}
	var cols []string
	table := ""
	switch {
	case ref.Sub != nil:
		o := outer
		if ref.Lateral {
			o = sc
		}
		cols = w.query(ref.Sub, o, ctes)
	case ref.Func != nil:
		w.exprs(ref.Func, sc, ctes)
		if name := funcColumn(*ref, alias); name != "" {
			cols = []string{name}
		}
	default:
		if c, ok := cteColumns(ctes, ref.Name); ok {
			cols = slices.Clone(c)
			break
		}
		db := w.db
		table = db.resolve(ref.Name)
		if rel := w.renameRel; rel[0] != "" && table == rel[0] {
			// The qualifiers of the query name the item as before.
			ref.Alias = alias
			ref.Name = rel[1]
			table = rel[1]
		}
		if w.bind && (db.views[table] != nil || db.defs[table] != nil) {
			// The server keeps the relation, not the name, so neither a
			// later relation of the name earlier in the search path nor a
			// rename resolves it elsewhere.
			ref.Alias = alias
			ref.Name = table
		}
		if v := db.views[table]; v != nil {
			if v.View != nil {
				cols = outputNames(v.View)
			}
			if cols != nil && len(v.ViewColumns) > 0 {
				cols = aliasColumns(cols, v.ViewColumns)
			}
		} else if def := db.defs[table]; def != nil {
			cols = slices.Clone(def.columns)
		}
	}
	if cols != nil && len(ref.Columns) > 0 {
		cols = aliasColumns(cols, ref.Columns)
	}
	if alias == "" {
		// A reference cannot name the item, so none is qualified with it.
		cols = nil
	}
	sc.items = append(sc.items, viewItem{alias: alias, table: table, cols: cols, ref: ref})
}

func cteColumns(ctes []map[string][]string, name string) ([]string, bool) {
	if strings.Contains(name, ".") {
		return nil, false
	}
	for _, m := range slices.Backward(ctes) {
		if c, ok := m[name]; ok {
			return c, true
		}
	}
	return nil, false
}

// aliasColumns gives the first columns of cols the names of list, nil
// where the list is longer, which the server refuses.
func aliasColumns(cols, list []string) []string {
	if len(list) > len(cols) {
		return nil
	}
	out := slices.Clone(cols)
	copy(out, list)
	return out
}

// expandStars replaces each * of targets with the columns it covers,
// leaving one over an item whose columns are not known.
func (w *viewWalk) expandStars(targets []sqlir.Target, sc *viewScope) []sqlir.Target {
	var out []sqlir.Target
	for _, t := range targets {
		if !t.Star {
			out = append(out, t)
			continue
		}
		var items []viewItem
		for _, it := range sc.items {
			if t.Table == "" || it.alias == t.Table {
				items = append(items, it)
			}
		}
		// An item that gives one name twice is read by position, which a
		// reference by name cannot tell apart.
		if len(items) == 0 || slices.ContainsFunc(items, func(it viewItem) bool { return it.cols == nil || hasDuplicate(it.cols) }) {
			w.loose["*"] = true
			out = append(out, t)
			continue
		}
		for _, it := range items {
			for _, c := range it.cols {
				out = append(out, sqlir.Target{Expr: &sqlir.ColumnRef{Table: it.alias, Column: c}, Alias: c})
			}
		}
	}
	return out
}

// exprs walks the expressions under n, each subquery as a query whose
// correlated references reach sc.
func (w *viewWalk) exprs(n any, sc *viewScope, ctes []map[string][]string) {
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			if s, ok := reflect.TypeAssert[*sqlir.SelectStmt](v); ok {
				w.query(s, sc, ctes)
				return
			}
			if r, ok := reflect.TypeAssert[*sqlir.ColumnRef](v); ok {
				w.ref(r, sc)
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
}

func (w *viewWalk) ref(r *sqlir.ColumnRef, sc *viewScope) {
	if w.bind {
		if r.Table == "" {
			w.qualify(r, sc)
		}
		return
	}
	if col := w.renameCol; col[0] != "" && r.Table != "" && r.Column == col[1] {
		it := innermostItem(sc, r.Table)
		if it == nil || it.table != col[0] {
			return
		}
		// A column the item's alias list renames is read by that name.
		if def := w.db.defs[col[0]]; def != nil && slices.Index(def.columns, col[2]) >= len(it.ref.Columns) {
			r.Column = col[2]
			w.renamed = true
		}
	}
}

// qualify names the item an unqualified reference reads, the innermost
// scope's item that has the column, and marks the walk loose where it
// cannot tell which.
func (w *viewWalk) qualify(r *sqlir.ColumnRef, sc *viewScope) {
	for s := sc; s != nil; s = s.outer {
		var hit *viewItem
		n, unknown := 0, false
		for i, it := range s.items {
			switch {
			case it.cols == nil:
				unknown = true
			case slices.Contains(it.cols, r.Column):
				n++
				hit = &s.items[i]
			}
		}
		if n == 1 && !unknown && innermostItem(sc, hit.alias) == hit {
			r.Table = hit.alias
			return
		}
		if n > 0 || unknown {
			break
		}
	}
	// Left as written: a whole-row reference, an output column, or one
	// the server would not have resolved.
	w.loose[r.Column] = true
}

// innermostItem is the item a qualifier names from sc, nil where none
// does or two of a scope do.
func innermostItem(sc *viewScope, alias string) *viewItem {
	for s := sc; s != nil; s = s.outer {
		var hit *viewItem
		for i, it := range s.items {
			if it.alias == alias {
				if hit != nil {
					return nil
				}
				hit = &s.items[i]
			}
		}
		if hit != nil {
			return hit
		}
	}
	return nil
}

func hasDuplicate(names []string) bool {
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			return true
		}
		seen[n] = true
	}
	return false
}
