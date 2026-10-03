package detest

import (
	"fmt"

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
		for _, o := range conflict {
			// A lock held by another transaction of the same process can never
			// be released while this process waits for it: the typical shape is
			// an RPC issued inside a transaction whose callee writes the same
			// row on the same database. Postgres does not detect it (the holder
			// is idle in transaction), so the statement hangs until a timeout.
			if o.p == tx.p {
				tx.aborted = true
				tx.p.r.note(tx.p, "waits for a lock on %s/%s held by its own open transaction", lk.table, lk.key)
				tx.p.r.pending = &violation{kind: "progress", err: fmt.Errorf("process %s waits for a row lock held by its own open transaction (RPC inside a transaction writing the same row?)", tx.p.name)}
				return ErrSelfWait
			}
		}
		if tx.rowWaitCycle(conflict) {
			tx.aborted = true
			tx.p.r.note(tx.p, "deadlock detected, transaction aborted")
			return tx.db.kind.Error(sqlir.Deadlock, "deadlock detected", "", "", "")
		}
		for _, o := range conflict {
			if tx.p.r.waitsFor(o.p, tx.p) {
				tx.p.r.pending = &violation{kind: "progress", err: fmt.Errorf("process %s waits for a lock on %s/%s held by %s, closing a cycle of waits the database cannot detect (a mutex held across a statement?)", tx.p.name, lk.table, lk.key, o.p.name)}
				break
			}
		}
		if tx.lockTimeout && tx.p.Choose("lock timeout on "+lk.table, 2) == 1 {
			tx.aborted = true
			tx.p.r.note(tx.p, "lock timeout waiting for %s/%s", lk.table, lk.key)
			return tx.db.kind.Error(sqlir.LockNotAvailable, "canceling statement due to lock timeout", relname(lk.table), "", "")
		}
		tx.p.blockOnRow(rowWait{key: lk, mode: mode, tx: tx}, conflict[0])
	}
}

// rowWaitCycle reports whether one of the transactions tx would wait for is,
// through row lock waits only, waiting for tx: a deadlock Postgres detects.
func (tx *Tx) rowWaitCycle(conflict []*Tx) bool {
	seen := map[*Tx]bool{}
	stack := append([]*Tx(nil), conflict...)
	for len(stack) > 0 {
		w := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if w == tx {
			return true
		}
		if seen[w] || w.p == nil || w.p.state != stateBlockedLock || w.p.waitRow == nil {
			continue
		}
		seen[w] = true
		wr := w.p.waitRow
		stack = append(stack, wr.tx.conflicting(wr.key, wr.mode)...)
	}
	return false
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
	if tx.p == nil {
		return
	}
	for _, p := range tx.p.r.procs {
		if p.state == stateBlockedLock && p.waitRow != nil && released[p.waitRow.key] {
			p.state = stateReady
			p.waitRow = nil
		}
	}
}

// heldByOther reports whether another transaction holds lk in a mode that
// conflicts with mode, for NOWAIT and SKIP LOCKED.
func (tx *Tx) heldByOther(lk lockKey, mode lockMode) bool {
	return len(tx.conflicting(lk, mode)) > 0
}
