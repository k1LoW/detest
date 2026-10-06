package detest

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// lockMode is a row lock strength, ordered weakest first.
type lockMode int

const (
	lockKeyShare    lockMode = iota + 1 // FOR KEY SHARE, which a foreign key check takes on the parent row
	lockShare                           // FOR SHARE
	lockNoKeyUpdate                     // FOR NO KEY UPDATE, and an UPDATE that changes no key column
	lockUpdate                          // FOR UPDATE, DELETE, an UPDATE of a key column, a new row, a unique value
)

func (m lockMode) String() string {
	return [...]string{"", "FOR KEY SHARE", "FOR SHARE", "FOR NO KEY UPDATE", "FOR UPDATE"}[m]
}

// conflicts is PostgreSQL's row lock conflict table.
func (m lockMode) conflicts(o lockMode) bool {
	switch {
	case m == lockUpdate || o == lockUpdate:
		return true
	case m == lockKeyShare || o == lockKeyShare:
		return false
	case m == lockShare && o == lockShare:
		return false
	}
	return true // NO KEY UPDATE against SHARE or NO KEY UPDATE
}

// rowLock is who holds a lock: each transaction with the strongest mode it
// took.
type rowLock map[*Tx]lockMode

// rowWait is the lock a blocked process waits for.
type rowWait struct {
	key  lockKey
	mode lockMode
	tx   *Tx
	// insert is the row an InnoDB insert waits to put into a gap another
	// transaction locked, and old the row it updates, if it is an update
	// moving an index value.
	insert, old Row
}

// blockers returns the transactions w waits for.
func (w *rowWait) blockers() []*Tx {
	if w.insert != nil {
		return w.tx.gapHolders(w.key.table, w.insert, w.old)
	}
	return w.tx.conflicting(w.key, w.mode)
}

// conflicting returns the transactions other than tx whose hold on lk
// conflicts with mode.
func (tx *Tx) conflicting(lk lockKey, mode lockMode) []*Tx {
	var out []*Tx
	for o, m := range tx.db.locks[lk] {
		if o != tx && m.conflicts(mode) {
			out = append(out, o)
		}
	}
	return out
}

// lock takes the row lock exclusively, as a write does.
func (tx *Tx) lock(lk lockKey) error { return tx.lockMode(lk, lockUpdate) }

// lockMode takes a row lock of the given strength, blocking while another
// transaction holds a conflicting one. A cycle of row lock waits fails with
// a deadlock error, as Postgres's detector does; a cycle through a mutex or
// another transaction of this process hangs in Postgres and is reported.
func (tx *Tx) lockMode(lk lockKey, mode lockMode) error {
	var key lockStruct
	if !strings.HasPrefix(lk.table, "\x00") {
		key = structKey(lk.table, "PRIMARY", mode, "record")
	}
	return tx.lockModeAs(lk, mode, key)
}

// lockModeAs is lockMode for a lock InnoDB keeps in the lock struct key,
// which a wait for it weighs too.
func (tx *Tx) lockModeAs(lk lockKey, mode lockMode, key lockStruct) error {
	return tx.lockWith(lk, mode, key, key)
}

// lockImplicit takes the lock InnoDB holds implicitly on a record the
// transaction writes, a row it inserts or an index entry it writes or
// removes: no lock struct, and, as a statement's rollback undoes the write,
// no lock left once it fails. A wait for it weighs as wait does.
func (tx *Tx) lockImplicit(lk lockKey, wait lockStruct) error {
	return tx.lockWith(lk, lockUpdate, lockStruct{}, wait)
}

