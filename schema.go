package detest

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	return &sqlExec{tx: tx, query: "(schema expression)", ctes: map[string][]Row{}, start: time.Now()}
}

// applyDefaults fills the columns a new row leaves out with their declared
// defaults, in declaration order so sequences advance deterministically.
func (x *sqlExec) applyDefaults(table string, row Row) error {
	def := x.tx.db.defs[table]
	if def == nil {
		return nil
	}
	for _, col := range def.columns {
		if _, ok := def.defaults[col]; !ok {
			continue
		}
		if _, set := row[col]; set {
			continue
		}
		v, err := x.columnDefault(table, col)
		if err != nil {
			return err
		}
		row[col] = v
	}
	return nil
}

// columnDefault computes the default of a column, for a row that leaves it
// out or a SET col = DEFAULT. A column without one, or a generated column,
// which is computed from the row afterwards, takes NULL.
func (x *sqlExec) columnDefault(table, col string) (any, error) {
	def := x.tx.db.defs[table]
	if def == nil || def.generated[col] != nil {
		return nil, nil
	}
	d, ok := def.defaults[col]
	if !ok {
		return nil, nil
	}
	// A default detest cannot compute is written as the Unknown marker,
	// not refused and not NULL. A default no process reads cannot change
	// the outcome, and refusing it would make every INSERT that leaves the
	// column out unsupported, while dumps routinely hold such defaults
	// (ARRAY[]::text[]). One a process does read comes back as a value the
	// server never produces, where NULL would pass for a real one. The
	// table's own key, NOT NULL, constraints and generated columns are
	// readers too, and would be decided from the marker, so a column one
	// of them reads is refused instead.
	v, err := x.eval(d, &env{})
	if err != nil && !errors.As(err, new(errUnknownExpr)) {
		return nil, err
	}
	if err != nil {
		if def.reads(col) {
			return nil, x.unsupported(fmt.Sprintf("the default of column %q, whose expression detest cannot compute and which a key, constraint or generated column reads", col))
		}
		v = sqlir.Unknown
	}
	return v, nil
}

