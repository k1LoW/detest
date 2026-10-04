package detest

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// selItem is one row of a query on its way out: the context its expressions
// are evaluated in (a row of FROM, or a group when the query aggregates) and
// the output row once projected.
type selItem struct {
	ctx *env
	agg *aggEnv // set for a group
	jr  jrow    // the FROM row, for SELECT *
	out Row
}

// evalSelect runs a query in PostgreSQL's order: FROM and WHERE, grouping and
// HAVING, window functions, the select list, ORDER BY, DISTINCT, then OFFSET
// and LIMIT. A query with FOR UPDATE takes the locking path instead, which
// PostgreSQL allows only without grouping, DISTINCT and windows.
func (x *sqlExec) evalSelect(sel *sqlir.SelectStmt, outer *env) ([]string, []Row, error) {
	if x.inSelect && x.blocks > 0 && x.tx.db.kind.InnoDB() {
		// A nested query block reads by its own locking clause: a plain
		// subquery of a locking read is a consistent read at Repeatable
		// Read, as InnoDB's locking clause covers its own block only.
		defer func(c bool) { x.consistent = c }(x.consistent)
		x.consistent = sel.Lock == nil && x.tx.consistent()
	}
	x.blocks++
	if len(sel.With) > 0 {
		// The CTEs a query block declares shadow outer ones of the same
		// name inside it only.
		defer func(ctes map[string][]Row) { x.ctes = ctes }(maps.Clone(x.ctes))
	}
	var locking lockPlan
	if len(sel.GroupBy) == 0 && sel.Having == nil && orderOnlyAggregate(sel) && !slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return hasAggregate(t.Expr) }) {
		return nil, nil, x.unsupported("an aggregate only in ORDER BY")
	}
	if sel.Lock != nil && (sel.SetOp != "" || sel.Values != nil) {
		return nil, nil, x.unsupported("FOR UPDATE on a set operation or VALUES")
	}
	if sel.Lock != nil {
		// Checked before the WITH queries run, which may have effects.
		var err error
		if locking, err = x.planLocking(sel); err != nil {
			return nil, nil, err
		}
	}
	for _, vs := range sel.Values {
		// Postgres refuses lists of different lengths when it analyzes the
		// statement, before evaluating anything.
		if len(vs) != len(sel.Values[0]) {
			return nil, nil, x.tx.db.kind.Error(sqlir.SyntaxError, "VALUES lists must all be the same length", "", "", "")
		}
	}
	if err := x.withCTEs(sel.With, outer); err != nil {
		return nil, nil, err
	}
	// OFFSET and LIMIT are taken, and a negative one refused, before the
	// query produces a row, as Postgres does before fetching rows. They may
	// read the WITH queries, so those come first.
	offset, limit, err := x.evalBounds(sel, outer)
	if err != nil {
		return nil, nil, err
	}
	if x.bounds == nil {
		x.bounds = map[*sqlir.SelectStmt][2]int{}
	}
	x.bounds[sel] = [2]int{offset, limit}
	switch {
	case sel.SetOp != "":
		return x.evalSetOp(sel, outer)
	case sel.Values != nil:
		return x.evalValues(sel, outer)
	case sel.Lock != nil:
		return x.evalLocking(sel, locking, outer)
	case x.tx.db.kind.InnoDB() && x.tx.iso == Serializable && x.tx.block:
		// InnoDB's Serializable turns every plain read of a transaction
		// block into a locking read in share mode, subqueries and CTEs too.
		locked := *sel
		locked.Lock = &sqlir.LockClause{Strength: "share"}
		plan, err := x.planLocking(&locked)
		if err != nil {
			return nil, nil, err
		}
		return x.evalLocking(&locked, plan, outer)
	}
	rows, err := x.scan(sel, outer)
	if err != nil {
		return nil, nil, err
	}
	if rows, err = x.where(sel, rows, outer); err != nil {
		return nil, nil, err
	}
	return x.compute(sel, rows, outer)
}

// compute is the rest of a query over the rows its FROM and WHERE found:
// grouping, window functions, the select list, ORDER BY, DISTINCT, LIMIT.
func (x *sqlExec) compute(sel *sqlir.SelectStmt, rows []jrow, outer *env) ([]string, []Row, error) {
	var items []*selItem
	var err error
	if isAggregate(sel) {
		if items, err = x.groups(sel, rows, outer); err != nil {
			return nil, nil, err
		}
	} else {
		for _, r := range rows {
			items = append(items, &selItem{ctx: r.env(outer), jr: r})
		}
	}
	if wins := windowsOf(sel); len(wins) > 0 {
		if err := x.computeWindows(wins, items); err != nil {
			return nil, nil, err
		}
	}
	cols, err := x.projectItems(sel, items, rows)
	if err != nil {
		return nil, nil, err
	}
	if err := x.sortItems(sel.OrderBy, items, cols); err != nil {
		return nil, nil, err
	}
	if items, err = x.distinct(sel, items, cols); err != nil {
		return nil, nil, err
	}
	out := make([]Row, len(items))
	for i, it := range items {
		out[i] = it.out
	}
	out, err = x.slice(sel, out, outer)
	return cols, out, err
}

