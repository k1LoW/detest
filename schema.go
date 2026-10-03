package detest

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// relname is a table name without its schema, the name a FROM item is known by
// when it has no alias.
func relname(table string) string {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return table[i+1:]
	}
	return table
}

func rowEnv(table string, row Row) *env {
	return &env{tables: map[string]Row{relname(table): row}, merged: row}
}

// evaluator evaluates schema expressions (defaults, expression indexes,
// partial index predicates) for a write made through the Tx API.
func (tx *Tx) evaluator() *sqlExec {
	return &sqlExec{tx: tx, query: "(schema expression)", ctes: map[string][]Row{}}
}

// applyDefaults fills the columns a new row leaves out with their declared
// defaults, in declaration order so sequences advance deterministically.
func (x *sqlExec) applyDefaults(table string, row Row) error {
	def := x.tx.db.defs[table]
	if def == nil {
		return nil
	}
	for _, col := range def.columns {
		d, ok := def.defaults[col]
		if !ok {
			continue
		}
		if _, set := row[col]; set {
			continue
		}
		v, err := x.eval(d, &env{})
		if err != nil {
			if errors.As(err, new(errUnknownExpr)) {
				continue // a default detest cannot evaluate leaves the column NULL
			}
			return err
		}
		row[col] = v
	}
	return nil
}

// uniqueValues returns the values row takes in the unique index u. ok is false
// when the row is not in the index: outside a partial index's predicate, or
// with a NULL, which never collides unless the index is NULLS NOT DISTINCT.
func (x *sqlExec) uniqueValues(table string, u *sqlir.UniqueDef, row Row) (vals []any, ok bool, err error) {
	e := rowEnv(table, row)
	if u.Where != nil {
		in, err := x.evalBool(u.Where, e)
		if err != nil || !in {
			return nil, false, err
		}
	}
	vals = make([]any, len(u.Elems))
	for i, el := range u.Elems {
		v, err := x.eval(el, e)
		if err != nil {
			return nil, false, err
		}
		if derefValue(v) == nil && !u.NullsNotDistinct {
			return nil, false, nil
		}
		vals[i] = v
	}
	return vals, true, nil
}

func sameValues(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameValue(a[i], b[i]) {
			return false
		}
	}
	return true
}

// uniqueLock stands for one value of a unique index. The transaction writing a
// row with the value holds it until it commits or rolls back, so a second
// writer of the value waits for the first to finish, as Postgres makes a
// second inserter wait on the first's uncommitted index entry.
func uniqueLock(table string, u *sqlir.UniqueDef, vals []any) lockKey {
	return lockKey{table: "\x00unique\x00" + table + "\x00" + u.Name, key: encodeKey(vals)}
}

// claimUnique takes row's entry in the unique index u, waiting for a writer of
// the same value that has not finished, and returns the visible row other
// than self holding the value afterwards, if any.
func (x *sqlExec) claimUnique(table string, u *sqlir.UniqueDef, row Row, self string) (Row, error) {
	vals, ok, err := x.uniqueValues(table, u, row)
	if err != nil || !ok {
		return nil, err
	}
	if u.Primary {
		// The primary key's entry is the row lock itself, which a plain INSERT
		// of the same key takes too.
		key := encodeKey(vals)
		if err := x.tx.lock(lockKey{table, key}); err != nil {
			return nil, err
		}
		if ex, found := x.tx.view(table, key); found && key != self {
			return ex, nil
		}
		return nil, nil
	}
	if err := x.tx.lock(uniqueLock(table, u, vals)); err != nil {
		return nil, err
	}
	for _, ex := range x.tx.selectNoYield(table, nil) {
		if ex.Key() == self {
			continue
		}
		evals, ok, err := x.uniqueValues(table, u, ex)
		if err != nil {
			return nil, err
		}
		if ok && sameValues(evals, vals) {
			return ex, nil
		}
	}
	return nil, nil
}

// checkUniques enforces the unique constraints other than the primary key for
// row, which is new (old nil) or replaces old, the row with key self.
func (x *sqlExec) checkUniques(table string, row Row, self string, old Row) error {
	def := x.tx.db.defs[table]
	if def == nil {
		return nil
	}
	for i := range def.uniques {
		u := &def.uniques[i]
		if old != nil {
			nv, nok, err := x.uniqueValues(table, u, row)
			if err != nil {
				return err
			}
			ov, ook, err := x.uniqueValues(table, u, old)
			if err != nil {
				return err
			}
			if nok == ook && (!nok || sameValues(nv, ov)) {
				continue // the update leaves the row's entry in this index as it was
			}
		}
		ex, err := x.claimUnique(table, u, row, self)
		if err != nil {
			return err
		}
		if ex != nil {
			return x.tx.db.duplicateKey(table, u.Name)
		}
	}
	return nil
}

