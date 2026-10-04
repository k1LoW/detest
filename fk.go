package detest

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/k1LoW/detest/internal/sqlir"
)

// refColumns are the parent columns a foreign key references: the ones it
// names, or the parent's primary key.
func (db *DB) refColumns(fk sqlir.ForeignKey) []string {
	if len(fk.RefColumns) > 0 {
		return fk.RefColumns
	}
	if def := db.defs[fk.RefTable]; def != nil {
		return def.pk
	}
	return nil
}

func values(r Row, cols []string) ([]any, bool) {
	vals := make([]any, len(cols))
	for i, c := range cols {
		v := derefValue(r[c])
		if v == nil {
			return nil, false // MATCH SIMPLE: a NULL in the key skips the check
		}
		vals[i] = v
	}
	return vals, true
}

// partlyNull reports a key with some but not all columns NULL, which MATCH
// FULL refuses.
func partlyNull(r Row, cols []string) bool {
	nulls := 0
	for _, c := range cols {
		if derefValue(r[c]) == nil {
			nulls++
		}
	}
	return nulls > 0 && nulls < len(cols)
}

// isDeferred reports whether fk's checks wait for the commit: INITIALLY
// DEFERRED, unless SET CONSTRAINTS said otherwise for a deferrable one.
func (tx *Tx) isDeferred(fk sqlir.ForeignKey) bool {
	if !fk.Deferrable {
		return false
	}
	if d, ok := tx.deferNamed[fk.Name]; ok {
		return d
	}
	if tx.deferAll != nil {
		return *tx.deferAll
	}
	return fk.Deferred
}

func (tx *Tx) childViolation(table string, fk sqlir.ForeignKey) error {
	return tx.db.kind.Error(sqlir.ForeignKeyViolation, fmt.Sprintf("insert or update on table %q violates foreign key constraint %q", relname(table), fk.Name), relname(table), "", fk.Name)
}

func (tx *Tx) parentViolation(table string, ck childKey) error {
	return tx.db.kind.Error(sqlir.ForeignKeyParentViolation, fmt.Sprintf("update or delete on table %q violates foreign key constraint %q on table %q", relname(table), ck.fk.Name, relname(ck.table)), relname(ck.table), "", ck.fk.Name)
}

func rowMatches(r Row, cols []string, vals []any) bool {
	for i, c := range cols {
		if !sameValue(r[c], vals[i]) {
			return false
		}
	}
	return true
}

// checkParents enforces row's foreign keys, as a write to table: the parent
// row must exist. It takes FOR KEY SHARE on the parent, as Postgres's check
// does, so a concurrent delete of the parent waits for this transaction. old
// is the row an UPDATE replaces; a key it leaves unchanged is not checked.
func (x *sqlExec) checkParents(table string, row, old Row) error {
	return x.checkParentsExcept(table, row, old, "")
}

// checkParentsExcept is checkParents without the foreign key named skip,
// which a cascade is applying: its parent row is being rewritten and is not
// visible with its new key yet.
func (x *sqlExec) checkParentsExcept(table string, row, old Row, skip string) error {
	if x.tx.noFKChecks {
		return nil // FOREIGN_KEY_CHECKS=0
	}
	def := x.tx.db.defs[table]
	if def == nil {
		return nil
	}
	for _, fk := range def.fks {
		if fk.Name == skip || x.tx.db.isIgnored(fk.RefTable) {
			continue
		}
		if fk.MatchFull && partlyNull(row, fk.Columns) {
			return x.tx.db.kind.Error(sqlir.ForeignKeyViolation, fmt.Sprintf("insert or update on table %q violates foreign key constraint %q: MATCH FULL does not allow mixing of null and nonnull key values", relname(table), fk.Name), relname(table), "", fk.Name)
		}
		vals, ok := values(row, fk.Columns)
		if !ok || x.tx.isDeferred(fk) {
			continue
		}
		if old != nil {
			if ov, ok := values(old, fk.Columns); ok && slices.EqualFunc(ov, vals, sameValue) {
				continue
			}
		}
		found, err := x.lockParent(fk, vals)
		if err != nil {
			return err
		}
		if !found {
			return x.tx.childViolation(table, fk)
		}
	}
	return nil
}

