package detest

import (
	"database/sql/driver"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
)

// This file executes detest's SQL IR against the abstract DB: single and joined
// SELECT with CTEs, aggregation, DISTINCT, ORDER BY, LIMIT/OFFSET and row
// locking; INSERT with ON CONFLICT DO NOTHING / DO UPDATE and RETURNING;
// UPDATE ... FROM and DELETE ... USING; subqueries in expressions. Statements
// the IR cannot express never get here: the dialect frontend rejects them.

type sqlResult struct {
	cols     []string
	rows     [][]driver.Value
	affected int64
}

// sqlExec is the per-statement executor state.
type sqlExec struct {
	tx    *Tx
	query string
	args  []driver.Value
	ctes  map[string][]Row
}

// env is the evaluation context of an expression: the rows of the tables in
// scope by alias, their merged columns, the enclosing query's context for
// correlated subqueries, and the proposed row of ON CONFLICT DO UPDATE.
type env struct {
	tables   map[string]Row
	merged   Row
	outer    *env
	excluded Row
	win      map[*sqlir.WindowFunc]any // the window function values of the row
}

func (e *env) lookup(table, col string) (any, bool) {
	for cur := e; cur != nil; cur = cur.outer {
		if table != "" {
			if table == "excluded" && cur.excluded != nil {
				v, ok := cur.excluded[col]
				return v, ok
			}
			if r, ok := cur.tables[table]; ok {
				v, ok := r[col]
				return v, ok
			}
			continue
		}
		if v, ok := cur.merged[col]; ok {
			return v, true
		}
	}
	return nil, false
}

// jrow is a row of a FROM clause: the rows of each table by alias plus the
// merged view used for unqualified columns.
type jrow struct {
	by     map[string]Row
	merged Row
	base   Row // the base table's row, for locking and writes (nil when derived)
}

func newJrow(alias string, r Row) jrow {
	j := jrow{by: map[string]Row{alias: r}, merged: Row{}, base: r}
	for k, v := range r {
		j.merged[k] = v
	}
	return j
}

func (j jrow) with(alias string, r Row) jrow {
	n := jrow{by: make(map[string]Row, len(j.by)+1), merged: make(Row, len(j.merged)+len(r)), base: j.base}
	for k, v := range j.by {
		n.by[k] = v
	}
	for k, v := range j.merged {
		n.merged[k] = v
	}
	if r != nil {
		n.by[alias] = r
		for k, v := range r {
			if _, exists := n.merged[k]; !exists {
				n.merged[k] = v
			}
		}
	} else {
		n.by[alias] = nil
	}
	return n
}

func (j jrow) env(outer *env) *env { return &env{tables: j.by, merged: j.merged, outer: outer} }

// rebind replaces the base table's row (after a lock granted a newer version)
// and rebuilds the merged view with the base row's columns taking precedence.
func (j jrow) rebind(alias string, cur Row) jrow {
	n := newJrow(alias, cur)
	for a, r := range j.by {
		if a == alias || r == nil {
			if a != alias {
				n.by[a] = nil
			}
			continue
		}
		n.by[a] = r
		for k, v := range r {
			if _, exists := n.merged[k]; !exists {
				n.merged[k] = v
			}
		}
	}
	return n
}

func (s *parsedStatement) exec(tx *Tx, args []driver.Value) (*sqlResult, error) {
	if sp, ok := s.stmt.(*sqlir.SavepointStmt); ok {
		// ROLLBACK TO is how an aborted transaction continues, so it skips the
		// aborted check the other statements take.
		tx.yieldf("%s: %s %s", tx.db.name, strings.ReplaceAll(sp.Op, "_", " "), sp.Name)
		return &sqlResult{}, tx.savepoint(sp.Op, sp.Name)
	}
	if err := tx.check(); err != nil {
		return nil, err
	}
	x := &sqlExec{tx: tx, query: s.query, args: args, ctes: map[string][]Row{}}
	return x.execStatement(s.stmt)
}

func (x *sqlExec) execStatement(stmt sqlir.Statement) (*sqlResult, error) {
	tx := x.tx
	switch st := stmt.(type) {
	case *sqlir.Script:
		res := &sqlResult{}
		for _, sub := range st.Stmts {
			x.ctes = map[string][]Row{}
			r, err := x.execStatement(sub)
			if err != nil {
				return nil, err
			}
			res = r
		}
		return res, nil
	case *sqlir.CreateTableAsStmt:
		return x.execCreateTableAs(st)
	case *sqlir.RefreshStmt:
		return x.execRefresh(st)
	case *sqlir.SetStmt:
		if st.Name == "lock_timeout" {
			// Any positive timeout makes every lock wait of the transaction a
			// choice between waiting and timing out; the duration does not
			// matter, as detest's waits have no length.
			x.tx.lockTimeout = st.Value != "" && st.Value != "0" && st.Value != "0ms" && st.Value != "0s"
		}
		return &sqlResult{}, nil
	case *sqlir.SelectStmt:
		return x.execSelect(st)
	case *sqlir.InsertStmt:
		return x.execInsert(st)
	case *sqlir.UpdateStmt:
		return x.execUpdate(st)
	case *sqlir.DeleteStmt:
		return x.execDelete(st)
	case *sqlir.SchemaStmt:
		if err := tx.db.applySchema(st, tx); err != nil {
			return nil, err
		}
		return &sqlResult{}, nil
	}
	return nil, unsupported(fmt.Sprintf("%T", stmt), x.query)
}

// execCreateTableAs declares the table, without keys, and fills it with the
// query's rows.
func (x *sqlExec) execCreateTableAs(st *sqlir.CreateTableAsStmt) (*sqlResult, error) {
	selCols, rows, err := x.evalSelect(st.Select, nil)
	if err != nil {
		return nil, err
	}
	cols := selCols
	if len(st.Columns) > 0 {
		cols = st.Columns
	}
	ch := sqlir.SchemaChange{Table: st.Table, Create: true, IfNotExists: st.IfNotExists}
	for _, c := range cols {
		ch.Columns = append(ch.Columns, sqlir.ColumnDef{Name: c})
	}
	if err := x.tx.db.applySchema(&sqlir.SchemaStmt{Changes: []sqlir.SchemaChange{ch}}, x.tx); err != nil {
		return nil, err
	}
	table := x.tx.db.resolve(st.Table)
	if st.Materialized {
		x.tx.db.matviews[table] = st
	}
	return x.fill(table, st, selCols, rows)
}