func (x *sqlExec) where(sel *sqlir.SelectStmt, rows []jrow, outer *env) ([]jrow, error) {
	if sel.Where == nil {
		return rows, nil
	}
	var kept []jrow
	for _, r := range rows {
		ok, err := x.evalBool(sel.Where, r.env(outer))
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

func isAggregate(sel *sqlir.SelectStmt) bool {
	if len(sel.GroupBy) > 0 || sel.Having != nil {
		return true
	}
	for _, t := range sel.Targets {
		if hasAggregate(t.Expr) {
			return true
		}
	}
	return orderOnlyAggregate(sel)
}

// orderOnlyAggregate reports an aggregate in ORDER BY of a query that has
// none elsewhere. Postgres makes such a query one group and refuses any
// column the select list reads outside an aggregate, a check detest does
// not make, so evalSelect refuses the query instead.
func orderOnlyAggregate(sel *sqlir.SelectStmt) bool {
	return slices.ContainsFunc(sel.OrderBy, func(k sqlir.OrderKey) bool { return hasAggregate(k.Expr) })
}

// groups folds the rows into groups by GROUP BY and keeps those HAVING
// accepts. Without GROUP BY the whole input is one group, even when empty.
func (x *sqlExec) groups(sel *sqlir.SelectStmt, rows []jrow, outer *env) ([]*selItem, error) {
	type group struct{ rows []jrow }
	var gs []*group
	index := map[string]*group{}
	for _, r := range rows {
		var kb strings.Builder
		for _, g := range sel.GroupBy {
			v, err := x.eval(g, r.env(outer))
			if err != nil {
				return nil, err
			}
			valuesKey(&kb, v)
		}
		k := kb.String()
		g, ok := index[k]
		if !ok {
			g = &group{}
			index[k] = g
			gs = append(gs, g)
		}
		g.rows = append(g.rows, r)
	}
	if len(gs) == 0 && len(sel.GroupBy) == 0 {
		gs = append(gs, &group{})
	}
	var items []*selItem
	for _, g := range gs {
		sample := jrow{by: map[string]Row{}, merged: Row{}}
		if len(g.rows) > 0 {
			sample = g.rows[0]
		}
		genv := &aggEnv{env: sample.env(outer), rows: g.rows, x: x}
		if sel.Having != nil {
			ok, err := x.evalBoolAgg(sel.Having, genv)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		items = append(items, &selItem{ctx: genv.env, agg: genv, jr: sample})
	}
	return items, nil
}

// value evaluates e for an item: over its group when the query aggregates.
func (x *sqlExec) value(it *selItem, e sqlir.Expr) (any, error) {
	if it.agg != nil {
		return x.evalAgg(e, it.agg)
	}
	return x.eval(e, it.ctx)
}

func (x *sqlExec) projectItems(sel *sqlir.SelectStmt, items []*selItem, rows []jrow) ([]string, error) {
	keys := outputKeys(sel.Targets)
	var cols []string
	for i, t := range sel.Targets {
		if t.Star {
			cols = append(cols, starColumns(t, rows)...)
			continue
		}
		cols = append(cols, keys[i])
	}
	for _, it := range items {
		o := Row{}
		for i, t := range sel.Targets {
			if t.Star {
				src := it.jr.merged
				if t.Table != "" {
					src = it.jr.by[t.Table]
				}
				for k, v := range src {
					if k != "_key" {
						o[k] = v
					}
				}
				continue
			}
			v, err := x.value(it, t.Expr)
			if err != nil {
				return nil, x.unsupportedExpr(err, "in the select list")
			}
			o[keys[i]] = v
		}
		it.out = o
	}
	return cols, nil
}

func starColumns(t sqlir.Target, rows []jrow) []string {
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
	all := make([]string, 0, len(set))
	for k := range set {
		all = append(all, k)
	}
	sort.Strings(all)
	return all
}

// outputValue resolves an ORDER BY or DISTINCT ON item as PostgreSQL does: an
// output column name or position, else an expression over the input.
func (x *sqlExec) outputValue(it *selItem, e sqlir.Expr, cols []string) (any, error) {
	switch v := e.(type) {
	case *sqlir.ColumnRef:
		if v.Table == "" {
			if val, ok := it.out[v.Column]; ok {
				return val, nil
			}
		}
	case *sqlir.Const:
		if n, ok := v.Value.(int64); ok && n >= 1 && int(n) <= len(cols) {
			return it.out[cols[n-1]], nil
		}
	}
	if it.ctx == nil {
		return x.eval(e, &env{merged: it.out})
	}
	return x.value(it, e)
}

func (x *sqlExec) sortItems(keys []sqlir.OrderKey, items []*selItem, cols []string) error {
	if len(keys) == 0 {
		return nil
	}
	vals := make([][]any, len(items))
	for i, it := range items {
		vals[i] = make([]any, len(keys))
		for j, k := range keys {
			v, err := x.outputValue(it, k.Expr, cols)
			if err != nil {
				return x.unsupportedExpr(err, "in ORDER BY")
			}
			vals[i][j] = v
		}
	}
	idx := make([]int, len(items))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return orderedBefore(keys, vals[idx[a]], vals[idx[b]]) })
	sorted := make([]*selItem, len(items))
	for i, j := range idx {
		sorted[i] = items[j]
	}
	copy(items, sorted)
	return nil
}

// orderedBefore reports whether a sorts before b under keys, placing NULLs
// last when ascending and first when descending unless NULLS says otherwise.
func orderedBefore(keys []sqlir.OrderKey, a, b []any) bool {
	for j, k := range keys {
		va, vb := derefValue(a[j]), derefValue(b[j])
		nullsFirst := k.Nulls == sqlir.NullsFirst || (k.Nulls == 0 && k.Desc)
		switch {
		case va == nil && vb == nil:
			continue
		case va == nil:
			return nullsFirst
		case vb == nil:
			return !nullsFirst
		}
		c, ok := compareValues(va, vb)
		if !ok || c == 0 {
			continue
		}
		if k.Desc {
			return c > 0
		}
		return c < 0
	}
	return false
}

// distinct keeps the first item of each DISTINCT ON key, or of each distinct
// output row, in the order ORDER BY left them.
func (x *sqlExec) distinct(sel *sqlir.SelectStmt, items []*selItem, cols []string) ([]*selItem, error) {
	if !sel.Distinct && len(sel.DistinctOn) == 0 {
		return items, nil
	}
	seen := map[string]bool{}
	var kept []*selItem
	for _, it := range items {
		var kb strings.Builder
		if len(sel.DistinctOn) > 0 {
			for _, e := range sel.DistinctOn {
				v, err := x.outputValue(it, e, cols)
				if err != nil {
					return nil, err
				}
				valuesKey(&kb, v)
			}
		} else {
			for _, c := range cols {
				valuesKey(&kb, it.out[c])
			}
		}
		if k := kb.String(); !seen[k] {
			seen[k] = true
			kept = append(kept, it)
		}
	}
	return kept, nil
}

// slice applies OFFSET and LIMIT.
func (x *sqlExec) slice(sel *sqlir.SelectStmt, rows []Row, outer *env) ([]Row, error) {
	offset, limit, err := x.offsetLimit(sel, outer)
	if err != nil {
		return nil, err
	}
	rows = rows[min(offset, len(rows)):]
	if limit >= 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows, nil
}

// ordersByAlias reports whether ORDER BY names a select alias, which may
// order by another expression than the column of the same name.
func ordersByAlias(sel *sqlir.SelectStmt) bool {
	for _, k := range sel.OrderBy {
		c, ok := k.Expr.(*sqlir.ColumnRef)
		if !ok || c.Table != "" {
			continue
		}
		for _, t := range sel.Targets {
			if t.Alias != "" && strings.EqualFold(t.Alias, c.Column) {
				return true
			}
		}
	}
	return false
}

// stopAt is where a scan with LIMIT stops: after the rows LIMIT and OFFSET
// take together. It is nil without a LIMIT.
func (x *sqlExec) stopAt(order []sqlir.OrderKey, limit, offset sqlir.Expr) (*scanStop, error) {
	n, err := x.count(limit, "LIMIT", nil)
	if err != nil || n < 0 {
		return nil, err
	}
	off, err := x.count(offset, "OFFSET", nil)
	if err != nil {
		return nil, err
	}
	return &scanStop{order: order, n: n + max(off, 0)}, nil
}

// count evaluates a LIMIT or OFFSET, -1 when there is none or it is NULL. A
// value bound to a parameter may be anything, so what is not a count is an
// error rather than a slice out of range.
func (x *sqlExec) count(e sqlir.Expr, what string, outer *env) (int, error) {
	if e == nil {
		return -1, nil
	}
	v, err := x.eval(e, &env{outer: outer})
	if err != nil {
		return 0, err
	}
	if derefValue(v) == nil {
		if x.tx.db.kind.InnoDB() {
			return 0, nil // MySQL reads a NULL count bound to a parameter as 0
		}
		return -1, nil // Postgres's LIMIT NULL is no limit
	}
	f, ok := toFloat(derefValue(v))
	if !ok || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		kind := sqlir.InvalidRowCountInLimit
		if what == "OFFSET" {
			kind = sqlir.InvalidRowCountInOffset
		}
		return 0, x.tx.db.kind.Error(kind, what+" must be a non-negative integer", "", "", "")
	}
	if f > math.MaxInt32 {
		return math.MaxInt32, nil // more than any table here holds, and an int cannot overflow
	}
	return int(f), nil
}

