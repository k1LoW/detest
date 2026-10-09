package detest

// What each step of a run reads and writes of the simulated resources, for
// PartialOrder. The explorer skips a reordering of two steps only when they
// are independent: neither writes a resource the other touches. Anything a
// step does that the trace cannot place is recorded as touching everything,
// so that an unknown access is a dependence rather than an assumption.
//
// Resources are named as strings. A table is "db:<db>:<table>" and one of its
// rows "db:<db>:<table>#<key>"; a table overlaps every one of its rows. A
// sequence is "seq:<db>:<name>", an AUTO_INCREMENT counter
// "autoinc:<db>:<table>", the uuid counter "uuid:<db>", a queue
// "queue:<name>", an External "ext:<name>", a mutex "mutex:<name>", the
// connection of a transaction goroutines share "txconn:<process>", a capped
// pool's connections "conn:<db>:<pool>", and "waits" every wait for a lock,
// whose order decides deadlocks and their victims. "*" is every resource.
//
// What is recorded where:
//   - A statement, when it starts after its yield point, records the tables
//     it names, from its parse tree: its target as a write, the others as
//     reads, and every table when a subquery, a CTE or a join leaves what it
//     reads to the evaluation. A statement whose WHERE pins every primary key
//     column to a value records the row instead of its table, and so does an
//     INSERT of given key values, unless a unique constraint makes other rows
//     matter too. The tables foreign keys lead to are recorded as well.
//   - The engine records what it then does: the rows it locks, the waits,
//     the values it draws from sequences and counters, the connection a
//     process takes, and a commit publishes the rows and tables the
//     transaction wrote or locked.
//   - Queues, Externals and mutexes record their calls; Proc.Step, a crash,
//     a stall and the clock touch everything.
//   - A call made while no step runs, such as by a goroutine the scheduler
//     waits for, is a step of its own that touches everything.