// execRefresh replaces a materialized view's rows with its query's.
func (x *sqlExec) execRefresh(st *sqlir.RefreshStmt) (*sqlResult, error) {
	table := x.tx.db.resolve(st.Table)
	mv := x.tx.db.matviews[table]
	if mv == nil {
		return nil, fmt.Errorf("detest: %q is not a materialized view", table)
	}
	for _, r := range x.tx.selectNoYield(table, nil) {
		lk := lockKey{table, r.Key()}
		if err := x.tx.lock(lk); err != nil {
			return nil, err
		}
		delete(x.tx.writes, lk)
		x.tx.deleted[lk] = true
	}
	if st.NoData {
		return &sqlResult{}, nil
	}
	selCols, rows, err := x.evalSelect(mv.Select, nil)
	if err != nil {
		return nil, err
	}
	return x.fill(table, mv, selCols, rows)
}

// fill inserts the rows of a CREATE TABLE AS or materialized view query.
func (x *sqlExec) fill(table string, st *sqlir.CreateTableAsStmt, selCols []string, rows []Row) (*sqlResult, error) {
	cols := selCols
	if len(st.Columns) > 0 {
		cols = st.Columns
	}
	out := &sqlResult{}
	if st.NoData {
		return out, nil
	}
	for _, sr := range rows {
		row := Row{}
		for i, c := range cols {
			if i < len(selCols) {
				row[c] = sr[selCols[i]]
			}
		}
		if err := x.tx.db.assignKey(table, row); err != nil {
			return nil, err
		}
		lk := lockKey{table, row.Key()}
		if err := x.tx.lock(lk); err != nil {
			return nil, err
		}
		x.tx.writes[lk] = row
		out.affected++
	}
	return out, nil
}

func (x *sqlExec) unsupported(what string) error { return unsupported(what, x.query) }

// functionRows evaluates a set-returning function in FROM.
func (x *sqlExec) functionRows(t sqlir.TableRef, outer *env) ([]Row, error) {
	en := outer
	if en == nil {
		en = &env{}
	}
	args := make([]any, len(t.Func.Args))
	for i, a := range t.Func.Args {
		v, err := x.eval(a, en)
		if err != nil {
			return nil, err
		}
		args[i] = derefValue(v)
	}
	col := t.Func.Name
	switch {
	case len(t.Columns) > 0:
		col = t.Columns[0]
	case t.Alias != "":
		col = t.Alias
	}
	ordCol := "ordinality"
	if len(t.Columns) > 1 {
		ordCol = t.Columns[1]
	}
	var vals []any
	switch t.Func.Name {
	case "generate_series":
		if len(args) < 2 || len(args) > 3 {
			return nil, x.unsupported("generate_series with other than two or three arguments")
		}
		for _, a := range args {
			if a == nil {
				return nil, nil // generate_series with a NULL bound returns no rows
			}
		}
		start, ok1 := toInt64(args[0])
		stop, ok2 := toInt64(args[1])
		step := int64(1)
		ok3 := true
		if len(args) == 3 {
			step, ok3 = toInt64(args[2])
		}
		if !ok1 || !ok2 || !ok3 || step == 0 {
			return nil, x.unsupported("generate_series over other than integers")
		}
		for v := start; (step > 0 && v <= stop) || (step < 0 && v >= stop); v += step {
			vals = append(vals, v)
		}
	default:
		return nil, x.unsupported("set-returning function " + t.Func.Name + " in FROM")
	}
	rows := make([]Row, len(vals))
	for i, v := range vals {
		rows[i] = Row{col: v}
		if t.Ordinality {
			rows[i][ordCol] = int64(i + 1)
		}
	}
	return rows, nil
}

// --- SELECT ---

func (x *sqlExec) execSelect(sel *sqlir.SelectStmt) (*sqlResult, error) {
	x.tx.yieldf("%s: %s", x.tx.db.name, lazyString(func() string { return x.summarize(sel) }))
	cols, rows, err := x.evalSelect(sel, nil)
	if err != nil {
		return nil, err
	}
	out := &sqlResult{cols: cols, affected: int64(len(rows))}
	for _, row := range rows {
		vals := make([]driver.Value, len(cols))
		for i, c := range cols {
			vals[i] = toDriverValue(row[c])
		}
		out.rows = append(out.rows, vals)
	}
	return out, nil
}

func (x *sqlExec) summarize(sel *sqlir.SelectStmt) string {
	table := "(no table)"
	if sel.From != nil {
		table = x.tableLabel(*sel.From)
		for _, j := range sel.Joins {
			table += " join " + x.tableLabel(j.Table)
		}
	}
	desc := fmt.Sprintf("select %s where %s", table, x.exprString(sel.Where))
	if sel.Lock != nil {
		desc += " for update"
		if sel.Lock.SkipLocked {
			desc += " skip locked"
		}
	}
	return desc
}

func (x *sqlExec) tableLabel(t sqlir.TableRef) string {
	if t.Sub != nil {
		return "(subquery)"
	}
	return t.Name
}

func (x *sqlExec) withCTEs(ctes []sqlir.CTE, outer *env) error {
	for _, cte := range ctes {
		cols, rows, err := x.evalSelect(cte.Select, outer)
		if err != nil {
			return err
		}
		_ = cols
		x.ctes[cte.Name] = rows
	}
	return nil
}

// tableRows returns the rows of a FROM item with the alias they are known by.
func (x *sqlExec) tableRows(t sqlir.TableRef, outer *env) (alias string, rows []Row, base bool, err error) {
	alias = t.Alias
	if t.Sub != nil {
		cols, rows, err := x.evalSelect(t.Sub, outer)
		if err == nil && len(t.Columns) > 0 {
			rows = renameColumns(rows, cols, t.Columns) // AS alias(a, b)
		}
		return alias, rows, false, err
	}
	if t.Func != nil {
		if alias == "" {
			alias = t.Func.Name
		}
		rows, err := x.functionRows(t, outer)
		return alias, rows, false, err
	}
	if alias == "" {
		alias = relname(t.Name)
	}
	if rows, ok := x.ctes[t.Name]; ok {
		return alias, rows, false, nil
	}
	if err := x.tx.db.checkTable(x.tx.db.resolve(t.Name)); err != nil {
		return "", nil, false, err
	}
	if v := x.tx.db.views[x.tx.db.resolve(t.Name)]; v != nil {
		cols, rows, err := x.evalSelect(v.View, nil)
		if err == nil && len(v.ViewColumns) > 0 {
			rows = renameColumns(rows, cols, v.ViewColumns)
		}
		return alias, rows, false, err
	}
	return alias, x.tx.selectNoYield(t.Name, nil), true, nil
}