// finish orders, then slices, rows that are already output rows: those of a
// set operation or a VALUES list.
func (x *sqlExec) finish(sel *sqlir.SelectStmt, cols []string, rows []Row, outer *env) ([]string, []Row, error) {
	items := make([]*selItem, len(rows))
	for i, r := range rows {
		items[i] = &selItem{out: r}
	}
	if err := x.sortItems(sel.OrderBy, items, cols); err != nil {
		return nil, nil, err
	}
	for i, it := range items {
		rows[i] = it.out
	}
	rows, err := x.slice(sel, rows, outer)
	return cols, rows, err
}

func (x *sqlExec) evalValues(sel *sqlir.SelectStmt, outer *env) ([]string, []Row, error) {
	var cols []string
	if len(sel.Values) > 0 {
		for i := range sel.Values[0] {
			cols = append(cols, fmt.Sprintf("column%d", i+1))
		}
	}
	en := outer
	if en == nil {
		en = &env{}
	}
	rows := make([]Row, 0, len(sel.Values))
	for _, vs := range sel.Values {
		r := Row{}
		for i, e := range vs {
			v, err := x.eval(e, en)
			if err != nil {
				return nil, nil, err
			}
			if i < len(cols) {
				r[cols[i]] = v
			}
		}
		rows = append(rows, r)
	}
	return x.finish(sel, cols, rows, outer)
}