// assignedValue evaluates the value a SET assigns, computing the column's
// default for SET col = DEFAULT, which the evaluator alone takes as NULL.
func (x *sqlExec) assignedValue(table string, a sqlir.Assignment, en *env, where string) (any, error) {
	if _, ok := a.Value.(*sqlir.Default); ok {
		return x.columnDefault(table, a.Column)
	}
	v, err := x.eval(a.Value, en)
	if err != nil {
		return nil, x.unsupportedExpr(err, where)
	}
	return v, nil
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
		if dup, err := x.sharedDuplicate(table, key, self); err != nil || dup != nil {
			return dup, err
		}
		if err := x.tx.lock(lockKey{table, key}); err != nil {
			return nil, err
		}
		if ex, found := x.tx.view(table, key); found && key != self {
			return ex, nil
		}
		return nil, nil
	}
	if x.tx.db.kind.InnoDB() {
		// A duplicate of a row already there is checked under a shared lock
		// on that row, as InnoDB's duplicate check takes one, so two inserts
		// failing on the same row do not wait for each other.
		for _, ex := range x.tx.selectNoYield(table, nil) {
			if ex.Key() == self {
				continue
			}
			evals, ok, err := x.uniqueValues(table, u, ex)
			if err != nil {
				return nil, err
			}
			if ok && sameValues(evals, vals) {
				if dup, err := x.sharedDuplicate(table, ex.Key(), self); err != nil || dup != nil {
					return dup, err
				}
			}
		}
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

// sharedDuplicate is, on InnoDB, the row under key that an insert duplicates,
// locked in share mode as InnoDB's duplicate check locks it, or nil when no
// such row is there, or it went while the lock waited, and the insert goes
// on to claim the key.
func (x *sqlExec) sharedDuplicate(table, key, self string) (Row, error) {
	if !x.tx.db.kind.InnoDB() || key == self {
		return nil, nil
	}
	if _, found := x.tx.view(table, key); !found {
		return nil, nil
	}
	if err := x.tx.lockMode(lockKey{table, key}, lockShare); err != nil {
		return nil, err
	}
	ex, found := x.tx.view(table, key)
	if !found {
		return nil, nil
	}
	return ex, nil
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
func (x *sqlExec) conflictTargets(table string, oc *sqlir.OnConflict) ([]sqlir.UniqueDef, error) {
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
	if oc.Constraint != "" {
		for _, u := range all {
			if u.Name == oc.Constraint || u.Primary && oc.Constraint == x.tx.db.pkConstraint(table) {
				return []sqlir.UniqueDef{u}, nil
			}
		}
		return nil, x.unsupported(fmt.Sprintf("ON CONFLICT ON CONSTRAINT %q, which is no unique constraint of the table", oc.Constraint))
	}
	if len(oc.Elems) == 0 {
		if !oc.DoNothing && !x.tx.db.kind.InnoDB() {
			return nil, x.tx.db.kind.Error(sqlir.SyntaxError, "ON CONFLICT DO UPDATE requires inference specification or constraint name", relname(table), "", "")
		}
		return all, nil
	}
	// Postgres arbitrates on every unique index whose elements are the
	// inference elements in any order, a partial one only when the
	// inference predicate implies its own.
	var out []sqlir.UniqueDef
	for _, u := range all {
		if !sameElems(u.Elems, oc.Elems) {
			continue
		}
		if u.Where != nil {
			if oc.InferWhere == nil {
				continue
			}
			if !reflect.DeepEqual(u.Where, oc.InferWhere) {
				// Postgres proves the implication; detest only sees an
				// identical predicate, and would otherwise pick other
				// arbiters than the server.
				return nil, x.unsupported("ON CONFLICT ... WHERE with a predicate other than the partial index's own")
			}
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil, x.tx.db.kind.Error(sqlir.InvalidColumnReference, "there is no unique or exclusion constraint matching the ON CONFLICT specification", relname(table), strings.Join(oc.Columns, ", "), "")
	}
	return out, nil
}

// sameElems reports whether a unique index's elements are the inference
// elements, in any order.
func sameElems(index, infer []sqlir.Expr) bool {
	if len(index) != len(infer) {
		return false
	}
	for _, e := range index {
		if !slices.ContainsFunc(infer, func(f sqlir.Expr) bool { return reflect.DeepEqual(e, f) }) {
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
	if def != nil && len(def.generated) > 0 {
		// Every write checks its row here, so this is where the generated
		// columns get their values: after the values given are checked, as
		// Postgres coerces them first, and before the checks that may read
		// the computed ones.
		if err := x.checkTypes(table, row); err != nil {
			return err
		}
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
			return x.unsupportedExpr(err, fmt.Sprintf("in check constraint %q", c.Name))
		}
		// NULL passes a CHECK, as in Postgres; only false fails it.
		if b, ok := derefValue(v).(bool); ok && !b {
			return x.tx.db.kind.Error(sqlir.CheckViolation, fmt.Sprintf("new row for relation %q violates check constraint %q", relname(table), c.Name), relname(table), "", c.Name)
		}
	}
	return nil
}

// intRange is the range of an integer column type, by width in bytes: int2
// (smallint) and int4 (integer) in Postgres, and MySQL's tinyint (int1)
// through int, with u for unsigned. An unsigned bigint (uint8) is checked
// only for being negative.
func intRange(t string) (lo, hi int64) {
	unsigned := t[0] == 'u'
	bits := map[byte]uint{'1': 8, '2': 16, '3': 24, '4': 32, '8': 64}[t[len(t)-1]]
	if unsigned {
		if bits == 64 {
			return 0, math.MaxInt64
		}
		return 0, 1<<bits - 1
	}
	return -(1 << (bits - 1)), 1<<(bits-1) - 1
}

func (x *sqlExec) checkTypes(table string, row Row) error {
	def := x.tx.db.defs[table]
	if def == nil || len(def.types) == 0 {
		return nil
	}
	// In the schema's column order, so that a row wrong in two columns
	// reports the same error in every replay.
	cols := slices.Clone(def.columns)
	for _, c := range slices.Sorted(maps.Keys(row)) {
		if !slices.Contains(cols, c) {
			cols = append(cols, c)
		}
	}
	for _, col := range cols {
		v, present := row[col]
		if !present {
			continue
		}
		v = derefValue(v)
		if v == nil {
			continue
		}
		t := def.types[col]
		if x.tx.db.kind.InnoDB() && (mysqlTextType(t) || mysqlTemporalType(t)) {
			if l, ok := def.strs[col]; ok && l.members != nil {
				stored, err := x.mysqlMember(table, col, l, v)
				if err != nil {
					return err
				}
				row[col] = stored
				continue
			}
			stored, err := x.mysqlStoredText(t, v, def.fsp[col])
			if err != nil {
				return err
			}
			if s, isStr := stored.(string); isStr && def.strs[col].maxLen > 0 && utf8.RuneCountInString(s) > def.strs[col].maxLen {
				return x.tx.db.kind.Error(sqlir.StringDataRightTruncation, fmt.Sprintf("Data too long for column '%s'", col), relname(table), col, "")
			}
			row[col] = stored
			continue
		}
		// Unknown stands for a value detest could not compute, not one the
		// statement wrote, so it is kept to be reported where it is read.
		if name, ok := numberTypes[t]; ok && !x.tx.db.kind.InnoDB() && v != sqlir.Unknown {
			if !isNumber(v) && !isText(v) {
				return x.unsupported(fmt.Sprintf("a %T written to the %s column %q", v, name, col))
			}
			if isOtherNumberText(v) {
				return x.unsupported(fmt.Sprintf("number text in a form detest does not model, written to column %q", col))
			}
			if t == "numeric" && !exactAsFloat(v) {
				return x.unsupported(fmt.Sprintf("a numeric value with more digits than a float keeps, written to column %q", col))
			}
			n, err := columnNumber(v, t)
			if errors.Is(err, strconv.ErrRange) {
				return x.tx.db.kind.Error(sqlir.NumericValueOutOfRange, fmt.Sprintf("value %q is out of range for type %s", v, name), relname(table), col, "")
			}
			if err != nil {
				return x.tx.db.kind.Error(sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type %s: %q", name, v), relname(table), col, "")
			}
			// Postgres sorts NaN above every number, which the comparisons
			// do not, so a NaN would match ranges it is outside of.
			if f, ok := toFloat(n); ok && math.IsNaN(f) {
				return x.unsupported(fmt.Sprintf("NaN written to column %q", col))
			}
			if strings.HasPrefix(t, "int") {
				if n, ok = integralNumber(n); !ok {
					return x.unsupported(fmt.Sprintf("a number with a fraction written to the %s column %q", name, col))
				}
			}
			// A numeric has no negative zero, while a float keeps one that
			// still equals 0, which keys made from the value tell apart.
			if f, ok := n.(float64); ok && f == 0 && math.Signbit(f) {
				if t != "numeric" {
					return x.unsupported(fmt.Sprintf("a negative zero written to column %q", col))
				}
				n = float64(0)
			}
			if l, ok := def.nums[col]; ok && t == "numeric" && !x.tx.db.kind.InnoDB() {
				if n, err = scaledNumeric(n, l); err != nil {
					return x.tx.db.kind.Error(sqlir.NumericValueOutOfRange, "numeric field overflow", relname(table), col, "")
				}
			}
			row[col], v = n, n
		}
		if textTypes[t] {
			// A number written to a text column is stored as its text, as
			// Postgres's assignment does, so it compares as text afterwards.
			// Postgres keeps the digits of a numeric literal (1.20) and a
			// driver sends a float parameter as it formats it, which the
			// value does not tell apart.
			switch {
			case isNumber(v):
				if _, ok := integer(v); !ok {
					return x.unsupported(fmt.Sprintf("a number with a fraction written to the text column %q", col))
				}
				v = fmt.Sprint(v)
			case isOther(v) && v != sqlir.Unknown:
				// The text a driver sends for a boolean or a time depends
				// on the driver, which detest does not model.
				return x.unsupported(fmt.Sprintf("a %T written to the text column %q", v, col))
			}
			// Bytes written to a text column are the text they hold, so
			// they order as text.
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if str, ok := v.(string); ok {
				if l, limited := def.strs[col]; limited && l.maxLen > 0 {
					stored, err := x.pgTextLimit(table, col, t, str, l.maxLen)
					if err != nil {
						return err
					}
					str = stored
				}
				row[col] = str
			}
			continue
		}
		switch t {
		case "double", "float", "decimal":
			if !x.tx.db.kind.InnoDB() {
				continue
			}
			// MySQL stores a number's value, whatever it is written as, so
			// '1.0' and 1 are the same key; a string that is no number is
			// refused in strict mode.
			v = boolAsInt(v)
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if str, ok := v.(string); ok {
				if !mysqlNumeric.MatchString(strings.TrimSpace(str)) {
					return x.tx.db.kind.Error(sqlir.DataTruncated, fmt.Sprintf("Data truncated for column '%s' at row 1", col), relname(table), col, "")
				}
				v = mysqlNumber(str)
			}
			f, ok := toFloat(v)
			if !ok {
				return x.unsupported(fmt.Sprintf("a %T stored in a %s column", v, t))
			}
			if t == "float" {
				f = float64(float32(f)) // FLOAT keeps single precision
			}
			row[col] = numeric(f)
		case "uuid":
			if s, ok := v.(string); ok && !validUUID(s) {
				return x.tx.db.kind.Error(sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type uuid: %q", s), relname(table), col, "")
			}
		case "int1", "int2", "int3", "int4", "int8", "uint1", "uint2", "uint3", "uint4", "uint8":
			if x.tx.db.kind.InnoDB() {
				// go-sql-driver sends a boolean as 1 or 0.
				if b, ok := v.(bool); ok {
					v = boolAsInt(b)
					row[col] = v
				}
				// database/sql may bind a string as []byte, which MySQL
				// converts as the string.
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				// MySQL stores a fraction or a numeric string in an integer
				// column rounded, and checks the range of what it stores.
				f, ok := toFloat(v)
				if s, isStr := v.(string); isStr {
					// Parsed exactly, as a float64 would lose BIGINTs above
					// 2^53.
					n, parsed, fits := mysqlIntegerString(s)
					switch {
					case !parsed:
						// Strict mode refuses what is not a number rather than store it.
						return x.tx.db.kind.Error(sqlir.InvalidTextRepresentation, fmt.Sprintf("Incorrect integer value: '%s' for column '%s'", s, col), relname(table), col, "")
					case !fits:
						return x.tx.db.kind.Error(sqlir.NumericValueOutOfRange, fmt.Sprintf("Out of range value for column '%s'", col), relname(table), col, "")
					}
					v, ok = n, false
					row[col] = v
				}
				if _, isInt := integer(v); ok && !isInt {
					v = int64(math.Round(f))
					row[col] = v
				}
			}
			n, ok := integer(v)
			if !ok {
				if x.tx.db.kind.InnoDB() && v != sqlir.Unknown {
					// What is left is a value the conversions above do not
					// know, such as a bound time.Time, whose text MySQL
					// would refuse.
					return x.unsupported(fmt.Sprintf("a %T stored in an integer column", v))
				}
				continue
			}
			lo, hi := intRange(t)
			if n < lo || n > hi {
				msg := map[string]string{"int2": "smallint out of range", "int4": "integer out of range"}[t]
				if x.tx.db.kind.InnoDB() {
					msg = fmt.Sprintf("Out of range value for column '%s'", col)
				}
				return x.tx.db.kind.Error(sqlir.NumericValueOutOfRange, msg, relname(table), col, "")
			}
		}
	}
	return nil
}

// mysqlStoredText is v as a MySQL text or temporal column stores it. A text
// column stores a number or a boolean as its decimal text, which later
// compares as text: '10' < '2'; a time as go-sql-driver sends one. A DATE,
// DATETIME or TIMESTAMP column holds a time, in UTC as go-sql-driver sends
// one by default, so a value written as a string and one written as a time
// compare alike; a DATE drops the time of day, and a DATETIME or TIMESTAMP
// rounds to the fractional seconds it keeps. A float's text, a number in
// a temporal column, which MySQL reads as a date, a temporal string detest
// does not parse, and a TIME value are refused.
func (x *sqlExec) mysqlStoredText(t string, v any, fsp int) (any, error) {
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	if t == "time" {
		// A duration of its own syntax and range, which detest does not
		// model.
		return nil, x.unsupported("a value stored in a TIME column")
	}
	if mysqlTemporalType(t) {
		var tm time.Time
		switch v := v.(type) {
		case time.Time:
			tm = v.UTC()
		case string:
			parsed, ok := mysqlParseTime(v)
			if !ok {
				return nil, x.unsupported(fmt.Sprintf("the %s value %q, in a format detest does not parse", t, v))
			}
			tm = parsed
		default:
			return nil, x.unsupported(fmt.Sprintf("a %T stored in a %s column", v, t))
		}
		if t == "date" {
			tm = time.Date(tm.Year(), tm.Month(), tm.Day(), 0, 0, 0, 0, time.UTC)
		}
		// MySQL rounds fractional seconds to the column's precision, none
		// without one declared.
		return tm.Round(time.Duration(math.Pow10(9 - fsp))), nil
	}
	switch v := boolAsInt(v).(type) {
	case string:
		return v, nil
	case time.Time:
		return v.UTC().Format("2006-01-02 15:04:05.999999"), nil
	case float64:
		return nil, x.unsupported("a float stored in a " + t + " column")
	}
	if n, ok := integer(boolAsInt(v)); ok {
		if mysqlTemporalType(t) {
			return nil, x.unsupported("a number stored in a " + t + " column")
		}
		return strconv.FormatInt(n, 10), nil
	}
	return v, nil
}

// strLimit is what a string column holds: at most maxLen characters, or,
// with members, one of them (a MySQL ENUM) or a set of them (SET).
type strLimit struct {
	maxLen  int
	members []string
	set     bool
}

// pgTextLimit is s as a Postgres varchar(n) or char(n) column stores it.
// Characters past n are dropped when they are spaces and refused otherwise.
// A char(n) pads a shorter value with spaces and then compares it without
// them, which detest's plain strings do not follow, so only values of the
// declared length are supported there, and 'a  ' in a char(3) is short too.
func (x *sqlExec) pgTextLimit(table, col, t, s string, n int) (string, error) {
	if utf8.RuneCountInString(s) > n {
		if utf8.RuneCountInString(strings.TrimRight(s, " ")) > n {
			name := fmt.Sprintf("character varying(%d)", n)
			if t == "bpchar" {
				name = fmt.Sprintf("character(%d)", n)
			}
			return "", x.tx.db.kind.Error(sqlir.StringDataRightTruncation, "value too long for type "+name, relname(table), col, "")
		}
		s = string([]rune(s)[:n])
	}
	if t == "bpchar" && utf8.RuneCountInString(strings.TrimRight(s, " ")) < n {
		return "", x.unsupported(fmt.Sprintf("a value shorter than the char(%d) column %q, which Postgres pads with spaces", n, col))
	}
	return s, nil
}

// numLimit is a NUMERIC(p, s) column's precision and scale.
type numLimit struct {
	precision int
	scale     int
}

// scaledNumeric is v as a NUMERIC(p, s) column stores it: rounded to s
// places, half away from zero, and refused when more than p - s digits are
// left before the point. The rounding reads the shortest decimal form of
// the float rather than its binary value, so 1.005 rounds to 1.01 as the
// text written does, not to 1.00 as the nearest float would.
func scaledNumeric(v any, l numLimit) (any, error) {
	f, ok := toFloat(v)
	if !ok || math.IsNaN(f) {
		return v, nil
	}
	if math.IsInf(f, 0) {
		return nil, errors.New("numeric field overflow") // no finite precision holds it
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'f', -1, 64))
	if !ok {
		return v, nil
	}
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(l.scale)), nil)
	num := new(big.Int).Mul(r.Num(), unit)
	q, rem := new(big.Int).QuoRem(num, r.Denom(), new(big.Int))
	if rem.Abs(rem).Lsh(rem, 1).Cmp(r.Denom()) >= 0 {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(l.precision)), nil)
	if new(big.Int).Abs(q).Cmp(limit) >= 0 {
		return nil, errors.New("numeric field overflow")
	}
	out, _ := new(big.Rat).SetFrac(q, unit).Float64()
	return numericValue(out), nil
}

// mysqlMember is v as an ENUM or a SET column stores it: the members it
// names, matched without regard to case as MySQL's default collation does,
// spelled as declared, a SET's in declaration order. A number, which MySQL
// reads as an index or a bitmask, is refused, and a value that names no
// member fails as strict mode fails it.
func (x *sqlExec) mysqlMember(table, col string, l strLimit, v any) (any, error) {
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	s, ok := v.(string)
	if !ok {
		return nil, x.unsupported(fmt.Sprintf("a %T stored in an ENUM or a SET column", v))
	}
	truncated := x.tx.db.kind.Error(sqlir.DataTruncated, fmt.Sprintf("Data truncated for column '%s' at row 1", col), relname(table), col, "")
	find := func(name string) int {
		return slices.IndexFunc(l.members, func(m string) bool { return strings.EqualFold(m, strings.TrimRight(name, " ")) })
	}
	if !l.set {
		i := find(s)
		if i < 0 {
			return nil, truncated
		}
		return l.members[i], nil
	}
	in := make([]bool, len(l.members))
	if s != "" {
		for name := range strings.SplitSeq(s, ",") {
			i := find(name)
			if i < 0 {
				return nil, truncated
			}
			in[i] = true
		}
	}
	var out []string
	for i, m := range l.members {
		if in[i] {
			out = append(out, m)
		}
	}
	return strings.Join(out, ","), nil
}

// numberTypes are the number types by the name Postgres gives them in errors.
var numberTypes = map[string]string{"int2": "smallint", "int4": "integer", "int8": "bigint", "float4": "real", "float8": "double precision", "numeric": "numeric"}

// exactAsFloat reports whether a numeric, an integer or text, reads back as
// the same number from the float detest keeps, so that two values Postgres tells
// apart, such as 9007199254740992 and 9007199254740993, do not become one.
// Text that is not a number is left to the conversion to report.
func exactAsFloat(v any) bool {
	if n, ok := integer(v); ok {
		f := float64(n)
		return f < 1<<63 && int64(f) == n
	}
	s, ok := v.(string)
	if b, isBytes := v.([]byte); isBytes {
		s, ok = string(b), true
	}
	if !ok {
		return true
	}
	s = strings.TrimSpace(s)
	want, ok := new(big.Rat).SetString(s)
	if !ok {
		return true
	}
	f, err := parseNumber(s)
	if err != nil {
		// A number beyond a float's range, such as 1e400, is valid input
		// that the float cannot keep, not a syntax error.
		return !errors.Is(err, strconv.ErrRange)
	}
	got, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	return ok && got.Cmp(want) == 0
}

// integralNumber returns a float written to an integer column as an integer.
// Postgres rounds a numeric literal with a fraction but refuses a float
// parameter with one, and the value does not tell which of the two it came
// from, so one with a fraction is refused.
func integralNumber(v any) (any, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	default:
		return v, true
	}
	if f != math.Trunc(f) || math.Abs(f) >= 1<<53 {
		return nil, false
	}
	return int64(f), true
}

// textTypes are the character types.
var textTypes = map[string]bool{"text": true, "varchar": true, "bpchar": true}

// columnNumber converts a value written to a column of the number type t to
// the number it holds, as the type's input function does for text. The error
// tells text that does not parse from a number out of range
// (strconv.ErrRange). The
// digits as written, such as the 0 of 1.50, are not kept.
func columnNumber(v any, t string) (any, error) {
	var s string
	switch b := v.(type) {
	case string:
		s = b
	case []byte:
		s = string(b)
	default:
		switch t {
		case "numeric":
			return numericValue(v), nil
		case "float4", "float8":
			// An integer is stored as the float it becomes, so arithmetic
			// on the column is float arithmetic (1 / 2 is 0.5).
			if n, ok := integer(v); ok {
				return float64(n), nil
			}
		}
		return v, nil
	}
	s = strings.TrimSpace(s)
	switch t {
	case "int2", "int4", "int8":
		return strconv.ParseInt(s, 10, 64)
	case "float4", "float8":
		return parseNumber(s)
	}
	f, err := parseNumber(s)
	return numericValue(f), err
}

// numericValue is the representation of a numeric: a float, so arithmetic on
// it is not integer arithmetic (1 / 2 is 0.5). Keeping an integer beyond 2^53
// as an integer would make arithmetic on it integer arithmetic, so such a
// value is refused before it gets here (exactAsFloat).
func numericValue(v any) any {
	if n, ok := integer(v); ok {
		return float64(n)
	}
	if f, ok := v.(float64); ok && f == 0 {
		return float64(0) // a numeric has no negative zero
	}
	return v
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
		// The schema loads with an expression detest cannot convert or
		// evaluate, but a value it cannot compute must not be stored as if
		// it were the column's.
		v, err := x.eval(g.Expr, g.env(table, row))
		if err != nil && !errors.As(err, new(errUnknownExpr)) {
			return err
		}
		if err != nil {
			return x.unsupported(fmt.Sprintf("generated column %q, whose expression detest cannot evaluate", col))
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