import (
	"slices"
	"strconv"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

const depAll = "*"

// depEvent is one step of a run: what it touched, the option the schedule
// took and the ones it could have taken, for the reduction.
type depEvent struct {
	proc    string
	kind    string // resume, deliver, start, crash, stall, lose, clock, waitoutside, outside
	op      uint64 // the operation a resume ran, for the trace hashing of tests
	choices []int  // the data choices made within the step
	acc     map[string]bool
	// ci is the index of the step choice in the run's choices, or -1 for a
	// forced step, an eager start or a barrier. ident is the option taken,
	// opts every option of the choice in order, ready the processes that
	// could run there and enabled every resume, deliver and start option
	// before the preemption bound, which the backtrack points respect.
	ci      int
	ident   string
	opts    []string
	ready   map[string]bool
	enabled map[string]bool
	// creator is the event the process was created in, for its first step.
	creator *depEvent
	eager   bool // an eager start, which the schedule does not pick
	// free marks a step taken where the running process could not go on,
	// so that any option there was a switch costing no preemption, and
	// preempt one taken away from a running process that could, which cost
	// one.
	free, preempt bool
	index         int
}

type depTrace struct {
	events []*depEvent
	cur    *depEvent
}

// dep returns the run's trace, under PartialOrder.
func (r *run) dep() *depTrace {
	if r.depTrace == nil {
		r.depTrace = &depTrace{}
	}
	return r.depTrace
}

// depOn reports whether the run records its trace: under PartialOrder, and
// for a test that observes the runs, whose full tree is compared by the
// histories the trace gives.
func (r *run) depOn() bool {
	return r != nil && (r.s.partialOrder || r.s.depObserve != nil) && !r.measuring
}

// depIdent names an option so that the same option can be found at the same
// state in another run: a process by its name, a message by its id in its
// queue, a start by its type.
func depIdent(o option) string {
	switch o.kind {
	case optResume:
		return "r:" + o.p.name
	case optDeliver:
		return "d:" + o.q.name + ":" + depMsgID(o.q.msgs[o.i]) + ":" + o.pt.name
	case optStart:
		return "s:" + o.pt.name
	case optCrash:
		return "c:" + o.p.name
	case optStall:
		return "t:" + o.p.name
	case optLose:
		return "l:" + o.q.name + ":" + depMsgID(o.q.msgs[o.i])
	}
	return "?"
}

func depMsgID(m *qmsg) string {
	id := strconv.Itoa(m.id)
	if m.duplicate {
		id += "dup"
	}
	return id
}

func depIsFault(ident string) bool {
	return strings.HasPrefix(ident, "c:") || strings.HasPrefix(ident, "t:") || strings.HasPrefix(ident, "l:")
}

// depStart opens the event of the step o, picked at the step choice ci
// among opts, or forced (ci < 0) when it was the only option.
func (r *run) depStart(o option, opts []option, ci int) {
	if !r.depOn() {
		return
	}
	d := r.dep()
	e := &depEvent{acc: map[string]bool{}, ci: ci, ident: depIdent(o)}
	if ci >= 0 {
		e.opts = make([]string, len(opts))
		for i, oo := range opts {
			e.opts[i] = depIdent(oo)
		}
		e.ready = map[string]bool{}
		e.enabled = map[string]bool{}
		for _, p := range r.procs {
			if p.state == stateReady && !r.keptOut(p) {
				e.ready[p.name] = true
				e.enabled["r:"+p.name] = true
			}
		}
		for _, oo := range opts {
			if oo.kind == optDeliver || oo.kind == optStart {
				e.enabled[depIdent(oo)] = true
			}
		}
		for _, oo := range r.depExtra {
			if oo.kind == optDeliver || oo.kind == optStart {
				e.enabled[depIdent(oo)] = true
			}
		}
	}
	cur := r.current
	e.free = cur == nil || cur.state != stateReady || r.keptOut(cur)
	e.preempt = !e.free && (o.kind != optResume || o.p != cur)
	switch o.kind {
	case optResume:
		e.proc, e.kind, e.op = o.p.name, "resume", o.p.depOp
		e.creator, o.p.depCreator = o.p.depCreator, nil
	case optStart:
		e.proc, e.kind = "start:"+o.pt.name, "start"
		e.acc["type:"+o.pt.name] = true
		if o.pt.after != nil || o.pt.when != nil || o.pt.kind == trigLoop {
			e.acc[depAll] = false // After, When and idle loops read the state
		}
	case optDeliver:
		e.proc, e.kind = "deliver:"+o.q.name, "deliver"
		e.acc["queue:"+o.q.name] = true
		e.acc["type:"+o.pt.name] = true
		e.creator = o.q.msgs[o.i].depEnq
		if o.pt.after != nil || o.pt.when != nil {
			e.acc[depAll] = false
		}
	case optLose:
		e.proc, e.kind = "lose:"+o.q.name, "lose"
		e.acc["queue:"+o.q.name] = true
	case optCrash:
		e.proc, e.kind = o.p.name, "crash"
		e.acc[depAll] = true
	case optStall:
		e.proc, e.kind = o.p.name, "stall"
		e.acc[depAll] = true
	}
	d.cur = e
	d.events = append(d.events, e)
}

// depEnd closes the step's event. A delivery or a start is the first step
// of the process it started, whose own steps follow it.
func (r *run) depEnd() {
	if !r.depOn() {
		return
	}
	d := r.dep()
	if e := d.cur; e != nil && (e.kind == "start" || e.kind == "deliver") && r.current != nil {
		e.proc = r.current.name
	}
	d.cur = nil
}

// depCur returns the event of the step being run, or nil between steps.
func (r *run) depCur() *depEvent {
	if !r.depOn() || r.depTrace == nil {
		return nil
	}
	return r.depTrace.cur
}

// depBarrier records something the scheduler did outside a step, such as
// advancing the clock, as a step every other depends on. Inside a step,
// such as a process parking outside detest, the step itself is what every
// other then depends on.
func (r *run) depBarrier(kind string) {
	if !r.depOn() {
		return
	}
	d := r.dep()
	if d.cur != nil {
		d.cur.acc[depAll] = true
		return
	}
	d.events = append(d.events, &depEvent{proc: "-", kind: kind, ci: -1, acc: map[string]bool{depAll: true}})
}

// depRecord records that the step being run touches res. Outside any step,
// such as a goroutine the scheduler waits for calling in, the call is a
// step of its own that touches everything, since whose step it belongs to
// is not known and timing must not decide what is recorded.
func (r *run) depRecord(res string, write bool) {
	if !r.depOn() || r.over() {
		return
	}
	d := r.dep()
	if d.cur == nil {
		d.cur = &depEvent{proc: "-", kind: "outside", ci: -1, acc: map[string]bool{depAll: true}}
		d.events = append(d.events, d.cur)
		r.sleep = nil // every transition depends on it
	}
	if write {
		d.cur.acc[res] = true
	} else if _, ok := d.cur.acc[res]; !ok {
		d.cur.acc[res] = false
	}
}

// depChoice records a data choice made within the step.
func (r *run) depChoice(label string, picked int) {
	if !r.depOn() || label == "step" {
		return
	}
	if e := r.dep().cur; e != nil {
		e.choices = append(e.choices, picked)
	}
}

// depCreated records the event p is created in, for the creation edge of
// its first step. Outside any step, such as a goroutine adopted when the
// scheduler settles, the step that ran last stands for it: what reached
// detest then was set going by that step or by the clock.
func (r *run) depCreated(p *Proc) {
	if !r.depOn() {
		return
	}
	d := r.dep()
	if d.cur != nil {
		p.depCreator = d.cur
	} else if n := len(d.events); n > 0 {
		p.depCreator = d.events[n-1]
	}
}

// depTable and depRow record a table or a row of db, in the transaction tx
// if any, whose end publishes what it wrote or locked.
func (r *run) depTable(tx *Tx, db *DB, table string, write bool) {
	r.depTouch(tx, "db:"+db.name+":"+table, write)
}

func (r *run) depRow(tx *Tx, db *DB, table, key string, write bool) {
	r.depTouch(tx, "db:"+db.name+":"+table+"#"+key, write)
}

func (r *run) depTouch(tx *Tx, res string, write bool) {
	if !r.depOn() {
		return
	}
	r.depRecord(res, write)
	if tx == nil || tx.closed {
		return
	}
	if write {
		if tx.depTouched == nil {
			tx.depTouched = map[string]bool{}
		}
		tx.depTouched[res] = true
	}
	// A snapshot taken for the whole transaction reads every table as of
	// now.
	if tx.block && tx.iso >= RepeatableRead && !tx.depSnapped {
		tx.depSnapped = true
		r.depRecord(depAll, false)
	}
}

// depTxEnd records a commit, a rollback or a savepoint's end, which
// publishes the writes and releases the locks of what the transaction
// touched, and frees its connection for the goroutines sharing it.
func (r *run) depTxEnd(tx *Tx) {
	if !r.depOn() {
		return
	}
	for res := range tx.depTouched {
		r.depRecord(res, true)
	}
	if tx.iso >= Serializable {
		r.depRecord(depAll, true)
	}
	if tx.p != nil {
		r.depRecord("txconn:"+tx.p.name, true)
	}
	tx.depTouched, tx.depSnapped, tx.depPin = nil, false, depPin{}
}

// depSavepoint records a savepoint's rollback or release, which publishes
// or gives up what the transaction wrote and locked after it, as its end
// would, while the transaction goes on.
func (r *run) depSavepoint(tx *Tx) {
	if !r.depOn() {
		return
	}
	for res := range tx.depTouched {
		r.depRecord(res, true)
	}
}

// depConflicts reports whether two events depend on each other: the same
// process's, a step outside any process's, or two touching one resource
// with a write among them. Under Always or Sometimes, which are checked
// after every step, two steps that change the state the invariants see are
// dependent as well, since the order of the two changes is observed.
func (e *depEvent) depConflicts(f *depEvent, global bool) bool {
	if e.proc == f.proc || e.proc == "-" || f.proc == "-" {
		return true
	}
	if global && e.writesState() && f.writesState() {
		return true
	}
	for res, w := range e.acc {
		for res2, w2 := range f.acc {
			if (w || w2) && depSameRes(res, res2) {
				return true
			}
		}
	}
	return false
}

func (e *depEvent) writesState() bool {
	for res, w := range e.acc {
		if w && (res == depAll || strings.HasPrefix(res, "db:") || strings.HasPrefix(res, "queue:")) {
			return true
		}
	}
	return false
}

// depSameRes reports whether two resources overlap: the same one, everything,
// or a table and one of its rows. A row's resource is its table's followed
// by "#" and the key, so the table is tested as a prefix rather than by
// splitting at the first "#", which a quoted table name may hold, where the
// split would make a table and its rows look distinct. Two tables where one's
// name is the other's followed by "#" count as overlapping, which only adds
// a dependence.
func depSameRes(a, b string) bool {
	if a == b || a == depAll || b == depAll {
		return true
	}
	return strings.HasPrefix(b, a+"#") || strings.HasPrefix(a, b+"#")
}

func (e *depEvent) isStep() bool {
	return e.kind == "resume" || e.kind == "deliver" || e.kind == "start"
}

// --- statements ---

// depStatement records, once a statement runs after its yield point, the
// tables it names and the row it pins, and keeps the pin for what the
// engine records of the statement's own table.
func (x *sqlExec) depStatement(stmt sqlir.Statement, write bool) {
	tx := x.tx
	if tx.p == nil || !tx.p.r.depOn() {
		return
	}
	r := tx.p.r
	db := tx.db
	all := false // the statement reads what only the evaluation decides
	refs := map[string]bool{}
	var walkSelect func(sel *sqlir.SelectStmt)
	walkRef := func(t *sqlir.TableRef) {
		switch {
		case t == nil:
		case t.Sub != nil:
			walkSelect(t.Sub)
		case t.Func != nil:
			all = true // a function may read any table
		case t.Name != "":
			refs[db.resolve(t.Name)] = true
		}
	}
	walkSelect = func(sel *sqlir.SelectStmt) {
		if sel == nil {
			return
		}
		if len(sel.With) > 0 || sel.SetOp != "" || len(sel.Values) > 0 {
			all = true
		}
		for _, c := range sel.With {
			walkSelect(c.Select)
		}
		walkSelect(sel.Larg)
		walkSelect(sel.Rarg)
		walkRef(sel.From)
		for i := range sel.Joins {
			walkRef(&sel.Joins[i].Table)
			if !depSimple(sel.Joins[i].On) {
				all = true
			}
		}
		if !simpleTargets(sel.Targets) || !depSimple(sel.Where) || !depSimple(sel.Having) || sel.Limit != nil || sel.Offset != nil {
			all = true
		}
		for _, g := range sel.GroupBy {
			if !simpleExpr(g) {
				all = true
			}
		}
		for _, o := range sel.OrderBy {
			if !simpleExpr(o.Expr) {
				all = true
			}
		}
	}
	target, key := "", ""
	var def *tableDef
	switch s := stmt.(type) {
	case *sqlir.SelectStmt:
		walkSelect(s)
		if s.From != nil && s.From.Name != "" && len(s.Joins) == 0 && len(s.With) == 0 && s.SetOp == "" {
			target = db.resolve(s.From.Name)
			def = db.defs[target]
			if def != nil && depPinnable(def, false) {
				key = x.depPinnedKey(def, target, s.From.Name, s.From.Alias, s.Where)
			}
		}
	case *sqlir.InsertStmt:
		target = db.resolve(s.Table)
		def = db.defs[target]
		if s.Select != nil {
			walkSelect(s.Select)
			all = true
		}
		if !simpleTargets(s.Returning) {
			all = true
		}
		if def != nil && s.Select == nil && s.OnConflict == nil && len(s.Rows) == 1 && depPinnable(def, true) {
			key = x.depInsertKey(def, target, s)
		}
	case *sqlir.UpdateStmt:
		target = db.resolve(s.Table)
		def = db.defs[target]
		if len(s.With) > 0 || len(s.From) > 0 || !simpleTargets(s.Returning) || s.Limit != nil || len(s.OrderBy) > 0 {
			all = true
		}
		for i := range s.From {
			walkRef(&s.From[i])
		}
		for _, a := range s.Set {
			if !simpleExpr(a.Value) {
				all = true
			}
		}
		if def != nil && !all && depPinnable(def, true) {
			key = x.depPinnedKey(def, target, s.Table, s.Alias, s.Where)
		}
	case *sqlir.DeleteStmt:
		target = db.resolve(s.Table)
		def = db.defs[target]
		if len(s.Using) > 0 || !simpleTargets(s.Returning) || s.Limit != nil || s.Truncate {
			all = true
		}
		for i := range s.Using {
			walkRef(&s.Using[i])
		}
		if def != nil && !all && depPinnable(def, false) {
			key = x.depPinnedKey(def, target, s.Table, s.Alias, s.Where)
		}
	default:
		all = true
	}
	if all {
		r.depRecord(depAll, false)
	}
	for t := range refs {
		if t != target {
			r.depTable(tx, db, t, false)
		}
	}
	if target == "" {
		return
	}
	if key != "" {
		r.depRow(tx, db, target, key, write)
	} else {
		r.depTable(tx, db, target, write)
	}
	tx.depPin = depPin{table: target, key: key}
	// The row the engine then locks and writes is recorded by its key, so
	// the pin is what the statement names, kept for the trace of tests.
	if write {
		_, insert := stmt.(*sqlir.InsertStmt)
		r.depFKs(tx, db, target, insert)
	}
}

// depFKs records the tables the foreign keys of table lead to, for a write
// of its rows: the tables it refers to, whose rows the write checks, and
// which the engine records only where it finds and locks one, not where
// the parent is absent and the write fails; and, unless the write is an
// insert, the tables referring to it, whose rows a delete or an update of
// a referenced row acts on.
func (r *run) depFKs(tx *Tx, db *DB, table string, insert bool) {
	def := db.defs[table]
	if !r.depOn() || def == nil {
		return
	}
	for _, fk := range def.fks {
		r.depTable(tx, db, db.resolve(fk.RefTable), false)
	}
	if insert {
		return
	}
	for name, other := range db.defs {
		for _, fk := range other.fks {
			if db.resolve(fk.RefTable) == table {
				r.depTable(tx, db, name, true)
			}
		}
	}
}

// depSimple reports whether an expression, absent or present, reads no
// table: simpleExpr takes an absent one for a subquery.
func depSimple(e sqlir.Expr) bool { return e == nil || simpleExpr(e) }

// depPin is the row a statement touches by its primary key, so that the
// engine's records of the statement's own table name the row instead.
type depPin struct{ table, key string }

// depPinnable reports whether a statement pinning a row of def by its
// primary key touches that row alone: no unique constraint makes another
// row's value matter, and nothing the table checks reads other rows.
func depPinnable(def *tableDef, writes bool) bool {
	if len(def.pk) == 0 {
		return false
	}
	if writes && (len(def.uniques) > 0 || len(def.autoInc) > 0) {
		return false
	}
	for _, c := range def.checks {
		if hasEffects(c.Expr) {
			return false
		}
	}
	return true
}

// depPinnedKey returns the primary key the WHERE pins every column of to
// a value, or "".
func (x *sqlExec) depPinnedKey(def *tableDef, table, name, alias string, where sqlir.Expr) string {
	if where == nil {
		return ""
	}
	vals := map[string]any{}
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
					v, err := x.eval(side[1], nil)
					if err == nil {
						v, err = x.columnValue(table, c.Column, v)
					}
					if err != nil {
						return false
					}
					vals[c.Column] = v
				}
			}
		}
		return true
	}
	if !walk(where) {
		return ""
	}
	return depKeyOf(def, vals)
}

