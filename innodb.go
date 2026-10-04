package detest

import (
	"maps"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// InnoDB's semantics where they differ from PostgreSQL's: consistent reads
// from a snapshot at Repeatable Read, a failed statement rolling back only
// itself, and gap locks (gap.go).

// version is a committed state of a row, nil after a delete, with the commit
// sequence number of the transaction that left it.
type version struct {
	seq int64
	row Row
}

// recordVersions keeps the rows a commit leaves as new versions, so that a
// snapshot taken before it still reads the old ones. Only InnoDB reads from
// snapshots; other servers skip the bookkeeping.
func (tx *Tx) recordVersions() {
	db := tx.db
	if !db.kind.InnoDB() || len(tx.writes)+len(tx.deleted) == 0 {
		return
	}
	db.seq++
	for lk, r := range tx.writes {
		db.addVersion(lk, r)
	}
	for lk := range tx.deleted {
		db.addVersion(lk, nil)
	}
}

func (db *DB) addVersion(lk lockKey, r Row) {
	t := db.history[lk.table]
	if t == nil {
		t = map[string][]version{}
		db.history[lk.table] = t
	}
	if len(t[lk.key]) == 0 {
		// The state before this first change was committed before any
		// snapshot of the run, as a seed or an earlier commit was.
		if old, ok := db.committed[lk.table][lk.key]; ok {
			t[lk.key] = append(t[lk.key], version{seq: 0, row: old})
		} else {
			t[lk.key] = append(t[lk.key], version{seq: 0})
		}
	}
	t[lk.key] = append(t[lk.key], version{seq: db.seq, row: r})
}

// consistent reports whether the transaction's plain reads come from a
// snapshot: InnoDB at Repeatable Read, or Serializable outside a
// transaction block, where a plain read is not turned into a locking one.
func (tx *Tx) consistent() bool {
	return tx.db.kind.InnoDB() && (tx.iso == RepeatableRead || tx.iso == Serializable)
}

// snapshotRows is a consistent read of a table: the rows as of the
// transaction's snapshot, taken at its first consistent read, with its own
// writes over them.
func (tx *Tx) snapshotRows(table string) []Row {
	table = tx.db.resolve(table)
	if tx.snap < 0 {
		tx.snap = tx.db.seq
	}
	keys := map[string]bool{}
	for k := range tx.db.committed[table] {
		keys[k] = true
	}
	for k := range tx.db.history[table] {
		keys[k] = true
	}
	for lk := range tx.writes {
		if lk.table == table {
			keys[lk.key] = true
		}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []Row
	for _, k := range sorted {
		lk := lockKey{table, k}
		if tx.deleted[lk] {
			continue
		}
		if w, ok := tx.writes[lk]; ok {
			out = append(out, w.clone())
			continue
		}
		if r := tx.db.versionAt(table, k, tx.snap); r != nil {
			out = append(out, r.clone())
		}
	}
	return out
}

// versionAt is the committed row as of commit sequence number seq.
func (db *DB) versionAt(table, key string, seq int64) Row {
	vs := db.history[table][key]
	if len(vs) == 0 {
		return db.committed[table][key] // unchanged since before any snapshot
	}
	i := sort.Search(len(vs), func(i int) bool { return vs[i].seq > seq })
	if i == 0 {
		return nil
	}
	return vs[i-1].row
}

// stmtMark is the state of a transaction before a statement, which InnoDB
// returns to when the statement fails.
type stmtMark struct {
	writes   map[lockKey]Row
	deleted  map[lockKey]bool
	moved    map[lockKey]string
	deferred int
	locks    int
}

func (tx *Tx) markStatement() stmtMark {
	m := stmtMark{writes: make(map[lockKey]Row, len(tx.writes)), deleted: maps.Clone(tx.deleted), moved: maps.Clone(tx.moved), deferred: len(tx.deferred), locks: len(tx.locks)}
	for k, v := range tx.writes {
		m.writes[k] = v.clone()
	}
	return m
}

// failStatement undoes a failed statement as InnoDB does: the statement's
// writes go, and the transaction goes on with the locks the statement took,
// but for those of the rows it inserted, which go with the rows.
// A deadlock is the exception, rolling back the whole transaction, which then
// starts over empty.
func (tx *Tx) failStatement(m stmtMark, deadlock bool) {
	tx.aborted = false
	if deadlock {
		tx.writes, tx.deleted, tx.moved, tx.deferred, tx.saves = map[lockKey]Row{}, map[lockKey]bool{}, nil, nil, nil
		tx.snap = -1
		if tx.p == nil || !tx.p.r.over() {
			tx.releaseLocks(tx.locks)
			tx.releaseGaps()
		}
		tx.locks = nil
		return
	}
	tx.writes, tx.deleted, tx.moved, tx.deferred = m.writes, m.deleted, m.moved, tx.deferred[:m.deferred]
	tx.releaseInsertLocks(m.locks)
}

// autoIncrement fills row's AUTO_INCREMENT column as MySQL does: NULL, 0 or
// no value takes the counter's next value, and a larger explicit value moves
// the counter past it. It returns the value it generated. The counter is
// kept apart from the column's default, which ALTER COLUMN may change.
func (x *sqlExec) autoIncrement(table string, row Row) (int64, bool) {
	def := x.tx.db.defs[table]
	if def == nil {
		return 0, false
	}
	seqs := x.tx.db.seqs
	for col := range def.autoInc {
		key := autoIncKey(table, col)
		v, present := row[col]
		n, isInt := autoIncValue(derefValue(v))
		if !present || derefValue(v) == nil || isInt && n == 0 && !x.tx.noAutoZero {
			seqs[key]++
			row[col] = seqs[key]
			return seqs[key], true
		}
		if isInt && n > seqs[key] {
			seqs[key] = n
		}
	}
	return 0, false
}

// autoIncValue is an explicit value for an AUTO_INCREMENT column as the
// column stores it: a fraction or a numeric string rounded to an integer.
// What is no number is left to the column's checks to refuse.
func autoIncValue(v any) (int64, bool) {
	v = boolAsInt(v)
	if n, ok := integer(v); ok {
		return n, true
	}
	if b, isBytes := v.([]byte); isBytes {
		v = string(b) // database/sql may bind a string as []byte
	}
	if s, isStr := v.(string); isStr {
		n, parsed, fits := mysqlIntegerString(s)
		return n, parsed && fits
	}
	f, ok := toFloat(v)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int64(math.Round(f)), true
}

// mysqlNumeric is MySQL's grammar of a decimal number in a string.
var mysqlNumeric = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

// mysqlIntegerString is the integer MySQL stores for a numeric string,
// rounded half away from zero, computed exactly rather than through a
// float64. parsed is false for what is no number, fits for a result in
// int64.
func mysqlIntegerString(s string) (n int64, parsed, fits bool) {
	s = strings.TrimSpace(s)
	if !mysqlNumeric.MatchString(s) {
		return 0, false, false // big.Rat would also take 1/2, 0x10 or 1_2
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, false, false
	}
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if new(big.Int).Mul(new(big.Int).Abs(m), big.NewInt(2)).Cmp(r.Denom()) >= 0 {
		if r.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, true, false
	}
	return q.Int64(), true, true
}

// autoIncKey is where a table's AUTO_INCREMENT counter is kept among the
// run's sequences: by the schema-qualified table, so that tables of the same
// name in two databases count apart, and under a name no SQL sequence can
// have.
func autoIncKey(table, col string) string { return "\x00auto_increment " + table + " " + col }

// moveAutoInc carries the AUTO_INCREMENT counters of table over to the table
// to, or drops them when to is empty, as a rename or a drop does.
func (db *DB) moveAutoInc(table, to string) {
	prefix := autoIncKey(table, "")
	for k, v := range db.seqs {
		if strings.HasPrefix(k, prefix) {
			delete(db.seqs, k)
			if to != "" {
				db.seqs[autoIncKey(to, strings.TrimPrefix(k, prefix))] = v
			}
		}
	}
}

// mysqlTruth is v as a MySQL condition tests it: NULL stays NULL, a number is
// true unless 0, a string by the number it starts with, as MySQL converts it.
func mysqlTruth(v any) any {
	switch v := v.(type) {
	case nil:
		return nil
	case bool:
		return v
	case string:
		return mysqlNumber(v) != 0
	}
	if f, ok := toFloat(v); ok {
		return f != 0
	}
	return true
}

// mysqlSigned is CAST(v AS SIGNED): a number rounded half away from zero, a
// string by the integer it starts with, after leading whitespace, or 0.
func mysqlSigned(v any) any {
	v = boolAsInt(v) // go-sql-driver sends a boolean as 1 or 0
	if n, ok := integer(v); ok {
		return n
	}
	if s, ok := v.(string); ok {
		s = strings.TrimLeft(s, " \t\n\r")
		end := 0
		if end < len(s) && (s[end] == '+' || s[end] == '-') {
			end++
		}
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		n, _ := strconv.ParseInt(s[:end], 10, 64)
		return n
	}
	if f, ok := toFloat(v); ok {
		return int64(math.Round(f))
	}
	return int64(0)
}

// mysqlNumber is the number MySQL converts a string to: the number it starts
// with, after leading whitespace, or 0.
func mysqlNumber(s string) float64 {
	s = strings.TrimLeft(s, " \t\n\r")
	end := 0
	for end < len(s) && strings.ContainsRune("+-.0123456789eE", rune(s[end])) {
		end++
	}
	for end > 0 {
		if f, err := strconv.ParseFloat(s[:end], 64); err == nil {
			return f
		}
		end--
	}
	return 0
}

func boolAsInt(v any) any {
	if b, ok := derefValue(v).(bool); ok {
		if b {
			return int64(1)
		}
		return int64(0)
	}
	return v
}

// mysqlNumericType reports a column type MySQL compares a string with as a
// number.
func mysqlNumericType(t string) bool {
	return strings.HasPrefix(t, "int") || strings.HasPrefix(t, "uint") || t == "double" || t == "float" || t == "decimal"
}

// mysqlTemporalType reports a column type holding dates or times.
func mysqlTemporalType(t string) bool {
	return t == "date" || t == "datetime" || t == "timestamp" || t == "time"
}

// mysqlParseTime parses a DATETIME or DATE string in the forms MySQL and
// go-sql-driver write them, as a UTC time.
func mysqlParseTime(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02"} {
		if tm, err := time.ParseInLocation(layout, strings.TrimSpace(s), time.UTC); err == nil {
			return tm, true
		}
	}
	return time.Time{}, false
}

// mysqlTextType reports a column type holding strings, which MySQL
// compares with a number as numbers.
func mysqlTextType(t string) bool {
	return strings.Contains(t, "char") || strings.Contains(t, "text") || strings.Contains(t, "blob") ||
		strings.Contains(t, "binary") || t == "enum" || t == "set"
}

// mysqlArithOperand is an operand of MySQL's arithmetic: a string as the
// number it converts to, a boolean as 1 or 0, anything else as it is.
func mysqlArithOperand(v any) any {
	v = boolAsInt(v)
	switch s := derefValue(v).(type) {
	case string:
		return mysqlNumber(s)
	case []byte:
		return mysqlNumber(string(s))
	}
	return v
}

// comparable converts the operands of a comparison as the server does: see
// mysqlOperands. Postgres compares them as they are.
func (x *sqlExec) comparable(l, r any) (any, any) {
	if !x.tx.db.kind.InnoDB() {
		return l, r
	}
	return mysqlOperands(l, r)
}

// mysqlOperands converts the operands of a comparison as MySQL does: a string
// compared with a number is compared as the number it converts to.
func mysqlOperands(l, r any) (any, any) {
	// A boolean goes to MySQL as 1 or 0, as go-sql-driver sends it.
	l, r = boolAsInt(l), boolAsInt(r)
	str := func(v any) (string, bool) {
		switch v := derefValue(v).(type) {
		case string:
			return v, true
		case []byte:
			return string(v), true
		}
		return "", false
	}
	// A string compared with a time is read as a time.
	if s, ok := str(l); ok {
		if _, isTime := derefValue(r).(time.Time); isTime {
			if tm, ok := mysqlParseTime(s); ok {
				return tm, r
			}
		}
	}
	if s, ok := str(r); ok {
		if _, isTime := derefValue(l).(time.Time); isTime {
			if tm, ok := mysqlParseTime(s); ok {
				return l, tm
			}
		}
	}
	if s, ok := str(l); ok {
		if _, num := toFloat(derefValue(r)); num {
			return mysqlNumber(s), r
		}
	}
	if s, ok := str(r); ok {
		if _, num := toFloat(derefValue(l)); num {
			return l, mysqlNumber(s)
		}
	}
	// database/sql may bind a string as []byte, which compares as the
	// string rather than as the slice's printed bytes.
	if s, ok := str(l); ok {
		l = s
	}
	if s, ok := str(r); ok {
		r = s
	}
	return l, r
}
