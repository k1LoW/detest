package detest

import (
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

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
	kind      Server
	s         *Sim
	committed map[string]map[string]Row
	locks     map[lockKey]rowLock
	touched   map[string]bool // tables committed to since the run's last snapshot

	// Declared by schema statements and kept across runs. Tables are named
	// schema-qualified ("public.orders"); resolve maps a name as written.
	defs     map[string]*tableDef
	matviews map[string]*sqlir.CreateTableAsStmt // the query each materialized view refreshes from
	views    map[string]*sqlir.SchemaChange      // the query of each view
	seqs     map[string]int64                    // sequence values of the run, for nextval
	uuids    int64                               // gen_random_uuid values handed out in the run
}

// tableDef is what the schema declares about a table.
type tableDef struct {
	pk       []string // primary key columns; nil without a primary key
	pkName   string
	uniques  []sqlir.UniqueDef // unique constraints and indexes other than the primary key
	fks      []sqlir.ForeignKey
	columns  []string          // in declaration order, for applying defaults deterministically
	types    map[string]string // column types, for the checks Postgres makes on write
	defaults map[string]sqlir.Expr
}

// Name returns the database name.
func (db *DB) Name() string { return db.name }

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
	tx := &Tx{db: db, p: p, writes: map[lockKey]Row{}, deleted: map[lockKey]bool{}}
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

