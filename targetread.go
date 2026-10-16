package detest

import (
	"fmt"
	"maps"
	"reflect"

	"github.com/k1LoW/detest/internal/sqlir"
)

// checkTargetRead fails, on MySQL, a write whose subquery reads the table it
// writes, with 1093 whatever rows it reads. MySQL materializes a derived
// table or a CTE before it reads the subquery around it, so the target read
// there is not the write's own and runs. It reads the rows of INSERT ...
// SELECT into a temporary table first, so that form is not checked at all.
// Postgres runs every such write.
func (x *sqlExec) checkTargetRead(stmt sqlir.Statement) error {
	db := x.tx.db
	if !db.kind.InnoDB() {
		return nil
	}
	var target, alias string
	var exprs []any
	var from []sqlir.TableRef
	switch st := stmt.(type) {
	case *sqlir.InsertStmt:
		if st.Select != nil {
			return nil
		}
		target, alias = st.Table, st.Alias
		exprs = []any{st.Rows, st.OnConflict}
	case *sqlir.UpdateStmt:
		target, alias, from = st.Table, st.Alias, st.From
		exprs = []any{st.Set, st.Where, st.OrderBy, st.Limit}
	case *sqlir.DeleteStmt:
		target, alias, from = st.Table, st.Alias, st.Using
		exprs = []any{st.Where, st.OrderBy, st.Limit}
	default:
		return nil
	}
	c := targetRead{x: x, table: db.resolve(target), name: alias}
	if c.name == "" {
		c.name = relname(c.table)
	}
	for _, t := range from {
		// MySQL merges a view or a derived table joined to the target when
		// its rules for merging allow, and only then does a subquery inside
		// it read the target as the write's own and fail with 1093 or 1443.
		// A materialized item runs. detest does not follow those rules.
		if body, ok := c.item(t); ok && c.mergedRead(body, map[string]bool{}) {
			return x.unsupported("a subquery that reads " + target + " in a view or derived table joined to the write")
		}
	}
	for _, n := range exprs {
		for _, q := range outerSelects(n) {
			if err := c.query(q, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

type targetRead struct {
	x     *sqlExec
	table string // the resolved target
	name  string // the name MySQL reports, the alias if there is one
}

// item returns the query a view or derived table in FROM stands for. A view
// whose query detest does not hold reads anything, so it returns nil.
func (c *targetRead) item(t sqlir.TableRef) (*sqlir.SelectStmt, bool) {
	if t.Sub != nil {
		return t.Sub, true
	}
	if t.Name == "" || t.Func != nil {
		return nil, false
	}
	v := c.x.tx.db.views[c.x.tx.db.resolve(t.Name)]
	if v == nil {
		return nil, false
	}
	return v.View, true
}

// query checks a subquery of the write: a FROM item naming the target fails
// with 1093, and a view reading the target is refused, since MySQL fails
// with 1443 or runs by whether it merges the view. ctes are the CTE names in
// scope, which hide a table of the same name.
func (c *targetRead) query(sel *sqlir.SelectStmt, ctes map[string]bool) error {
	if sel == nil {
		return nil
	}
	if len(sel.With) > 0 {
		ctes = maps.Clone(ctes)
		if ctes == nil {
			ctes = map[string]bool{}
		}
		for _, cte := range sel.With {
			ctes[cte.Name] = true
		}
	}
	if err := c.query(sel.Larg, ctes); err != nil {
		return err
	}
	if err := c.query(sel.Rarg, ctes); err != nil {
		return err
	}
	refs := []*sqlir.TableRef{sel.From}
	rest := *sel
	rest.With, rest.Larg, rest.Rarg, rest.From = nil, nil, nil, nil
	rest.Joins = make([]sqlir.Join, len(sel.Joins))
	for i := range sel.Joins {
		refs = append(refs, &sel.Joins[i].Table)
		rest.Joins[i].On = sel.Joins[i].On
	}
	db := c.x.tx.db
	for _, t := range refs {
		if t == nil || t.Sub != nil || t.Func != nil || t.Name == "" || ctes[t.Name] {
			continue
		}
		name := db.resolve(t.Name)
		if name == c.table {
			return db.kind.Error(sqlir.TargetTableRead, fmt.Sprintf("You can't specify target table '%s' for update in FROM clause", c.name), relname(c.table), "", "")
		}
		if _, ok := db.views[name]; ok && c.viewRead(name, map[string]bool{}) {
			return c.x.unsupported("a subquery of the write that reads " + relname(c.table) + " through the view " + t.Name)
		}
	}
	for _, q := range outerSelects(rest) {
		if err := c.query(q, ctes); err != nil {
			return err
		}
	}
	return nil
}

// reads reports whether n reads the target anywhere, through a view too.
func (c *targetRead) reads(n any, seen map[string]bool) bool {
	db := c.x.tx.db
	for _, name := range freeNames(n) {
		r := db.resolve(name)
		if r == c.table {
			return true
		}
		if _, ok := db.views[r]; ok && c.viewRead(r, seen) {
			return true
		}
	}
	return false
}

func (c *targetRead) viewRead(view string, seen map[string]bool) bool {
	if seen[view] {
		return false
	}
	seen[view] = true
	v := c.x.tx.db.views[view]
	return v.View == nil || c.reads(v.View, seen)
}

// mergedRead reports whether a subquery under sel, which MySQL may merge
// into the write, reads the target. sel naming the target in its own FROM
// is a self-join once merged, which MySQL runs.
func (c *targetRead) mergedRead(sel *sqlir.SelectStmt, seen map[string]bool) bool {
	if sel == nil {
		return true
	}
	for _, cte := range sel.With {
		if c.mergedRead(cte.Select, seen) {
			return true
		}
	}
	if sel.SetOp != "" {
		return c.mergedRead(sel.Larg, seen) || c.mergedRead(sel.Rarg, seen)
	}
	rest := *sel
	rest.With, rest.From = nil, nil
	rest.Joins = make([]sqlir.Join, len(sel.Joins))
	items := []sqlir.TableRef{}
	if sel.From != nil {
		items = append(items, *sel.From)
	}
	for i, j := range sel.Joins {
		items = append(items, j.Table)
		rest.Joins[i].On = j.On
	}
	for _, t := range items {
		body, ok := c.item(t)
		if !ok {
			continue
		}
		if t.Sub == nil {
			name := c.x.tx.db.resolve(t.Name)
			if seen[name] {
				continue
			}
			seen[name] = true
		}
		if c.mergedRead(body, seen) {
			return true
		}
	}
	for _, q := range outerSelects(rest) {
		if c.reads(q, map[string]bool{}) {
			return true
		}
	}
	return false
}

// outerSelects returns the queries under n that no other query under n
// holds: the subqueries of an expression, not the ones inside them.
func outerSelects(n any) []*sqlir.SelectStmt {
	var out []*sqlir.SelectStmt
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
				out = append(out, s)
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
	return out
}