// depInsertKey returns the primary key an INSERT of one row gives, or "".
func (x *sqlExec) depInsertKey(def *tableDef, table string, s *sqlir.InsertStmt) string {
	vals := map[string]any{}
	cols := s.Columns
	if len(cols) == 0 {
		cols = def.columns // the row gives every column in declaration order
	}
	for i, c := range cols {
		if i < len(s.Rows[0]) && isValue(s.Rows[0][i]) {
			v, err := x.eval(s.Rows[0][i], nil)
			if err == nil {
				v, err = x.columnValue(table, c, v)
			}
			if err != nil {
				return ""
			}
			vals[c] = v
		}
	}
	return depKeyOf(def, vals)
}

// depKeyOf returns the identity of the row whose primary key is vals, as
// encodeKey gives it to the row and to its locks, so that the row a
// statement pins and the row the engine locks are one resource; or "" when
// a key column is missing, or a value is bytes, which the comparisons take
// as the text they spell while encodeKey does not.
func depKeyOf(def *tableDef, vals map[string]any) string {
	key := make([]any, 0, len(def.pk))
	for _, c := range def.pk {
		v, ok := vals[c]
		if !ok {
			return ""
		}
		if _, isBytes := derefValue(v).([]byte); isBytes {
			return ""
		}
		key = append(key, v)
	}
	return encodeKey(key)
}

