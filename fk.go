package detest

import (
	"fmt"
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
	def := x.tx.db.defs[table]
	if def == nil {
		return nil
	}
	for _, fk := range def.fks {
		vals, ok := values(row, fk.Columns)
		if !ok || fk.Deferred {
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
			return x.tx.db.kind.Error(sqlir.ForeignKeyViolation, fmt.Sprintf("insert or update on table %q violates foreign key constraint %q", relname(table), fk.Name), relname(table), "", fk.Name)
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
			return false, nil
		}
	}
	if err := x.tx.lockMode(lockKey{parent, key}, lockKeyShare); err != nil {
		return false, err
	}
	r, ok := x.tx.view(parent, key) // the version visible once the lock is granted
	return ok && rowMatches(r, cols, vals), nil
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
				lk := lockKey{ck.table, k.Key()}
				if err := x.tx.lock(lk); err != nil {
					return err
				}
				cur, ok := x.tx.view(ck.table, k.Key())
				if !ok {
					continue
				}
				delete(x.tx.writes, lk)
				x.tx.deleted[lk] = true
				if err := x.onParentDelete(ck.table, cur); err != nil {
					return err
				}
			}
		case "set null":
			for _, k := range kids {
				lk := lockKey{ck.table, k.Key()}
				if err := x.tx.lockMode(lk, x.tx.db.updateLock(ck.table, ck.fk.Columns)); err != nil {
					return err
				}
				cur, ok := x.tx.view(ck.table, k.Key())
				if !ok {
					continue
				}
				for _, c := range ck.fk.Columns {
					cur[c] = nil
				}
				x.tx.writes[lk] = cur
			}
		case "set default":
			return x.unsupported("ON DELETE SET DEFAULT")
		default: // no action, restrict
			return x.tx.db.kind.Error(sqlir.ForeignKeyViolation, fmt.Sprintf("update or delete on table %q violates foreign key constraint %q on table %q", relname(table), ck.fk.Name, relname(ck.table)), relname(ck.table), "", ck.fk.Name)
		}
	}
	return nil
}

// onParentUpdate refuses an update of referenced columns that children still
// reference. ON UPDATE CASCADE is not modeled.
func (x *sqlExec) onParentUpdate(table string, old, row Row) error {
	for _, ck := range x.tx.db.referencing(table) {
		cols := x.tx.db.refColumns(ck.fk)
		ov, ok := values(old, cols)
		if !ok {
			continue
		}
		if nv, ok := values(row, cols); ok && slices.EqualFunc(ov, nv, sameValue) {
			continue
		}
		if len(x.children(ck.table, ck.fk, ov)) == 0 {
			continue
		}
		if ck.fk.OnUpdate == "cascade" || ck.fk.OnUpdate == "set null" || ck.fk.OnUpdate == "set default" {
			return x.unsupported("ON UPDATE " + ck.fk.OnUpdate)
		}
		return x.tx.db.kind.Error(sqlir.ForeignKeyViolation, fmt.Sprintf("update or delete on table %q violates foreign key constraint %q on table %q", relname(table), ck.fk.Name, relname(ck.table)), relname(ck.table), "", ck.fk.Name)
	}
	return nil
}