// lockWith takes a lock that InnoDB keeps in the struct grant once granted,
// none for an implicit lock, and in the struct wait while it waits.
func (tx *Tx) lockWith(lk lockKey, mode lockMode, grant, wait lockStruct) error {
	tx.started = true
	table, _ := entryIndex(lk)
	tx.noteTableLock(table, mode)
	var cancelWait func()
	for {
		conflict := tx.conflicting(lk, mode)
		if len(conflict) == 0 {
			holders := tx.db.locks[lk]
			if holders == nil {
				holders = rowLock{}
				tx.db.locks[lk] = holders
			}
			if cur, held := holders[tx]; !held {
				holders[tx] = mode
				tx.locks = append(tx.locks, lk)
			} else if mode > cur {
				holders[tx] = mode
			}
			if grant == (lockStruct{}) && tx.db.kind.InnoDB() {
				if tx.implicit == nil {
					tx.implicit = map[lockKey]bool{}
				}
				tx.implicit[lk] = true
			}
			if grant != (lockStruct{}) && tx.db.kind.InnoDB() && !tx.covers(lk, grant) {
				if tx.grants == nil {
					tx.grants = map[lockKey][]lockStruct{}
				}
				tx.grants[lk] = append(tx.grants[lk], grant)
				tx.addStruct(grant)
				if strings.HasPrefix(lk.table, "\x00") {
					if tx.explicit == nil {
						tx.explicit = map[lockKey]bool{}
					}
					tx.explicit[lk] = true
				}
			}
			return nil
		}
		what := fmt.Sprintf("a lock on %s/%s", lk.table, lk.key)
		if err := tx.selfWait(conflict, what, "a row lock"); err != nil {
			return err
		}
		if cancelWait == nil {
			cancelWait = tx.noteWait(wait)
		}
		// A holder that took the lock while this one waited turns its
		// implicit lock explicit too.
		for _, o := range conflict {
			o.convertImplicit(lk)
		}
		// The timeout is decided before a deadlock victim, since a timeout
		// that ends this wait breaks the cycle and no victim is aborted.
		if tx.lockTimeout && tx.proc().Choose("lock timeout on "+lk.table, 2) == 1 {
			cancelWait()
			tx.aborted = true
			tx.p.r.note(tx.proc(), "lock timeout waiting for %s/%s", lk.table, lk.key)
			markPassedOver(conflict) // gave up on the lock, as NOWAIT does
			return tx.db.kind.Error(sqlir.LockWaitTimeout, "canceling statement due to lock timeout", relname(lk.table), "", "")
		}
		if err := tx.breakCycle(conflict, what); err != nil {
			return err
		}
		tx.proc().blockOnRow(rowWait{key: lk, mode: mode, tx: tx}, conflict[0])
		if err := tx.victim(); err != nil {
			return err
		}
	}
}

// selfWait fails a wait of tx for another transaction of the same process,
// on a lock the trace calls what and a report calls kind. Such a lock can
// never be released while this process waits for it: the typical shape is
// an RPC issued inside a transaction whose callee writes the same row on the
// same database. Postgres does not detect it (the holder is idle in
// transaction), so the statement hangs until a timeout.
func (tx *Tx) selfWait(conflict []*Tx, what, kind string) error {
	for _, o := range conflict {
		if o.p == tx.p {
			tx.aborted = true
			tx.p.r.note(tx.proc(), "waits for %s held by its own open transaction", what)
			tx.p.r.pending = &violation{kind: "progress", err: fmt.Errorf("process %s waits for %s held by its own open transaction (RPC inside a transaction writing the same row?)", tx.p.name, kind)}
			return ErrSelfWait
		}
	}
	return nil
}

// breakCycle settles a wait of tx for conflict that closes a cycle of lock
// waits, row and gap waits alike: it picks the victim, which may be tx, and
// reports a cycle through other waits, which the database cannot detect.
func (tx *Tx) breakCycle(conflict []*Tx, what string) error {
	cycle := tx.rowWaitCycle(conflict)
	if cycle != nil {
		// Each waiter checks for a deadlock once deadlock_timeout passes in
		// its own wait, and the one that finds the cycle aborts itself. Which
		// one that is depends on timing, so every member may be the victim.
		members := append([]*Tx{tx}, cycle...)
		var v *Tx
		if tx.db.kind.InnoDB() {
			v = innodbVictim(members)
		} else {
			v = members[tx.proc().Choose("deadlock victim", len(members))]
		}
		// The victim gives up on the locks it waited for, as NOWAIT does,
		// so their holders' release may let an idle loop it ran in retry.
		if v == tx {
			markPassedOver(conflict)
			tx.aborted = true
			tx.p.r.note(tx.proc(), "deadlock detected, transaction aborted")
			return tx.db.kind.Error(sqlir.Deadlock, "deadlock detected", "", "", "")
		}
		v.deadlockVictim = true
		for _, w := range v.waiters() {
			markPassedOver(w.waitRow.blockers())
			w.state = stateReady
			w.waitRow = nil
		}
	}
	// A cycle left after another victim was picked is one the database
	// detects too, once that victim's abort wakes this wait.
	for _, o := range conflict {
		if cycle == nil && tx.p.r.waitsFor(o.p, tx.proc()) {
			tx.p.r.pending = &violation{kind: "progress", err: fmt.Errorf("process %s waits for %s held by %s, closing a cycle of waits the database cannot detect (a mutex held across a statement?)", tx.proc().name, what, o.p.name)}
			break
		}
	}
	return nil
}