// lockParent finds the parent row with vals in the referenced columns and
// locks it FOR KEY SHARE.
func (x *sqlExec) lockParent(fk sqlir.ForeignKey, vals []any) (bool, error) {
	parent := fk.RefTable
	cols := x.tx.db.refColumns(fk)
	var key string
	if pdef := x.tx.db.defs[parent]; pdef != nil && slices.Equal(cols, pdef.pk) {
		key = encodeKey(vals)
	} else {
		for _, r := range x.tx.selectNoYield(parent, nil) {
			if rowMatches(r, cols, vals) {
				key = r.Key()
				break
			}
		}
		if key == "" {
			return false, x.lockMissingParent(parent, cols, vals)
		}
	}
	if u := x.tx.db.uniqueOn(parent, cols); u != nil && x.tx.db.kind.InnoDB() {
		// InnoDB checks a parent found on a unique secondary index with a
		// shared lock on that index's record, which a writer of the parent
		// row's other columns does not touch.
		if err := x.tx.lockModeAs(uniqueLock(parent, u, vals), lockShare, structKey(parent, uniqueIndex(u), lockShare, "record")); err != nil {
			return false, err
		}
		x.tx.noteLockStruct(parent, uniqueIndex(u), lockShare, "record")
		for _, r := range x.tx.selectNoYield(parent, nil) {
			if rowMatches(r, cols, vals) {
				return true, nil
			}
		}
		return false, x.lockMissingParent(parent, cols, vals)
	}
	mode := lockKeyShare
	if x.tx.db.kind.InnoDB() {
		mode = lockShare // InnoDB checks a parent with a shared lock
	}
	if err := x.tx.lockMode(lockKey{parent, key}, mode); err != nil {
		return false, err
	}
	r, ok := x.tx.view(parent, key) // the version visible once the lock is granted
	if ok && rowMatches(r, cols, vals) {
		return true, nil
	}
	return false, x.lockMissingParent(parent, cols, vals)
}

// uniqueOn is the unique index of table, other than the primary key, over
// exactly cols, if any.
func (db *DB) uniqueOn(table string, cols []string) *sqlir.UniqueDef {
	def := db.defs[table]
	if def == nil {
		return nil
	}
	for i := range def.uniques {
		u := &def.uniques[i]
		if u.Where != nil || len(u.Elems) != len(cols) {
			continue
		}
		if !slices.EqualFunc(u.Elems, cols, func(e sqlir.Expr, c string) bool {
			r, ok := e.(*sqlir.ColumnRef)
			return ok && r.Column == c
		}) {
			continue
		}
		return u
	}
	return nil
}

// lockMissingParent takes the shared gap lock InnoDB keeps where a missing
// parent key would be, so that a concurrent insert of that parent waits for
// the transaction whose check failed.
func (x *sqlExec) lockMissingParent(parent string, cols []string, vals []any) error {
	if !x.tx.db.kind.InnoDB() || x.tx.db.ignored[parent] {
		return nil
	}
	var where sqlir.Expr
	for i, c := range cols {
		eq := &sqlir.BinaryExpr{Op: "=", L: &sqlir.ColumnRef{Column: c}, R: &sqlir.Const{Value: vals[i]}}
		if where == nil {
			where = eq
		} else {
			where = &sqlir.BinaryExpr{Op: "AND", L: where, R: eq}
		}
	}
	return x.lockRange(parent, relname(parent), where, lockShare, nil)
}

// referencing returns the tables whose foreign keys reference table, sorted
// so that cascades run in a fixed order.
func (db *DB) referencing(table string) []childKey {
	var out []childKey
	for child, def := range db.defs {
		for _, fk := range def.fks {
			if fk.RefTable == table {
				out = append(out, childKey{child, fk})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].table != out[j].table {
			return out[i].table < out[j].table
		}
		return out[i].fk.Name < out[j].fk.Name
	})
	return out
}

type childKey struct {
	table string
	fk    sqlir.ForeignKey
}

// children returns the visible rows of child whose key references vals.
func (x *sqlExec) children(child string, fk sqlir.ForeignKey, vals []any) []Row {
	var out []Row
	for _, r := range x.tx.selectNoYield(child, nil) {
		if rowMatches(r, fk.Columns, vals) {
			out = append(out, r)
		}
	}
	return out
}

// onParentDelete applies the foreign keys that reference a deleted row:
// NO ACTION and RESTRICT refuse the delete, CASCADE deletes the children (and
// theirs), SET NULL clears their key.
func (x *sqlExec) onParentDelete(table string, row Row) error {
	if x.tx.noFKChecks {
		return nil // FOREIGN_KEY_CHECKS=0
	}
	for _, ck := range x.tx.db.referencing(table) {
		vals, ok := values(row, x.tx.db.refColumns(ck.fk))
		if !ok {
			continue
		}
		kids := x.children(ck.table, ck.fk, vals)
		if len(kids) == 0 {
			continue
		}
		switch ck.fk.OnDelete {
		case "cascade":
			for _, k := range kids {
				lk, cur, ok, err := x.lockChild(ck, k, row, lockUpdate)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				if err := x.releaseEntries(ck.table, cur, nil); err != nil {
					return err
				}
				delete(x.tx.writes, lk)
				x.tx.deleted[lk] = true
				x.tx.undo++
				if err := x.onParentDelete(ck.table, cur); err != nil {
					return err
				}
			}
		case "set null", "set default":
			if err := x.setChildren(ck, kids, row, nil, ck.fk.OnDelete); err != nil {
				return err
			}
		case "no action":
			if x.tx.isDeferred(ck.fk) {
				continue // the commit checks the children are gone by then
			}
			return x.tx.parentViolation(table, ck)
		default: // restrict, which is never deferred
			return x.tx.parentViolation(table, ck)
		}
	}
	return nil
}