// defaultConstraintName is the name Postgres gives an unnamed unique
// constraint: table_column_key.
func defaultConstraintName(table string, u sqlir.UniqueDef) string {
	var name strings.Builder
	name.WriteString(relname(table))
	for _, e := range u.Elems {
		if c, ok := e.(*sqlir.ColumnRef); ok {
			name.WriteString("_" + c.Column)
		} else {
			name.WriteString("_expr")
		}
	}
	name.WriteString("_key")
	return name.String()
}

// rekey moves a row whose primary key an UPDATE changes: a row's identity is
// its key, so the old key's row goes and the new key takes the row, colliding
// with an existing row as an insert would. It returns the lock key to write
// the row under.
func (x *sqlExec) rekey(table string, lk lockKey, updated Row) (lockKey, error) {
	def := x.tx.db.defs[table]
	if def == nil || len(def.pk) == 0 {
		return lk, nil
	}
	if err := x.tx.db.assignKey(table, updated); err != nil {
		return lk, err
	}
	nlk := lockKey{table, updated.Key()}
	if nlk == lk {
		return lk, nil
	}
	if err := x.tx.lock(nlk); err != nil {
		return lk, err
	}
	if _, exists := x.tx.view(table, nlk.key); exists {
		return lk, x.tx.db.duplicateKey(table, x.tx.db.pkConstraint(table))
	}
	delete(x.tx.writes, lk)
	x.tx.deleted[lk] = true
	delete(x.tx.deleted, nlk)
	if x.tx.moved == nil {
		x.tx.moved = map[lockKey]string{}
	}
	x.tx.moved[lk] = nlk.key
	return nlk, nil
}

// conflictTargets returns the unique indexes an INSERT ... ON CONFLICT (cols)
// arbitrates on: the one over exactly cols, or every one, primary key
// included, without a column list.
func (x *sqlExec) conflictTargets(table string, cols []string) ([]sqlir.UniqueDef, error) {
	def := x.tx.db.defs[table]
	var all []sqlir.UniqueDef
	if len(def.pk) > 0 {
		pk := sqlir.UniqueDef{Name: "primary key", Primary: true}
		for _, c := range def.pk {
			pk.Elems = append(pk.Elems, &sqlir.ColumnRef{Column: c})
		}
		all = append(all, pk)
	}
	all = append(all, def.uniques...)
	if len(cols) == 0 {
		return all, nil
	}
	for _, u := range all {
		if sameColumns(u, cols) {
			return []sqlir.UniqueDef{u}, nil
		}
	}
	return nil, x.tx.db.kind.Error(sqlir.InvalidColumnReference, "there is no unique or exclusion constraint matching the ON CONFLICT specification", relname(table), strings.Join(cols, ", "), "")
}

func sameColumns(u sqlir.UniqueDef, cols []string) bool {
	if len(u.Elems) != len(cols) || u.Where != nil {
		return false
	}
	for _, e := range u.Elems {
		c, ok := e.(*sqlir.ColumnRef)
		if !ok || !strings.Contains(","+strings.Join(cols, ",")+",", ","+c.Column+",") {
			return false
		}
	}
	return true
}

// checkTypes makes the checks Postgres makes when a value is stored in a
// typed column: a uuid must parse, and an integer must fit its width. Types
// detest does not check accept any value.
// checkRow makes the checks Postgres makes on a row about to be written, in
// its order: the column types, NOT NULL, then CHECK constraints.
func (x *sqlExec) checkRow(table string, row Row) error {
	def := x.tx.db.defs[table]
	if def != nil {
		// Every write checks its row here, so this is where the generated
		// columns get their values, before the checks that may read them.
		if err := x.generate(table, def, row); err != nil {
			return err
		}
	}
	if err := x.checkTypes(table, row); err != nil {
		return err
	}
	if def == nil {
		return nil
	}
	for _, col := range def.columns { // in declaration order, for a stable error
		if def.notNull[col] && derefValue(row[col]) == nil {
			return x.tx.db.kind.Error(sqlir.NotNullViolation, fmt.Sprintf("null value in column %q of relation %q violates not-null constraint", col, relname(table)), relname(table), col, "")
		}
	}
	for _, c := range def.checks {
		v, err := x.eval(c.Expr, c.env(table, row))
		if err != nil {
			if errors.As(err, new(errUnknownExpr)) {
				continue // a check detest cannot evaluate passes, as an unknown default does
			}
			return err
		}
		// NULL passes a CHECK, as in Postgres; only false fails it.
		if b, ok := derefValue(v).(bool); ok && !b {
			return x.tx.db.kind.Error(sqlir.CheckViolation, fmt.Sprintf("new row for relation %q violates check constraint %q", relname(table), c.Name), relname(table), "", c.Name)
		}
	}
	return nil
}