// innodbVictim is the transaction InnoDB rolls back to break a deadlock
// among members, the first of which closed the cycle: the lightest by
// innodbWeight, and of equal ones the one that closed the cycle, as InnoDB's
// detector picks it the moment the wait begins, with no timing involved.
func innodbVictim(members []*Tx) *Tx {
	v, vw := members[0], innodbWeight(members[0])
	for _, m := range members[1:] {
		if w := innodbWeight(m); w < vw {
			v, vw = m, w
		}
	}
	return v
}

// innodbWeight is a transaction's weight as InnoDB's deadlock detector takes
// it: the undo records it wrote and the lock structs it holds. InnoDB keeps a
// lock struct per table and table lock mode, and per index page, mode and
// kind of record lock (the record alone, the gap alone, or next-key with the
// gap before it), and a lock that waited keeps a struct of its own. The
// statements record theirs as they lock (noteTableLock, noteLockStruct,
// noteWait); a row locked otherwise, by a write or a foreign key or duplicate check,
// holds a record lock on the primary key. detest has no pages, so an index's
// records count as one page, and a row the transaction inserted holds an
// implicit lock, no struct.
func innodbWeight(t *Tx) int { return t.undo + len(t.heldStructs()) }

// heldStructs is the set of lock structs innodbWeight counts: those its
// locks and waits were recorded in, and IX on each table it wrote, which an
// insert's implicit locks leave as the only struct of the table.
func (t *Tx) heldStructs() map[lockStruct]bool {
	structs := maps.Clone(t.lockStructs)
	if structs == nil {
		structs = map[lockStruct]bool{}
	}
	for lk := range t.writes {
		structs[tableStruct(lk.table, "IX")] = true
	}
	for lk := range t.deleted {
		structs[tableStruct(lk.table, "IX")] = true
	}
	return structs
}

// shareDuplicate turns the implicit exclusive lock an insert took on lk,
// after waiting for a writer that then committed a duplicate there, into
// the shared lock InnoDB's duplicate check holds on that record, in struct
// s, which a statement's rollback keeps.
func (tx *Tx) shareDuplicate(lk lockKey, s lockStruct) {
	if !tx.db.kind.InnoDB() || !tx.implicit[lk] {
		return
	}
	holders := tx.db.locks[lk]
	if _, held := holders[tx]; !held {
		return
	}
	holders[tx] = lockShare
	delete(tx.implicit, lk)
	if strings.HasPrefix(lk.table, "\x00") {
		if tx.explicit == nil {
			tx.explicit = map[lockKey]bool{}
		}
		tx.explicit[lk] = true
	}
	if !tx.covers(lk, s) {
		if tx.grants == nil {
			tx.grants = map[lockKey][]lockStruct{}
		}
		tx.grants[lk] = append(tx.grants[lk], s)
		tx.addStruct(s)
	}
	tx.wake(map[lockKey]bool{lk: true})
}

// covers reports whether a lock tx holds on lk covers a request for one in
// struct g, as InnoDB grants such a request without a lock of its own: a
// lock of the same index at least as strong, next-key covering the record
// and the gap alone, or tx's implicit lock as the record's writer.
func (tx *Tx) covers(lk lockKey, g lockStruct) bool {
	if tx.implicit[lk] && g.kind != "gap" {
		return true
	}
	for _, h := range tx.grants[lk] {
		if h.table == g.table && h.index == g.index && (h.mode == "X" || h.mode == g.mode) &&
			(h.kind == g.kind || h.kind == "next-key" && (g.kind == "record" || g.kind == "gap")) {
			return true
		}
	}
	return false
}

// lockClass is the InnoDB lock mode a row lock of detest's strength is: S
// for the shared ones, X otherwise.
func lockClass(m lockMode) string {
	if m == lockShare || m == lockKeyShare {
		return "S"
	}
	return "X"
}

// noteLockStruct records the lock struct InnoDB keeps for record locks of a
// kind ("record", "gap" or "next-key") and mode on an index of table, which
// weighs the transaction as a deadlock victim.
func (tx *Tx) noteLockStruct(table, index string, mode lockMode, kind string) {
	if !tx.db.kind.InnoDB() {
		return
	}
	tx.addStruct(structKey(table, index, mode, kind))
}