func (def *tableDef) addConstraint(kind Server, table string, u sqlir.UniqueDef) error {
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
func (def *tableDef) dropConstraint(name string) bool {
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

// dropColumn forgets a column's default and, as Postgres does, the unique
// indexes over it.
func (def *tableDef) dropColumn(col string) {
	def.columns = slices.DeleteFunc(def.columns, func(c string) bool { return c == col })
	delete(def.defaults, col)
	delete(def.types, col)
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
	if t, ok := def.types[old]; ok {
		delete(def.types, old)
		def.types[nw] = t
	}
	for i, c := range def.pk {
		if c == old {
			def.pk[i] = nw
		}
	}
	for _, u := range def.uniques {
		for _, e := range u.Elems {
			if c, ok := e.(*sqlir.ColumnRef); ok && c.Column == old {
				c.Column = nw
			}
		}
	}
	for i := range def.fks {
		for j, c := range def.fks[i].Columns {
			if c == old {
				def.fks[i].Columns[j] = nw
			}
		}
	}
}

func refersTo(u sqlir.UniqueDef, col string) bool {
	for _, e := range u.Elems {
		if c, ok := e.(*sqlir.ColumnRef); ok && c.Column == col {
			return true
		}
	}
	return false
}

// SeedRowNow inserts a committed row from a fake during a run, without a
// transaction or a yield: the fake's own step is the yield point.
func (db *DB) SeedRowNow(table string, row Row) { db.SeedRow(table, row) }

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
	if srv.Parser() == nil {
		s.t.Fatal("detest: DB needs a server, such as postgres.New()")
	}
	if err := srv.Check(srv.Isolation()); err != nil {
		s.t.Fatal(err)
	}
	db := &DB{name: name, kind: srv, s: s}
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
	for _, col := range ch.Columns {
		if !slices.Contains(def.columns, col.Name) {
			def.columns = append(def.columns, col.Name)
		}
		if col.Type != "" {
			if def.types == nil {
				def.types = map[string]string{}
			}
			def.types[col.Name] = col.Type
		}
		if col.TypeOnly {
			continue
		}
		if col.Default != nil {
			def.defaults[col.Name] = col.Default
		} else {
			delete(def.defaults, col.Name)
		}
	}
	for _, u := range ch.Constraints {
		if err := def.addConstraint(db.kind, table, u); err != nil {
			return err
		}
	}
	for _, fk := range ch.ForeignKeys {
		if fk.Name == "" {
			fk.Name = relname(table) + "_" + strings.Join(fk.Columns, "_") + "_fkey"
		}
		fk.RefTable = db.resolve(fk.RefTable)
		def.fks = append(def.fks, fk)
	}
	for _, name := range ch.DropConstraints {
		def.dropConstraint(name)
	}
	for _, col := range ch.DropColumns {
		def.dropColumn(col)
	}
	if old, nw := ch.RenameColumn[0], ch.RenameColumn[1]; old != "" {
		def.renameColumn(old, nw)
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
	return nil
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
		if ch.Drop && def.dropConstraint(idx) {
			return nil
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
	db       *DB
	p        *Proc
	writes   map[lockKey]Row
	deleted  map[lockKey]bool
	locks    []lockKey
	aborted  bool
	closed   bool
	deferred []func()
	atomic   bool
	block    bool // begun with BeginTx, so SAVEPOINT may be used
	// lockTimeout is set by SET LOCAL lock_timeout: a lock wait may then fail
	// with 55P03 instead of waiting on, which the explorer chooses.
	lockTimeout bool
	saves       []savepoint
	// deferAll and deferNamed are what SET CONSTRAINTS set, for ALL and by
	// constraint name; nil when it was not run.
	deferAll   *bool
	deferNamed map[string]bool
}

// savepoint is what ROLLBACK TO restores: the transaction's writes and the
// locks it held when the savepoint was set.
type savepoint struct {
	name     string
	writes   map[lockKey]Row
	deleted  map[lockKey]bool
	locks    int
	deferred int
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
	table = tx.db.resolve(table)
	if err := tx.check(); err != nil {
		return err
	}
	row = row.clone()
	x := tx.evaluator()
	if err := x.applyDefaults(table, row); err != nil {
		return err
	}
	if err := x.checkTypes(table, row); err != nil {
		return err
	}
	if err := tx.db.assignKey(table, row); err != nil {
		return err
	}
	lk := lockKey{table, row.Key()}
	tx.yieldf("%s: insert %s %s", tx.db.name, table, row)
	if err := tx.lock(lk); err != nil {
		return err
	}
	if _, exists := tx.view(table, row.Key()); exists {
		return tx.db.duplicateKey(table, tx.db.pkConstraint(table))
	}
	if err := x.checkUniques(table, row, "", nil); err != nil {
		return err
	}
	if err := x.checkParents(table, row, nil); err != nil {
		return err
	}
	delete(tx.deleted, lk)
	tx.writes[lk] = row.clone()
	return nil
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
	db.seqs = map[string]int64{}
	db.uuids = 0
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
	delete(tx.writes, lk)
	tx.deleted[lk] = true
	if err := tx.evaluator().onParentDelete(table, cur); err != nil {
		return false, err
	}
	return true, nil
}

// Enqueue publishes a message when the transaction commits (outbox pattern).
func (tx *Tx) Enqueue(q *Queue, msg Msg) {
	tx.deferred = append(tx.deferred, func() { q.push(msg) })
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
		sp := savepoint{name: name, writes: map[lockKey]Row{}, deleted: map[lockKey]bool{}, locks: len(tx.locks), deferred: len(tx.deferred)}
		for k, v := range tx.writes {
			sp.writes[k] = v.clone()
		}
		maps.Copy(sp.deleted, tx.deleted)
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
	sp := tx.saves[i]
	tx.saves = tx.saves[:i+1] // ROLLBACK TO keeps the savepoint itself
	tx.writes, tx.deleted = map[lockKey]Row{}, map[lockKey]bool{}
	for k, v := range sp.writes {
		tx.writes[k] = v.clone()
	}
	maps.Copy(tx.deleted, sp.deleted)
	tx.deferred = tx.deferred[:sp.deferred]
	tx.releaseLocks(tx.locks[sp.locks:])
	tx.locks = tx.locks[:sp.locks]
	tx.aborted = false
	return nil
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
	table = tx.db.resolve(table)
	if err := tx.check(); err != nil {
		return 0, err
	}
	tx.yieldf("%s: update %s set %s where %s", tx.db.name, table, fields, desc)
	n := 0
	for _, r := range tx.selectNoYield(table, pred) {
		lk := lockKey{table, r.Key()}
		cols := make([]string, 0, len(fields))
		for c := range fields {
			cols = append(cols, c)
		}
		if err := tx.lockMode(lk, tx.db.updateLock(table, cols)); err != nil {
			return n, err
		}
		cur, ok := tx.view(table, r.Key())
		if !ok || !pred(cur) {
			continue
		}
		old := cur.clone()
		maps.Copy(cur, fields)
		x := tx.evaluator()

		if err := x.checkTypes(table, cur); err != nil {
			return n, err
		}
		if err := x.checkUniques(table, cur, r.Key(), old); err != nil {
			return n, err
		}
		if err := x.checkParents(table, cur, old); err != nil {
			return n, err
		}
		if err := x.onParentUpdate(table, old, cur); err != nil {
			return n, err
		}
		var err error
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
	for lk, r := range tx.writes {
		t := tx.db.committed[lk.table]
		if t == nil {
			t = map[string]Row{}
			tx.db.committed[lk.table] = t
		}
		t[lk.key] = r
		tx.db.touched[lk.table] = true
	}
	for lk := range tx.deleted {
		delete(tx.db.committed[lk.table], lk.key)
		tx.db.touched[lk.table] = true
	}
	for _, fn := range tx.deferred {
		fn()
	}
	if tx.p != nil && len(tx.writes)+len(tx.deleted)+len(tx.deferred) > 0 {
		tx.p.r.version++
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
}

// checkTable reports a table that does not exist. A database with a declared
// schema takes it as complete; one without lets any table spring up empty.
func (db *DB) checkTable(table string) error {
	if len(db.defs) == 0 || db.defs[table] != nil || db.views[table] != nil {
		return nil
	}
	return db.kind.Error(sqlir.UndefinedTable, fmt.Sprintf("relation %q does not exist", relname(table)), relname(table), "", "")
}

// updateLock is the row lock an UPDATE of these columns takes: FOR NO KEY
// UPDATE unless it changes a key column, one in the primary key or in a
// unique index a foreign key could reference. A table without a declared
// schema is keyed by id.
func (db *DB) updateLock(table string, cols []string) lockMode {
	def := db.defs[table]
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
