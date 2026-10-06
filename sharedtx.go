package detest

import (
	"fmt"
	"slices"

	"github.com/k1LoW/detest/internal/sqlir"
)

// Statements of goroutines sharing a transaction run at once (see
// sqlConn.runShared), in the order database/sql's lock hands the connection
// out, which the Go runtime decides rather than the schedule. Goroutines that
// run in the same step therefore run in an order a replay does not repeat.
// That is harmless while their statements commute, such as deletes of rows of
// their own by primary key, and leaves the outcome to the runtime otherwise,
// such as two updates of one row, which a replay would not reproduce. So the
// statements each step's goroutines ran are kept, and a statement whose
// outcome could depend on its order with another goroutine's stops the
// exploration, as nondeterminism does.

// sharedAccess is what a statement on a shared transaction touched.
type sharedAccess struct {
	gid   string
	db    *DB
	query string
	// keys are the rows and index entries the statement wrote or locked,
	// when its shape pins them by primary key so that no other statement
	// can change which ones it touches. nil stands for the whole database.
	keys  map[lockKey]bool
	write bool
}

func (a *sharedAccess) conflicts(b *sharedAccess) bool {
	if a.gid == b.gid || a.db != b.db || !a.write && !b.write {
		return false
	}
	if a.keys == nil || b.keys == nil {
		return true
	}
	for k := range a.keys {
		if b.keys[k] {
			return true
		}
	}
	return false
}

// sharedMark is what a transaction had written and locked before a
// statement on it, to tell what the statement wrote or locked. The two are
// apart, as a statement may write a row the transaction locked before.
type sharedMark struct {
	written, locked map[lockKey]bool
}

func (tx *Tx) markShared() sharedMark {
	m := sharedMark{written: map[lockKey]bool{}, locked: map[lockKey]bool{}}
	for lk := range tx.writes {
		m.written[lk] = true
	}
	for lk := range tx.deleted {
		m.written[lk] = true
	}
	for _, lk := range tx.locks {
		m.locked[lk] = true
	}
	return m
}

// sinceShared returns the keys written or locked since m. A key written
// again, which m already holds, is not told apart from one left alone, so
// the caller takes an empty set for the whole database.
func (tx *Tx) sinceShared(m sharedMark) map[lockKey]bool {
	out := map[lockKey]bool{}
	for lk := range tx.writes {
		if !m.written[lk] {
			out[lk] = true
		}
	}
	for lk := range tx.deleted {
		if !m.written[lk] {
			out[lk] = true
		}
	}
	for _, lk := range tx.locks {
		if !m.locked[lk] {
			out[lk] = true
		}
	}
	return out
}

// recordShared checks a statement a goroutine ran on a shared transaction
// against those other goroutines ran in the same step, and keeps it for the
// ones after it. A conflict stops the exploration.
func (r *run) recordShared(a *sharedAccess) {
	if e := r.s.epoch.Load(); e != r.sharedEpoch {
		r.sharedEpoch, r.sharedLog = e, nil
	}
	for i := range r.sharedLog {
		b := &r.sharedLog[i]
		if !a.conflicts(b) {
			continue
		}
		if r.pending == nil {
			r.pending = &violation{kind: "fatal", err: fmt.Errorf("detest: goroutines sharing a transaction ran statements in one step whose outcome depends on their order, which database/sql and the Go runtime decide rather than the schedule, so a replay would not reproduce it: %q and %q. Let one goroutine run statements that touch the same rows, or have each goroutine touch rows of its own by primary key", b.query, a.query)}
		}
		return
	}
	r.sharedLog = append(r.sharedLog, *a)
}

// checkUnshared stops the exploration when a process ran a statement that
// did not park, such as a SET, in a step after goroutines sharing a
// transaction ran theirs, which it may have run alongside. A statement that
// parked runs only once the scheduler resumes it, before anything it starts,
// and one before the goroutines' statements in the step is ordered before
// them by starting them, so only this order is told, and the statement is
// not kept for the ones after it.
func (r *run) checkUnshared(a *sharedAccess) {
	if r.s.epoch.Load() != r.sharedEpoch {
		return
	}
	for i := range r.sharedLog {
		b := &r.sharedLog[i]
		if a.conflicts(b) && r.pending == nil {
			r.pending = &violation{kind: "fatal", err: fmt.Errorf("detest: a process ran %q alongside goroutines sharing a transaction, which ran %q in the same step, so their order is the Go runtime's rather than the schedule's, and a replay would not reproduce it", a.query, b.query)}
			return
		}
	}
}