// lockStruct identifies a lock struct InnoDB keeps: of a table lock (kind
// "table", mode IS or IX), of record locks of a kind and mode (S or X) on an
// index of table, or of a lock that waited (wait, numbered from 1), which
// joins no other.
type lockStruct struct {
	table, index, mode, kind string
	wait                     int
}

// structKey is the lock struct of record locks of a kind and mode on an
// index of table.
func structKey(table, index string, mode lockMode, kind string) lockStruct {
	return lockStruct{table: table, index: index, mode: lockClass(mode), kind: kind}
}

func tableStruct(table, mode string) lockStruct {
	return lockStruct{table: table, mode: mode, kind: "table"}
}

func (tx *Tx) addStruct(k lockStruct) {
	if tx.lockStructs == nil {
		tx.lockStructs = map[lockStruct]bool{}
	}
	tx.lockStructs[k] = true
}

// noteTableLock records the intention lock InnoDB takes on table before it
// locks rows of it, which a statement keeps even when it locks no row, as
// when SKIP LOCKED skips them all. IX covers IS, so IS taken after IX is no
// lock of its own, while IX taken after IS is.
func (tx *Tx) noteTableLock(table string, mode lockMode) {
	if !tx.db.kind.InnoDB() || strings.HasPrefix(table, "\x00") {
		return
	}
	if lockClass(mode) == "S" && tx.lockStructs[tableStruct(table, "IX")] {
		return
	}
	tx.addStruct(tableStruct(table, "I"+lockClass(mode)))
}

// noteWait records the lock struct InnoDB creates for a lock that has to
// wait, of the struct key (the zero one when the lock has none of its own). A
// waiting lock never joins a struct the transaction holds, and it stays once
// the lock is granted, so a wait for a kind of lock already held adds a
// struct, and one for a new kind is that kind's struct. It returns a
// function to drop the struct again when the wait ends in a timeout, which
// removes the waiting lock.
func (tx *Tx) noteWait(key lockStruct) (cancel func()) {
	if !tx.db.kind.InnoDB() {
		return func() {}
	}
	if key == (lockStruct{}) || tx.heldStructs()[key] {
		tx.waits++
		key = lockStruct{wait: tx.waits}
	}
	tx.addStruct(key)
	return func() { delete(tx.lockStructs, key) }
}

// convertImplicit records the lock struct InnoDB creates for tx when another
// transaction waits for a record tx holds implicitly: a row it inserted, or
// an entry of a unique secondary index it wrote. The lock turns explicit,
// joining a struct of its kind tx already holds.
func (tx *Tx) convertImplicit(lk lockKey) {
	if !tx.db.kind.InnoDB() || !tx.implicit[lk] || tx.explicit[lk] {
		return
	}
	table, index := entryIndex(lk)
	if index == "" {
		def := tx.db.defs[table]
		if def == nil {
			return
		}
		uname := strings.TrimPrefix(lk.table, "\x00unique\x00"+table+"\x00")
		i := slices.IndexFunc(def.uniques, func(u sqlir.UniqueDef) bool { return u.Name == uname })
		if i < 0 {
			return
		}
		index = uniqueIndex(&def.uniques[i])
	}
	tx.addStruct(structKey(table, index, lockUpdate, "record"))
}

// entryIndex is the table a lock key is of, and the index whose record it
// stands for: "PRIMARY" for a row, a plain index's name for its entry, and
// "" for a unique index's value, which names the index by its constraint.
func entryIndex(lk lockKey) (table, index string) {
	if name, ok := strings.CutPrefix(lk.table, "\x00unique\x00"); ok {
		table, _, _ = strings.Cut(name, "\x00")
		return table, ""
	}
	if name, ok := strings.CutPrefix(lk.table, "\x00index\x00"); ok {
		table, index, _ = strings.Cut(name, "\x00")
		return table, index
	}
	return lk.table, "PRIMARY"
}

// victim ends a wait that another transaction's deadlock check broke by
// picking tx as the victim.
func (tx *Tx) victim() error {
	if !tx.deadlockVictim {
		return nil
	}
	tx.deadlockVictim = false
	tx.aborted = true
	tx.p.r.note(tx.proc(), "deadlock detected, transaction aborted")
	return tx.db.kind.Error(sqlir.Deadlock, "deadlock detected", "", "", "")
}