// onParentUpdate applies the foreign keys that reference the columns an
// update of a row changes: NO ACTION and RESTRICT refuse it while children
// reference the old key, CASCADE moves the children to the new key, SET NULL
// and SET DEFAULT clear theirs.
func (x *sqlExec) onParentUpdate(table string, old, row Row) error {
	if x.tx.noFKChecks {
		return nil // FOREIGN_KEY_CHECKS=0
	}
	for _, ck := range x.tx.db.referencing(table) {
		cols := x.tx.db.refColumns(ck.fk)
		ov, ok := values(old, cols)
		if !ok {
			continue
		}
		nv, _ := values(row, cols)
		if nv != nil && slices.EqualFunc(ov, nv, sameValue) {
			continue
		}
		kids := x.children(ck.table, ck.fk, ov)
		if len(kids) == 0 {
			continue
		}
		switch ck.fk.OnUpdate {
		case "cascade", "set null", "set default":
			if err := x.setChildren(ck, kids, old, row, ck.fk.OnUpdate); err != nil {
				return err
			}
		case "no action":
			if x.tx.isDeferred(ck.fk) {
				continue
			}
			return x.tx.parentViolation(table, ck)
		default:
			return x.tx.parentViolation(table, ck)
		}
	}
	return nil
}

// setChildren rewrites the key of the child rows kids that reference old, a
// parent row being deleted or updated to parent, as action says: to the
// parent's new key (cascade), to NULL (set null) or to the columns' defaults
// (set default). Each child goes through the checks an UPDATE of it would,
// and its own children follow.
// lockChild locks a child row of a referential action and returns its newest
// version, following it if a concurrent change moved its key. ok is false
// when the child is gone or no longer references parent's key.
func (x *sqlExec) lockChild(ck childKey, k, parent Row, mode lockMode) (lockKey, Row, bool, error) {
	key, cur, ok, err := x.tx.lockLatest(ck.table, k.Key(), func(lk lockKey) error { return x.tx.lockMode(lk, mode) })
	if err != nil || !ok {
		return lockKey{}, nil, false, err
	}
	vals, ok := values(cur, ck.fk.Columns)
	if !ok || !rowMatches(parent, x.tx.db.refColumns(ck.fk), vals) {
		return lockKey{}, nil, false, nil
	}
	return lockKey{ck.table, key}, cur, true, nil
}

func (x *sqlExec) setChildren(ck childKey, kids []Row, old, parent Row, action string) error {
	def := x.tx.db.defs[ck.table]
	for _, k := range kids {
		lk, cur, ok, err := x.lockChild(ck, k, old, x.tx.db.updateLock(ck.table, ck.fk.Columns))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		updated := cur.clone()
		skip := ""
		for i, c := range ck.fk.Columns {
			switch {
			case action == "cascade":
				updated[c] = parent[x.tx.db.refColumns(ck.fk)[i]]
				skip = ck.fk.Name // the parent's new key is not written yet
			case action == "set default" && def != nil && def.defaults[c] != nil:
				// The column is a foreign key column, which the key reads,
				// so a default detest cannot compute is refused as
				// applyDefaults refuses one a key reads: the marker would be
				// matched against the parent's keys.
				v, err := x.eval(def.defaults[c], &env{})
				if err != nil && !errors.As(err, new(errUnknownExpr)) {
					return err
				}
				if err != nil {
					return x.unsupported(fmt.Sprintf("the default of column %q, whose expression detest cannot compute, for ON ... SET DEFAULT", c))
				}
				updated[c] = v
			default:
				updated[c] = nil
			}
		}
		if action == "set default" {
			// The default may name the very key going away, which the check
			// below would still find.
			if vals, ok := values(updated, ck.fk.Columns); ok && rowMatches(old, x.tx.db.refColumns(ck.fk), vals) {
				return x.tx.childViolation(ck.table, ck.fk)
			}
		}
		if err := x.checkRow(ck.table, updated); err != nil {
			return err
		}
		if err := x.checkUniques(ck.table, updated, lk.key, cur); err != nil {
			return err
		}
		if err := x.checkParentsExcept(ck.table, updated, cur, skip); err != nil {
			return err
		}
		if err := x.onParentUpdate(ck.table, cur, updated); err != nil {
			return err
		}
		nlk, err := x.rekey(ck.table, lk, updated)
		if err != nil {
			return err
		}
		// A cascade that moves a child's key into another transaction's
		// locked gap waits, as an UPDATE of the child does.
		if err := x.tx.moveIntention(ck.table, updated, cur); err != nil {
			return err
		}
		x.tx.writes[nlk] = updated
		x.tx.undo++
	}
	return nil
}