// scan produces the FROM rows of a query: the base table joined with each
// JOIN item by nested loops.
func (x *sqlExec) scan(sel *sqlir.SelectStmt, outer *env) ([]jrow, string, error) {
	if sel.From == nil {
		return []jrow{{by: map[string]Row{}, merged: Row{}}}, "", nil
	}
	alias, rows, isBase, err := x.tableRows(*sel.From, outer)
	if err != nil {
		return nil, "", err
	}
	baseTable := ""
	if isBase && len(sel.Joins) == 0 {
		baseTable = x.tx.db.resolve(sel.From.Name)
	}
	var out []jrow
	for _, r := range rows {
		j := newJrow(alias, r)
		if !isBase {
			j.base = nil
		}
		out = append(out, j)
	}
	for _, join := range sel.Joins {
		jalias, jrows, _, err := x.tableRows(join.Table, outer)
		if err != nil {
			return nil, "", err
		}
		var next []jrow
		switch join.Kind {
		case sqlir.InnerJoin, sqlir.CrossJoin, sqlir.LeftJoin:
			for _, l := range out {
				if join.Table.Lateral {
					// A lateral item sees the row it joins to.
					if _, jrows, _, err = x.tableRows(join.Table, l.env(outer)); err != nil {
						return nil, "", err
					}
				}
				matched := false
				for _, r := range jrows {
					cand := l.with(jalias, r)
					ok := true
					if join.On != nil {
						if ok, err = x.evalBool(join.On, cand.env(outer)); err != nil {
							return nil, "", err
						}
					}
					if ok {
						matched = true
						next = append(next, cand)
					}
				}
				if !matched && join.Kind == sqlir.LeftJoin {
					next = append(next, l.with(jalias, nil))
				}
			}
		default:
			return nil, "", x.unsupported("right or full join")
		}
		out = next
	}
	return out, baseTable, nil
}

func (x *sqlExec) aliasOf(sel *sqlir.SelectStmt) string {
	if sel.From.Alias != "" {
		return sel.From.Alias
	}
	return relname(sel.From.Name)
}

func (x *sqlExec) order(keys []sqlir.OrderKey, rows []jrow, outer *env) error {
	if len(keys) == 0 {
		return nil
	}
	vals := make([][]any, len(rows))
	for i, r := range rows {
		vals[i] = make([]any, len(keys))
		for j, k := range keys {
			v, err := x.eval(k.Expr, r.env(outer))
			if err != nil {
				if _, unknown := err.(errUnknownExpr); unknown {
					continue // order by an expression detest cannot evaluate: keep key order
				}
				return err
			}
			vals[i][j] = v
		}
	}
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return orderedBefore(keys, vals[idx[a]], vals[idx[b]]) })
	sorted := make([]jrow, len(rows))
	for i, j := range idx {
		sorted[i] = rows[j]
	}
	copy(rows, sorted)
	return nil
}

func (x *sqlExec) offsetLimit(sel *sqlir.SelectStmt, rows []jrow, outer *env) ([]jrow, int, error) {
	limit := -1
	if sel.Limit != nil {
		v, err := x.eval(sel.Limit, &env{outer: outer})
		if err != nil {
			return nil, 0, err
		}
		if f, ok := toFloat(derefValue(v)); ok {
			limit = int(f)
		}
	}
	if sel.Offset != nil {
		v, err := x.eval(sel.Offset, &env{outer: outer})
		if err != nil {
			return nil, 0, err
		}
		if f, ok := toFloat(derefValue(v)); ok {
			if int(f) < len(rows) {
				rows = rows[int(f):]
			} else {
				rows = nil
			}
		}
	}
	return rows, limit, nil
}

func (x *sqlExec) project(sel *sqlir.SelectStmt, rows []jrow, outer *env) ([]string, []Row, error) {
	var cols []string
	starCols := func(t sqlir.Target) []string {
		set := map[string]bool{}
		for _, r := range rows {
			src := r.merged
			if t.Table != "" {
				src = r.by[t.Table]
			}
			for k := range src {
				if k != "_key" {
					set[k] = true
				}
			}
		}
		var all []string
		for k := range set {
			all = append(all, k)
		}
		sort.Strings(all)
		return all
	}
	for _, t := range sel.Targets {
		if t.Star {
			cols = append(cols, starCols(t)...)
			continue
		}
		cols = append(cols, targetName(t))
	}
	var out []Row
	for _, r := range rows {
		o := Row{}
		for _, t := range sel.Targets {
			if t.Star {
				src := r.merged
				if t.Table != "" {
					src = r.by[t.Table]
				}
				for k, v := range src {
					if k != "_key" {
						o[k] = v
					}
				}
				continue
			}
			v, err := x.eval(t.Expr, r.env(outer))
			if err != nil {
				return nil, nil, err
			}
			o[targetName(t)] = v
		}
		if !sel.Distinct || !seenDistinct(out, o, cols) {
			out = append(out, o)
		}
	}
	return cols, out, nil
}

type aggEnv struct {
	env  *env
	rows []jrow
	x    *sqlExec
}

func hasAggregate(e sqlir.Expr) bool {
	switch v := e.(type) {
	case nil:
		return false
	case *sqlir.FuncCall:
		switch v.Name {
		case "count", "sum", "min", "max", "avg":
			return true
		}
		for _, a := range v.Args {
			if hasAggregate(a) {
				return true
			}
		}
	case *sqlir.BinaryExpr:
		return hasAggregate(v.L) || hasAggregate(v.R)
	case *sqlir.UnaryExpr:
		return hasAggregate(v.X)
	case *sqlir.Cast:
		return hasAggregate(v.X)
	case *sqlir.CaseExpr:
		for _, w := range v.Whens {
			if hasAggregate(w.When) || hasAggregate(w.Then) {
				return true
			}
		}
		return hasAggregate(v.Else)
	}
	return false
}

