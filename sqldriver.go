package detest

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/k1LoW/detest/internal/seam"
	"github.com/k1LoW/detest/internal/sqlir"
)

// CheckSQL reports whether detest can execute a statement on a database of
// kind d, without executing it. Collect the SQL a service emits and run it
// through CheckSQL to measure coverage before modeling the service.
//
// The check runs against a database with no schema, so it reports the cases
// the statement decides on its own, and refuses a function or an operator
// detest does not evaluate, or a call with other arguments than the function
// takes, wherever it stands in a SELECT, INSERT, UPDATE or DELETE, where a
// run reaches only the expressions it evaluates. A case that depends on the
// schema, such as a write that needs a generated column detest cannot
// compute, or a comparison whose outcome depends on the column's type,
// passes here and fails with ErrUnsupportedSQL when the statement runs. So
// does an expression in a CHECK or a generated column, which loads with the
// schema and is refused by the write that evaluates it, and a default detest
// cannot evaluate, which a write that leaves the column out stores as the
// Unknown marker unless the table's own schema reads the column.
func CheckSQL(d Server, query string) error {
	kind := sqlir.ImplOf(d)
	if kind == nil {
		return errors.New("detest: CheckSQL needs a server, such as postgres.New()")
	}
	s, err := parseWith(kind.Parser(), query)
	if err != nil {
		return err
	}
	if err := checkStatic(s.stmt, query, kind.InnoDB()); err != nil {
		return err
	}
	stmts := []sqlir.Statement{s.stmt}
	if script, ok := s.stmt.(*sqlir.Script); ok {
		stmts = script.Stmts
	}
	args := make([]driver.Value, 65536) // more than any statement binds
	for _, stmt := range stmts {
		// Each statement of a script gets a probe of its own rather than
		// running after the one before it. A script stops at its first error,
		// and a probe that went on would abort with 25P02, which hides the
		// statements after the error as well.
		pdb := &DB{name: "probe", kind: kind}
		pdb.reset()
		probe := pdb.newTx(nil)
		probe.atomic, probe.block, probe.checking = true, true, true
		_, err := (&parsedStatement{query: query, stmt: stmt}).exec(probe, args)
		// An error of the server, such as 23505, is an outcome of the probe's
		// data rather than of what detest can run. Every parameter is NULL and
		// a table without a schema is keyed by id, so the rows of a multi-row
		// INSERT collide.
		if _, ok := errors.AsType[*DBError](err); ok {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type sqlConnector struct {
	db   *DB
	pool *sql.DB // the pool the connector serves, for PartialOrder
	id   int     // the pool's number among the simulation's
}

func (c *sqlConnector) Connect(context.Context) (driver.Conn, error) {
	return &sqlConn{db: c.db, pool: c.pool, poolID: c.id}, nil
}

// depConn records that the statement takes or gives back a connection of a
// capped pool, which another process may hold or wait for. An uncapped
// pool never makes a process wait, so its connections are no resource the
// processes share.
func (c *sqlConn) depConn() {
	r := c.db.s.run
	if r == nil || !r.depOn() || c.pool == nil || c.pool.Stats().MaxOpenConnections <= 0 {
		return
	}
	r.depRecord(fmt.Sprintf("conn:%s:%d", c.db.name, c.poolID), true)
}
func (c *sqlConnector) Driver() driver.Driver { return sqlDriver{} }

type sqlDriver struct{}

func (sqlDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("detest: use Sim.DB")
}

// sqlConn is a connection. It holds the open detest transaction, if any.
type sqlConn struct {
	db     *DB
	pool   *sql.DB
	poolID int
	tx     *Tx
	// lastInsertID is MySQL's LAST_INSERT_ID() of the connection: the first
	// AUTO_INCREMENT value its latest insert that generated one generated.
	lastInsertID int64
	// lockTimeout is a lock timeout set for the session (SET lock_timeout,
	// SET innodb_lock_wait_timeout), which every later transaction of the
	// connection starts with.
	lockTimeout bool
	// timeZone is Postgres's TimeZone set for the session (SET TIME ZONE),
	// nil for the server's, which every later transaction starts in.
	timeZone *time.Location
	// noAutoZero is the session's NO_AUTO_VALUE_ON_ZERO, and noFKChecks its
	// FOREIGN_KEY_CHECKS=0.
	noAutoZero, noFKChecks bool
	// lastRun is the run that used the connection last, whose session state
	// lastInsertID and lockTimeout are.
	lastRun *run
	// bad marks a connection whose statement was canceled by its context.
	// pgx and go-sql-driver close such a connection, so every later call on
	// it fails and the pool drops it.
	bad bool
}

// IsValid tells database/sql not to put a connection back in the pool once
// a canceled statement closed it.
func (c *sqlConn) IsValid() bool { return !c.bad }

func (c *sqlConn) Prepare(query string) (driver.Stmt, error) {
	if c.bad {
		return nil, driver.ErrBadConn
	}
	return &sqlStmt{c: c, query: query}, nil
}
func (c *sqlConn) Close() error { return nil }
func (c *sqlConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *sqlConn) Ping(context.Context) error {
	if c.bad {
		return driver.ErrBadConn
	}
	return nil
}

func (c *sqlConn) BeginTx(ctx context.Context, opts driver.TxOptions) (_ driver.Tx, err error) {
	defer c.db.s.leave()
	c.depConn()
	defer func() {
		// A begin cut short by the end of the run returns no Tx, so
		// database/sql never rolls it back: the connection must not keep it.
		if rec := recover(); rec != nil {
			if _, ok := rec.(abortSentinel); !ok {
				panic(rec)
			}
			c.tx, err = nil, errRunOver
		}
	}()
	// The caller is resolved before anything else, as a goroutine of an
	// ended run must not read the connection's state, nor s.run, which the
	// scheduler may be setting for the next run.
	if c.bad {
		return nil, driver.ErrBadConn
	}
	p := c.db.s.currentAs(func() string { return canonical(c.db.name, "begin", opts.Isolation, opts.ReadOnly) })
	if p.stale() {
		return nil, errRunOver
	}
	defer c.db.s.enter(p)()
	c.dropStaleTx()
	if c.tx != nil {
		return nil, fmt.Errorf("detest: nested transaction on one connection")
	}
	// Refuse a level detest does not implement for the kind rather than run the
	// transaction with other semantics than production's.
	iso, err := txIsolation(c.db.kind, sql.IsolationLevel(opts.Isolation))
	if err != nil {
		return nil, err
	}
	tx := c.db.newTx(p)
	// The session's settings hold for a transaction it begins, inside a
	// process or not.
	tx.block, tx.iso, tx.lockTimeout, tx.noAutoZero, tx.noFKChecks = true, iso, c.lockTimeout, c.noAutoZero, c.noFKChecks
	tx.timeZone = c.timeZone
	if slot := seam.SlotOf(ctx); slot != nil {
		slot.Hook = seamTx{tx: tx}
	}
	if p == nil {
		tx.atomic = true
		c.tx = tx
		return &sqlTx{c: c, ctx: ctx}, nil
	}
	p.txs = append(p.txs, tx)
	c.tx = tx
	p.yieldf("%s: begin", c.db.name)
	return &sqlTx{c: c, ctx: ctx}, nil
}

// ExecContext and QueryContext return database errors as the server's Errors
// option converts them, the type the production code's driver returns.
func (c *sqlConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (_ driver.Result, err error) {
	defer c.db.s.leave()
	c.depConn()
	defer c.depConn() // the connection goes back to the pool
	defer c.depStmtEnd()
	defer recoverRunOver(&err)
	rows, affected, err := c.run(ctx, query, args)
	if err != nil {
		return nil, c.db.kind.Convert(err)
	}
	if c.db.kind.InnoDB() {
		return innodbResult{affected: affected, lastID: rows.lastID}, nil
	}
	return driver.RowsAffected(affected), nil
}

type sqlTx struct {
	c *sqlConn
	// ctx is the context the transaction began with, which database/sql
	// rolls the transaction back on when it ends.
	ctx context.Context
}

func (t *sqlTx) Commit() (err error) {
	defer t.c.db.s.leave()
	defer t.c.depConn()
	defer recoverRunOver(&err)
	if t.c.bad {
		return driver.ErrBadConn
	}
	if tx := t.c.tx; tx != nil && tx.p != nil && !tx.p.r.over() && !tx.p.isCaller() {
		// A commit is seen by every other process, and a goroutine that shares
		// the transaction runs without yielding (see runShared), so where it
		// commits could not be explored. database/sql rolls nothing back after
		// a failed commit and puts the connection back in the pool, so the
		// transaction ends here, rather than stay on the connection for the
		// next statement to run in.
		t.c.tx = nil
		tx.p.r.postCancel(txCancel{tx: tx, why: "rolls back, as its commit by a goroutine sharing it was refused"})
		exit := t.c.db.s.enter(nil)
		tx.p.r.stopEnding("COMMIT")
		exit()
		err := unsupported("a transaction committed by another goroutine than the one that began it", "COMMIT")
		if u, ok := errors.AsType[*sqlir.ErrUnsupportedSQL](err); ok {
			t.c.db.s.refuse(u)
		}
		return err
	}
	// The caller is resolved first, so that a goroutine of an ended run is
	// turned away before it reads the connection.
	caller := t.c.current()
	if caller.stale() {
		return errRunOver
	}
	defer t.c.db.s.enter(caller)()
	tx := t.c.tx
	t.c.tx = nil
	if tx == nil {
		return nil
	}
	if tx.p != nil && tx.p.r.over() {
		// The run's databases are reset, or are a later run's, and a commit
		// after the end of its run never counts.
		return errRunOver
	}
	if tx.p != nil {
		// The process keeps the transaction until the commit is done, so a
		// crash at the commit's yield point rolls it back.
		defer tx.p.forgetTxUnlessOver(tx)
		if tx.aborted {
			tx.p.yieldf("%s: commit (aborted transaction rolls back)", tx.db.name)
			tx.rollback()
			return t.c.db.kind.Convert(tx.abortedError())
		}
		tx.p.yieldf("%s: commit", tx.db.name)
		if tx.closed {
			return errRunOver // the process crashed at the yield point
		}
	}
	if err := tx.checkCommit(); err != nil {
		tx.rollback()
		return t.c.db.kind.Convert(err)
	}
	tx.commit()
	if tx.pendingLockTimeout != nil {
		t.c.lockTimeout = *tx.pendingLockTimeout
		t.c.db.s.breakEager(tx.p, sessionSet)
	}
	if tx.pendingTimeZone != nil {
		t.c.timeZone = tx.pendingTimeZone.loc
		t.c.db.s.breakEager(tx.p, sessionSet)
	}
	return nil
}

func (t *sqlTx) Rollback() (err error) {
	defer t.c.db.s.leave()
	defer t.c.depConn()
	defer recoverRunOver(&err)
	if t.c.bad {
		return driver.ErrBadConn
	}
	// database/sql rolls back from a goroutine of its own when the context
	// ends, which Current would adopt, so a stale caller is only looked up.
	if t.c.db.s.staleCaller() {
		return errRunOver
	}
	tx := t.c.tx
	t.c.tx = nil
	if tx == nil {
		return nil
	}
	if tx.p.stale() {
		return errRunOver // as Commit
	}
	if tx.p != nil && !tx.p.r.over() && (t.ctx.Err() != nil || !tx.p.isCaller()) {
		// database/sql's own goroutine rolling back as the context ended, or
		// a goroutine that shares the transaction. It runs alongside the
		// process, which may be in the middle of a step, so the scheduler
		// rolls the transaction back at its next one. Nothing can use the
		// transaction in between, since database/sql has ended it. Once the
		// context ended, the process's own rollback races with database/sql's
		// for the transaction, so it takes this way too, rather than yield
		// in the runs where it wins.
		why := "rolls back, as its context ended"
		if t.ctx.Err() == nil {
			why = "rolls back, by a goroutine sharing it"
			exit := t.c.db.s.enter(nil)
			tx.p.r.stopEnding("ROLLBACK")
			exit()
		}
		tx.p.r.postCancel(txCancel{tx: tx, why: why})
		return nil
	}
	if tx.p == nil || tx.p.r.over() {
		// Once the run is over, database/sql's goroutines roll back the
		// transactions of processes still unwinding, which are no caller to
		// enter the engine as, and nothing they do counts.
		defer t.c.db.s.enter(nil)()
		tx.rollback()
		return nil
	}
	tx.p.comeBack()
	defer tx.p.forgetTxUnlessOver(tx)
	defer t.c.db.s.enter(tx.p)()
	tx.p.yieldf("%s: rollback", tx.db.name)
	tx.rollback()
	return nil
}

// forgetTxUnlessOver is forgetTx while the run lasts. Once it is over, the
// process unwinding and database/sql's cleanup of the transaction may both
// get here at once, and the list no longer matters.
func (p *Proc) forgetTxUnlessOver(tx *Tx) {
	if !p.r.over() {
		p.forgetTx(tx)
	}
}

func (p *Proc) forgetTx(tx *Tx) {
	for i, t := range p.txs {
		if t == tx {
			p.txs = append(p.txs[:i], p.txs[i+1:]...)
			return
		}
	}
}

func (c *sqlConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (_ driver.Rows, err error) {
	defer c.db.s.leave()
	c.depConn()
	defer c.depConn()
	defer c.depStmtEnd()
	defer recoverRunOver(&err)
	tx := c.tx
	rows, _, err := c.run(ctx, query, args)
	if err != nil {
		return nil, c.db.kind.Convert(err)
	}
	if tx != nil {
		if p := tx.seamCaller(); p != nil {
			rows.release = tx.holdConn(p)
		}
	}
	return rows, nil
}

// current returns the process issuing statements on this connection. Outside
// a run (seeding) or outside any process, statements run directly on the
// committed state without yielding.
// errRunOver is what a statement of a process returns once its run ended:
// the process is unwinding and nothing it does counts.
var errRunOver = errors.New("detest: the run ended")

// recoverRunOver turns the panic that unwinds a process of an ended run into
// an error, at the driver's entry points. database/sql releases some of its
// locks only on its error paths, not on a panic: a Tx's read lock taken for
// a query stays held, and the Tx's cleanup on context cancellation then
// waits for it forever.
func recoverRunOver(err *error) {
	if rec := recover(); rec != nil {
		if _, ok := rec.(abortSentinel); !ok {
			panic(rec)
		}
		*err = errRunOver
	}
}

func (c *sqlConn) current() *Proc { return c.db.s.Current() }

// statementTx returns the transaction a statement runs in: the open one, or an
// autocommit transaction committed right after the statement.
// dropStaleTx forgets a transaction of an ended run that the connection still
// holds, so that a pooled connection carries nothing into the next run.
func (c *sqlConn) dropStaleTx() {
	if c.tx != nil && c.tx.p != nil && c.tx.p.r.over() {
		c.tx = nil
	}
	// The session's state is the run's too: a pooled connection a later run
	// reuses must not bring a generated id or a lock timeout from another
	// schedule into it.
	// The run is the Sim's, so a seed, which runs before any process is
	// current, sees the change of run as well.
	if r := c.db.s.run; r != nil && r != c.lastRun {
		c.lastRun, c.lastInsertID, c.lockTimeout, c.noAutoZero, c.noFKChecks = r, 0, false, false, false
		c.timeZone = nil
	}
}

func (c *sqlConn) statementTx() (tx *Tx, auto bool) {
	c.dropStaleTx()
	if c.tx != nil {
		return c.tx, false
	}
	p := c.current()
	tx = c.db.newTx(p)
	tx.atomic, tx.lockTimeout, tx.noAutoZero, tx.noFKChecks = p == nil, c.lockTimeout, c.noAutoZero, c.noFKChecks
	tx.timeZone = c.timeZone
	return tx, true
}

func (c *sqlConn) run(ctx context.Context, query string, named []driver.NamedValue) (*sqlRows, int64, error) {
	rows, affected, err := c.runQuery(ctx, query, named)
	if u, ok := errors.AsType[*sqlir.ErrUnsupportedSQL](err); ok {
		c.db.s.refuse(u)
	}
	return rows, affected, err
}

func (c *sqlConn) runQuery(ctx context.Context, query string, named []driver.NamedValue) (*sqlRows, int64, error) {
	if c.bad {
		return nil, 0, driver.ErrBadConn
	}
	args := make([]driver.Value, len(named))
	for i, nv := range named {
		args[i] = nv.Value
		if b, ok := nv.Value.([]byte); ok {
			// database/sql hands the caller's slice over without a copy,
			// and a server has read it by the time the call returns, so a
			// caller reusing the buffer must not change what was written.
			args[i] = bytes.Clone(b)
		}
	}
	// A goroutine that came through a seam package (see seamTx) holds the
	// transaction's connection, so none of its siblings is in database/sql,
	// and it may park here as itself.
	if tx := c.tx; tx != nil && tx.p != nil && !tx.p.r.over() && !tx.p.isCaller() && tx.seamCaller() == nil {
		return c.runShared(ctx, tx, query, args)
	}
	// A statement whose context ended before it reached the driver, such as
	// one waiting for database/sql's lock of the connection meanwhile, is not
	// sent, as pgx and go-sql-driver check the context first. The connection
	// and its transaction stay as they were.
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	p := c.db.s.currentAs(func() string { return canonical(c.db.name, query, args) })
	if p != nil {
		if p.stale() {
			return nil, 0, errRunOver
		}
		p.syncOutside()
		p.drain()
	}
	defer c.db.s.enter(p)()
	// The context may have ended while the statement waited above.
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if p == nil {
		return c.parseExec(ctx, query, args)
	}
	p.stmtDone = ctx.Done()
	defer func() { p.stmtDone = nil }()
	return c.parseExec(ctx, query, args)
}

// access describes a statement that touched the database as a whole, which
// writes unless it is a plain read that succeeded.
func (c *sqlConn) access(gid, query string, err error) *sharedAccess {
	a := &sharedAccess{gid: gid, db: c.db, query: query, write: true}
	if stmt, perr := parseWith(c.db.kind.Parser(), query); perr == nil {
		if sel, ok := stmt.stmt.(*sqlir.SelectStmt); ok && err == nil {
			// A read alone commutes with other reads. One that locks or calls
			// a function such as nextval does not, and a failed statement may
			// have aborted the transaction.
			a.write = queryHasEffects(sel)
		}
	}
	return a
}

// runShared runs a statement of a goroutine on a transaction another process
// began, such as one of an errgroup sharing its process's *sql.Tx. database/sql
// holds the transaction's locks while the driver runs, so a goroutine parked
// here would block its siblings on a sync.Mutex, which synctest does not
// count as blocked, and the run would never settle. The statement therefore
// runs at once, without being adopted and without a yield, in the step of
// whichever process woke the goroutine, and fails as unsupported where it
// would have to wait for a lock. Its result is the server's, and only its
// interleaving with other processes is not explored, which the report
// states as a bound.
func (c *sqlConn) runShared(ctx context.Context, tx *Tx, query string, args []driver.Value) (*sqlRows, int64, error) {
	defer c.db.s.enter(nil)()
	if tx.p.r.over() {
		return nil, 0, errRunOver
	}
	// The process that began the transaction crashed or returned, which
	// drops its connection with the transaction. A goroutine detest did not
	// adopt outlives it, and must not reach the closed transaction, which
	// ROLLBACK TO SAVEPOINT, unlike other statements, does not check.
	if tx.closed {
		return nil, 0, driver.ErrBadConn
	}
	// A goroutine sharing a transaction whose context ended, such as by a
	// sibling canceling a context they share, would not send its statement,
	// as in runQuery. Whether it got here before or after the cancel is the
	// runtime's choice, and so is which siblings' statements ran in the
	// transaction, so the exploration stops. One that database/sql turns
	// away before the driver, having seen the context end first, never gets
	// here, which detest cannot tell.
	if err := ctx.Err(); err != nil {
		tx.p.r.stopCanceled(query)
		return nil, 0, err
	}
	tx.shared = query
	defer func() { tx.shared = "" }()
	c.db.s.countShared(tx.p.r)
	// Only a statement that pins its rows has keys worth telling apart, so
	// the rest skip copying what the transaction holds.
	var mark *sharedMark
	if stmt, perr := parseWith(c.db.kind.Parser(), query); perr == nil {
		if _, ok := pointTable(c.db, stmt.stmt); ok {
			m := tx.markShared()
			mark = &m
		}
	}
	rows, n, err := c.parseExec(ctx, query, args)
	a := c.access(goroutineID(), query, err)
	if mark != nil && err == nil {
		if keys := tx.sinceShared(*mark); len(keys) > 0 {
			a.keys = keys
		}
	}
	tx.p.r.recordShared(a)
	return rows, n, err
}

func (c *sqlConn) parseExec(ctx context.Context, query string, args []driver.Value) (*sqlRows, int64, error) {
	stmt, err := parseWith(c.db.kind.Parser(), query)
	if err != nil {
		if pe, ok := errors.AsType[*sqlir.ParseError](err); ok {
			err = c.db.kind.Error(sqlir.SyntaxError, pe.Err.Error(), "", "", "")
			if c.tx != nil && !c.db.kind.InnoDB() {
				c.tx.abort() // as any failed statement does in Postgres
			}
		}
		if c.db.s.sqlObserver != nil {
			c.db.s.sqlObserver(query, err)
		}
		return nil, 0, err
	}
	if script, ok := stmt.stmt.(*sqlir.Script); ok && c.db.kind.InnoDB() {
		// MySQL runs each statement of a multi-statement query as its own:
		// committed on its own under autocommit, rolled back alone when it
		// fails. A failing one stops the rest.
		var rows *sqlRows
		var affected, lastID int64
		for _, st := range script.Stmts {
			r, n, err := c.exec(ctx, &parsedStatement{query: query, stmt: st}, args)
			if err != nil {
				return nil, 0, err
			}
			// go-sql-driver's result reports the last statement's.
			affected, lastID = n, r.lastID
			if _, ok := st.(*sqlir.SelectStmt); ok {
				rows = r // the one result set, which the statements after it do not replace
			}
		}
		if rows == nil {
			rows = &sqlRows{}
		}
		rows.lastID = lastID
		return rows, affected, nil
	}
	return c.exec(ctx, stmt, args)
}

// exec runs one parsed statement on the connection.
func (c *sqlConn) exec(ctx context.Context, stmt *parsedStatement, args []driver.Value) (_ *sqlRows, _ int64, err error) {
	query := stmt.query
	tx, auto := c.statementTx()
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		sc, ok := rec.(stmtCanceled)
		if !ok {
			panic(rec)
		}
		err = c.cancel(sc.p, tx, query)
	}()
	tx.lastInsertID = &c.lastInsertID
	innodb := c.db.kind.InnoDB() && !auto
	var mark stmtMark
	if innodb {
		mark = tx.markStatement()
	}
	// A statement starts the transaction in InnoDB when it reads or locks
	// its data (selectNoYield, snapshotRows, lockWith); a refused one leaves
	// it as it was.
	started := tx.started
	res, err := stmt.exec(tx, args)
	if errors.As(err, new(*sqlir.ErrUnsupportedSQL)) {
		tx.started = started
	}
	if c.db.s.sqlObserver != nil {
		c.db.s.sqlObserver(query, err)
	}
	if err == nil && res.hasLastID {
		c.lastInsertID = res.lastID
	}
	if set, ok := stmt.stmt.(*sqlir.SetStmt); ok && err == nil && c.db.kind.InnoDB() && (set.Name == "lock_timeout" || set.Name == "all") && !set.Local {
		// MySQL session settings take effect immediately, even in a transaction.
		c.lockTimeout = tx.lockTimeout
		c.db.s.breakEager(tx.p, sessionSet)
	}
	if set, ok := stmt.stmt.(*sqlir.SetStmt); ok && err == nil {
		switch set.Name {
		case "no_auto_value_on_zero":
			c.noAutoZero = tx.noAutoZero
			c.db.s.breakEager(tx.p, sessionSet)
		case "foreign_key_checks":
			c.noFKChecks = tx.noFKChecks
			c.db.s.breakEager(tx.p, sessionSet)
		}
	}
	switch {
	case innodb && err != nil:
		deadlock := errors.Is(err, sqlir.ErrDeadlock)
		tx.failStatement(mark, deadlock)
		if deadlock {
			// MySQL has ended the transaction: later statements through the
			// same database/sql Tx run in autocommit, and its Commit or
			// Rollback has nothing left to end.
			c.tx, tx.closed = nil, true
			if tx.p != nil {
				tx.p.forgetTxUnlessOver(tx)
			}
		}
	case auto:
		if err == nil && !tx.aborted {
			err = tx.checkCommit()
		}
		if err != nil || tx.aborted {
			tx.rollback()
		} else {
			tx.commit()
			if tx.pendingLockTimeout != nil {
				c.lockTimeout = *tx.pendingLockTimeout
				c.db.s.breakEager(tx.p, sessionSet)
			}
			if tx.pendingTimeZone != nil {
				c.timeZone = tx.pendingTimeZone.loc
				c.db.s.breakEager(tx.p, sessionSet)
			}
		}
	case err != nil:
		// A failed statement aborts the Postgres transaction.
		tx.abort()
	}
	if err != nil {
		return nil, 0, err
	}
	return &sqlRows{cols: res.cols, rows: res.rows, lastID: res.lastID}, res.affected, nil
}

// cancel ends a statement of p parked in the driver whose context ended. It
// has to return, as database/sql holds the transaction's locks until it
// does, and its own goroutine waits for them to roll the transaction back.
// It is refused rather than given the context's error. pgx and go-sql-driver
// only close the connection, and the server goes on running the statement.
// A canceled autocommit UPDATE waiting for a row is applied once the row is
// free, on Postgres and on MySQL alike, and a MySQL transaction keeps its
// locks until then. When that happens depends on the lock wait and on when
// the server notices the closed connection, which detest does not model.
// The connection is dropped and its transaction rolled back by the
// scheduler at its next step, as p now runs alongside the process it
// resumed, and p is blocked outside detest until then.
func (c *sqlConn) cancel(p *Proc, tx *Tx, query string) error {
	c.bad = true
	if c.tx == tx {
		c.tx = nil
	}
	r := p.r
	p.away.Store(awayOutside)
	if !p.adopted {
		r.outside.Add(1) // as parkOutside
	}
	r.postCancel(txCancel{p: p, tx: tx})
	return unsupported("a statement whose context ended while it ran or waited in the database, which the server goes on running", query)
}

type sqlStmt struct {
	c     *sqlConn
	query string
}

func (s *sqlStmt) Close() error  { return nil }
func (s *sqlStmt) NumInput() int { return -1 }
func (s *sqlStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.c.ExecContext(context.Background(), s.query, named(args))
}
func (s *sqlStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.query, named(args))
}
func (s *sqlStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.c.ExecContext(ctx, s.query, args)
}
func (s *sqlStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.c.QueryContext(ctx, s.query, args)
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}

type sqlRows struct {
	cols []string
	rows [][]driver.Value
	i    int
	// lastID is the AUTO_INCREMENT value the statement generated first, 0
	// when it generated none.
	lastID int64
	// release gives back the connection a query through a seam package
	// holds while its rows are open (see Tx.holdConn).
	release func()
}

// innodbResult is the result of a statement on MySQL, which reports the
// AUTO_INCREMENT value an insert generated. Other servers answer
// LastInsertId with an error, as their drivers do.
type innodbResult struct{ affected, lastID int64 }

func (r innodbResult) LastInsertId() (int64, error) { return r.lastID, nil }
func (r innodbResult) RowsAffected() (int64, error) { return r.affected, nil }

func (r *sqlRows) Columns() []string {
	cols := make([]string, len(r.cols))
	for i, c := range r.cols {
		cols[i], _, _ = strings.Cut(c, "\x00") // see outputKeys
	}
	return cols
}
func (r *sqlRows) Close() error {
	if r.release != nil {
		r.release()
	}
	return nil
}
func (r *sqlRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

// sessionSet is what breakEager says of a session setting set on a
// connection.
const sessionSet = "set a session setting on its connection, which the pool hands to the process that takes it next"

// depStmtEnd forgets the row the statement pinned, once it ran.
func (c *sqlConn) depStmtEnd() {
	if c.tx != nil {
		c.tx.depPin = depPin{}
	}
}