// pointTable returns the table a statement touches only the rows of, by its
// primary key, so that which rows it touches cannot depend on another
// statement: an INSERT of given keys, or an UPDATE, a DELETE or a locking
// SELECT whose WHERE pins every primary key column to a value. The table
// must have no foreign key to or from it, whose checks and actions read and
// write other tables. ok is false for any other statement.
func pointTable(db *DB, stmt sqlir.Statement) (table string, ok bool) {
	switch s := stmt.(type) {
	case *sqlir.InsertStmt:
		if s.Select != nil || s.OnConflict != nil || len(s.Rows) == 0 || !simpleTargets(s.Returning) {
			return "", false
		}
		table = db.resolve(s.Table)
		def := isolatedTable(db, table)
		// MySQL also generates an AUTO_INCREMENT value for a NULL or a 0
		// given, and a larger one given moves the counter on.
		if def == nil || len(def.autoInc) > 0 {
			return "", false
		}
		given := map[string]bool{}
		for _, c := range s.Columns {
			given[c] = true
		}
		for _, c := range def.pk {
			if !given[c] {
				return "", false
			}
		}
		for _, c := range def.columns {
			if given[c] {
				continue
			}
			// A sequence, or a function such as gen_random_uuid, hands out
			// its values in the order of the inserts.
			if d, ok := def.defaults[c]; ok && hasEffects(d) {
				return "", false
			}
		}
		for _, row := range s.Rows {
			for _, e := range row {
				if !simpleExpr(e) {
					return "", false
				}
			}
		}
		return table, true
	case *sqlir.UpdateStmt:
		if len(s.With) > 0 || len(s.From) > 0 || len(s.OrderBy) > 0 || s.Limit != nil || !simpleTargets(s.Returning) {
			return "", false
		}
		for _, a := range s.Set {
			if !simpleExpr(a.Value) {
				return "", false
			}
		}
		table, ok := pinnedTable(db, s.Table, s.Alias, s.Where)
		if !ok {
			return "", false
		}
		def := db.defs[table]
		// A unique value one row gives up or takes decides whether another
		// row's update fails, which no key of the two tells, and a new
		// primary key or AUTO_INCREMENT value moves the row or the counter.
		if len(def.uniques) > 0 {
			return "", false
		}
		for _, a := range s.Set {
			if slices.Contains(def.pk, a.Column) || def.autoInc[a.Column] {
				return "", false
			}
		}
		return table, true
	case *sqlir.DeleteStmt:
		if len(s.With) > 0 || len(s.Using) > 0 || len(s.OrderBy) > 0 || s.Limit != nil || s.Truncate || !simpleTargets(s.Returning) {
			return "", false
		}
		return pinnedTable(db, s.Table, s.Alias, s.Where)
	case *sqlir.SelectStmt:
		// A plain read takes no lock, so what it read leaves no trace to
		// compare, and it stands for the whole database.
		if s.Lock == nil || len(s.With) > 0 || s.SetOp != "" || s.From == nil || s.From.Name == "" || len(s.Joins) > 0 ||
			len(s.GroupBy) > 0 || s.Having != nil || s.Limit != nil || s.Offset != nil || len(s.DistinctOn) > 0 || !simpleTargets(s.Targets) {
			return "", false
		}
		return pinnedTable(db, s.From.Name, s.From.Alias, s.Where)
	}
	return "", false
}

func pinnedTable(db *DB, name, alias string, where sqlir.Expr) (string, bool) {
	table := db.resolve(name)
	def := isolatedTable(db, table)
	if def == nil || where == nil {
		return "", false
	}
	pinned := map[string]bool{}
	var walk func(e sqlir.Expr) bool
	walk = func(e sqlir.Expr) bool {
		b, ok := e.(*sqlir.BinaryExpr)
		if ok && b.Op == "AND" {
			return walk(b.L) && walk(b.R)
		}
		if !simpleExpr(e) {
			return false
		}
		if ok && b.Op == "=" {
			for _, side := range [][2]sqlir.Expr{{b.L, b.R}, {b.R, b.L}} {
				c, isCol := side[0].(*sqlir.ColumnRef)
				if isCol && (c.Table == "" || c.Table == name || c.Table == alias) && isValue(side[1]) {
					pinned[c.Column] = true
				}
			}
		}
		return true
	}
	if !walk(where) {
		return "", false
	}
	for _, c := range def.pk {
		if !pinned[c] {
			return "", false
		}
	}
	return table, true
}

// isolatedTable returns the definition of a table with a primary key and no
// foreign key to or from it, and nil for any other.
func isolatedTable(db *DB, table string) *tableDef {
	def := db.defs[table]
	if def == nil || len(def.pk) == 0 || len(def.fks) > 0 {
		return nil
	}
	for _, other := range db.defs {
		for _, fk := range other.fks {
			if db.resolve(fk.RefTable) == table {
				return nil
			}
		}
	}
	return def
}

// simpleExpr reports whether e reads nothing but the row and the statement's
// values, as a subquery or a function such as nextval would.
func simpleExpr(e sqlir.Expr) bool {
	switch e := e.(type) {
	case *sqlir.ColumnRef, *sqlir.Param, *sqlir.Const:
		return true
	case *sqlir.BinaryExpr:
		return simpleExpr(e.L) && simpleExpr(e.R)
	case *sqlir.UnaryExpr:
		return simpleExpr(e.X)
	case *sqlir.Cast:
		return simpleExpr(e.X)
	}
	return false
}

func isValue(e sqlir.Expr) bool {
	switch e := e.(type) {
	case *sqlir.Param, *sqlir.Const:
		return true
	case *sqlir.Cast:
		return isValue(e.X)
	}
	return false
}

func simpleTargets(ts []sqlir.Target) bool {
	for _, t := range ts {
		if !t.Star && !simpleExpr(t.Expr) {
			return false
		}
	}
	return true
}