func (x *sqlExec) evalBoolAgg(e sqlir.Expr, g *aggEnv) (bool, error) {
	v, err := x.evalAgg(e, g)
	if err != nil {
		return false, err
	}
	b, _ := derefValue(v).(bool)
	return b, nil
}

// evalAgg evaluates an expression over a group: aggregates fold the group's
// rows, other expressions use the group's sample row.
func (x *sqlExec) evalAgg(e sqlir.Expr, g *aggEnv) (any, error) {
	switch v := e.(type) {
	case *sqlir.WindowFunc:
		return x.eval(v, g.env)
	case *sqlir.FuncCall:
		switch v.Name {
		case "count", "sum", "min", "max", "avg":
			if v.Star {
				return int64(len(g.rows)), nil
			}
			var vals []any
			seen := map[string]bool{}
			for _, r := range g.rows {
				val, err := x.eval(v.Args[0], r.env(g.env.outer))
				if err != nil {
					return nil, err
				}
				if derefValue(val) == nil {
					continue
				}
				if v.Distinct {
					k := fmt.Sprint(derefValue(val))
					if seen[k] {
						continue
					}
					seen[k] = true
				}
				vals = append(vals, val)
			}
			return foldAggregate(v.Name, false, vals, len(g.rows)), nil
		}
		args := make([]any, len(v.Args))
		for i, a := range v.Args {
			val, err := x.evalAgg(a, g)
			if err != nil {
				return nil, err
			}
			args[i] = val
		}
		return x.callFunc(v.Name, args)
	case *sqlir.BinaryExpr:
		l, err := x.evalAgg(v.L, g)
		if err != nil {
			return nil, err
		}
		r, err := x.evalAgg(v.R, g)
		if err != nil {
			return nil, err
		}
		return x.binary(v.Op, l, r)
	case *sqlir.Cast:
		val, err := x.evalAgg(v.X, g)
		if err != nil {
			return nil, err
		}
		return castValue(val, v.Type), nil
	}
	return x.eval(e, g.env)
}

// --- INSERT ---

func (x *sqlExec) execInsert(ins *sqlir.InsertStmt) (*sqlResult, error) {
	table := x.tx.db.resolve(ins.Table)
	if err := x.tx.db.checkTable(table); err != nil {
		return nil, err
	}
	var rows []Row
	switch {
	case ins.Select != nil:
		cols, srows, err := x.evalSelect(ins.Select, nil)
		if err != nil {
			return nil, err
		}
		for _, sr := range srows {
			row := Row{}
			for i, c := range ins.Columns {
				if i < len(cols) {
					row[c] = sr[cols[i]]
				}
			}
			rows = append(rows, row)
		}
	default:
		for _, exprs := range ins.Rows {
			if len(exprs) != len(ins.Columns) {
				return nil, fmt.Errorf("detest: insert has %d columns and %d values: %s", len(ins.Columns), len(exprs), x.query)
			}
			row := Row{}
			for i, e := range exprs {
				v, err := x.eval(e, &env{})
				if err != nil {
					if _, ok := err.(errUnknownExpr); ok {
						v = sqlir.Unknown
					} else {
						return nil, err
					}
				}
				if _, isDefault := e.(*sqlir.Default); isDefault {
					continue
				}
				row[ins.Columns[i]] = v
			}
			rows = append(rows, row)
		}
	}
	alias := ins.Alias
	if alias == "" {
		alias = relname(ins.Table)
	}
	out := &sqlResult{cols: x.returningCols(ins.Returning, ins.Columns)}
	for _, row := range rows {
		if err := x.applyDefaults(table, row); err != nil {
			return nil, err
		}
		if err := x.checkTypes(table, row); err != nil {
			return nil, err
		}
		if err := x.tx.db.assignKey(table, row); err != nil {
			return nil, err
		}
		x.tx.yieldf("%s: insert %s %s", x.tx.db.name, ins.Table, row)
		var existing Row
		if ins.OnConflict != nil {
			var err error
			if existing, err = x.findConflict(table, ins.OnConflict.Columns, row); err != nil {
				return nil, err
			}
		}
		if existing != nil {
			if ins.OnConflict.DoNothing {
				if x.tx.p != nil {
					x.tx.p.r.note(x.tx.p, "on conflict do nothing: row skipped")
				}
				continue
			}
			// DO UPDATE: lock the existing row, re-read it, apply SET with
			// EXCLUDED bound to the proposed row.
			lk := lockKey{table, existing.Key()}
			if err := x.tx.lockMode(lk, x.tx.db.updateLock(table, assignedColumns(ins.OnConflict.Set))); err != nil {
				return nil, err
			}
			cur, _ := x.tx.view(table, existing.Key())
			e := &env{tables: map[string]Row{alias: cur}, merged: cur, excluded: row}
			if ins.OnConflict.Where != nil {
				ok, err := x.evalBool(ins.OnConflict.Where, e)
				if err != nil {
					return nil, err
				}
				if !ok {
					if x.tx.p != nil {
						x.tx.p.r.note(x.tx.p, "on conflict do update: WHERE false, row skipped")
					}
					continue
				}
			}
			updated := cur.clone()
			for _, a := range ins.OnConflict.Set {
				v, err := x.eval(a.Value, e)
				if err != nil {
					if _, ok := err.(errUnknownExpr); ok {
						v = sqlir.Unknown
					} else {
						return nil, err
					}
				}
				updated[a.Column] = v
			}

			if err := x.checkTypes(table, updated); err != nil {
				return nil, err
			}
			if err := x.checkUniques(table, updated, existing.Key(), cur); err != nil {
				return nil, err
			}
			if err := x.checkParents(table, updated, cur); err != nil {
				return nil, err
			}
			if err := x.onParentUpdate(table, cur, updated); err != nil {
				return nil, err
			}
			lk, err := x.rekey(table, lk, updated)
			if err != nil {
				return nil, err
			}
			x.tx.writes[lk] = updated
			if x.tx.p != nil {
				x.tx.p.r.note(x.tx.p, "on conflict do update: %s", updated)
			}
			out.affected++
			x.appendReturning(out, ins.Returning, updated)
			continue
		}
		lk := lockKey{table, row.Key()}
		if err := x.tx.lock(lk); err != nil {
			return nil, err
		}
		if _, exists := x.tx.view(table, row.Key()); exists {
			return nil, x.tx.db.duplicateKey(table, x.tx.db.pkConstraint(table))
		}
		if err := x.checkUniques(table, row, "", nil); err != nil {
			return nil, err
		}
		if err := x.checkParents(table, row, nil); err != nil {
			return nil, err
		}
		delete(x.tx.deleted, lk)
		x.tx.writes[lk] = row
		out.affected++
		x.appendReturning(out, ins.Returning, row)
	}
	return out, nil
}

