package detest

import "sync"

// seamTx is the seam.Tx of a transaction a package under db/seam began (see
// internal/seam).
type seamTx struct{ tx *Tx }

// Enter adopts the calling goroutine and waits for the transaction's
// connection as a hand-written transaction's operation does, but gives the
// engine mutex back before it returns, as the caller goes on into
// database/sql and the driver, which takes the mutex itself.
func (h seamTx) Enter() (release func(), err error) {
	tx := h.tx
	s := tx.db.s
	defer s.leave()
	var caller *Proc
	defer func() { absorbAbort(recover(), caller, &err) }()
	caller = tx.caller(func() string { return canonical(tx.db.name, "use the transaction") })
	if caller == nil {
		return func() {}, nil
	}
	if caller.stale() || tx.p.r.over() {
		return func() {}, errRunOver
	}
	exit := s.enter(caller)
	defer exit()
	tx.takeConn(caller)
	return func() { tx.giveConnOutside(caller) }, nil
}

// giveConnOutside is giveConn for a goroutine that may run alongside the
// scheduler, as one does once database/sql's call returned, or one of
// database/sql's own closing rows. It touches only the connection, under the
// engine mutex, and leaves waking the processes waiting for it to the
// scheduler, which changes their state without the mutex.
func (tx *Tx) giveConnOutside(p *Proc) {
	defer tx.db.s.enterAny()()
	if tx.dropConn(p) {
		tx.p.r.postCancel(txCancel{tx: tx, conn: true})
	}
}

// seamCaller returns the process that holds the transaction's connection
// through seamTx.Enter when it is the calling goroutine, nil otherwise.
func (tx *Tx) seamCaller() *Proc {
	if tx.p == nil || tx.atomic {
		return nil
	}
	defer tx.db.s.enterAny()()
	if by := tx.conn.by; by != nil && by.isCaller() {
		return by
	}
	return nil
}

// holdConn keeps the connection p holds through seamTx.Enter until the
// returned function is called. A query's rows are read after database/sql's
// call returns, with the transaction's lock held, and a sibling that entered
// database/sql meanwhile and parked in the driver would block that read on a
// sync.Mutex.
func (tx *Tx) holdConn(p *Proc) func() {
	exit := tx.db.s.enterAny()
	if tx.conn.by == p {
		tx.conn.depth++
	}
	exit()
	return sync.OnceFunc(func() { tx.giveConnOutside(p) })
}