// setColumnFloat reports whether column i of a set operation is a numeric
// or a float by one of the queries' select lists, as floatTyped tells.
func setColumnFloat(sel *sqlir.SelectStmt, i int) bool {
	if sel.SetOp != "" {
		return setColumnFloat(sel.Larg, i) || setColumnFloat(sel.Rarg, i)
	}
	return i < len(sel.Targets) && !sel.Targets[i].Star && floatTyped(sel.Targets[i].Expr)
}

// evalSetOp combines two queries by column position, with the left one's
// column names. UNION, INTERSECT and EXCEPT remove duplicates; with ALL they
// keep them as a multiset.
func (x *sqlExec) evalSetOp(sel *sqlir.SelectStmt, outer *env) ([]string, []Row, error) {
	lcols, lrows, err := x.evalSelect(sel.Larg, outer)
	if err != nil {
		return nil, nil, err
	}
	rcols, rrows, err := x.evalSelect(sel.Rarg, outer)
	if err != nil {
		return nil, nil, err
	}
	if len(lcols) != len(rcols) {
		return nil, nil, x.tx.db.kind.Error(sqlir.SyntaxError, fmt.Sprintf("each %s query must have the same number of columns", strings.ToUpper(sel.SetOp)), "", "", "")
	}
	for i, r := range rrows {
		m := Row{}
		for j, c := range rcols {
			m[lcols[j]] = r[c]
		}
		rrows[i] = m
	}
	// Postgres gives each column of a set operation one type, so '1' under
	// an integer column is the integer 1, and text or a boolean under it is
	// an error. detest keeps values, which compare as they are here, so a
	// column that holds values of two kinds is refused.
	for i, c := range lcols {
		kinds := map[string]bool{}
		for _, r := range slices.Concat(lrows, rrows) {
			if k := valueKind(r[c]); k != "" {
				kinds[k] = true
			}
		}
		if len(kinds) > 1 {
			return nil, nil, x.unsupported("a set operation over a column of values of two types")
		}
		// An integer under a numeric or a float takes the column's type,
		// so 1 UNION ALL 3.0 gives numerics and x / 2 is 0.5 for both. The
		// type is the queries' whether or not a float row survives them.
		rows := slices.Concat(lrows, rrows)
		if kinds["number"] && (setColumnFloat(sel, i) || slices.ContainsFunc(rows, func(r Row) bool { return isFloat(r[c]) })) {
			for _, r := range rows {
				if n, ok := integer(derefValue(r[c])); ok {
					r[c] = float64(n)
				}
			}
		}
	}
	key := func(r Row) string {
		var kb strings.Builder
		for _, c := range lcols {
			valuesKey(&kb, r[c])
		}
		return kb.String()
	}
	var out []Row
	switch sel.SetOp {
	case "union":
		out = append(append(out, lrows...), rrows...)
		if !sel.SetAll {
			seen := map[string]bool{}
			var kept []Row
			for _, r := range out {
				if k := key(r); !seen[k] {
					seen[k] = true
					kept = append(kept, r)
				}
			}
			out = kept
		}
	case "intersect", "except":
		right := map[string]int{}
		for _, r := range rrows {
			right[key(r)]++
		}
		emitted := map[string]bool{}
		for _, r := range lrows {
			k := key(r)
			in := right[k] > 0
			if sel.SetOp == "except" {
				in = !in
			}
			switch {
			case sel.SetAll && sel.SetOp == "intersect" && in:
				right[k]--
				out = append(out, r)
			case sel.SetAll && sel.SetOp == "except":
				if right[k] > 0 {
					right[k]--
				} else {
					out = append(out, r)
				}
			case !sel.SetAll && in && !emitted[k]:
				emitted[k] = true
				out = append(out, r)
			}
		}
	}
	return x.finish(sel, lcols, out, outer)
}

