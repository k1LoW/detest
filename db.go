package detest

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
)

// Row is a database row. The key column is "id".
type Row map[string]any

// Key returns the row's identity: the id column, or the hidden _key assigned
// at insert for tables without an id.
func (r Row) Key() string {
	if k, ok := r["_key"]; ok {
		return keyString(k)
	}
	return keyString(r["id"])
}

// keyString is fmt.Sprint(v), with the key types rows use most spelled out:
// keys are built on every row access, and fmt allocates for each.
func keyString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case float64:
		// Equal numbers have one key whatever their Go type, as the
		// comparisons take them as equal: a whole float is written as the
		// integer, and -0 as 0.
		if v == math.Trunc(v) && v >= -1<<63 && v < 1<<63 {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return fmt.Sprint(v)
}

// Str returns a column as a string ("" when absent).
func (r Row) Str(k string) string {
	if v, ok := r[k]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// Int returns a column as an int (0 when absent).
func (r Row) Int(k string) int {
	switch v := r[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// Int64 returns a column as an int64 (0 when absent).
func (r Row) Int64(k string) int64 {
	switch v := r[k].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

// Bool returns a column as a bool (false when absent).
func (r Row) Bool(k string) bool { v, _ := r[k].(bool); return v }

func (r Row) String() string {
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "_key" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, r[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func (r Row) clone() Row {
	c := make(Row, len(r))
	maps.Copy(c, r)
	return c
}

func (r Row) ensureKey() {
	if _, ok := r["id"]; ok {
		return
	}
	if _, ok := r["_key"]; ok {
		return
	}
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, derefValue(r[k])))
	}
	r["_key"] = strings.Join(parts, "|")
}

type lockKey struct{ table, key string }

// DB is a simulated database with row-level locks and Read Committed visibility.
type DB struct {
	name      string
	kind      *sqlir.Impl
	s         *Sim
	committed map[string]map[string]Row
	locks     map[lockKey]rowLock
	touched   map[string]bool // tables committed to since the run's last snapshot
	// moved maps the key of a row an UPDATE gave a new primary key to that
	// key, so a transaction that waited on the row can follow it.
	moved map[lockKey]string

	// Declared by schema statements and kept across runs. Tables are named
	// schema-qualified ("public.orders"); resolve maps a name as written.
	defs     map[string]*tableDef
	matviews map[string]*sqlir.CreateTableAsStmt // the query each materialized view refreshes from
	views    map[string]*sqlir.SchemaChange      // the query of each view
	seqs     map[string]int64                    // sequence values of the run, for nextval
	uuids    int64                               // gen_random_uuid values handed out in the run
	ignored  map[string]bool                     // tables Ignore took out of the simulation

	// InnoDB's state of a run: the commit sequence number, the versions
	// commits left for snapshots to read, and the gap locks held.
	seq     int64
	history map[string]map[string][]version
	gaps    []*gapLock
}

// tableCheck is a CHECK constraint of a table. alias maps a column name its
// expression uses to the column's name now, after RENAME COLUMN: the
// expression is kept as written.
type tableCheck struct {
	sqlir.CheckDef
	alias map[string]string
}

// columns returns the columns the check's expression refers to, by their
// current names.
// renameColumn records that a column the expression refers to is renamed.
func (c *tableCheck) renameColumn(old, nw string) {
	renamed := false
	for k, v := range c.alias {
		if v == old {
			c.alias[k], renamed = nw, true
		}
	}
	// A name the expression wrote that already maps elsewhere refers to the
	// renamed column, not to one that took the name since.
	_, aliased := c.alias[old]
	if !renamed && !aliased && slices.ContainsFunc(sqlir.ColumnRefs(c.Expr), func(r *sqlir.ColumnRef) bool { return r.Column == old }) {
		if c.alias == nil {
			c.alias = map[string]string{}
		}
		c.alias[old] = nw
	}
}

// env is the row as the expression sees it, under the column names it was
// written with.
func (c tableCheck) env(table string, row Row) *env {
	r := row
	if len(c.alias) > 0 {
		r = row.clone()
		for written, now := range c.alias {
			r[written] = row[now]
		}
	}
	return &env{tables: map[string]Row{relname(table): r}, merged: r}
}

func (c tableCheck) columns() []string {
	var out []string
	for _, r := range sqlir.ColumnRefs(c.Expr) {
		name := r.Column
		if n, ok := c.alias[name]; ok {
			name = n
		}
		out = append(out, name)
	}
	return out
}

// tableDef is what the schema declares about a table.
type tableDef struct {
	pk       []string // primary key columns; nil without a primary key
	pkName   string
	uniques  []sqlir.UniqueDef // unique constraints and indexes other than the primary key
	fks      []sqlir.ForeignKey
	columns  []string          // in declaration order, for applying defaults deterministically
	types    map[string]string // column types, for the checks Postgres makes on write
	notNull  map[string]bool   // NOT NULL columns besides the primary key's
	checks   []tableCheck
	indexes  []sqlir.IndexDef      // indexes that are not unique, for InnoDB's gap locks
	autoInc  map[string]bool       // MySQL's AUTO_INCREMENT columns
	onUpdate map[string]sqlir.Expr // MySQL's ON UPDATE CURRENT_TIMESTAMP columns
	// strs are the limits on what a string column holds: CHAR(n) and
	// VARCHAR(n) lengths, and MySQL's ENUM and SET members.
	strs map[string]strLimit
	// nums are the Postgres NUMERIC(p, s) columns' precision and scale.
	nums map[string]numLimit
	// collation is a MySQL table's default collation, and ci the text
	// columns whose collation is case-insensitive, which detest's exact
	// string comparison does not follow.
	collation string
	ci        map[string]bool
	// fsp is the fractional seconds each MySQL DATETIME and TIMESTAMP
	// column keeps.
	fsp map[string]int
	// autoIncFloor is the counter AUTO_INCREMENT=n leaves, n - 1, which
	// every run starts from: the schema outlives the runs, the counters not.
	autoIncFloor int64
	defaults     map[string]sqlir.Expr
	// generated are the expressions of generated columns, kept as written
	// with the renames since in alias, as a CHECK's are.
	generated map[string]*tableCheck
}

// Name returns the database name.
func (db *DB) Name() string { return db.name }

// Ignore takes tables out of the simulation: writes to them succeed and are
// dropped, reads find them empty, they need no CREATE TABLE, take no locks
// and are no scheduling point, and foreign keys referencing them go
// unchecked. It is for tables the code under test writes but the invariants
// do not look at, such as an audit log, whose rows and locks would only grow
// the exploration. Names resolve on the search path as statements do.
func (db *DB) Ignore(tables ...string) {
	db.s.declare("Ignore")
	if db.ignored == nil {
		db.ignored = map[string]bool{}
	}
	for _, t := range tables {
		db.ignored[db.resolve(t)] = true
	}
}

var defaultSearchPath = []string{"public"}

// SeedRow inserts a committed row during Seed.
func (db *DB) SeedRow(table string, row Row) {
	table = db.resolve(table)
	row = row.clone()
	if err := db.assignKey(table, row); err != nil {
		panic(err)
	}
	t := db.committed[table]
	if t == nil {
		t = map[string]Row{}
		db.committed[table] = t
	}
	t[row.Key()] = row.clone()
	db.touched[table] = true
	if def := db.defs[table]; def != nil {
		for col := range def.autoInc {
			// An explicit value moves the counter past it, as an insert's
			// does, so the next generated id does not collide with it.
			if n, ok := autoIncValue(derefValue(row[col])); ok && n > db.seqs[autoIncKey(table, col)] {
				db.seqs[autoIncKey(table, col)] = n
			}
		}
	}
}

// encodeKey is the identity of a row with these primary key values. One value
// encodes as itself, so a table keyed by id is looked up by the id.
func encodeKey(vals []any) string {
	if len(vals) == 1 {
		return keyString(derefValue(vals[0]))
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = keyString(derefValue(v))
	}
	return strings.Join(parts, "\x1f")
}

// Tx runs fn in a transaction: commit on nil, rollback on error.
func (db *DB) Tx(p *Proc, fn func(tx *Tx) error) error {
	if p.tx != nil {
		panic("detest: nested transaction on " + p.name)
	}
	tx := db.newTx(p)
	p.tx = tx
	p.yieldf("%s: begin", db.name)
	err := fn(tx)
	if tx.closed {
		p.tx = nil
		return err
	}
	if err != nil || tx.aborted {
		p.yieldf("%s: rollback", db.name)
		tx.rollback()
		p.tx = nil
		if err == nil {
			err = tx.abortedError()
		}
		return err
	}
	p.yieldf("%s: commit", db.name)
	if err := tx.checkCommit(); err != nil {
		tx.rollback()
		p.tx = nil
		return err
	}
	tx.commit()
	p.tx = nil
	return nil
}

// Get reads one committed row outside a transaction (autocommit statement).
func (db *DB) Get(p *Proc, table, key string) (Row, bool) {
	table = db.resolve(table)
	p.yieldf("%s: select %s id=%s", db.name, table, key)
	r, ok := db.committed[table][key]
	if !ok {
		return nil, false
	}
	return r.clone(), true
}

// sequenceName normalizes how a sequence is written in nextval and setval
// ('"public"."orders_id_seq"', 'public.orders_id_seq', 'orders_id_seq') to
// one name.
func sequenceName(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(p, `"`)
	}
	if len(parts) == 2 && parts[0] == "public" {
		parts = parts[1:]
	}
	return strings.Join(parts, ".")
}

// Select reads committed rows outside a transaction (autocommit statement).
func (db *DB) Select(p *Proc, table string, pred func(Row) bool) []Row {
	table = db.resolve(table)
	p.yieldf("%s: select %s where ...", db.name, table)
	return db.selectCommitted(table, pred)
}

// Peek returns the committed rows of a table without yielding. It is for fakes
// that need to observe another database's state (for example a fake runtime that
// completes an execution only after its tracking row exists), never for model
// code, which must read through transactions.
func (db *DB) Peek(table string) []Row { return db.selectCommitted(table, nil) }

// Select returns rows matching pred (all rows when pred is nil), sorted by key.
func (tx *Tx) Select(table string, pred func(Row) bool) []Row {
	table = tx.db.resolve(table)
	if tx.check() != nil {
		return nil
	}
	tx.yieldf("%s: select %s where ...", tx.db.name, table)
	seen := map[string]bool{}
	var keys []string
	for k := range tx.db.committed[table] {
		keys = append(keys, k)
		seen[k] = true
	}
	for lk := range tx.writes {
		if lk.table == table && !seen[lk.key] {
			keys = append(keys, lk.key)
		}
	}
	sort.Strings(keys)
	var out []Row
	for _, k := range keys {
		r, ok := tx.view(table, k)
		if ok && (pred == nil || pred(r)) {
			out = append(out, r)
		}
	}
	return out
}

// Open returns another database/sql handle on the database, a pool of its own,
// for a second service that shares the database.
func (db *DB) Open() *sql.DB {
	sqlDB := sql.OpenDB(&sqlConnector{db: db})
	db.s.sqlDBs = append(db.s.sqlDBs, sqlDB)
	return sqlDB
}

func (def *tableDef) addConstraint(kind *sqlir.Impl, table string, u sqlir.UniqueDef) error {
	if !u.Primary {
		if u.Name == "" {
			u.Name = defaultConstraintName(table, u)
		}
		def.uniques = append(def.uniques, u)
		return nil
	}
	if def.pk != nil {
		return kind.Error(sqlir.InvalidTableDefinition, fmt.Sprintf("multiple primary keys for table %q are not allowed", relname(table)), relname(table), "", "")
	}
	def.pkName = u.Name
	if def.pkName == "" {
		def.pkName = relname(table) + "_pkey"
	}
	for _, e := range u.Elems {
		c, ok := e.(*sqlir.ColumnRef)
		if !ok {
			return fmt.Errorf("detest: primary key of %q on an expression", table)
		}
		def.pk = append(def.pk, c.Column)
	}
	return nil
}

// dropConstraint removes a unique constraint or index, or the primary key.
// The rows keep the identity the primary key gave them.
// dropIndex drops the index named name: the primary key, a unique index or
// another index, never a foreign key or a check of the same name.
func (def *tableDef) dropIndex(name string) {
	if def.pkName == name {
		def.pk, def.pkName = nil, ""
	}
	def.uniques = slices.DeleteFunc(def.uniques, func(u sqlir.UniqueDef) bool { return u.Name == name })
	def.indexes = slices.DeleteFunc(def.indexes, func(ix sqlir.IndexDef) bool { return ix.Name == name })
}

// dropConstraint drops the constraint of the name. A plain index is no
// constraint, so one of the same name stays.
func (def *tableDef) dropConstraint(name string) bool {
	for i, c := range def.checks {
		if c.Name == name {
			def.checks = slices.Delete(def.checks, i, i+1)
			return true
		}
	}
	for i, fk := range def.fks {
		if fk.Name == name {
			def.fks = slices.Delete(def.fks, i, i+1)
			return true
		}
	}
	if def.pkName == name {
		def.pk, def.pkName = nil, ""
		return true
	}
	for i, u := range def.uniques {
		if u.Name == name {
			def.uniques = slices.Delete(def.uniques, i, i+1)
			return true
		}
	}
	return false
}

func (def *tableDef) renameConstraint(old, nw string) bool {
	for i := range def.indexes {
		if def.indexes[i].Name == old {
			def.indexes[i].Name = nw
			return true
		}
	}
	for i := range def.checks {
		if def.checks[i].Name == old {
			def.checks[i].Name = nw
			return true
		}
	}
	for i := range def.fks {
		if def.fks[i].Name == old {
			def.fks[i].Name = nw
			return true
		}
	}
	if def.pkName == old {
		def.pkName = nw
		return true
	}
	for i := range def.uniques {
		if def.uniques[i].Name == old {
			def.uniques[i].Name = nw
			return true
		}
	}
	return false
}

// setCaseInsensitive records whether the MySQL text column col, of the
// collation declared for it, or else the table's or the server's default,
// compares strings without regard to case, as every _ci collation, MySQL
// 8's default utf8mb4_0900_ai_ci among them, does.
func (def *tableDef) setCaseInsensitive(col, collation, server string) {
	if !mysqlTextType(def.types[col]) {
		delete(def.ci, col)
		return
	}
	collation = cmp.Or(collation, def.collation, server)
	if !strings.HasSuffix(collation, "_ci") {
		delete(def.ci, col)
		return
	}
	if def.ci == nil {
		def.ci = map[string]bool{}
	}
	def.ci[col] = true
}

// indexedBy reports whether an index leads with cols, as the index InnoDB
// uses for a foreign key on them must.
func (def *tableDef) indexedBy(cols []string) bool {
	leads := func(ix []string) bool { return len(ix) >= len(cols) && slices.Equal(ix[:len(cols)], cols) }
	if leads(def.pk) {
		return true
	}
	for _, u := range def.uniques {
		var ix []string
		for _, e := range u.Elems {
			c, ok := e.(*sqlir.ColumnRef)
			if !ok {
				break
			}
			ix = append(ix, c.Column)
		}
		if leads(ix) {
			return true
		}
	}
	return slices.ContainsFunc(def.indexes, func(ix sqlir.IndexDef) bool { return leads(ix.Columns) })
}

// renameIndex renames an index, leaving a foreign key or a check of the same
// name alone.
func (def *tableDef) renameIndex(old, nw string) {
	for i := range def.indexes {
		if def.indexes[i].Name == old {
			def.indexes[i].Name = nw
			return
		}
	}
	for i := range def.uniques {
		if def.uniques[i].Name == old {
			def.uniques[i].Name = nw
			return
		}
	}
}

// dropColumn forgets a column's default and, as Postgres does, the unique
// indexes over it.
func (def *tableDef) dropColumn(col string) {
	def.columns = slices.DeleteFunc(def.columns, func(c string) bool { return c == col })
	delete(def.defaults, col)
	delete(def.onUpdate, col)
	delete(def.strs, col)
	delete(def.nums, col)
	delete(def.ci, col)
	delete(def.fsp, col)
	delete(def.generated, col)
	delete(def.types, col)
	delete(def.notNull, col)
	delete(def.autoInc, col)
	def.indexes = slices.DeleteFunc(def.indexes, func(ix sqlir.IndexDef) bool { return slices.Contains(ix.Columns, col) || ix.Prefix == col })
	def.checks = slices.DeleteFunc(def.checks, func(c tableCheck) bool { return slices.Contains(c.columns(), col) })
	def.uniques = slices.DeleteFunc(def.uniques, func(u sqlir.UniqueDef) bool { return refersTo(u, col) })
	def.fks = slices.DeleteFunc(def.fks, func(fk sqlir.ForeignKey) bool { return slices.Contains(fk.Columns, col) })
}

func (def *tableDef) renameColumn(old, nw string) {
	for i, c := range def.columns {
		if c == old {
			def.columns[i] = nw
		}
	}
	if d, ok := def.defaults[old]; ok {
		delete(def.defaults, old)
		def.defaults[nw] = d
	}
	if e, ok := def.onUpdate[old]; ok {
		delete(def.onUpdate, old)
		def.onUpdate[nw] = e
	}
	if l, ok := def.strs[old]; ok {
		delete(def.strs, old)
		def.strs[nw] = l
	}
	if l, ok := def.nums[old]; ok {
		delete(def.nums, old)
		def.nums[nw] = l
	}
	if def.ci[old] {
		delete(def.ci, old)
		def.ci[nw] = true
	}
	if f, ok := def.fsp[old]; ok {
		delete(def.fsp, old)
		def.fsp[nw] = f
	}
	if g, ok := def.generated[old]; ok {
		delete(def.generated, old)
		def.generated[nw] = g
	}
	for _, g := range def.generated {
		g.renameColumn(old, nw)
	}
	if t, ok := def.types[old]; ok {
		delete(def.types, old)
		def.types[nw] = t
	}
	if def.notNull[old] {
		delete(def.notNull, old)
		def.notNull[nw] = true
	}
	if def.autoInc[old] {
		delete(def.autoInc, old)
		def.autoInc[nw] = true
	}
	for i := range def.indexes {
		for j, c := range def.indexes[i].Columns {
			if c == old {
				def.indexes[i].Columns[j] = nw
			}
		}
		if def.indexes[i].Prefix == old {
			def.indexes[i].Prefix = nw
		}
	}
	for i := range def.checks {
		def.checks[i].renameColumn(old, nw)
	}
	for i, c := range def.pk {
		if c == old {
			def.pk[i] = nw
		}
	}
	for i := range def.uniques {
		u := &def.uniques[i]
		// The expressions are the parsed statement's, which every database
		// running the same query shares, so they are copied before the
		// rename rather than rewritten where they are.
		elems := make([]sqlir.Expr, len(u.Elems))
		for j, e := range u.Elems {
			elems[j] = renameRefs(e, old, nw)
		}
		u.Elems = elems
		u.Where = renameRefs(u.Where, old, nw)
	}
	for i := range def.fks {
		for j, c := range def.fks[i].Columns {
			if c == old {
				def.fks[i].Columns[j] = nw
			}
		}
	}
}

// renameRefs returns e with its references to the column old renamed to
// nw, copying e first when it has one.
func renameRefs(e sqlir.Expr, old, nw string) sqlir.Expr {
	if e == nil || !slices.ContainsFunc(sqlir.ColumnRefs(e), func(r *sqlir.ColumnRef) bool { return r.Column == old }) {
		return e
	}
	e = sqlir.CloneExpr(e)
	for _, r := range sqlir.ColumnRefs(e) {
		if r.Column == old {
			r.Column = nw
		}
	}
	return e
}

func refersTo(u sqlir.UniqueDef, col string) bool {
	for _, e := range u.Elems {
		if c, ok := e.(*sqlir.ColumnRef); ok && c.Column == col {
			return true
		}
	}
	return false
}

// reads reports whether the table's own primary key, NOT NULL, a unique
// constraint or index, a foreign key, a CHECK, a generated column or the
// type check of a uuid or narrow integer column reads col, so that a value stood in for the
// column would decide one of them.
func (def *tableDef) reads(col string) bool {
	if slices.Contains(def.pk, col) || def.notNull[col] || slices.Contains([]string{"uuid", "int2", "int4"}, def.types[col]) {
		return true
	}
	for _, u := range def.uniques {
		exprs := u.Elems
		if u.Where != nil {
			exprs = append(slices.Clone(exprs), u.Where)
		}
		for _, e := range exprs {
			if slices.ContainsFunc(sqlir.ColumnRefs(e), func(r *sqlir.ColumnRef) bool { return r.Column == col }) {
				return true
			}
		}
	}
	for _, fk := range def.fks {
		if slices.Contains(fk.Columns, col) {
			return true
		}
	}
	for _, c := range def.checks {
		if slices.Contains(c.columns(), col) {
			return true
		}
	}
	for _, g := range def.generated {
		if slices.Contains(g.columns(), col) {
			return true
		}
	}
	return false
}

// SeedRowNow inserts a committed row from a fake during a run, without a
// transaction or a yield: the fake's own step is the yield point.
func (db *DB) SeedRowNow(table string, row Row) {
	if !db.kind.InnoDB() {
		db.SeedRow(table, row)
		return
	}
	// A change in the middle of a run is a commit, which a snapshot taken
	// before it does not see.
	resolved := db.resolve(table)
	before := maps.Clone(db.committed[resolved])
	db.SeedRow(table, row)
	db.seq++
	for k, r := range db.committed[resolved] {
		old, existed := before[k]
		if existed && sameRow(old, r) {
			continue
		}
		t := db.history[resolved]
		if t == nil {
			t = map[string][]version{}
			db.history[resolved] = t
		}
		if len(t[k]) == 0 {
			t[k] = append(t[k], version{seq: 0, row: old}) // nil when it is new
		}
		t[k] = append(t[k], version{seq: db.seq, row: r})
	}
}

// resolve returns the schema-qualified name of a table as written. An
// unqualified name is looked up along the search path, as Postgres does: the
// first schema with a declared or populated table of that name, else the
// first schema of the path. A qualified name resolves to itself.
func (db *DB) resolve(name string) string {
	if strings.Contains(name, ".") {
		return name
	}
	path := db.kind.SearchPath()
	if len(path) == 0 {
		path = defaultSearchPath
	}
	for _, schema := range path {
		q := schema + "." + name
		if db.defs[q] != nil || db.views[q] != nil {
			return q
		}
		if _, ok := db.committed[q]; ok {
			return q
		}
	}
	return path[0] + "." + name
}

// assignKey sets the row's identity: its primary key values when the table
// declares one, else the id column or, without one, all its columns.
func (db *DB) assignKey(table string, row Row) error {
	def := db.defs[table]
	if def == nil || len(def.pk) == 0 {
		row.ensureKey()
		return nil
	}
	vals := make([]any, len(def.pk))
	for i, c := range def.pk {
		v := derefValue(row[c])
		if v == nil {
			return db.kind.Error(sqlir.NotNullViolation, fmt.Sprintf("null value in column %q of relation %q violates not-null constraint", c, relname(table)), relname(table), c, "")
		}
		vals[i] = v
	}
	row["_key"] = encodeKey(vals)
	return nil
}

// DB declares a database on a server of kind s, such as postgres.New(), and
// returns a database/sql handle on it with the database itself. Production storage code (GORM, sqlx, sqlc,
// database/sql) runs unchanged on the handle: every statement is a yield
// point, and transactions map to detest transactions of the process issuing
// them. A hand-written model uses the database's Tx API instead and can ignore
// the handle. Explore closes the handle after the exploration; closing it in
// the declaration function would close it before any run.
func (s *Sim) DB(name string, srv Server) (*sql.DB, *DB) {
	s.declare("DB")
	kind := sqlir.ImplOf(srv)
	if kind == nil || kind.Parser() == nil {
		s.t.Fatal("detest: DB needs a server, such as postgres.New()")
	}
	if err := kind.Check(kind.Isolation()); err != nil {
		s.t.Fatal(err)
	}
	db := &DB{name: name, kind: kind, s: s}
	db.reset()
	s.dbs = append(s.dbs, db)
	return db.Open(), db
}

func (db *DB) nextval(seq string) int64 {
	seq = sequenceName(seq)
	db.seqs[seq]++
	return db.seqs[seq]
}

// setval sets a sequence so that nextval returns v+1, or v when not called.
func (db *DB) setval(seq string, v int64, called bool) {
	if !called {
		v--
	}
	db.seqs[sequenceName(seq)] = v
}

// newUUID returns the run's next generated UUID. It counts instead of drawing
// random bits, so a schedule replays with the same ids.
func (db *DB) newUUID() string {
	db.uuids++
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", db.uuids)
}

// applySchema records what a schema statement declares. DDL is transactional
// in Postgres and sees the transaction's own writes, so renames and drops
// carry tx's pending rows along.
func (db *DB) applySchema(st *sqlir.SchemaStmt, tx *Tx) error {
	if db.defs == nil {
		db.defs = map[string]*tableDef{}
		db.matviews = map[string]*sqlir.CreateTableAsStmt{}
		db.views = map[string]*sqlir.SchemaChange{}
	}
	for _, ch := range st.Changes {
		if err := db.applyChange(ch, tx); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) applyChange(ch sqlir.SchemaChange, tx *Tx) error {
	table := db.resolve(ch.Table)
	switch {
	case ch.Object == "index":
		return db.indexChange(table, ch)
	case ch.Object == "view" && !ch.Drop:
		if _, exists := db.views[table]; exists && !ch.Replace {
			return db.kind.Error(sqlir.DuplicateTable, fmt.Sprintf("relation %q already exists", relname(table)), relname(table), "", "")
		}
		v := ch
		db.views[table] = &v
		return nil
	case ch.Drop:
		_, isView := db.views[table]
		if db.defs[table] == nil && !isView && !ch.IfExists {
			return db.kind.Error(sqlir.UndefinedTable, fmt.Sprintf("relation %q does not exist", relname(table)), relname(table), "", "")
		}
		delete(db.defs, table)
		delete(db.committed, table)
		delete(db.history, table) // a table created again under the name starts with no versions
		db.moveAutoInc(table, "")
		delete(db.matviews, table)
		delete(db.views, table)
		tx.pending(table, func(lk lockKey, _ Row) { delete(tx.writes, lk) })
		return nil
	case ch.RenameTo != "":
		return db.renameTable(table, ch, tx)
	}
	def := db.defs[table]
	switch {
	case ch.Create:
		if def != nil {
			if ch.IfNotExists {
				return nil
			}
			return db.kind.Error(sqlir.DuplicateTable, fmt.Sprintf("relation %q already exists", relname(table)), relname(table), "", "")
		}
		def = &tableDef{defaults: map[string]sqlir.Expr{}}
		db.defs[table] = def
	case def == nil:
		if ch.IfExists {
			return nil
		}
		return db.kind.Error(sqlir.UndefinedTable, fmt.Sprintf("relation %q does not exist", relname(table)), relname(table), "", "")
	}
	var added []sqlir.ColumnDef
	redefined := false // a MySQL MODIFY or CHANGE of an existing column
	if ch.Collation != "" {
		def.collation = ch.Collation
	}
	for _, col := range ch.Columns {
		if col.After != "" && (col.After == col.Name || !slices.Contains(def.columns, col.After)) {
			// Checked before the column is added or moved, so a failed
			// ALTER leaves the column as it was.
			return fmt.Errorf("detest: column %q of relation %q does not exist", col.After, relname(table))
		}
		if !slices.Contains(def.columns, col.Name) {
			if !ch.Create && col.AutoIncrement && db.holdsRows(table, tx) {
				// MySQL numbers the rows already there, which would rekey
				// them; a schema the test sets up rarely needs it.
				return unsupported("adding an AUTO_INCREMENT column to a table that holds rows", "")
			}
			def.columns = append(def.columns, col.Name)
			switch {
			case !ch.Create && col.Default != nil:
				added = append(added, col)
			case !ch.Create && col.NotNull && db.kind.InnoDB():
				redefined = true // the rows are checked for the NULL they hold in it
			}
		}
		if col.First || col.After != "" {
			def.columns = slices.DeleteFunc(def.columns, func(c string) bool { return c == col.Name })
			at := 0
			if col.After != "" {
				at = slices.Index(def.columns, col.After) + 1
			}
			def.columns = slices.Insert(def.columns, at, col.Name)
		}
		if col.Type != "" {
			if def.types == nil {
				def.types = map[string]string{}
			}
			if _, had := def.types[col.Name]; had && db.kind.InnoDB() {
				redefined = true
			}
			def.types[col.Name] = col.Type
			if db.kind.InnoDB() {
				def.setCaseInsensitive(col.Name, col.Collation, db.kind.Collation())
			}
		}
		switch {
		case col.NotNull:
			if def.notNull == nil {
				def.notNull = map[string]bool{}
			}
			def.notNull[col.Name] = true
		case col.DropNotNull:
			delete(def.notNull, col.Name)
		}
		switch {
		case col.AutoIncrement:
			for other := range def.autoInc {
				if other != col.Name {
					// One column per table, which the counter and replays
					// depend on; MySQL refuses a second.
					return unsupported("more than one AUTO_INCREMENT column", "")
				}
			}
			if def.autoInc == nil {
				def.autoInc = map[string]bool{}
			}
			if !def.autoInc[col.Name] {
				db.raiseAutoInc(table, col.Name, tx)
			}
			def.autoInc[col.Name] = true
		case col.DropAutoIncrement:
			delete(def.autoInc, col.Name)
		}
		if col.Type != "" {
			if col.MaxLen > 0 || col.Members != nil {
				if def.strs == nil {
					def.strs = map[string]strLimit{}
				}
				def.strs[col.Name] = strLimit{maxLen: col.MaxLen, members: col.Members, set: col.Set}
			} else {
				delete(def.strs, col.Name)
			}
			if col.Precision > 0 {
				if def.nums == nil {
					def.nums = map[string]numLimit{}
				}
				def.nums[col.Name] = numLimit{precision: col.Precision, scale: col.Scale}
			} else {
				delete(def.nums, col.Name)
			}
			if def.fsp == nil {
				def.fsp = map[string]int{}
			}
			def.fsp[col.Name] = col.FSP
			// ALTER COLUMN TYPE rewrites the rows under the new type: an
			// integer becomes a numeric's float, a numeric(p, s) rounds
			// what they hold, and a varchar(n) refuses what is too long.
			if col.TypeOnly {
				redefined = true
			}
		}
		if col.TypeOnly {
			continue
		}
		if col.Type != "" {
			// A whole column definition, which says ON UPDATE again or drops
			// it, as MySQL's MODIFY and CHANGE do; ALTER COLUMN leaves it.
			if col.OnUpdate != nil {
				if def.onUpdate == nil {
					def.onUpdate = map[string]sqlir.Expr{}
				}
				def.onUpdate[col.Name] = col.OnUpdate
			} else {
				delete(def.onUpdate, col.Name)
			}
		}
		if col.Generated != nil {
			if def.generated == nil {
				def.generated = map[string]*tableCheck{}
			}
			def.generated[col.Name] = &tableCheck{CheckDef: sqlir.CheckDef{Name: col.Name, Expr: col.Generated}}
		}
		if col.Default != nil {
			def.defaults[col.Name] = col.Default
		} else {
			delete(def.defaults, col.Name)
		}
	}
	if ch.AutoIncrement > 0 {
		// The counter holds the last value generated. MySQL sets it to the
		// value asked for, or past the largest value the column holds, so
		// a table emptied may start over.
		for col := range def.autoInc {
			k := autoIncKey(table, col)
			db.seqs[k] = 0
			db.raiseAutoInc(table, col, tx)
			db.seqs[k] = max(db.seqs[k], ch.AutoIncrement-1)
		}
		def.autoIncFloor = ch.AutoIncrement - 1
	}
	for _, u := range ch.Constraints {
		if err := def.addConstraint(db.kind, table, u); err != nil {
			return err
		}
	}
	def.indexes = append(def.indexes, ch.Indexes...)
	for _, c := range ch.Checks {
		if c.Name == "" {
			// Postgres names it after the first column it refers to.
			c.Name = relname(table) + "_check"
			if refs := sqlir.ColumnRefs(c.Expr); len(refs) > 0 {
				c.Name = relname(table) + "_" + refs[0].Column + "_check"
			}
		}
		def.checks = append(def.checks, tableCheck{CheckDef: c})
	}
	for col := range def.autoInc {
		// MySQL refuses an AUTO_INCREMENT column that leads no index, so no
		// schema built on it has one.
		if !def.indexedBy([]string{col}) {
			return unsupported("an AUTO_INCREMENT column that leads no index", "")
		}
	}
	if err := db.backfill(table, added, redefined, tx); err != nil {
		return err
	}
	if ch.ConvertCollation && db.kind.InnoDB() {
		for col := range def.types {
			def.setCaseInsensitive(col, "", db.kind.Collation()) // CONVERT TO gives every text column the table's
		}
	}
	for _, fk := range ch.ForeignKeys {
		if fk.Name == "" {
			fk.Name = relname(table) + "_" + strings.Join(fk.Columns, "_") + "_fkey"
		}
		fk.RefTable = db.resolve(fk.RefTable)
		def.fks = append(def.fks, fk)
		if db.kind.InnoDB() && !def.indexedBy(fk.Columns) {
			// InnoDB creates an index for a foreign key that has none, which
			// the next-key locks search by.
			def.indexes = append(def.indexes, sqlir.IndexDef{Name: fk.Name, Columns: fk.Columns})
		}
	}
	for _, name := range ch.DropConstraints {
		def.dropConstraint(name)
	}
	for _, name := range ch.DropIndexes {
		def.dropIndex(name)
	}
	for _, name := range ch.DropForeignKeys {
		def.fks = slices.DeleteFunc(def.fks, func(fk sqlir.ForeignKey) bool { return fk.Name == name })
	}
	for _, name := range ch.DropChecks {
		def.checks = slices.DeleteFunc(def.checks, func(c tableCheck) bool { return c.Name == name })
	}
	for _, col := range ch.DropColumns {
		def.dropColumn(col)
		db.dropColumnInRows(table, col, tx)
	}
	if old, nw := ch.RenameColumn[0], ch.RenameColumn[1]; old != "" {
		def.renameColumn(old, nw)
		if v, ok := db.seqs[autoIncKey(table, old)]; ok {
			delete(db.seqs, autoIncKey(table, old))
			db.seqs[autoIncKey(table, nw)] = v
		}
		db.renameColumnInRows(table, old, nw)
		for _, other := range db.defs { // foreign keys elsewhere that reference the column
			for i := range other.fks {
				if other.fks[i].RefTable == table {
					for j, c := range other.fks[i].RefColumns {
						if c == old {
							other.fks[i].RefColumns[j] = nw
						}
					}
				}
			}
		}
		tx.pending(table, func(lk lockKey, r Row) {
			if v, ok := r[old]; ok {
				delete(r, old)
				r[nw] = v
			}
		})
	}
	if old, nw := ch.RenameConstraint[0], ch.RenameConstraint[1]; old != "" {
		def.renameConstraint(old, nw)
	}
	if old, nw := ch.RenameIndex[0], ch.RenameIndex[1]; old != "" {
		def.renameIndex(old, nw)
	}
	return nil
}

// raiseAutoInc moves the AUTO_INCREMENT counter of a column that becomes
// AUTO_INCREMENT past the values it already holds, as MySQL starts it.
func (db *DB) raiseAutoInc(table, col string, tx *Tx) {
	k := autoIncKey(table, col)
	raise := func(r Row) {
		if n, ok := integer(derefValue(r[col])); ok && n > db.seqs[k] {
			db.seqs[k] = n
		}
	}
	for _, r := range db.committed[table] {
		raise(r)
	}
	if tx != nil {
		for lk, r := range tx.writes {
			if lk.table == table {
				raise(r)
			}
		}
	}
}

// backfill gives the rows a table already holds the defaults of the columns
// ADD COLUMN added, as an existing row reads a new column's default, and
// converts its values to the column types, as MySQL converts the rows when
// MODIFY or CHANGE redefines a column (redefined).
func (db *DB) backfill(table string, cols []sqlir.ColumnDef, redefined bool, tx *Tx) error {
	if len(cols) == 0 && !redefined {
		return nil
	}
	if tx == nil {
		tx = db.newTx(nil)
	}
	x := tx.evaluator()
	def := db.defs[table]
	// A row gets one value of each default, which its versions and pending
	// write share, so a volatile default such as UUID() reads the same from
	// a snapshot as from the latest row.
	filled := map[string]map[string]any{}
	fill := func(r Row) (Row, error) {
		n := r.clone()
		vals := filled[r.Key()]
		if vals == nil {
			vals = map[string]any{}
			filled[r.Key()] = vals
		}
		for _, c := range cols {
			if _, ok := n[c.Name]; ok {
				continue
			}
			v, seen := vals[c.Name]
			if !seen {
				var err error
				if v, err = x.eval(c.Default, &env{}); err != nil {
					return nil, err
				}
				vals[c.Name] = v
			}
			n[c.Name] = v
		}
		// The default is stored as an insert stores it, converted to the
		// column's type and checked against it.
		if err := x.checkTypes(table, n); err != nil {
			return nil, err
		}
		if redefined {
			// MySQL refuses an ALTER that leaves NULL in a NOT NULL column,
			// or fills in a value of its own, rules detest does not follow.
			for c := range def.notNull {
				if derefValue(n[c]) == nil {
					return nil, unsupported("an ALTER that leaves NULL in the NOT NULL column "+c, "")
				}
			}
			// A converted key would file the row under another identity,
			// which its versions and locks are not moved to.
			k := n.clone()
			delete(k, "_key")
			if err := db.assignKey(table, k); err != nil {
				return nil, err
			}
			if k.Key() != r.Key() {
				return nil, unsupported("MODIFY or CHANGE of a column that converts a row's primary key", "")
			}
		}
		return n, nil
	}
	// In key order, so that a volatile default numbers the rows the same in
	// every run.
	for _, k := range slices.Sorted(maps.Keys(db.committed[table])) {
		r := db.committed[table][k]
		n, err := fill(r)
		if err != nil {
			return err
		}
		db.committed[table][k] = n
		db.touched[table] = true
	}
	// The versions consistent reads see are rows of the table too.
	for _, vs := range db.history[table] {
		for i, v := range vs {
			if v.row == nil {
				continue
			}
			n, err := fill(v.row)
			if err != nil {
				return err
			}
			vs[i].row = n
		}
	}
	for lk, r := range tx.writes {
		if lk.table == table {
			n, err := fill(r)
			if err != nil {
				return err
			}
			tx.writes[lk] = n
		}
	}
	return nil
}

// dropColumnInRows removes a dropped column's values from the rows, the
// versions snapshots read and the transaction's writes, so a column added
// later under the name starts from its default.
func (db *DB) dropColumnInRows(table, col string, tx *Tx) {
	drop := func(r Row) Row {
		if _, ok := r[col]; !ok {
			return r
		}
		n := r.clone()
		delete(n, col)
		return n
	}
	for k, r := range db.committed[table] {
		db.committed[table][k] = drop(r)
		db.touched[table] = true
	}
	for _, vs := range db.history[table] {
		for i := range vs {
			if vs[i].row != nil {
				vs[i].row = drop(vs[i].row)
			}
		}
	}
	if tx != nil {
		for lk, r := range tx.writes {
			if lk.table == table {
				tx.writes[lk] = drop(r)
			}
		}
	}
}

// holdsRows reports whether a table holds rows, committed or written by tx.
func (db *DB) holdsRows(table string, tx *Tx) bool {
	if len(db.committed[table]) > 0 {
		return true
	}
	if tx != nil {
		for lk := range tx.writes {
			if lk.table == table {
				return true
			}
		}
	}
	return false
}

// renameColumnInRows renames the column in the committed rows, which a commit
// replaces rather than changes, so each renamed row is a new one.
func (db *DB) renameColumnInRows(table, old, nw string) {
	for k, r := range db.committed[table] {
		if v, ok := r[old]; ok {
			n := r.clone()
			delete(n, old)
			n[nw] = v
			db.committed[table][k] = n
			db.touched[table] = true
		}
	}
	// The versions consistent reads see are rows of the table too.
	for _, vs := range db.history[table] {
		for i, v := range vs {
			if val, ok := v.row[old]; ok {
				n := v.row.clone()
				delete(n, old)
				n[nw] = val
				vs[i].row = n
			}
		}
	}
}

// indexChange drops or renames an index, which lives in the schema of the
// name it is given, on whichever table it indexes.
func (db *DB) indexChange(name string, ch sqlir.SchemaChange) error {
	schema := name[:strings.LastIndex(name, ".")]
	idx := relname(name)
	for t, def := range db.defs {
		if !strings.HasPrefix(t, schema+".") {
			continue
		}
		if ch.Drop {
			if i := slices.IndexFunc(def.indexes, func(ix sqlir.IndexDef) bool { return ix.Name == idx }); i >= 0 {
				def.indexes = slices.Delete(def.indexes, i, i+1)
				return nil
			}
			if def.dropConstraint(idx) {
				return nil
			}
		}
		if !ch.Drop && def.renameConstraint(ch.RenameConstraint[0], ch.RenameConstraint[1]) {
			return nil
		}
	}
	return nil // a plain index, which detest does not keep
}

// Tx is an open transaction. Reads see committed data plus the transaction's
// own writes (Read Committed, statement-level). Writes take a row lock that is
// held until commit or rollback; a waiting writer re-reads the row after the
// lock is granted, as Postgres does.
type Tx struct {
	db *DB
	p  *Proc
	// passedOver records that NOWAIT or SKIP LOCKED gave up on a row this
	// transaction holds. Letting a lock go or weakening it then changes what
	// such a read finds, so an idle loop may tick again even if no row
	// changed, whether the transaction ends or a savepoint or a statement
	// rolls back.
	passedOver bool
	writes     map[lockKey]Row
	deleted    map[lockKey]bool
	moved      map[lockKey]string // the keys this transaction changed, as DB.moved
	// start is when the transaction began, which now() and
	// CURRENT_TIMESTAMP return throughout it.
	start   time.Time
	locks   []lockKey
	aborted bool
	// deadlockVictim is set by the transaction that closed a cycle of lock
	// waits when the explorer picked this waiting one to break it.
	deadlockVictim bool
	closed         bool
	deferred       []func()
	atomic         bool
	block          bool // begun with BeginTx, so SAVEPOINT may be used
	checking       bool // CheckSQL's, which is a block for SAVEPOINT but stands for autocommit too
	// undo counts the undo records of the transaction's row changes, and
	// lockStructs the lock structs its statements and waits took, which
	// InnoDB weighs a deadlock victim by.
	undo        int
	lockStructs map[lockStruct]bool
	// waits counts the lock waits, each a lock struct of its own.
	waits int
	// explicit is the unique index entries the transaction locked
	// explicitly, by a search or a check, as opposed to the ones it wrote.
	explicit map[lockKey]bool
	// implicit is the records the transaction holds implicitly, as their
	// writer: rows it put into a primary key and index entries it wrote or
	// removed, which turn explicit when another transaction waits for one.
	implicit map[lockKey]bool
	// grants is the lock structs each of the transaction's locks was
	// granted in, which a request a granted lock covers does not add to.
	grants map[lockKey][]lockStruct
	// put is the rows the running statement put into a primary key, which
	// leave a gap lock if the statement fails.
	put []putRow
	// putting is set while the last of put is being inserted, before its
	// checks have passed.
	putting bool
	// inserts is every row the transaction put into a primary key, which a
	// rollback to a savepoint takes out again, also one deleted or moved
	// since, leaving gap locks where it was.
	inserts []putRow
	// entries is the secondary index entries the transaction's updates
	// wrote, whose rollback leaves a gap lock where each was.
	entries []entryWrite
	// started is set once the transaction has run a statement in InnoDB,
	// other than a savepoint's.
	started bool
	// updating is the rows the transaction is updating, by their key, with
	// their new values, while the checks of their secondary indexes run.
	updating map[lockKey]Row
	// pendingLockTimeout is a Postgres session SET lock_timeout run in the
	// transaction, which the session keeps only once the transaction
	// commits, and which ROLLBACK TO a savepoint before it undoes.
	pendingLockTimeout *bool
	// lockTimeout is set by SET LOCAL lock_timeout: a lock wait may then fail
	// with 55P03 instead of waiting on, which the explorer chooses.
	lockTimeout bool
	// noAutoZero is MySQL's NO_AUTO_VALUE_ON_ZERO, set for the session as a
	// dump sets it: an explicit 0 in an AUTO_INCREMENT column is kept.
	noAutoZero bool
	// noFKChecks is MySQL's FOREIGN_KEY_CHECKS=0, set for the session as a
	// dump sets it: foreign keys are neither checked nor acted on.
	noFKChecks bool
	saves      []savepoint
	// deferAll and deferNamed are what SET CONSTRAINTS set, for ALL and by
	// constraint name; nil when it was not run.
	deferAll   *bool
	deferNamed map[string]bool
	// iso is the level the transaction runs at. snap is the commit sequence
	// number its InnoDB consistent reads see, -1 until its first one.
	iso  IsolationLevel
	snap int64
	// lastInsertID is where an InnoDB insert leaves the AUTO_INCREMENT value
	// it generated first, for LastInsertId and LAST_INSERT_ID(); nil when the
	// statement does not come through a connection.
	lastInsertID *int64
}

// newTx begins a transaction of p at the server's default level.
func (db *DB) newTx(p *Proc) *Tx {
	return &Tx{db: db, p: p, writes: map[lockKey]Row{}, deleted: map[lockKey]bool{}, iso: db.kind.Isolation(), snap: -1, start: time.Now()}
}

// savepoint is what ROLLBACK TO restores: the transaction's writes and the
// locks it held when the savepoint was set.
type savepoint struct {
	name    string
	pending *bool // the transaction's pendingLockTimeout
	undo    int   // and the undo records it had written
	timeout bool  // and its lockTimeout
	writes  map[lockKey]Row
	deleted map[lockKey]bool
	moved   map[lockKey]string
	locks   int
	// modes is the strength each lock held then had, since a lock taken
	// before the savepoint may be strengthened after it.
	modes    map[lockKey]lockMode
	deferred int
	// unstarted is a savepoint taken before the transaction ran anything
	// in InnoDB.
	unstarted bool
	inserts   int // the length of the transaction's inserts
	entries   int // and of its entries
}

// Get reads one row.
func (tx *Tx) Get(table, key string) (Row, bool) {
	table = tx.db.resolve(table)
	if tx.check() != nil {
		return nil, false
	}
	tx.yieldf("%s: select %s id=%s", tx.db.name, table, key)
	return tx.view(table, key)
}

// GetForUpdate reads one row and takes its lock (SELECT ... FOR UPDATE).
func (tx *Tx) GetForUpdate(table, key string) (Row, bool, error) {
	table = tx.db.resolve(table)
	if err := tx.check(); err != nil {
		return nil, false, err
	}
	tx.yieldf("%s: select %s id=%s for update", tx.db.name, table, key)
	if err := tx.lock(lockKey{table, key}); err != nil {
		return nil, false, err
	}
	r, ok := tx.view(table, key)
	return r, ok, nil
}

// Insert adds a row. Returns ErrUniqueViolation when the key exists.
func (tx *Tx) Insert(table string, row Row) error {
	return tx.asStatement(func() error { return tx.insert(table, row) })
}

// Update sets columns of one row. Returns false when the row does not exist.
func (tx *Tx) Update(table, key string, fields Row) (bool, error) {
	n, err := tx.UpdateWhere(table, func(r Row) bool { return r.Key() == key }, fields, fmt.Sprintf("id=%s", key))
	return n == 1, err
}

// CAS updates one row only if column field equals from (UPDATE ... WHERE id=?
// AND field=?). Returns whether a row was updated.
func (tx *Tx) CAS(table, key, field string, from, to any) (bool, error) {
	n, err := tx.updateWhere(table, func(r Row) bool { return r.Key() == key && r[field] == from }, Row{field: to},
		lazyString(func() string { return fmt.Sprintf("id=%s and %s=%v -> %v", key, field, from, to) }))
	return n == 1, err
}

// renameTable moves a table, view or materialized view to a new name in its
// schema.
func (db *DB) renameTable(table string, ch sqlir.SchemaChange, tx *Tx) error {
	to := table[:strings.LastIndex(table, ".")+1] + ch.RenameTo
	if i := strings.LastIndex(ch.RenameTo, "."); i >= 0 {
		if !strings.EqualFold(ch.RenameTo[:i], table[:strings.LastIndex(table, ".")]) {
			// Moving a table between databases is left out: applications
			// do not do it at run time, and refusing it keeps detest from
			// renaming the table within its own database instead.
			return unsupported("renaming a table into another database", "")
		}
		to = table[:strings.LastIndex(table, ".")+1] + ch.RenameTo[i+1:]
	}
	moved := false
	tx.pending(table, func(lk lockKey, r Row) {
		delete(tx.writes, lk)
		tx.writes[lockKey{to, lk.key}] = r
		if holders, ok := tx.db.locks[lk]; ok {
			delete(tx.db.locks, lk)
			tx.db.locks[lockKey{to, lk.key}] = holders
		}
	})
	if def, ok := db.defs[table]; ok {
		delete(db.defs, table)
		db.defs[to] = def
		moved = true
		for _, other := range db.defs { // foreign keys that reference the table
			for i := range other.fks {
				if other.fks[i].RefTable == table {
					other.fks[i].RefTable = to
				}
			}
		}
	}
	if versions, ok := db.history[table]; ok {
		delete(db.history, table)
		db.history[to] = versions
	}
	db.moveAutoInc(table, to)
	if rows, ok := db.committed[table]; ok {
		delete(db.committed, table)
		db.committed[to] = rows
		db.touched[table], db.touched[to] = true, true
	}
	if mv, ok := db.matviews[table]; ok {
		delete(db.matviews, table)
		db.matviews[to] = mv
		moved = true
	}
	if v, ok := db.views[table]; ok {
		delete(db.views, table)
		db.views[to] = v
		moved = true
	}
	if !moved && !ch.IfExists {
		return db.kind.Error(sqlir.UndefinedTable, fmt.Sprintf("relation %q does not exist", relname(table)), relname(table), "", "")
	}
	return nil
}

func (db *DB) reset() {
	db.committed = map[string]map[string]Row{}
	db.locks = map[lockKey]rowLock{}
	db.touched = map[string]bool{}
	db.moved = map[lockKey]string{}
	db.seqs = map[string]int64{}
	for table, def := range db.defs {
		for col := range def.autoInc {
			if def.autoIncFloor > 0 {
				db.seqs[autoIncKey(table, col)] = def.autoIncFloor
			}
		}
	}
	db.uuids = 0
	db.seq, db.history, db.gaps = 0, map[string]map[string][]version{}, nil
}

func (db *DB) selectCommitted(table string, pred func(Row) bool) []Row {
	table = db.resolve(table)
	t := db.committed[table]
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Row
	for _, k := range keys {
		if pred == nil || pred(t[k]) {
			out = append(out, t[k].clone())
		}
	}
	return out
}

// duplicateKey is the error of a write that collides with a row holding the
// same value in the unique index named constraint.
func (db *DB) duplicateKey(table, constraint string) error {
	return db.kind.Error(sqlir.UniqueViolation, fmt.Sprintf("duplicate key value violates unique constraint %q", constraint), relname(table), "", constraint)
}

// UpdateWhere updates every row matching pred and returns the count. Each
// matching row is locked, then pred is re-evaluated on the version visible after
// the lock is granted.
func (tx *Tx) UpdateWhere(table string, pred func(Row) bool, fields Row, desc string) (int, error) {
	return tx.updateWhere(table, pred, fields, desc)
}

// Delete removes one row. Returns whether it existed.
func (tx *Tx) Delete(table, key string) (bool, error) {
	var found bool
	err := tx.asStatement(func() error {
		var err error
		found, err = tx.delete(table, key)
		return err
	})
	return found, err
}

// Enqueue publishes a message when the transaction commits (outbox pattern).
func (tx *Tx) Enqueue(q *Queue, msg Msg) {
	tx.deferred = append(tx.deferred, func() { q.push(tx.p, msg) })
}

// asStatement runs a write of the transaction API as InnoDB runs a
// statement: one that fails rolls back alone, with the bookkeeping a failed
// SQL statement gets, and a deadlock rolls back the transaction.
func (tx *Tx) asStatement(f func() error) error {
	if !tx.db.kind.InnoDB() {
		return f()
	}
	m := tx.markStatement()
	err := f()
	if err != nil {
		tx.failStatement(m, errors.Is(err, sqlir.ErrDeadlock))
	}
	return err
}

func (tx *Tx) insert(table string, row Row) error {
	table = tx.db.resolve(table)
	if err := tx.check(); err != nil {
		return err
	}
	if err := tx.givesGenerated(table, row, "Tx.Insert"); err != nil {
		return err
	}
	row = row.clone()
	x := tx.evaluator()
	if err := x.applyDefaults(table, row); err != nil {
		return err
	}
	if err := x.checkRow(table, row); err != nil {
		return err
	}
	if err := tx.db.assignKey(table, row); err != nil {
		return err
	}
	lk := lockKey{table, row.Key()}
	tx.yieldf("%s: insert %s %s", tx.db.name, table, row)
	tx.noteTableLock(table, lockUpdate)
	if err := tx.lockImplicit(lk, lockStruct{}); err != nil {
		return err
	}
	if _, exists := tx.view(table, row.Key()); exists {
		// The duplicate check holds the row there in share mode, as
		// InnoDB's does.
		tx.shareDuplicate(lk, structKey(table, "PRIMARY", lockShare, "record"))
		return tx.db.duplicateKey(table, tx.db.pkConstraint(table))
	}
	tx.undo++ // written as the row goes into the primary key, before the checks
	if tx.db.kind.InnoDB() {
		// The row is in the primary key while its checks run, where
		// another transaction's locking search meets it.
		tx.put, tx.putting = []putRow{{table: table, row: row}}, true
		defer func() { tx.put, tx.putting = nil, false }()
	}
	if err := x.insertEntries(table, row); err != nil {
		return err
	}
	delete(tx.deleted, lk)
	tx.writes[lk] = row.clone()
	if tx.db.kind.InnoDB() {
		tx.inserts = append(tx.inserts, putRow{table: table, row: row.clone()})
	}
	return nil
}

func (tx *Tx) delete(table, key string) (bool, error) {
	table = tx.db.resolve(table)
	if err := tx.check(); err != nil {
		return false, err
	}
	lk := lockKey{table, key}
	tx.yieldf("%s: delete %s id=%s", tx.db.name, table, key)
	if err := tx.lock(lk); err != nil {
		return false, err
	}
	cur, ok := tx.view(table, key)
	if !ok {
		return false, nil
	}
	tx.undo++ // written before the secondary entries, which may wait
	if err := tx.evaluator().releaseEntries(table, cur, nil); err != nil {
		return false, err
	}
	delete(tx.writes, lk)
	tx.deleted[lk] = true
	if err := tx.evaluator().onParentDelete(table, cur); err != nil {
		return false, err
	}
	return true, nil
}

// pending applies f to the rows tx has written to table and not committed.
func (tx *Tx) pending(table string, f func(lk lockKey, r Row)) {
	if tx == nil {
		return
	}
	for lk, r := range tx.writes {
		if lk.table == table {
			f(lk, r)
		}
	}
}

// savepoint runs SAVEPOINT, RELEASE SAVEPOINT and ROLLBACK TO SAVEPOINT.
// ROLLBACK TO undoes the writes since the savepoint, releases the locks taken
// since, and lets an aborted transaction continue, as PostgreSQL does.
func (tx *Tx) savepoint(op, name string) error {
	if !tx.block {
		return tx.db.kind.Error(sqlir.NoActiveTransaction, fmt.Sprintf("%s can only be used in transaction blocks", strings.ToUpper(strings.ReplaceAll(op, "_", " "))), "", "", "")
	}
	if op == "savepoint" {
		if err := tx.check(); err != nil {
			return err
		}
		sp := savepoint{name: name, writes: map[lockKey]Row{}, deleted: map[lockKey]bool{}, locks: len(tx.locks), modes: map[lockKey]lockMode{}, deferred: len(tx.deferred)}
		for _, lk := range tx.locks {
			sp.modes[lk] = tx.db.locks[lk][tx]
		}
		for k, v := range tx.writes {
			sp.writes[k] = v.clone()
		}
		maps.Copy(sp.deleted, tx.deleted)
		sp.moved = maps.Clone(tx.moved)
		sp.pending, sp.timeout, sp.undo, sp.inserts, sp.entries = tx.pendingLockTimeout, tx.lockTimeout, tx.undo, len(tx.inserts), len(tx.entries)
		sp.unstarted = !tx.started
		tx.saves = append(tx.saves, sp)
		return nil
	}
	i := len(tx.saves) - 1
	for ; i >= 0 && tx.saves[i].name != name; i-- {
	}
	if i < 0 {
		return tx.db.kind.Error(sqlir.InvalidSavepoint, fmt.Sprintf("savepoint %q does not exist", name), "", "", "")
	}
	if op == "release" {
		if err := tx.check(); err != nil {
			return err
		}
		tx.saves = tx.saves[:i]
		return nil
	}
	sp := &tx.saves[i]
	tx.saves = tx.saves[:i+1] // ROLLBACK TO keeps the savepoint itself
	var inserted []putRow
	if tx.db.kind.InnoDB() && sp.inserts <= len(tx.inserts) {
		for _, p := range tx.inserts[sp.inserts:] {
			if _, committed := tx.db.committed[p.table][p.row.Key()]; !committed {
				inserted = append(inserted, putRow{p.table, p.row, -1})
			}
		}
		tx.inserts = tx.inserts[:sp.inserts]
	}
	tx.writes, tx.deleted = map[lockKey]Row{}, map[lockKey]bool{}
	for k, v := range sp.writes {
		tx.writes[k] = v.clone()
	}
	maps.Copy(tx.deleted, sp.deleted)
	tx.moved = maps.Clone(sp.moved)
	tx.pendingLockTimeout, tx.undo = sp.pending, sp.undo
	if !tx.db.kind.InnoDB() {
		tx.lockTimeout = sp.timeout // MySQL's setting is the session's, which no rollback undoes
	}
	tx.deferred = tx.deferred[:sp.deferred]
	switch {
	case tx.db.kind.InnoDB() && sp.unstarted:
		// InnoDB had not started the transaction when the savepoint was
		// taken, so rolling back to it rolls back all InnoDB holds of it,
		// every lock and gap included, as measured on MySQL 8.4.
		tx.releaseLocks(tx.locks)
		tx.locks = nil
		tx.releaseGaps()
		tx.lockStructs, tx.grants, tx.explicit, tx.implicit = nil, nil, nil, nil
		tx.entries = nil
		tx.started = false
	case tx.db.kind.InnoDB():
		tx.releaseInsertLocks(sp.locks)
		for _, p := range inserted {
			tx.inheritGap(p.table, p.row, -1)
		}
		tx.rollEntries(sp.entries)
	default:
		tx.rollbackLocks(sp)
	}
	tx.aborted = false
	return nil
}

// abort puts the transaction in the failed state after a statement error.
// Postgres releases the locks of the aborted subtransaction at once, not at
// ROLLBACK: those taken since the innermost savepoint, or all of them.
func (tx *Tx) abort() {
	tx.aborted = true
	if tx.p != nil && tx.p.r.over() {
		return // as release, while the processes of an ended run unwind
	}
	if n := len(tx.saves); n > 0 {
		tx.rollbackLocks(&tx.saves[n-1])
		return
	}
	tx.releaseLocks(tx.locks)
	tx.locks = nil
}

// releaseInsertLocks undoes the locks taken since the first from locks as
// InnoDB undoes them with the writes they came with: it keeps them, except
// those of rows inserted since, which go with the rows.
func (tx *Tx) releaseInsertLocks(from int) {
	since := tx.locks[from:]
	tx.locks = tx.locks[:from:from]
	var gone []lockKey
	for _, lk := range since {
		_, written := tx.writes[lk]
		_, committed := tx.db.committed[lk.table][lk.key]
		if lk.key == gapWaitKey || written || committed || tx.explicit[lk] {
			tx.locks = append(tx.locks, lk)
		} else {
			gone = append(gone, lk)
		}
	}
	tx.releaseLocks(gone)
}

func (tx *Tx) yieldf(format string, args ...any) {
	if tx.atomic || tx.p == nil {
		return
	}
	tx.p.yieldf(format, args...)
}

func (tx *Tx) view(table, key string) (Row, bool) {
	table = tx.db.resolve(table)
	lk := lockKey{table, key}
	if tx.deleted[lk] {
		return nil, false
	}
	if w, ok := tx.writes[lk]; ok {
		return w.clone(), true
	}
	r, ok := tx.db.committed[table][key]
	if !ok {
		return nil, false
	}
	return r.clone(), true
}

// givesGenerated refuses a value for a generated column, which Postgres
// rejects and the write would overwrite.
func (tx *Tx) givesGenerated(table string, row Row, call string) error {
	def := tx.db.defs[table]
	if def == nil {
		return nil
	}
	for _, col := range def.columns { // in declaration order, for a stable error
		if _, given := row[col]; given && def.generated[col] != nil {
			return unsupported(fmt.Sprintf("a value for generated column %q", col), call)
		}
	}
	return nil
}

// lockLatest locks the row read under key and returns its newest version,
// following it to the key an UPDATE moved it to and locking it there too.
// ok is false when the row is gone.
func (tx *Tx) lockLatest(table, key string, lock func(lockKey) error) (string, Row, bool, error) {
	held := len(tx.locks)
	for {
		before := len(tx.locks)
		if err := lock(lockKey{table, key}); err != nil {
			return "", nil, false, err
		}
		next, cur, ok := tx.latest(table, key)
		if ok && next != key {
			// The row moved: a lock taken here on the key it left would keep
			// an insert of that key waiting, which the moved row does not.
			tx.releaseLocks(tx.locks[before:])
			tx.locks = tx.locks[:before]
		}
		if !ok {
			// Postgres keeps no lock on a row that is gone, and detest's
			// would block an insert of the same key: let go of those taken
			// here, keeping the ones the transaction held before.
			tx.releaseLocks(tx.locks[held:])
			tx.locks = tx.locks[:held]
			return "", nil, false, nil
		}
		if next == key {
			return key, cur, true, nil
		}
		key = next
	}
}

// latest returns the newest version of the row read under key, following the
// keys UPDATEs moved it to, as Postgres follows a row's update chain after a
// wait. A row's identity is its key, so a row since inserted under the old
// key is taken for it.
func (tx *Tx) latest(table, key string) (string, Row, bool) {
	table = tx.db.resolve(table)
	for range len(tx.db.moved) + 1 {
		if r, ok := tx.view(table, key); ok {
			return key, r, true
		}
		next, ok := tx.db.moved[lockKey{table, key}]
		if !ok {
			break
		}
		key = next
	}
	return "", nil, false
}

func (tx *Tx) check() error {
	if tx.closed || tx.aborted {
		return tx.abortedError()
	}
	return nil
}

func (tx *Tx) abortedError() error {
	return tx.db.kind.Error(sqlir.InFailedTransaction, "current transaction is aborted, commands ignored until end of transaction block", "", "", "")
}

func (tx *Tx) updateWhere(table string, pred func(Row) bool, fields Row, desc any) (int, error) {
	var n int
	err := tx.asStatement(func() error {
		var err error
		n, err = tx.update(table, pred, fields, desc)
		return err
	})
	return n, err
}

func (tx *Tx) update(table string, pred func(Row) bool, fields Row, desc any) (int, error) {
	table = tx.db.resolve(table)
	if err := tx.check(); err != nil {
		return 0, err
	}
	if err := tx.givesGenerated(table, fields, "Tx.Update"); err != nil {
		return 0, err
	}
	tx.yieldf("%s: update %s set %s where %s", tx.db.name, table, fields, desc)
	n := 0
	cols := make([]string, 0, len(fields))
	for c := range fields {
		cols = append(cols, c)
	}
	mode := tx.db.updateLock(table, cols)
	done := map[string]bool{}
	x := tx.evaluator() // one for the operation, so statement_timestamp() is one time
	for _, r := range tx.selectNoYield(table, pred) {
		key, cur, ok, err := tx.lockLatest(table, r.Key(), func(lk lockKey) error { return tx.lockMode(lk, mode) })
		if err != nil {
			return n, err
		}
		if !ok || done[key] || !pred(cur) {
			continue
		}
		done[key] = true
		lk := lockKey{table, key}
		old := cur.clone()
		maps.Copy(cur, fields)

		if err := x.checkRow(table, cur); err != nil {
			return n, err
		}
		if !sameRow(old, cur) {
			tx.undo++ // written before the secondary indexes' checks, which may wait
		}
		tx.beginUpdate(lk, cur)
		defer tx.endUpdate(lk)
		if err := x.checkUniques(table, cur, key, old); err != nil {
			return n, err
		}
		if err := x.checkParents(table, cur, old); err != nil {
			return n, err
		}
		if err := x.onParentUpdate(table, old, cur); err != nil {
			return n, err
		}
		if lk, err = x.rekey(table, lk, cur); err != nil {
			return n, err
		}
		tx.writes[lk] = cur
		n++
	}
	return n, nil
}

func (tx *Tx) selectNoYield(table string, pred func(Row) bool) []Row {
	table = tx.db.resolve(table)
	tx.started = true // reading InnoDB's data starts its transaction
	seen := map[string]bool{}
	var keys []string
	for k := range tx.db.committed[table] {
		keys = append(keys, k)
		seen[k] = true
	}
	for lk := range tx.writes {
		if lk.table == table && !seen[lk.key] {
			keys = append(keys, lk.key)
		}
	}
	sort.Strings(keys)
	var out []Row
	for _, k := range keys {
		r, ok := tx.view(table, k)
		if ok && (pred == nil || pred(r)) {
			out = append(out, r)
		}
	}
	return out
}

func (tx *Tx) commit() {
	if tx.p != nil && tx.p.r.over() {
		tx.closed = true // a commit while the processes of an ended run unwind
		return
	}
	for lk := range tx.writes {
		delete(tx.db.moved, lk) // the key holds a row of its own now
	}
	for lk := range tx.deleted {
		delete(tx.db.moved, lk) // a row moved here before is not this one
	}
	maps.Copy(tx.db.moved, tx.moved)
	tx.recordVersions()
	// Only a commit that changes a row counts as a change for idle loops. A
	// MySQL UPDATE leaving its rows as they were still commits writes, and
	// two idle loops issuing such updates would otherwise wake each other
	// forever. The enqueues in tx.deferred count themselves.
	changed := false
	for lk, r := range tx.writes {
		t := tx.db.committed[lk.table]
		if t == nil {
			t = map[string]Row{}
			tx.db.committed[lk.table] = t
		}
		if prev, ok := t[lk.key]; !ok || !sameRow(prev, r) {
			changed = true
		}
		t[lk.key] = r
		tx.db.touched[lk.table] = true
	}
	for lk := range tx.deleted {
		if _, ok := tx.db.committed[lk.table][lk.key]; ok {
			changed = true
		}
		delete(tx.db.committed[lk.table], lk.key)
		tx.db.touched[lk.table] = true
	}
	for _, fn := range tx.deferred {
		fn()
	}
	if tx.p != nil && changed {
		tx.p.r.bump(tx.p)
	}
	tx.release()
}

func (tx *Tx) rollback() { tx.release() }

func (tx *Tx) release() {
	tx.closed = true
	if tx.p != nil && tx.p.r.over() {
		// The run ended and the next one resets the simulated resources. Releasing here
		// would race with the other cleanups running while the processes unwind.
		return
	}
	tx.releaseLocks(tx.locks)
	tx.releaseGaps()
}

// checkTable reports a table that does not exist. A database with a declared
// schema takes it as complete; one without lets any table spring up empty.
func (db *DB) checkTable(table string) error {
	if len(db.defs) == 0 || db.defs[table] != nil || db.views[table] != nil || db.ignored[table] {
		return nil
	}
	return db.kind.Error(sqlir.UndefinedTable, fmt.Sprintf("relation %q does not exist", relname(table)), relname(table), "", "")
}

func (db *DB) isIgnored(table string) bool { return db.ignored[db.resolve(table)] }

// updateLock is the row lock an UPDATE of these columns takes: FOR NO KEY
// UPDATE unless it changes a key column, one in the primary key or in a
// unique index a foreign key could reference. A table without a declared
// schema is keyed by id.
func (db *DB) updateLock(table string, cols []string) lockMode {
	if db.kind.InnoDB() {
		return lockUpdate // InnoDB has no lock weaker than exclusive for writes
	}
	def := db.defs[table]
	if def != nil {
		// A generated column changes with the columns it is computed from,
		// so it counts as written when one of them is.
		for _, col := range def.columns {
			if g := def.generated[col]; g != nil && slices.ContainsFunc(g.columns(), func(dep string) bool { return slices.Contains(cols, dep) }) {
				cols = append(slices.Clip(cols), col)
			}
		}
	}
	for _, c := range cols {
		if def == nil {
			if c == "id" {
				return lockUpdate
			}
			continue
		}
		if slices.Contains(def.pk, c) {
			return lockUpdate
		}
		for _, u := range def.uniques {
			if u.Where == nil && refersTo(u, c) {
				return lockUpdate
			}
		}
	}
	return lockNoKeyUpdate
}

// pkConstraint is the name of table's primary key constraint.
func (db *DB) pkConstraint(table string) string {
	if def := db.defs[table]; def != nil && def.pkName != "" {
		return def.pkName
	}
	return relname(table) + "_pkey"
}