// depLock records a lock the transaction takes or waits for: the row for a
// row lock, the table for an entry of a unique or an index, and for the
// gap an insert waits for, which the gap's holder records as the table too
// (see depGap).
func (r *run) depLock(tx *Tx, lk lockKey) {
	if !r.depOn() {
		return
	}
	table, _ := entryIndex(lk)
	if lk.table == table && lk.key != gapWaitKey {
		r.depRow(tx, tx.db, table, lk.key, true)
	} else {
		r.depTable(tx, tx.db, table, true)
	}
}

// depGap records a gap lock InnoDB takes, which keeps other transactions
// from inserting into the gap: a write of the table, as the rows it covers
// have no key to name, and the statement's own pin would name only the row
// it found or missed.
func (tx *Tx) depGap(table string) {
	tx.depTable(table, true)
}

// depCascade records a write a foreign key action makes to another table
// than the statement's, which may reach a table the statement does not
// name through a chain of keys.
func (tx *Tx) depCascade(table string) {
	if tx.p != nil {
		tx.p.r.depTable(tx, tx.db, table, true)
	}
}

// depSorted returns the resources of an event, for tests and traces.
func (e *depEvent) depSorted() []string {
	out := make([]string, 0, len(e.acc))
	for res, w := range e.acc {
		if w {
			out = append(out, res+"=w")
		} else {
			out = append(out, res+"=r")
		}
	}
	slices.Sort(out)
	return out
}