// evalLocking runs a query with FOR UPDATE or FOR SHARE: ORDER BY before
// locking, and OFFSET and LIMIT after, so they count the rows actually locked
// (FOR UPDATE SKIP LOCKED skips rows held by others) and the rows OFFSET
// skips are locked as well. On InnoDB the first table's search takes its
// next-key locks first, and an aggregate is computed over the rows locked.
func (x *sqlExec) evalLocking(sel *sqlir.SelectStmt, plan lockPlan, outer *env) ([]string, []Row, error) {
	var stop *scanStop // where the range locks stopped, which the rows follow
	var err error
	if x.tx.db.kind.InnoDB() {
		// A locking read reads the latest rows, also as a subquery of a
		// plain read that reads a snapshot.
		defer func(c bool) { x.consistent = c }(x.consistent)
		x.consistent = false
		if stop, err = x.rangeLocks(sel, plan); err != nil {
			return nil, nil, err
		}
	}
	rows, err := x.scan(sel, outer)
	if err != nil {
		return nil, nil, err
	}
	if rows, err = x.where(sel, rows, outer); err != nil {
		return nil, nil, err
	}
	if stop != nil {
		f := sel.From
		if err := x.inScanOrder(x.tx.db.resolve(f.Name), cmp.Or(f.Alias, relname(f.Name)), sel.Where, stop, rows); err != nil {
			return nil, nil, err
		}
	}
	if isAggregate(sel) || sel.Distinct || len(sel.DistinctOn) > 0 || len(windowsOf(sel)) > 0 {
		// planLocking lets these through on InnoDB only, which locks the
		// rows the query reads and computes the rest over them.
		locked, err := x.lockRows(sel, plan, rows, 0, -1, outer)
		if err != nil {
			return nil, nil, err
		}
		return x.compute(sel, locked, outer)
	}
	if err := x.order(sel.OrderBy, rows, outer); err != nil {
		return nil, nil, err
	}
	offset, limit, err := x.offsetLimit(sel, outer)
	if err != nil {
		return nil, nil, err
	}
	locked, err := x.lockRows(sel, plan, rows, offset, limit, outer)
	if err != nil {
		return nil, nil, err
	}
	return x.project(sel, locked, outer)
}