// findConflict returns the existing row the proposed row collides with: on
// the conflict columns when given, otherwise on the key.
func (x *sqlExec) findConflict(table string, cols []string, row Row) (Row, error) {
	if x.tx.db.defs[table] != nil {
		targets, err := x.conflictTargets(table, cols)
		if err != nil {
			return nil, err
		}
		for i := range targets {
			ex, err := x.claimUnique(table, &targets[i], row, "")
			if err != nil || ex != nil {
				return ex, err
			}
		}
		return nil, nil
	}
	// Without a declared schema: the row's key, or a scan on the target columns.
	if len(cols) == 0 {
		if ex, ok := x.tx.view(table, row.Key()); ok {
			return ex, nil
		}
		return nil, nil
	}
	for _, ex := range x.tx.selectNoYield(table, nil) {
		same := true
		for _, c := range cols {
			if !sameValue(ex[c], row[c]) {
				same = false
				break
			}
		}
		if same {
			return ex, nil
		}
	}
	return nil, nil
}

func (x *sqlExec) returningCols(ret []sqlir.Target, all []string) []string {
	var cols []string
	for _, t := range ret {
		if t.Star {
			cols = append(cols, all...)
			continue
		}
		cols = append(cols, targetName(t))
	}
	return cols
}

func (x *sqlExec) appendReturning(out *sqlResult, ret []sqlir.Target, row Row) {
	if len(ret) == 0 {
		return
	}
	vals := make([]driver.Value, 0, len(out.cols))
	e := &env{merged: row}
	for _, t := range ret {
		if t.Star {
			for _, c := range out.cols {
				vals = append(vals, toDriverValue(row[c]))
			}
			break
		}
		v, _ := x.eval(t.Expr, e)
		vals = append(vals, toDriverValue(v))
	}
	out.rows = append(out.rows, vals)
}

// --- UPDATE / DELETE ---

