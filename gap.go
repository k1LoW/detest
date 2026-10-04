package detest

import (
	"fmt"
	"slices"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// InnoDB's next-key locks, as detest models them. At Repeatable Read a
// locking read, an UPDATE or a DELETE locks the index records it scans and
// the gaps around them, so no other transaction can insert a row the
// statement would have seen. detest finds the index a statement searches by
// from its WHERE: the primary key, a unique index or another index, whose
// leading column has a condition it can evaluate before the scan. Equalities
// on the leading columns and a range on the next one bound the search in the
// index's key order, as InnoDB's range scan over a composite index does. It
// locks every row whose key falls in the searched range, matching the rest
// of the WHERE or not, and the gap from the nearest key below the range to
// the nearest above, with that next record. Without such an index it locks
// every row and the whole table, as InnoDB's full scan does. An equality
// search on every column of a unique index that finds its row locks only
// the row. Gap locks never conflict with each other, only with inserts into
// them.

// ixKey is the values of a row in an index's columns, or a prefix of them as
// a range's bound, compared in key order: NULL first, and a key equal to a
// bound in the bound's columns compares equal to it.
type ixKey []any

// keyOf is row's key in the index on cols.
func keyOf(row Row, cols []string) ixKey {
	k := make(ixKey, len(cols))
	for i, c := range cols {
		k[i] = derefValue(row[c])
	}
	return k
}

// keyCompare compares two keys over the columns both have.
func keyCompare(a, b ixKey) int {
	for i := range min(len(a), len(b)) {
		switch x, y := a[i], b[i]; {
		case x == nil && y == nil:
			continue
		case x == nil:
			return -1
		case y == nil:
			return 1
		}
		c, ok := compareValues(mysqlOperands(a[i], b[i]))
		if !ok {
			return 0
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// sameKey reports whether a and b hold the same values in cols.
func sameKey(a, b Row, cols []string) bool {
	return !slices.ContainsFunc(cols, func(c string) bool { return !sameValue(a[c], b[c]) })
}

// hasNull reports a key with NULL in one of its columns, which bounds no gap.
func (k ixKey) hasNull() bool { return slices.Contains(k, nil) }

// valRange is an interval of values, of a column as columnRanges finds them
// or of keys (ixKey) as a search over an index bounds them; an absent bound
// is unbounded.
type valRange struct {
	lo, hi         any
	hasLo, hasHi   bool
	loOpen, hiOpen bool
	// null is the range of NULL alone, which IS NULL searches; NULL sorts
	// before every value in an InnoDB index.
	null bool
}

func (r valRange) contains(v any) bool {
	if r.hasLo {
		c, ok := boundCompare(v, r.lo)
		if !ok || c < 0 || c == 0 && r.loOpen {
			return false
		}
	}
	if r.hasHi {
		c, ok := boundCompare(v, r.hi)
		if !ok || c > 0 || c == 0 && r.hiOpen {
			return false
		}
	}
	return true
}

// boundCompare compares a value with a bound, keys in key order.
func boundCompare(v, bound any) (int, bool) {
	if k, isKey := v.(ixKey); isKey {
		if b, ok := bound.(ixKey); ok {
			return keyCompare(k, b), true
		}
	}
	return compareValues(mysqlOperands(v, bound))
}

// gapLock is a lock on the gaps of an index: the open intervals of keys in
// the index on cols that no other transaction may insert, or all of the
// table when cols is empty.
type gapLock struct {
	tx     *Tx
	table  string
	cols   []string
	ranges []valRange
}

func (g *gapLock) covers(row Row) bool {
	if len(g.cols) == 0 {
		return true
	}
	k := keyOf(row, g.cols)
	// NULL sorts first in an InnoDB index, so a key with one falls in a gap
	// that reaches down to the start of the index.
	return slices.ContainsFunc(g.ranges, func(r valRange) bool { return r.contains(k) })
}

// gapWaitKey is the lock key a transaction waiting to insert into a gap waits
// on; releasing gaps wakes its waiters.
const gapWaitKey = "\x00gap"

// gapHolders returns the transactions other than tx whose gap locks cover
// row in table. With old, the row is an update of old, which enters a gap of
// an index only when it changes the value that index holds.
func (tx *Tx) gapHolders(table string, row, old Row) []*Tx {
	var out []*Tx
	for _, g := range tx.db.gaps {
		// An update enters a gap only by changing the value the gap's index
		// holds; for a whole-table gap that is the primary key, which a
		// changed key inserts anew.
		if old != nil && (len(g.cols) == 0 && old.Key() == row.Key() || len(g.cols) > 0 && sameKey(old, row, g.cols)) {
			continue
		}
		if g.tx != tx && g.table == table && g.covers(row) && !slices.Contains(out, g.tx) {
			out = append(out, g.tx)
		}
	}
	return out
}

// insertIntention waits while another transaction holds a gap the new row
// falls into, as InnoDB's insert intention lock does.
func (tx *Tx) insertIntention(table string, row Row) error {
	return tx.moveIntention(table, row, nil)
}

// moveIntention is insertIntention for an update of old to row: changing the
// value an index holds puts a new entry into it, which waits for a gap lock
// the new value falls into, as an insert does.
func (tx *Tx) moveIntention(table string, row, old Row) error {
	if !tx.db.kind.InnoDB() {
		return nil
	}
	lk := lockKey{table, gapWaitKey}
	for {
		holders := tx.gapHolders(table, row, old)
		if len(holders) == 0 {
			return nil
		}
		if tx.p == nil {
			return fmt.Errorf("detest: an insert outside any process waits for a gap lock held by %s", procName(holders[0].p))
		}
		what := "a gap lock on " + table
		if err := tx.selfWait(holders, what, "a gap lock"); err != nil {
			return err
		}
		if tx.lockTimeout && tx.p.Choose("lock timeout on "+table, 2) == 1 {
			tx.p.r.note(tx.p, "lock timeout waiting for a gap in %s", table)
			return tx.db.kind.Error(sqlir.LockWaitTimeout, "lock wait timeout exceeded", relname(table), "", "")
		}
		if err := tx.breakCycle(holders, what); err != nil {
			return err
		}
		tx.p.blockOnRow(rowWait{key: lk, mode: lockUpdate, tx: tx, insert: row, old: old}, holders[0])
		if err := tx.victim(); err != nil {
			return err
		}
	}
}

// releaseGaps drops tx's gap locks and wakes the inserts waiting on them.
func (tx *Tx) releaseGaps() {
	db := tx.db
	tables := map[string]bool{}
	db.gaps = slices.DeleteFunc(db.gaps, func(g *gapLock) bool {
		if g.tx == tx {
			tables[g.table] = true
			return true
		}
		return false
	})
	if len(tables) == 0 || tx.p == nil {
		return
	}
	for _, p := range tx.p.r.procs {
		if p.state == stateBlockedLock && p.waitRow != nil && p.waitRow.key.key == gapWaitKey && tables[p.waitRow.key.table] {
			p.state, p.waitRow = stateReady, nil
		}
	}
}

// nextKeyLocks takes the record and gap locks a locking read, an UPDATE or a
// DELETE of table with where takes in InnoDB at Repeatable Read or
// Serializable. alias is what where calls the table. policy is a locking
// read's NOWAIT or SKIP LOCKED, which apply to the record locks only: a gap
// lock never waits, so it is taken all the same.
func (x *sqlExec) nextKeyLocks(table, alias string, where sqlir.Expr, mode lockMode, policy *sqlir.LockClause, stop *scanStop) error {
	tx := x.tx
	if !tx.db.kind.InnoDB() || (tx.iso != RepeatableRead && tx.iso != Serializable) || tx.db.ignored[table] {
		return nil
	}
	if err := x.prefixSearch(table, alias, where); err != nil {
		return err
	}
	switch sr := x.searchRange(table, alias, where); {
	case sr.ambiguous:
		// Which index MySQL searches, and so which range it locks, depends
		// on its optimizer's statistics, which detest does not model.
		return x.unsupported("a locking search that more than one index serves")
	case sr.descending:
		return x.unsupported("a locking search by a descending index")
	}
	if stop != nil {
		done, err := x.lockScanTo(table, alias, where, mode, policy, stop)
		if err != nil || done {
			return err
		}
	}
	return x.lockRange(table, alias, where, mode, policy)
}

// scanCompare orders two rows as a scan of the index on cols meets them:
// equal keys of a secondary index by the primary key, pk, which InnoDB
// appends to them.
func scanCompare(cols, pk []string, a, b Row) int {
	c := keyCompare(keyOf(a, cols), keyOf(b, cols))
	if c == 0 {
		c = keyCompare(keyOf(a, pk), keyOf(b, pk))
	}
	if c == 0 {
		c = strings.Compare(a.Key(), b.Key())
	}
	return c
}

// inScanOrder puts the rows of a statement whose LIMIT stops the scan, with
// no ORDER BY, in the order of the index the scan follows, which lockScanTo
// locks along, so the rows it takes are the ones MySQL's scan reaches first
// and, where gaps are locked, the ones whose records and gaps it locked.
func (x *sqlExec) inScanOrder(table, alias string, where sqlir.Expr, stop *scanStop, rows []jrow) {
	if stop == nil || len(stop.order) > 0 || !x.tx.db.kind.InnoDB() {
		return // the scan follows the index at any isolation level, gap locks or not
	}
	sr := x.searchRange(table, alias, where)
	if len(sr.cols) == 0 || len(sr.ranges) != 1 {
		return
	}
	pk := x.tx.db.defs[table].pk
	slices.SortStableFunc(rows, func(a, b jrow) int { return scanCompare(sr.cols, pk, a.by[alias], b.by[alias]) })
}

// scanStop is where a statement with LIMIT stops scanning: after n rows
// that match its WHERE, in the order of order, which the index scan follows
// when it is empty or names the index's first column the search does not fix
// by equality.
type scanStop struct {
	order []sqlir.OrderKey
	n     int
}

// lockScanTo takes the locks of a search that stops after stop.n matching
// rows, as InnoDB's scan of the index stops: the records it scanned, and the
// gap up to the last of them. done is false when the scan is not one that
// stops, by its index or its order, or runs out of the range first, and the
// whole range is to be locked instead.
func (x *sqlExec) lockScanTo(table, alias string, where sqlir.Expr, mode lockMode, policy *sqlir.LockClause, stop *scanStop) (done bool, err error) {
	tx := x.tx
	if stop.n == 0 {
		return true, nil // LIMIT 0 reads nothing
	}
	sr := x.searchRange(table, alias, where)
	cols := sr.cols
	if len(cols) == 0 || len(sr.ranges) != 1 || len(stop.order) > 1 {
		return false, nil
	}
	desc := false
	if len(stop.order) == 1 {
		c, ok := stop.order[0].Expr.(*sqlir.ColumnRef)
		if !ok || sr.eq >= len(cols) || c.Column != cols[sr.eq] || c.Table != "" && c.Table != alias && c.Table != relname(table) {
			return false, nil
		}
		desc = stop.order[0].Desc
	}
	rg := sr.ranges[0]
	var rows []Row
	count := 0
	var last Row
	// A record lock may wait, and its holder may delete the row or move its
	// value; the scan then starts over from the rows as they are, keeping the
	// locks it took.
scan:
	for {
		rows = tx.selectNoYield(table, nil)
		var in []Row
		for _, r := range rows {
			if rg.contains(keyOf(r, cols)) {
				in = append(in, r)
			}
		}
		slices.SortStableFunc(in, func(a, b Row) int {
			c := scanCompare(cols, tx.db.defs[table].pk, a, b)
			if desc {
				return -c
			}
			return c
		})
		count, last = 0, nil
		for _, r := range in {
			lk := lockKey{table, r.Key()}
			skipped := false
			if policy != nil && tx.heldByOther(lk, mode) {
				switch {
				case policy.SkipLocked:
					skipped = true
				case policy.NoWait:
					return false, tx.db.kind.Error(sqlir.LockNotAvailable, fmt.Sprintf("could not obtain lock on row in relation %q", relname(table)), relname(table), "", "")
				}
			}
			if !skipped {
				if err := tx.lockMode(lk, mode); err != nil {
					return false, err
				}
				cur, ok := tx.view(table, r.Key())
				if !ok || !sameKey(cur, r, cols) {
					continue scan
				}
				match := where == nil
				if !match {
					if match, err = x.evalBool(where, newJrow(alias, cur).env(nil)); err != nil {
						return false, err
					}
				}
				if match {
					count++
				}
			}
			last = r
			if count >= stop.n {
				break scan
			}
		}
		break
	}
	if count < stop.n {
		return false, nil
	}
	// The gap runs from where the scan started to the last record it read.
	gap := valRange{}
	lv := keyOf(last, cols)
	if desc {
		gap.lo, gap.hasLo, gap.loOpen = lv, true, true
		gap.hi, gap.hasHi, gap.hiOpen = rg.hi, rg.hasHi, rg.hiOpen
		var next Row
		for _, r := range rows {
			v := keyOf(r, cols)
			if v.hasNull() || rg.contains(v) || !rg.hasHi || greater(rg.hi, v) {
				continue
			}
			if next == nil || greater(keyOf(next, cols), v) {
				next = r
			}
		}
		if next != nil {
			gap.hi, gap.hasHi, gap.hiOpen = keyOf(next, cols), true, true
			if err := tx.lockMode(lockKey{table, next.Key()}, mode); err != nil {
				return false, err
			}
		} else {
			gap.hasHi = false
		}
	} else {
		gap.hi, gap.hasHi, gap.hiOpen = lv, true, true
		for _, r := range rows {
			v := keyOf(r, cols)
			if v.hasNull() || rg.contains(v) || !rg.hasLo || greater(v, rg.lo) {
				continue
			}
			if !gap.hasLo || greater(v, gap.lo) {
				gap.lo, gap.hasLo, gap.loOpen = v, true, true
			}
		}
	}
	tx.db.gaps = append(tx.db.gaps, &gapLock{tx: tx, table: table, cols: cols, ranges: []valRange{gap}})
	return true, nil
}

// lockRange takes the record and gap locks of a search of table by where,
// at any isolation level: InnoDB's foreign key checks take them even at
// Read Committed.
func (x *sqlExec) lockRange(table, alias string, where sqlir.Expr, mode lockMode, policy *sqlir.LockClause) error {
	tx := x.tx
	rows := tx.selectNoYield(table, nil)
	sr := x.searchRange(table, alias, where)
	cols := sr.cols
	lockRecord := func(r Row) error {
		lk := lockKey{table, r.Key()}
		if policy != nil && tx.heldByOther(lk, mode) {
			if policy.SkipLocked {
				return nil
			}
			if policy.NoWait {
				return tx.db.kind.Error(sqlir.LockNotAvailable, fmt.Sprintf("could not obtain lock on row in relation %q", relname(table)), relname(table), "", "")
			}
		}
		return tx.lockMode(lk, mode)
	}
	lockRows := func(pick func(Row) bool) error {
		for _, r := range rows {
			if pick(r) {
				if err := lockRecord(r); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if len(cols) == 0 {
		if err := lockRows(func(Row) bool { return true }); err != nil {
			return err
		}
		tx.db.gaps = append(tx.db.gaps, &gapLock{tx: tx, table: table})
		return nil
	}
	g := &gapLock{tx: tx, table: table, cols: cols}
	for _, rg := range sr.ranges {
		inRange := func(r Row) bool { return rg.contains(keyOf(r, cols)) }
		// A record lock may wait, and its holder may delete a row, move it
		// out of the range or into it. Lock and read again until the rows in
		// the range hold still, so the point check and the gap below see
		// what the statement will.
		for {
			if err := lockRows(inRange); err != nil {
				return err
			}
			fresh := tx.selectNoYield(table, nil)
			if slices.Equal(rangeKeys(rows, inRange), rangeKeys(fresh, inRange)) {
				rows = fresh
				break
			}
			rows = fresh
		}
		point := sr.unique
		if point && slices.ContainsFunc(rows, inRange) {
			continue // a unique equality search that finds its row locks the row only
		}
		// The gap runs from the nearest value below the range to the nearest
		// above, whose record is locked too unless the search is a unique
		// equality that missed.
		bounds := func(rows []Row) (gap valRange, next Row) {
			for _, r := range rows {
				v := keyOf(r, cols)
				if v.hasNull() || inRange(r) {
					continue
				}
				if rg.hasLo {
					if c, ok := boundCompare(v, rg.lo); ok && c <= 0 {
						if !gap.hasLo || greater(v, gap.lo) {
							gap.lo, gap.hasLo, gap.loOpen = v, true, true
						}
						continue
					}
				}
				if !gap.hasHi || greater(gap.hi, v) {
					gap.hi, gap.hasHi, gap.hiOpen, next = v, true, true, r
				}
			}
			return gap, next
		}
		gap, next := bounds(rows)
		// Locking the next record may wait, and its holder may delete it or
		// move its value, or the record below; the bounds are taken again
		// until they hold still.
		for next != nil && !point {
			if err := lockRecord(next); err != nil {
				return err
			}
			rows = tx.selectNoYield(table, nil)
			again, n := bounds(rows)
			if n != nil && n.Key() == next.Key() && sameValue(again.hi, gap.hi) && again.hasLo == gap.hasLo && sameValue(again.lo, gap.lo) {
				break
			}
			gap, next = again, n
		}
		g.ranges = append(g.ranges, gap)
	}
	if len(g.ranges) > 0 {
		tx.db.gaps = append(tx.db.gaps, g)
	}
	return nil
}

// rangeKeys is the sorted keys of the rows pick takes.
func rangeKeys(rows []Row, pick func(Row) bool) []string {
	var out []string
	for _, r := range rows {
		if pick(r) {
			out = append(out, r.Key())
		}
	}
	slices.Sort(out)
	return out
}

// greater compares index values or keys as MySQL compares them, the only
// server with gap locks.
func greater(a, b any) bool {
	c, ok := boundCompare(a, b)
	return ok && c > 0
}

// indexSearch is how a WHERE searches table by an index: the index's
// columns in key order, the ranges of keys searched, how many leading
// columns the search fixes by equality, and whether the index is unique on
// columns that are all fixed, which makes the search a point lookup. cols is
// empty when no index serves the WHERE.
type indexSearch struct {
	cols   []string
	ranges []valRange
	eq     int
	unique bool
	// ambiguous is a search more than one index serves, none of them by a
	// unique point lookup, which MySQL's optimizer chooses between by its
	// statistics.
	ambiguous bool
	// descending is a search by an index with a descending part.
	descending bool
}

// maxSearchKeys bounds the keys equalities on several columns multiply into,
// past which the search keeps the columns before.
const maxSearchKeys = 64

// searchRange finds the index a WHERE searches table by and the keys it
// searches: the first index whose leading column the WHERE bounds, with the
// equalities (and IN lists) on its leading columns and the range on the next
// one, as InnoDB's range scan over a composite index takes them.
func (x *sqlExec) searchRange(table, alias string, where sqlir.Expr) indexSearch {
	def := x.tx.db.defs[table]
	if def == nil || where == nil {
		return indexSearch{}
	}
	type index struct {
		cols   []string
		unique bool
		desc   bool
	}
	var indexes []index
	if len(def.pk) > 0 {
		indexes = append(indexes, index{cols: def.pk, unique: true})
	}
	for _, u := range def.uniques {
		if u.Where != nil {
			continue
		}
		var cols []string
		for _, e := range u.Elems {
			c, ok := e.(*sqlir.ColumnRef)
			if !ok {
				break // a prefix or an expression ends the columns the search can use
			}
			cols = append(cols, c.Column)
		}
		if len(cols) > 0 {
			indexes = append(indexes, index{cols: cols, unique: len(cols) == len(u.Elems)})
		}
	}
	for _, ix := range def.indexes {
		if len(ix.Columns) > 0 {
			indexes = append(indexes, index{ix.Columns, false, ix.Desc})
		}
	}
	conjuncts := splitAnd(where)
	var found []indexSearch
	for _, ix := range indexes {
		prefixes := []ixKey{{}} // the keys the equalities on the leading columns fix
		var last []valRange     // the range on the column after them, nil for none
		eq := 0
		for _, col := range ix.cols {
			rs, ok := x.columnRanges(col, alias, table, conjuncts)
			if !ok {
				break
			}
			points := len(rs) == 0 || !slices.ContainsFunc(rs, func(r valRange) bool { return !r.point() })
			if !points || len(prefixes)*max(len(rs), 1) > maxSearchKeys {
				last = rs
				break
			}
			var next []ixKey
			for _, p := range prefixes {
				for _, r := range rs {
					next = append(next, append(slices.Clone(p), r.lo))
				}
			}
			prefixes = next
			eq++
		}
		if eq == 0 && last == nil {
			continue // the index's leading column is not bounded
		}
		// A unique index holds any number of keys with a NULL, so a key with
		// one is no point lookup.
		nullKey := slices.ContainsFunc(prefixes, func(p ixKey) bool { return p.hasNull() })
		sr := indexSearch{cols: ix.cols, eq: eq, unique: ix.unique && eq == len(ix.cols) && !nullKey, descending: ix.desc}
		for _, p := range prefixes {
			if last == nil {
				sr.ranges = append(sr.ranges, valRange{lo: p, hi: p, hasLo: true, hasHi: true})
				continue
			}
			for _, r := range last {
				sr.ranges = append(sr.ranges, keyRange(p, r))
			}
		}
		if sr.unique {
			return sr // MySQL reads a unique point lookup by its index, whatever the others
		}
		found = append(found, sr)
	}
	if len(found) == 0 {
		return indexSearch{}
	}
	sr := found[0]
	sr.ambiguous = len(found) > 1
	return sr
}

// point reports a range of one value, NULL alone included.
func (r valRange) point() bool {
	return r.null || r.hasLo && r.hasHi && !r.loOpen && !r.hiOpen && sameValue(r.lo, r.hi)
}

// keyRange is the range of keys with prefix p whose next column falls in r.
// Without a lower bound the range starts past the NULLs, which a range on the
// column does not match.
func keyRange(p ixKey, r valRange) valRange {
	with := func(v any) ixKey { return append(slices.Clone(p), v) }
	if r.null {
		return valRange{lo: with(nil), hi: with(nil), hasLo: true, hasHi: true}
	}
	out := valRange{lo: with(nil), hasLo: true, loOpen: true}
	if r.hasLo {
		out.lo, out.loOpen = with(r.lo), r.loOpen
	}
	switch {
	case r.hasHi:
		out.hi, out.hasHi, out.hiOpen = with(r.hi), true, r.hiOpen
	case len(p) > 0:
		out.hi, out.hasHi = p, true
	}
	return out
}

// prefixSearch refuses a search that no index of whole columns serves but
// one on a prefix of a column does, such as name(3). InnoDB would lock along
// that index by the prefix, which the gap locks do not model, and locking the
// whole table instead would make waits and deadlocks InnoDB does not have.
func (x *sqlExec) prefixSearch(table, alias string, where sqlir.Expr) error {
	def := x.tx.db.defs[table]
	if def == nil || where == nil {
		return nil
	}
	if len(x.searchRange(table, alias, where).cols) > 0 {
		return nil
	}
	var prefixed []string
	for _, ix := range def.indexes {
		if ix.Prefix != "" {
			prefixed = append(prefixed, ix.Prefix)
		}
	}
	for _, u := range def.uniques {
		if f, ok := u.Elems[0].(*sqlir.FuncCall); ok && f.Name == "left" && u.Where == nil {
			if c, ok := f.Args[0].(*sqlir.ColumnRef); ok {
				prefixed = append(prefixed, c.Column)
			}
		}
	}
	conjuncts := splitAnd(where)
	for _, col := range prefixed {
		if _, ok := x.columnRanges(col, alias, table, conjuncts); ok {
			return x.unsupported("a locking search by a prefix index")
		}
	}
	return nil
}

// volatile reports whether e calls a function that returns another value each
// time, such as UUID(), whose value as a bound would not be the one the WHERE
// then tests.
func volatile(e sqlir.Expr) bool {
	return slices.ContainsFunc(sqlir.FuncCalls(e), func(f *sqlir.FuncCall) bool {
		return f.Name == "gen_random_uuid" || f.Name == "uuid_generate_v4" || f.Name == "random"
	})
}

func splitAnd(e sqlir.Expr) []sqlir.Expr {
	if b, ok := e.(*sqlir.BinaryExpr); ok && b.Op == "AND" {
		return append(splitAnd(b.L), splitAnd(b.R)...)
	}
	return []sqlir.Expr{e}
}

// columnRanges intersects the conditions on col among conjuncts into the
// ranges of values they allow. ok is false when no conjunct constrains col
// with a value known before the scan.
func (x *sqlExec) columnRanges(col, alias, table string, conjuncts []sqlir.Expr) ([]valRange, bool) {
	isCol := func(e sqlir.Expr) bool {
		c, ok := e.(*sqlir.ColumnRef)
		return ok && c.Column == col && (c.Table == "" || c.Table == alias || c.Table == relname(table))
	}
	isNullConstant := func(e sqlir.Expr) bool {
		if len(sqlir.ColumnRefs(e)) > 0 || volatile(e) {
			return false
		}
		v, err := x.eval(e, &env{})
		return err == nil && derefValue(v) == nil
	}
	numeric, text := false, false
	if def := x.tx.db.defs[table]; def != nil {
		numeric, text = mysqlNumericType(def.types[col]), mysqlTextType(def.types[col])
	}
	constant := func(e sqlir.Expr) (any, bool) {
		if len(sqlir.ColumnRefs(e)) > 0 || volatile(e) {
			return nil, false
		}
		v, err := x.eval(e, &env{})
		if err != nil || derefValue(v) == nil {
			return nil, false
		}
		if numeric {
			// A string compared with a number column compares as the
			// number it converts to, which orders the bounds as the index
			// does: '5' is below '20'.
			return mysqlArithOperand(v), true
		}
		if _, isNum := toFloat(derefValue(boolAsInt(v))); text && isNum {
			// A text column compared with a number compares as the number
			// each value converts to, which the index's text order does not
			// follow ('01' and '1' both equal 1), so MySQL cannot search
			// the index by it.
			return nil, false
		}
		return derefValue(v), true
	}
	nullOnly := false // IS NULL or <=> NULL: the NULL range alone
	bound := valRange{}
	var points []any // the values equality and IN allow, nil until one constrains col
	found := false
	// Each equality or IN conjunct narrows the allowed values to those it
	// also allows: the conjuncts are ANDed.
	allow := func(vs []any) {
		if points == nil {
			points = vs
		} else {
			points = slices.DeleteFunc(points, func(p any) bool {
				return !slices.ContainsFunc(vs, func(v any) bool { return sameValue(mysqlOperands(p, v)) })
			})
			if points == nil {
				points = []any{} // nothing allowed, which is not unconstrained
			}
		}
		found = true
	}
	for _, c := range conjuncts {
		switch e := c.(type) {
		case *sqlir.BinaryExpr:
			op, other := e.Op, e.R
			if !isCol(e.L) {
				if !isCol(e.R) {
					continue
				}
				other = e.L
				op = map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<=", "=": "=", "<=>": "<=>"}[op]
			}
			if k, ok := other.(*sqlir.Const); ok && op == "<=>" && derefValue(k.Value) == nil {
				nullOnly, found = true, true // <=> NULL is IS NULL
				continue
			}
			v, ok := constant(other)
			if !ok {
				continue
			}
			switch op {
			case "=", "<=>": // the bound is no NULL, which <=> would match as =
				allow([]any{v})
			case ">", ">=":
				switch {
				case !bound.hasLo || greater(v, bound.lo):
					bound.lo, bound.hasLo, bound.loOpen = v, true, op == ">"
				case op == ">" && !greater(bound.lo, v):
					bound.loOpen = true // the same bound, open if either conjunct is
				}
				found = true
			case "<", "<=":
				switch {
				case !bound.hasHi || greater(bound.hi, v):
					bound.hi, bound.hasHi, bound.hiOpen = v, true, op == "<"
				case op == "<" && !greater(v, bound.hi):
					bound.hiOpen = true
				}
				found = true
			}
		case *sqlir.IsNull:
			if !e.Not && isCol(e.X) {
				nullOnly, found = true, true
			}
		case *sqlir.InExpr:
			if e.Not || e.Sub != nil || !isCol(e.X) {
				continue
			}
			vs := []any{}
			for _, it := range e.List {
				v, ok := constant(it)
				if !ok {
					if isNullConstant(it) {
						continue // a NULL member matches no row, and bounds nothing
					}
					vs = nil
					break
				}
				vs = append(vs, v)
			}
			if vs != nil {
				allow(vs)
			}
		}
	}
	if !found {
		return nil, false
	}
	if nullOnly {
		if points != nil || bound.hasLo || bound.hasHi {
			return nil, true // NULL and a value at once: no row matches
		}
		return []valRange{{null: true}}, true
	}
	// constant made the bounds of a number column numbers and kept numbers
	// off a text column, so both bounds compare in the column's own order.
	if bound.hasLo && bound.hasHi {
		if c, ok := compareValues(mysqlOperands(bound.lo, bound.hi)); ok && (c > 0 || c == 0 && (bound.loOpen || bound.hiOpen)) {
			return nil, true // the bounds cross: no row matches, and nothing is locked
		}
	}
	if points == nil {
		return []valRange{bound}, true
	}
	var out []valRange
	for _, p := range points {
		if bound.contains(p) {
			out = append(out, valRange{lo: p, hi: p, hasLo: true, hasHi: true})
		}
	}
	return out, true
}