// lockRows locks the rows a locking read found, in order, with the locking
// clause's wait policy, and returns those it locked as they are now, past
// offset and up to limit (-1 for none).
func (x *sqlExec) lockRows(sel *sqlir.SelectStmt, plan lockPlan, rows []jrow, offset, limit int, outer *env) ([]jrow, error) {
	from, targets := plan.from, plan.targets
	mode := lockModeOf(sel.Lock)
	var locked []jrow
	skipped := 0 // rows locked for OFFSET, which Postgres locks too
rows:
	for _, r := range rows {
		if limit >= 0 && len(locked) >= limit {
			break
		}
		latest := map[string]Row{}
		changed := false
		for _, a := range targets {
			table := from.tables[a]
			_, cur, ok, err := x.tx.lockLatest(table, r.by[a].Key(), func(lk lockKey) error {
				if x.skipped[lk] && sel.Lock.SkipLocked {
					return errSkipLocked
				}
				if (sel.Lock.SkipLocked || sel.Lock.NoWait) && x.tx.heldByOther(lk, mode) {
					if sel.Lock.SkipLocked {
						return errSkipLocked
					}
					if sel.Lock.NoWait {
						return x.tx.db.kind.Error(sqlir.LockNotAvailable, fmt.Sprintf("could not obtain lock on row in relation %q", relname(table)), relname(table), "", "")
					}
				}
				return x.tx.lockMode(lk, mode)
			})
			if errors.Is(err, errSkipLocked) {
				continue rows
			}
			if err != nil {
				return nil, err
			}
			if !ok {
				continue rows // deleted while waited for
			}
			latest[a] = cur
			changed = changed || !reflect.DeepEqual(cur, r.by[a])
		}
		// Postgres re-evaluates the predicate only for a row another
		// transaction changed, which matters for a volatile one.
		if changed {
			var ok bool
			var err error
			if r, ok, err = x.recheck(sel, r, latest, from, outer); err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		if skipped < offset {
			skipped++
			continue
		}
		locked = append(locked, r)
	}
	return locked, nil
}

// rangeLocks takes InnoDB's next-key locks of the first table's search, and
// of a joined table's lookup for each row the first table's search finds.
// Which table MySQL reads first is its optimizer's choice, except when the
// first one is a const table, looked up by a unique key, which it reads
// before the others; a locking join of any other shape is refused.
func (x *sqlExec) rangeLocks(sel *sqlir.SelectStmt, plan lockPlan) (*scanStop, error) {
	f := sel.From
	if f == nil || f.Name == "" || x.isCTE(f.Name) {
		return nil, nil
	}
	alias := cmp.Or(f.Alias, relname(f.Name))
	table := x.tx.db.resolve(f.Name)
	mode := lockModeOf(sel.Lock)
	if len(sel.Joins) > 0 && (x.tx.iso == RepeatableRead || x.tx.iso == Serializable) {
		return nil, x.joinLocks(sel, plan, table, alias, mode)
	}
	if !slices.Contains(plan.targets, alias) {
		return nil, nil // FOR UPDATE OF names other tables
	}
	var stop *scanStop
	if len(sel.Joins) == 0 && len(sel.GroupBy) == 0 && !isAggregate(sel) && !sel.Distinct && len(windowsOf(sel)) == 0 && !ordersByAlias(sel) {
		var err error
		if stop, err = x.stopAt(sel.OrderBy, sel.Limit, sel.Offset); err != nil {
			return nil, err
		}
	}
	return stop, x.nextKeyLocks(table, alias, sel.Where, mode, sel.Lock, stop)
}

// joinLocks takes the next-key locks of a locking read joining a const table,
// first, with one more: the first table's lookup, then the joined table's
// search by ON and WHERE with each row found, as MySQL's nested loop runs
// them.
func (x *sqlExec) joinLocks(sel *sqlir.SelectStmt, plan lockPlan, table, alias string, mode lockMode) error {
	if len(sel.Joins) > 1 {
		return x.unsupported("a locking read joining more than two tables, whose join order MySQL's optimizer chooses")
	}
	j := sel.Joins[0]
	if j.Table.Name == "" || x.isCTE(j.Table.Name) {
		return x.unsupported("a locking read joining a subquery, a CTE or a function")
	}
	if sel.Limit != nil || sel.Offset != nil {
		return x.unsupported("a locking read joining tables with LIMIT or OFFSET, which stop MySQL's nested loop partway")
	}
	sr := x.searchRange(table, alias, sel.Where)
	if !sr.unique || len(sr.ranges) != 1 {
		return x.unsupported("a locking read joining tables whose first table is not looked up by one value of a unique key, so MySQL's optimizer chooses which table it reads first")
	}
	jalias := cmp.Or(j.Table.Alias, relname(j.Table.Name))
	jtable := x.tx.db.resolve(j.Table.Name)
	where := j.On
	if sel.Where != nil {
		where = &sqlir.BinaryExpr{Op: "AND", L: j.On, R: sel.Where}
	}
	// outer is the const row MySQL reads first, if it is there and passes
	// the conditions on the first table alone; without one, MySQL reads
	// nothing of the joined table.
	var own []sqlir.Expr
	for _, c := range splitAnd(sel.Where) {
		if !slices.ContainsFunc(sqlir.ColumnRefs(c), func(c *sqlir.ColumnRef) bool { return x.searchedColumn(c, jtable, jalias) }) {
			own = append(own, c)
		}
	}
	outer := func() ([]Row, error) {
		var out []Row
	rows:
		for _, r := range x.tx.selectNoYield(table, nil) {
			if !sr.ranges[0].contains(keyOf(r, sr.key)) {
				continue
			}
			for _, c := range own {
				if ok, err := x.evalBool(c, newJrow(alias, r).env(nil)); err != nil {
					return nil, err
				} else if !ok {
					continue rows
				}
			}
			out = append(out, r)
		}
		return out, nil
	}
	defer func() { x.searchOuter = nil }()
	locksJoined := slices.Contains(plan.targets, jalias)
	// Checked before anything is locked, as a refused statement must leave
	// the transaction as it was.
	if err := x.checkSearch(table, alias, sel.Where); err != nil {
		return err
	}
	if locksJoined {
		rows, err := outer()
		if err != nil {
			return err
		}
		for _, r := range rows {
			x.searchOuter = &searchOuter{table: jtable, alias: jalias, env: newJrow(alias, r).env(nil)}
			if err := x.checkSearch(jtable, jalias, where); err != nil {
				return err
			}
		}
	}
	x.searchOuter = nil
	if slices.Contains(plan.targets, alias) {
		if err := x.nextKeyLocks(table, alias, sel.Where, mode, sel.Lock, nil); err != nil {
			return err
		}
	}
	if !locksJoined {
		return nil
	}
	rows, err := outer() // as they are once the first table's lock is granted
	if err != nil {
		return err
	}
	for _, r := range rows {
		x.searchOuter = &searchOuter{table: jtable, alias: jalias, env: newJrow(alias, r).env(nil)}
		if err := x.nextKeyLocks(jtable, jalias, where, mode, sel.Lock, nil); err != nil {
			return err
		}
	}
	return nil
}

// isCTE reports whether name is a CTE of the statement, which shadows a table
// of the same name. An empty CTE has no rows, so its presence is the key's.
func (x *sqlExec) isCTE(name string) bool {
	_, ok := x.ctes[name]
	return ok
}

// lockPlan is what a locking read locks, worked out before it runs.
type lockPlan struct {
	from    fromItems
	targets []string
}

// planLocking checks a locking read and works out the FROM items it locks,
// from the statement alone.
func (x *sqlExec) planLocking(sel *sqlir.SelectStmt) (lockPlan, error) {
	// InnoDB locks the rows such a query reads; Postgres refuses it.
	if (isAggregate(sel) || sel.Distinct || len(sel.DistinctOn) > 0 || len(windowsOf(sel)) > 0) && !x.tx.db.kind.InnoDB() {
		return lockPlan{}, x.unsupported("FOR UPDATE with GROUP BY, DISTINCT or window functions")
	}
	// Postgres allows neither in WHERE or ON at all.
	preds := []sqlir.Expr{sel.Where}
	for _, j := range sel.Joins {
		preds = append(preds, j.On)
	}
	for _, p := range preds {
		if hasAggregate(p) || len(windowsIn(p)) > 0 {
			return lockPlan{}, x.unsupported("an aggregate or window function in WHERE or ON")
		}
	}
	from := x.fromItemsOf(sel)
	targets, err := x.lockTargets(sel.Lock, from)
	return lockPlan{from: from, targets: targets}, err
}

// lockTargets is the FROM items whose rows a locking clause locks: those it
// names, or every table in FROM.
func (x *sqlExec) lockTargets(l *sqlir.LockClause, from fromItems) ([]string, error) {
	names := l.Of
	if len(names) == 0 {
		names = from.aliases
	}
	if from.lateral {
		// A lateral item's rows come from the rows before it, which the
		// re-check after a wait would have to feed again.
		return nil, x.unsupported("FOR UPDATE with LATERAL or a function in FROM")
	}
	var out []string
	for _, a := range names {
		if !slices.Contains(from.aliases, a) {
			return nil, x.tx.db.kind.Error(sqlir.UndefinedTable, fmt.Sprintf("relation %q in FOR UPDATE clause not found in FROM clause", a), a, "", "")
		}
		if kind := from.kinds[a]; kind != "" {
			// Postgres locks the tables behind a view or subquery, which
			// detest does not. A WITH query or a function it leaves alone
			// unless named, and then refuses.
			if (kind == "cte" || kind == "function") && len(l.Of) == 0 {
				continue
			}
			return nil, x.unsupported("FOR UPDATE on a " + kind)
		}
		if from.nullable[a] {
			return nil, x.unsupported("FOR UPDATE on the nullable side of an outer join")
		}
		out = append(out, a)
	}
	return out, nil
}

// recheck evaluates the predicate again with the locked rows of r at their
// newest version, latest, as Read Committed does after a wait: a row no
// longer matching is not returned.
func (x *sqlExec) recheck(sel *sqlir.SelectStmt, r jrow, latest map[string]Row, from fromItems, outer *env) (jrow, bool, error) {
	if len(latest) == 0 {
		return r, true, nil
	}
	by := maps.Clone(r.by)
	maps.Copy(by, latest)
	// Join again in FROM order, as scan does, with the rows each item was
	// joined to: Postgres does not look for new join partners either.
	n := newJrow(from.aliases[0], by[from.aliases[0]])
	if from.tables[from.aliases[0]] == "" {
		n.base = nil
	}
	for i, join := range sel.Joins {
		a := from.aliases[i+1]
		cand := n.with(a, by[a])
		if join.On != nil && by[a] != nil {
			ok, err := x.evalBool(join.On, cand.env(outer))
			if err != nil {
				return jrow{}, false, err
			}
			if !ok {
				if join.Kind != sqlir.LeftJoin {
					return jrow{}, false, nil
				}
				cand = n.with(a, nil)
			}
		}
		n = cand
	}
	if sel.Where != nil {
		ok, err := x.evalBool(sel.Where, n.env(outer))
		if err != nil || !ok {
			return jrow{}, false, err
		}
	}
	return n, true, nil
}

// errSkipLocked stops lockLatest at a row SKIP LOCKED passes over.
var errSkipLocked = errors.New("row skipped by SKIP LOCKED")

// lockModeOf is the row lock a locking clause takes.
func lockModeOf(l *sqlir.LockClause) lockMode {
	switch l.Strength {
	case "no key update":
		return lockNoKeyUpdate
	case "share":
		return lockShare
	case "key share":
		return lockKeyShare
	}
	return lockUpdate
}

// windowsOf returns the window functions of the select list, ORDER BY and
// DISTINCT ON.
func windowsOf(sel *sqlir.SelectStmt) []*sqlir.WindowFunc {
	var out []*sqlir.WindowFunc
	for _, t := range sel.Targets {
		out = append(out, windowsIn(t.Expr)...)
	}
	for _, k := range sel.OrderBy {
		out = append(out, windowsIn(k.Expr)...)
	}
	for _, e := range sel.DistinctOn {
		out = append(out, windowsIn(e)...)
	}
	return out
}

// windowsIn returns the window functions in e, not those of its subqueries.
func windowsIn(e sqlir.Expr) []*sqlir.WindowFunc {
	var out []*sqlir.WindowFunc
	var walk func(e sqlir.Expr)
	walk = func(e sqlir.Expr) {
		switch v := e.(type) {
		case *sqlir.WindowFunc:
			out = append(out, v)
		case *sqlir.FuncCall:
			for _, a := range v.Args {
				walk(a)
			}
		case *sqlir.BinaryExpr:
			walk(v.L)
			walk(v.R)
		case *sqlir.UnaryExpr:
			walk(v.X)
		case *sqlir.Cast:
			walk(v.X)
		case *sqlir.IsNull:
			walk(v.X)
		case *sqlir.InExpr:
			walk(v.X)
			for _, e := range v.List {
				walk(e)
			}
		case *sqlir.RowExpr:
			for _, e := range v.Items {
				walk(e)
			}
		case *sqlir.CaseExpr:
			walk(v.Arg)
			for _, w := range v.Whens {
				walk(w.When)
				walk(w.Then)
			}
			walk(v.Else)
		}
	}
	walk(e)
	return out
}

// computeWindows evaluates each window function for every item and keeps the
// value in the item's context, where eval finds it.
func (x *sqlExec) computeWindows(wins []*sqlir.WindowFunc, items []*selItem) error {
	for _, it := range items {
		if it.ctx.win == nil {
			it.ctx.win = map[*sqlir.WindowFunc]any{}
		}
	}
	for _, w := range wins {
		var parts [][]*selItem
		index := map[string]int{}
		for _, it := range items {
			var kb strings.Builder
			for _, e := range w.Partition {
				v, err := x.value(it, e)
				if err != nil {
					return err
				}
				valuesKey(&kb, v)
			}
			k := kb.String()
			i, ok := index[k]
			if !ok {
				i = len(parts)
				index[k] = i
				parts = append(parts, nil)
			}
			parts[i] = append(parts[i], it)
		}
		for _, part := range parts {
			if err := x.computeWindow(w, part); err != nil {
				return err
			}
		}
	}
	return nil
}

func (x *sqlExec) computeWindow(w *sqlir.WindowFunc, part []*selItem) error {
	keys := make([][]any, len(part))
	for i, it := range part {
		keys[i] = make([]any, len(w.Order))
		for j, k := range w.Order {
			v, err := x.value(it, k.Expr)
			if err != nil {
				return err
			}
			keys[i][j] = v
		}
	}
	idx := make([]int, len(part))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return orderedBefore(w.Order, keys[idx[a]], keys[idx[b]]) })
	sorted := make([]*selItem, len(part))
	skeys := make([][]any, len(part))
	for i, j := range idx {
		sorted[i], skeys[i] = part[j], keys[j]
	}
	peer := func(a, b int) bool {
		return !orderedBefore(w.Order, skeys[a], skeys[b]) && !orderedBefore(w.Order, skeys[b], skeys[a])
	}
	// lastPeer[i] is the last row sharing row i's ORDER BY values: the end of
	// the default frame.
	lastPeer := make([]int, len(sorted))
	for i := len(sorted) - 1; i >= 0; i-- {
		lastPeer[i] = i
		if i+1 < len(sorted) && peer(i, i+1) {
			lastPeer[i] = lastPeer[i+1]
		}
	}
	arg := func(it *selItem, n int) (any, error) {
		if n >= len(w.Func.Args) {
			return nil, nil
		}
		return x.value(it, w.Func.Args[n])
	}
	rank, dense := 0, 0
	for i, it := range sorted {
		frameEnd := len(sorted) - 1
		if len(w.Order) > 0 && !w.Whole {
			frameEnd = lastPeer[i]
		}
		if i == 0 || !peer(i-1, i) {
			rank, dense = i+1, dense+1
		}
		var v any
		switch name := w.Func.Name; name {
		case "row_number":
			v = int64(i + 1)
		case "rank":
			v = int64(rank)
		case "dense_rank":
			v = int64(dense)
		case "count", "sum", "min", "max", "avg":
			var vals []any
			n := 0
			for _, f := range sorted[:frameEnd+1] {
				n++
				if w.Func.Star {
					continue
				}
				a, err := arg(f, 0)
				if err != nil {
					return err
				}
				if derefValue(a) != nil {
					vals = append(vals, x.aggOperand(name, a))
				}
			}
			v = foldAggregate(name, w.Func.Star, vals, n)
		case "first_value", "last_value":
			at := 0
			if name == "last_value" {
				at = frameEnd
			}
			var err error
			if v, err = arg(sorted[at], 0); err != nil {
				return err
			}
		case "lag", "lead":
			off := int64(1)
			if len(w.Func.Args) > 1 {
				o, err := arg(it, 1)
				if err != nil {
					return err
				}
				n, ok := toInt64(derefValue(o))
				if x.tx.db.kind.InnoDB() && (!ok || n < 0) {
					return x.unsupported(name + " with an offset other than a nonnegative integer")
				}
				if ok {
					off = n
				}
			}
			at := i - int(off)
			if name == "lead" {
				at = i + int(off)
			}
			var err error
			if at >= 0 && at < len(sorted) {
				v, err = arg(sorted[at], 0)
			} else {
				v, err = arg(it, 2)
			}
			if err != nil {
				return err
			}
		default:
			return x.unsupported("window function " + name)
		}
		it.ctx.win[w] = v
	}
	return nil
}