// writeCandidates pairs each row of the target table with the FROM/USING rows
// that satisfy WHERE (or just the row when there are none).
func (x *sqlExec) writeCandidates(table, alias string, extra []sqlir.TableRef, where sqlir.Expr) ([]jrow, error) {
	if err := x.tx.db.checkTable(x.tx.db.resolve(table)); err != nil {
		return nil, err
	}
	if alias == "" {
		alias = relname(table)
	}
	var rows []jrow
	for _, r := range x.tx.selectNoYield(table, nil) {
		rows = append(rows, newJrow(alias, r))
	}
	for _, t := range extra {
		ealias, erows, _, err := x.tableRows(t, nil)
		if err != nil {
			return nil, err
		}
		var next []jrow
		for _, l := range rows {
			for _, r := range erows {
				next = append(next, l.with(ealias, r))
			}
		}
		rows = next
	}
	if where == nil {
		return rows, nil
	}
	var kept []jrow
	for _, r := range rows {
		ok, err := x.evalBool(where, r.env(nil))
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

func (x *sqlExec) execUpdate(up *sqlir.UpdateStmt) (*sqlResult, error) {
	table := x.tx.db.resolve(up.Table)
	alias := up.Alias
	if alias == "" {
		alias = relname(up.Table)
	}
	// Validate the predicate and preview SET for the trace.
	if _, err := x.writeCandidates(up.Table, up.Alias, nil, up.Where); err != nil && len(up.From) == 0 {
		return nil, err
	}
	preview := lazyString(func() string {
		row := Row{}
		for _, a := range up.Set {
			v, err := x.eval(a.Value, &env{})
			if err != nil {
				v = sqlir.Unknown
			}
			row[a.Column] = v
		}
		return row.String()
	})
	x.tx.yieldf("%s: update %s set %s where %s", x.tx.db.name, up.Table, preview, lazyString(func() string { return x.exprString(up.Where) }))
	cands, err := x.writeCandidates(up.Table, up.Alias, up.From, up.Where)
	if err != nil {
		return nil, err
	}
	out := &sqlResult{cols: x.returningCols(up.Returning, nil)}
	done := map[string]bool{}
	for _, c := range cands {
		key := c.base.Key()
		if done[key] {
			continue
		}
		lk := lockKey{table, key}
		if err := x.tx.lockMode(lk, x.tx.db.updateLock(table, assignedColumns(up.Set))); err != nil {
			return nil, err
		}
		// Re-evaluate the predicate on the version visible after the lock, as
		// Postgres Read Committed does.
		cur, ok := x.tx.view(table, key)
		if !ok {
			continue
		}
		c2 := c.rebind(alias, cur)
		if up.Where != nil {
			ok, err := x.evalBool(up.Where, c2.env(nil))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		done[key] = true
		updated := cur.clone()
		for _, a := range up.Set {
			v, err := x.eval(a.Value, c2.env(nil))
			if err != nil {
				if _, ok := err.(errUnknownExpr); ok {
					v = sqlir.Unknown
				} else {
					return nil, err
				}
			}
			updated[a.Column] = v
		}

		if err := x.checkTypes(table, updated); err != nil {
			return nil, err
		}
		if err := x.checkUniques(table, updated, key, cur); err != nil {
			return nil, err
		}
		if err := x.checkParents(table, updated, cur); err != nil {
			return nil, err
		}
		if err := x.onParentUpdate(table, cur, updated); err != nil {
			return nil, err
		}
		lk, err := x.rekey(table, lk, updated)
		if err != nil {
			return nil, err
		}
		x.tx.writes[lk] = updated
		out.affected++
		x.appendReturning(out, up.Returning, updated)
	}
	return out, nil
}

func (x *sqlExec) execDelete(del *sqlir.DeleteStmt) (*sqlResult, error) {
	x.tx.yieldf("%s: delete %s where %s", x.tx.db.name, del.Table, lazyString(func() string { return x.exprString(del.Where) }))
	cands, err := x.writeCandidates(del.Table, del.Alias, del.Using, del.Where)
	if err != nil {
		return nil, err
	}
	table := x.tx.db.resolve(del.Table)
	alias := del.Alias
	if alias == "" {
		alias = relname(del.Table)
	}
	out := &sqlResult{cols: x.returningCols(del.Returning, nil)}
	done := map[string]bool{}
	for _, c := range cands {
		key := c.base.Key()
		if done[key] {
			continue
		}
		lk := lockKey{table, key}
		if err := x.tx.lock(lk); err != nil {
			return nil, err
		}
		cur, ok := x.tx.view(table, key)
		if !ok {
			continue
		}
		if del.Where != nil {
			c2 := c.rebind(alias, cur)
			ok, err := x.evalBool(del.Where, c2.env(nil))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		done[key] = true
		delete(x.tx.writes, lk)
		x.tx.deleted[lk] = true
		if err := x.onParentDelete(table, cur); err != nil {
			return nil, err
		}
		out.affected++
		x.appendReturning(out, del.Returning, cur)
	}
	return out, nil
}

// --- expressions ---

type errUnknownExpr struct{ what string }

func (e errUnknownExpr) Error() string { return "detest: cannot evaluate SQL expression: " + e.what }

func (x *sqlExec) evalBool(e sqlir.Expr, en *env) (bool, error) {
	v, err := x.eval(e, en)
	if err != nil {
		return false, err
	}
	b, _ := derefValue(v).(bool)
	return b, nil
}

func (x *sqlExec) eval(e sqlir.Expr, en *env) (any, error) {
	switch v := e.(type) {
	case nil:
		return true, nil
	case *sqlir.ColumnRef:
		val, _ := en.lookup(v.Table, v.Column)
		return val, nil
	case *sqlir.Param:
		if v.Index < 0 || v.Index >= len(x.args) {
			return nil, fmt.Errorf("detest: parameter %d out of range in %s", v.Index+1, x.query)
		}
		return x.args[v.Index], nil
	case *sqlir.Const:
		return v.Value, nil
	case *sqlir.WindowFunc:
		for cur := en; cur != nil; cur = cur.outer {
			if val, ok := cur.win[v]; ok {
				return val, nil
			}
		}
		return nil, x.unsupported("window function outside the select list, ORDER BY or DISTINCT ON")
	case *sqlir.Default:
		return nil, nil
	case *sqlir.Cast:
		val, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		return castValue(val, v.Type), nil
	case *sqlir.UnaryExpr:
		val, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		switch v.Op {
		case "NOT":
			b, _ := derefValue(val).(bool)
			return !b, nil
		case "-":
			if f, ok := toFloat(derefValue(val)); ok {
				return numeric(-f), nil
			}
		}
		return nil, errUnknownExpr{"unary " + v.Op}
	case *sqlir.BinaryExpr:
		switch v.Op {
		case "AND":
			l, err := x.evalBool(v.L, en)
			if err != nil || !l {
				return false, err
			}
			return x.evalBool(v.R, en)
		case "OR":
			l, err := x.evalBool(v.L, en)
			if err != nil || l {
				return l, err
			}
			return x.evalBool(v.R, en)
		}
		l, err := x.eval(v.L, en)
		if err != nil {
			return nil, err
		}
		r, err := x.eval(v.R, en)
		if err != nil {
			return nil, err
		}
		return x.binary(v.Op, l, r)
	case *sqlir.IsNull:
		val, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		isNull := derefValue(val) == nil
		if v.Not {
			return !isNull, nil
		}
		return isNull, nil
	case *sqlir.InExpr:
		l, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		in := false
		if v.Sub != nil {
			cols, rows, err := x.evalSelect(v.Sub, en)
			if err != nil {
				return nil, err
			}
			var lhs []any
			if re, ok := v.X.(*sqlir.RowExpr); ok {
				lhs = l.([]any)
				_ = re
			} else {
				lhs = []any{l}
			}
			for _, r := range rows {
				match := len(cols) == len(lhs)
				for i := 0; match && i < len(cols); i++ {
					if !equalValues(lhs[i], r[cols[i]]) {
						match = false
					}
				}
				if match {
					in = true
					break
				}
			}
		} else {
			for _, it := range v.List {
				val, err := x.eval(it, en)
				if err != nil {
					return nil, err
				}
				if equalValues(l, val) {
					in = true
					break
				}
			}
		}
		if v.Not {
			return !in, nil
		}
		return in, nil
	case *sqlir.Exists:
		_, rows, err := x.evalSelect(v.Select, en)
		if err != nil {
			return nil, err
		}
		if v.Not {
			return len(rows) == 0, nil
		}
		return len(rows) > 0, nil
	case *sqlir.SubQuery:
		cols, rows, err := x.evalSelect(v.Select, en)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 || len(cols) == 0 {
			return nil, nil
		}
		return rows[0][cols[0]], nil
	case *sqlir.RowExpr:
		vals := make([]any, len(v.Items))
		for i, it := range v.Items {
			val, err := x.eval(it, en)
			if err != nil {
				return nil, err
			}
			vals[i] = val
		}
		return vals, nil
	case *sqlir.CaseExpr:
		var arg any
		if v.Arg != nil {
			a, err := x.eval(v.Arg, en)
			if err != nil {
				return nil, err
			}
			arg = a
		}
		for _, w := range v.Whens {
			cond, err := x.eval(w.When, en)
			if err != nil {
				return nil, err
			}
			hit := false
			if v.Arg != nil {
				hit = equalValues(arg, cond)
			} else {
				hit, _ = derefValue(cond).(bool)
			}
			if hit {
				return x.eval(w.Then, en)
			}
		}
		if v.Else != nil {
			return x.eval(v.Else, en)
		}
		return nil, nil
	case *sqlir.FuncCall:
		args := make([]any, len(v.Args))
		for i, a := range v.Args {
			val, err := x.eval(a, en)
			if err != nil {
				return nil, err
			}
			args[i] = val
		}
		return x.callFunc(v.Name, args)
	}
	return nil, errUnknownExpr{fmt.Sprintf("%T", e)}
}

func (x *sqlExec) binary(op string, l, r any) (any, error) {
	switch op {
	case "=":
		return equalValues(l, r), nil
	case "<>", "!=":
		if derefValue(l) == nil || derefValue(r) == nil {
			return false, nil
		}
		return !equalValues(l, r), nil
	case "<", "<=", ">", ">=":
		c, ok := compareValues(l, r)
		if !ok {
			return false, nil
		}
		switch op {
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		default:
			return c >= 0, nil
		}
	case "LIKE", "ILIKE", "NOT LIKE", "NOT ILIKE":
		m := likeMatch(l, r, strings.HasSuffix(op, "ILIKE"))
		if strings.HasPrefix(op, "NOT") {
			return !m, nil
		}
		return m, nil
	case "+", "-", "*", "/", "%":
		v, err := arith(op, l, r)
		if ke, ok := err.(kindError); ok {
			return nil, x.tx.db.kind.Error(ke.kind, ke.msg, "", "", "")
		}
		return v, err
	case "||":
		return fmt.Sprint(derefValue(l)) + fmt.Sprint(derefValue(r)), nil
	}
	return nil, errUnknownExpr{"operator " + op}
}

// arith handles numeric arithmetic and timestamp/interval arithmetic
// (intervals are time.Duration).
func arith(op string, l, r any) (any, error) {
	l, r = derefValue(l), derefValue(r)
	if l == nil || r == nil {
		return nil, nil
	}
	if t, ok := l.(time.Time); ok {
		switch rv := r.(type) {
		case time.Duration:
			if op == "+" {
				return t.Add(rv), nil
			}
			if op == "-" {
				return t.Add(-rv), nil
			}
		case time.Time:
			if op == "-" {
				return t.Sub(rv), nil
			}
		}
		return nil, errUnknownExpr{"timestamp arithmetic " + op}
	}
	if d, ok := l.(time.Duration); ok {
		switch rv := r.(type) {
		case time.Duration:
			if op == "+" {
				return d + rv, nil
			}
			if op == "-" {
				return d - rv, nil
			}
		case time.Time:
			if op == "+" {
				return rv.Add(d), nil
			}
		}
		if f, ok := toFloat(r); ok && op == "*" {
			return time.Duration(float64(d) * f), nil
		}
		return nil, errUnknownExpr{"interval arithmetic " + op}
	}
	if il, ok := integer(l); ok {
		if ir, ok := integer(r); ok {
			return intArith(op, il, ir)
		}
	}
	fl, okl := toFloat(l)
	fr, okr := toFloat(r)
	if !okl || !okr {
		return nil, errUnknownExpr{"arithmetic on non-numeric values"}
	}
	var v float64
	switch op {
	case "+":
		v = fl + fr
	case "-":
		v = fl - fr
	case "*":
		v = fl * fr
	case "/":
		if fr == 0 {
			return nil, kindError{sqlir.DivisionByZero, "division by zero"}
		}
		v = fl / fr
	case "%":
		if fr == 0 {
			return nil, kindError{sqlir.DivisionByZero, "division by zero"}
		}
		v = math.Mod(fl, fr)
	}
	return numeric(v), nil
}

func numeric(v float64) any {
	if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
		return int64(v)
	}
	return v
}

func castValue(v any, typ string) any {
	v = derefValue(v)
	if v == nil {
		return nil
	}
	switch typ {
	case "int", "int4", "int8", "bigint", "integer", "smallint":
		if f, ok := toFloat(v); ok {
			return int64(f)
		}
	case "float8", "float4", "double precision", "numeric", "real":
		if f, ok := toFloat(v); ok {
			return f
		}
	case "text", "varchar", "bpchar":
		return fmt.Sprint(v)
	case "bool", "boolean":
		if b, ok := v.(bool); ok {
			return b
		}
		return fmt.Sprint(v) == "true"
	case "interval":
		if s, ok := v.(string); ok {
			if d, err := time.ParseDuration(strings.ReplaceAll(strings.ReplaceAll(s, " seconds", "s"), " second", "s")); err == nil {
				return d
			}
		}
	}
	return v
}

// callFunc evaluates the scalar functions that appear on control paths.
// pg_try_advisory_xact_lock is a non-blocking lock held until the end of the
// transaction, modeled in the DB's lock table.
func (x *sqlExec) callFunc(name string, args []any) (any, error) {
	d := func(i int) any {
		if i < len(args) {
			return derefValue(args[i])
		}
		return nil
	}
	switch name {
	case "coalesce":
		for i := range args {
			if d(i) != nil {
				return args[i], nil
			}
		}
		return nil, nil
	case "nullif":
		if len(args) == 2 && equalValues(args[0], args[1]) {
			return nil, nil
		}
		return args[0], nil
	case "greatest", "least":
		var best any
		for i := range args {
			v := d(i)
			if v == nil {
				continue
			}
			if best == nil {
				best = v
				continue
			}
			c, _ := compareValues(v, best)
			if (name == "greatest" && c > 0) || (name == "least" && c < 0) {
				best = v
			}
		}
		return best, nil
	case "lower":
		return strings.ToLower(fmt.Sprint(d(0))), nil
	case "upper":
		return strings.ToUpper(fmt.Sprint(d(0))), nil
	case "length", "char_length":
		return int64(len([]rune(fmt.Sprint(d(0))))), nil
	case "concat":
		var b strings.Builder
		for i := range args {
			if v := d(i); v != nil {
				b.WriteString(fmt.Sprint(v))
			}
		}
		return b.String(), nil
	case "hashtext":
		return fmt.Sprint(d(0)), nil
	case "nextval":
		// A sequence is not transactional: a rolled back nextval stays used.
		if len(args) != 1 {
			return nil, x.unsupported("nextval with other than one argument")
		}
		return x.tx.db.nextval(fmt.Sprint(derefValue(args[0]))), nil
	case "setval":
		if len(args) < 2 || len(args) > 3 {
			return nil, x.unsupported("setval with other than two or three arguments")
		}
		if derefValue(args[0]) == nil || derefValue(args[1]) == nil {
			return nil, nil // strict: NULL in, NULL out
		}
		v, ok := toInt64(derefValue(args[1]))
		if !ok {
			return nil, x.unsupported("setval to a non-integer")
		}
		called := true
		if len(args) == 3 {
			called, _ = derefValue(args[2]).(bool)
		}
		x.tx.db.setval(fmt.Sprint(derefValue(args[0])), v, called)
		return v, nil
	case "gen_random_uuid", "uuid_generate_v4":
		return x.tx.db.newUUID(), nil
	case "now", "clock_timestamp", "current_timestamp", "transaction_timestamp", "statement_timestamp":
		return time.Now(), nil
	case "random":
		return 0.5, nil
	case "abs":
		if f, ok := toFloat(d(0)); ok {
			return numeric(math.Abs(f)), nil
		}
	case "floor":
		if f, ok := toFloat(d(0)); ok {
			return numeric(math.Floor(f)), nil
		}
	case "ceil", "ceiling":
		if f, ok := toFloat(d(0)); ok {
			return numeric(math.Ceil(f)), nil
		}
	case "round":
		if f, ok := toFloat(d(0)); ok {
			return numeric(math.Round(f)), nil
		}
	case "power", "pow":
		a, oka := toFloat(d(0))
		b, okb := toFloat(d(1))
		if oka && okb {
			return math.Pow(a, b), nil
		}
	case "make_interval":
		// make_interval(secs => x) is the form that appears in backoff SQL; a
		// single argument is taken as seconds.
		if f, ok := toFloat(d(0)); ok {
			return time.Duration(f * float64(time.Second)), nil
		}
	case "pg_try_advisory_xact_lock", "pg_advisory_xact_lock":
		var kb strings.Builder
		for i := range args {
			fmt.Fprintf(&kb, "%v|", d(i))
		}
		lk := lockKey{"__advisory__", kb.String()}
		if x.tx.heldByOther(lk, lockUpdate) && name == "pg_try_advisory_xact_lock" {
			return false, nil
		}
		if err := x.tx.lock(lk); err != nil {
			return nil, err
		}
		if name == "pg_advisory_xact_lock" {
			return nil, nil
		}
		return true, nil
	}
	return nil, errUnknownExpr{name + "(...)"}
}

func targetName(t sqlir.Target) string {
	if t.Alias != "" {
		return t.Alias
	}
	switch v := t.Expr.(type) {
	case *sqlir.ColumnRef:
		return v.Column
	case *sqlir.FuncCall:
		return v.Name
	case *sqlir.WindowFunc:
		return v.Func.Name
	case *sqlir.Exists:
		return "exists"
	}
	return "?column?"
}

func seenDistinct(rows []Row, o Row, cols []string) bool {
	for _, r := range rows {
		same := true
		for _, c := range cols {
			if !sameValue(r[c], o[c]) {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// exprString renders a predicate for the trace with parameter values filled in.
func (x *sqlExec) exprString(e sqlir.Expr) string {
	switch v := e.(type) {
	case nil:
		return "true"
	case *sqlir.ColumnRef:
		if v.Table != "" {
			return v.Table + "." + v.Column
		}
		return v.Column
	case *sqlir.Param:
		if v.Index >= 0 && v.Index < len(x.args) {
			return fmt.Sprint(x.args[v.Index])
		}
		return "?"
	case *sqlir.Const:
		return fmt.Sprint(v.Value)
	case *sqlir.BinaryExpr:
		return x.exprString(v.L) + " " + strings.ToLower(v.Op) + " " + x.exprString(v.R)
	case *sqlir.UnaryExpr:
		return strings.ToLower(v.Op) + " " + x.exprString(v.X)
	case *sqlir.IsNull:
		if v.Not {
			return x.exprString(v.X) + " is not null"
		}
		return x.exprString(v.X) + " is null"
	case *sqlir.InExpr:
		var items []string
		for _, it := range v.List {
			items = append(items, x.exprString(it))
		}
		rhs := "(" + strings.Join(items, ", ") + ")"
		if v.Sub != nil {
			rhs = "(subquery)"
		}
		if v.Not {
			return x.exprString(v.X) + " not in " + rhs
		}
		return x.exprString(v.X) + " in " + rhs
	case *sqlir.Cast:
		return x.exprString(v.X)
	case *sqlir.FuncCall:
		return v.Name + "(...)"
	case *sqlir.Exists:
		return "exists (subquery)"
	case *sqlir.SubQuery:
		return "(subquery)"
	case *sqlir.RowExpr:
		var items []string
		for _, it := range v.Items {
			items = append(items, x.exprString(it))
		}
		return "(" + strings.Join(items, ", ") + ")"
	}
	return "..."
}

func toDriverValue(v any) driver.Value {
	v = derefValue(v)
	switch x := v.(type) {
	case nil, int64, float64, bool, []byte, string, time.Time:
		return x
	case time.Duration:
		return x.String()
	}
	if f, ok := toFloat(v); ok {
		return int64(f)
	}
	return fmt.Sprint(v)
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

// renameColumns renames rows' columns by position, as a column alias list
// does.
func renameColumns(rows []Row, cols, names []string) []Row {
	for i, r := range rows {
		m := Row{}
		for j, c := range cols {
			if j < len(names) {
				m[names[j]] = r[c]
			} else {
				m[c] = r[c]
			}
		}
		rows[i] = m
	}
	return rows
}

func assignedColumns(set []sqlir.Assignment) []string {
	cols := make([]string, len(set))
	for i, a := range set {
		cols[i] = a.Column
	}
	return cols
}

// kindError is a database error raised where the server is not at hand, such
// as in arithmetic; eval turns it into the server's SQLError.
type kindError struct {
	kind sqlir.ErrorKind
	msg  string
}

func (e kindError) Error() string { return "detest: " + e.msg }

func integer(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int16:
		return int64(n), true
	}
	return 0, false
}

// intArith is integer arithmetic as Postgres does it on bigint: overflow is an
// error rather than a wrap, and division truncates toward zero.
func intArith(op string, l, r int64) (any, error) {
	overflow := kindError{sqlir.NumericValueOutOfRange, "bigint out of range"}
	switch op {
	case "+":
		v := l + r
		if (r > 0 && v < l) || (r < 0 && v > l) {
			return nil, overflow
		}
		return v, nil
	case "-":
		v := l - r
		if (r < 0 && v < l) || (r > 0 && v > l) {
			return nil, overflow
		}
		return v, nil
	case "*":
		if l == 0 || r == 0 {
			return int64(0), nil
		}
		v := l * r
		if v/r != l || (l == -1 && r == math.MinInt64) || (r == -1 && l == math.MinInt64) {
			return nil, overflow
		}
		return v, nil
	case "/", "%":
		if r == 0 {
			return nil, kindError{sqlir.DivisionByZero, "division by zero"}
		}
		if l == math.MinInt64 && r == -1 {
			if op == "%" {
				return int64(0), nil
			}
			return nil, overflow
		}
		if op == "/" {
			return l / r, nil
		}
		return l % r, nil
	}
	return nil, errUnknownExpr{"integer operator " + op}
}