func (x *sqlExec) checkTypes(table string, row Row) error {
	def := x.tx.db.defs[table]
	if def == nil || len(def.types) == 0 {
		return nil
	}
	for col, v := range row {
		v = derefValue(v)
		if v == nil {
			continue
		}
		switch t := def.types[col]; t {
		case "uuid":
			if s, ok := v.(string); ok && !validUUID(s) {
				return x.tx.db.kind.Error(sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type uuid: %q", s), relname(table), col, "")
			}
		case "int2", "int4":
			n, ok := integer(v)
			if !ok {
				continue
			}
			lim, name := int64(math.MaxInt32), "integer"
			if t == "int2" {
				lim, name = math.MaxInt16, "smallint"
			}
			if n > lim || n < -lim-1 {
				return x.tx.db.kind.Error(sqlir.NumericValueOutOfRange, name+" out of range", relname(table), col, "")
			}
		}
	}
	return nil
}

// validUUID accepts what Postgres's uuid input does: 32 hex digits, with or
// without hyphens between groups of four, optionally in braces.
func validUUID(s string) bool {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	n := 0
	for i, c := range s {
		switch {
		case c == '-':
			if i == 0 || n%4 != 0 || strings.HasSuffix(s[:i], "-") {
				return false
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
			n++
		default:
			return false
		}
	}
	return n == 32 && !strings.HasSuffix(s, "-")
}

// generate sets the generated columns of row from the other columns.
func (x *sqlExec) generate(table string, def *tableDef, row Row) error {
	for _, col := range def.columns {
		g := def.generated[col]
		if g == nil {
			continue
		}
		v, err := x.eval(g.Expr, g.env(table, row))
		if err != nil {
			if !errors.As(err, new(errUnknownExpr)) {
				return err
			}
			v = sqlir.Unknown
		}
		row[col] = v
	}
	return nil
}

// writesGenerated refuses a statement that gives a generated column a value
// other than DEFAULT, which Postgres rejects. exprs are the values written to
// cols, nil when they come from a query.
func (x *sqlExec) writesGenerated(table string, cols []string, exprs []sqlir.Expr) error {
	def := x.tx.db.defs[table]
	if def == nil || len(def.generated) == 0 {
		return nil
	}
	for i, col := range cols {
		if def.generated[col] == nil {
			continue
		}
		if exprs != nil {
			if _, isDefault := exprs[i].(*sqlir.Default); isDefault {
				continue
			}
		}
		return x.unsupported(fmt.Sprintf("a value other than DEFAULT for generated column %q", col))
	}
	return nil
}

// insertsGenerated refuses an INSERT that writes a generated column, before
// it evaluates anything, as Postgres does when it plans the statement.
func (x *sqlExec) insertsGenerated(table string, ins *sqlir.InsertStmt, cols []string) error {
	def := x.tx.db.defs[table]
	if def == nil || len(def.generated) == 0 {
		return nil
	}
	if ins.OnConflict != nil {
		if err := x.writesGenerated(table, assignedColumns(ins.OnConflict.Set), assignedValues(ins.OnConflict.Set)); err != nil {
			return err
		}
	}
	for _, exprs := range ins.Rows {
		if err := x.writesGenerated(table, cols[:min(len(cols), len(exprs))], exprs); err != nil {
			return err
		}
	}
	if ins.Select == nil {
		return nil
	}
	width := len(cols)
	if len(ins.Columns) == 0 {
		var ok bool
		if width, ok = selectWidth(ins.Select); !ok {
			return x.unsupported("INSERT ... SELECT without a column list into a table with generated columns")
		}
	}
	return x.writesGenerated(table, cols[:min(len(cols), width)], nil)
}

// selectWidth is the number of columns a query returns, when it is known
// without running it.
func selectWidth(sel *sqlir.SelectStmt) (int, bool) {
	switch {
	case sel.SetOp != "":
		return selectWidth(sel.Larg)
	case sel.Values != nil:
		if len(sel.Values) == 0 {
			return 0, false
		}
		return len(sel.Values[0]), true
	}
	for _, t := range sel.Targets {
		if t.Star {
			return 0, false
		}
	}
	return len(sel.Targets), true
}