// checkDeferred runs, at commit or SET CONSTRAINTS ... IMMEDIATE, the checks
// of the foreign keys deferred until then that only selects: every row the
// transaction wrote to a child table must have its parent, and every parent
// key it deleted or changed must have no children left. It looks at the
// transaction's writes rather than a log of the statements, so a savepoint
// rolled back or a key rewritten leaves nothing stale to check.
func (x *sqlExec) checkDeferred(only func(sqlir.ForeignKey) bool) error {
	tx := x.tx
	var childTables []string
	for table, def := range tx.db.defs {
		if slices.ContainsFunc(def.fks, only) {
			childTables = append(childTables, table)
		}
	}
	sort.Strings(childTables)
	for _, table := range childTables {
		for _, lk := range sortedKeys(tx.writes, table) {
			row := tx.writes[lk]
			for _, fk := range tx.db.defs[table].fks {
				if !only(fk) || tx.db.isIgnored(fk.RefTable) {
					continue
				}
				vals, ok := values(row, fk.Columns)
				if !ok {
					continue
				}
				found, err := x.lockParent(fk, vals)
				if err != nil {
					return err
				}
				if !found {
					return tx.childViolation(table, fk)
				}
			}
		}
	}
	parents := map[string]bool{}
	for _, def := range tx.db.defs {
		for _, fk := range def.fks {
			if only(fk) {
				parents[fk.RefTable] = true
			}
		}
	}
	for _, table := range slices.Sorted(maps.Keys(parents)) {
		for _, key := range tx.touchedKeys(table) {
			old, existed := tx.db.committed[table][key]
			if !existed {
				continue // a row of this transaction's own: no child committed before it
			}
			cur, ok := tx.view(table, key)
			for _, ck := range tx.db.referencing(table) {
				if !only(ck.fk) {
					continue
				}
				cols := tx.db.refColumns(ck.fk)
				ov, ok2 := values(old, cols)
				if !ok2 {
					continue
				}
				if ok {
					if nv, ok3 := values(cur, cols); ok3 && slices.EqualFunc(ov, nv, sameValue) {
						continue // the key is still there
					}
				}
				if len(x.children(ck.table, ck.fk, ov)) > 0 {
					return tx.parentViolation(table, ck)
				}
			}
		}
	}
	return nil
}

// checkCommit runs the foreign key checks deferred to the commit. A failure
// fails the commit, which rolls the transaction back.
func (tx *Tx) checkCommit() error { return tx.evaluator().checkDeferred(tx.isDeferred) }

// setConstraints runs SET CONSTRAINTS. Constraints it makes immediate run the
// checks deferred so far, as Postgres does.
func (x *sqlExec) setConstraints(st *sqlir.SetConstraintsStmt) error {
	tx := x.tx
	var before []sqlir.ForeignKey
	for _, def := range tx.db.defs {
		for _, fk := range def.fks {
			if tx.isDeferred(fk) {
				before = append(before, fk)
			}
		}
	}
	if len(st.Names) == 0 {
		d := st.Deferred
		tx.deferAll, tx.deferNamed = &d, nil
	} else {
		if tx.deferNamed == nil {
			tx.deferNamed = map[string]bool{}
		}
		for _, n := range st.Names {
			tx.deferNamed[n] = st.Deferred
		}
	}
	if st.Deferred {
		return nil
	}
	return x.checkDeferred(func(fk sqlir.ForeignKey) bool {
		return !tx.isDeferred(fk) && slices.ContainsFunc(before, func(b sqlir.ForeignKey) bool { return b.Name == fk.Name })
	})
}

// touchedKeys returns the keys of table the transaction wrote or deleted, in a
// fixed order.
func (tx *Tx) touchedKeys(table string) []string {
	seen := map[string]bool{}
	for lk := range tx.writes {
		if lk.table == table {
			seen[lk.key] = true
		}
	}
	for lk := range tx.deleted {
		if lk.table == table {
			seen[lk.key] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// sortedKeys returns the keys of writes in table, in a fixed order.
func sortedKeys(writes map[lockKey]Row, table string) []lockKey {
	var out []lockKey
	for lk := range writes {
		if lk.table == table {
			out = append(out, lk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}
