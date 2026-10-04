package detest

import (
	"fmt"
	"slices"

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
			return nil
		}
		what := fmt.Sprintf("a lock on %s/%s", lk.table, lk.key)
		if err := tx.selfWait(conflict, what, "a row lock"); err != nil {
			return err
		}
		// The timeout is decided before a deadlock victim, since a timeout
		// that ends this wait breaks the cycle and no victim is aborted.
		if tx.lockTimeout && tx.p.Choose("lock timeout on "+lk.table, 2) == 1 {
			tx.aborted = true
			tx.p.r.note(tx.p, "lock timeout waiting for %s/%s", lk.table, lk.key)
			return tx.db.kind.Error(sqlir.LockWaitTimeout, "canceling statement due to lock timeout", relname(lk.table), "", "")
		}
		if err := tx.breakCycle(conflict, what); err != nil {
			return err
		}
		tx.p.blockOnRow(rowWait{key: lk, mode: mode, tx: tx}, conflict[0])
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
			tx.p.r.note(tx.p, "waits for %s held by its own open transaction", what)
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
		v := members[tx.p.Choose("deadlock victim", len(members))]
		if v == tx {
			tx.aborted = true
			tx.p.r.note(tx.p, "deadlock detected, transaction aborted")
			return tx.db.kind.Error(sqlir.Deadlock, "deadlock detected", "", "", "")
		}
		v.deadlockVictim = true
		v.p.state = stateReady
		v.p.waitRow = nil
	}
	// A cycle left after another victim was picked is one the database
	// detects too, once that victim's abort wakes this wait.
	for _, o := range conflict {
		if cycle == nil && tx.p.r.waitsFor(o.p, tx.p) {
			tx.p.r.pending = &violation{kind: "progress", err: fmt.Errorf("process %s waits for %s held by %s, closing a cycle of waits the database cannot detect (a mutex held across a statement?)", tx.p.name, what, o.p.name)}
			break
		}
	}
	return nil
}

// victim ends a wait that another transaction's deadlock check broke by
// picking tx as the victim.
func (tx *Tx) victim() error {
	if !tx.deadlockVictim {
		return nil
	}
	tx.deadlockVictim = false
	tx.aborted = true
	tx.p.r.note(tx.p, "deadlock detected, transaction aborted")
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
		if w.p == nil || w.p.state != stateBlockedLock || w.p.waitRow == nil || w.p.waitRow.tx != w {
			return nil
		}
		return w.p.waitRow.blockers()
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
// conflicts with mode, for NOWAIT and SKIP LOCKED.
func (tx *Tx) heldByOther(lk lockKey, mode lockMode) bool {
	holders := tx.conflicting(lk, mode)
	for _, h := range holders {
		h.passedOver = true
	}
	return len(holders) > 0
}