// aggOperand is a value SUM or AVG adds up: on MySQL, a string as the
// number it converts to, which foldAggregate would otherwise count as 0.
func (x *sqlExec) aggOperand(name string, v any) any {
	if (name == "sum" || name == "avg") && x.tx.db.kind.InnoDB() {
		return mysqlArithOperand(v)
	}
	return v
}

// foldAggregate folds the non-NULL values of an aggregate's argument; n is
// the row count, which count(*) returns.
func foldAggregate(name string, star bool, vals []any, n int) any {
	switch name {
	case "count":
		if star {
			return int64(n)
		}
		return int64(len(vals))
	case "sum", "avg":
		if len(vals) == 0 {
			return nil
		}
		total, ints := 0.0, true
		for _, val := range vals {
			f, _ := toFloat(derefValue(val))
			total += f
			if _, ok := integer(derefValue(val)); !ok {
				ints = false
			}
		}
		if name == "avg" {
			return total / float64(len(vals))
		}
		// The sum of floats or numerics stays a float, so dividing it is
		// not integer division.
		if !ints {
			return total
		}
		return numeric(total)
	default:
		if len(vals) == 0 {
			return nil
		}
		best := vals[0]
		for _, val := range vals[1:] {
			c, _ := compareValues(val, best)
			if (name == "min" && c < 0) || (name == "max" && c > 0) {
				best = val
			}
		}
		return best
	}
}

// outputKeys are the keys of the select list's columns in an output row: the
// column names, with a repeated name told apart by a suffix the driver strips
// (Postgres returns both ?column? columns of SELECT 1, 2 under that name).
func outputKeys(targets []sqlir.Target) []string {
	keys := make([]string, len(targets))
	seen := map[string]bool{}
	for i, t := range targets {
		if t.Star {
			continue
		}
		k := targetName(t)
		if seen[k] {
			k = fmt.Sprintf("%s\x00%d", k, i)
		}
		seen[k] = true
		keys[i] = k
	}
	return keys
}