// rowWaitCycle returns the waiting transactions that are on a cycle of row
// lock waits through tx, were tx to wait for conflict: a deadlock Postgres
// detects. It returns them in the order their processes started, since the
// lock tables are maps and the explorer needs the same options in the same
// order on every run. It returns nil when there is no cycle.
func (tx *Tx) rowWaitCycle(conflict []*Tx) []*Tx {
	waitsFor := func(w *Tx) []*Tx {
		// The process's wait may be another transaction's of the same process:
		// one it keeps open while a second connection waits.
		// The waiting processes may also be goroutines w's process handed it
		// to, several at once.
		if w.p == nil {
			return nil
		}
		var out []*Tx
		for _, p := range w.waiters() {
			out = append(out, p.waitRow.blockers()...)
		}
		return out
	}
	// The transactions tx would wait for, directly or through their waits,
	// and for each one the transactions waiting for it among them.
	reached := map[*Tx]bool{}
	waiters := map[*Tx][]*Tx{}
	stack := slices.Clone(conflict)
	for _, c := range conflict {
		reached[c] = true
	}
	for len(stack) > 0 {
		w := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, o := range waitsFor(w) {
			waiters[o] = append(waiters[o], w)
			if o != tx && !reached[o] {
				reached[o] = true
				stack = append(stack, o)
			}
		}
	}
	// Those of them that wait for tx, directly or through each other.
	onCycle := map[*Tx]bool{}
	stack = []*Tx{tx}
	for len(stack) > 0 {
		o := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, w := range waiters[o] {
			if !onCycle[w] {
				onCycle[w] = true
				stack = append(stack, w)
			}
		}
	}
	var cycle []*Tx
	for _, p := range tx.p.r.procs {
		for w := range onCycle {
			if w.p == p {
				cycle = append(cycle, w)
			}
		}
	}
	return cycle
}

// releaseLocks drops tx's hold on the given locks and wakes the processes
// waiting for them; each re-checks when resumed.
func (tx *Tx) releaseLocks(keys []lockKey) {
	released := map[lockKey]bool{}
	for _, lk := range keys {
		holders := tx.db.locks[lk]
		if _, held := holders[tx]; !held {
			continue
		}
		delete(holders, tx)
		delete(tx.grants, lk)
		delete(tx.implicit, lk)
		delete(tx.explicit, lk)
		if len(holders) == 0 {
			delete(tx.db.locks, lk)
		}
		released[lk] = true
	}
	tx.wake(released)
}

// rollbackLocks returns tx's locks to what it held at sp: it releases the
// ones taken since and weakens the ones strengthened since, as Postgres does
// when it aborts the subtransaction that took them.
func (tx *Tx) rollbackLocks(sp *savepoint) {
	tx.releaseLocks(tx.locks[sp.locks:])
	tx.locks = tx.locks[:sp.locks]
	weakened := map[lockKey]bool{}
	for _, lk := range tx.locks {
		if holders := tx.db.locks[lk]; holders[tx] > sp.modes[lk] {
			holders[tx] = sp.modes[lk]
			weakened[lk] = true
		}
	}
	tx.wake(weakened)
}

// wake makes the processes waiting for one of keys ready; each re-checks
// when resumed.
func (tx *Tx) wake(keys map[lockKey]bool) {
	if tx.p == nil || len(keys) == 0 {
		return
	}
	if tx.passedOver {
		tx.p.r.bump(tx.p)
	}
	for _, p := range tx.p.r.procs {
		if p.state == stateBlockedLock && p.waitRow != nil && keys[p.waitRow.key] {
			p.state = stateReady
			p.waitRow = nil
		}
	}
}

// heldByOther reports whether another transaction holds lk in a mode that
// conflicts with mode, for NOWAIT, SKIP LOCKED and the try locks, which give
// up on the lock rather than wait. It marks the holders as passed over, so a
// read that waits instead asks conflicting directly.
func (tx *Tx) heldByOther(lk lockKey, mode lockMode) bool {
	holders := tx.conflicting(lk, mode)
	markPassedOver(holders)
	return len(holders) > 0
}

// markPassedOver marks holders whose lock an operation gave up on.
func markPassedOver(holders []*Tx) {
	for _, h := range holders {
		h.passedOver = true
	}
}
