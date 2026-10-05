package detest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"

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
	pdb := &DB{name: "probe", kind: kind}
	pdb.reset()
	probe := pdb.newTx(nil)
	probe.atomic, probe.block, probe.checking = true, true, true
	args := make([]driver.Value, 65536) // more than any statement binds
	_, err = s.exec(probe, args)
	return err
}

type sqlConnector struct{ db *DB }

func (c *sqlConnector) Connect(context.Context) (driver.Conn, error) {
	return &sqlConn{db: c.db}, nil
}
func (c *sqlConnector) Driver() driver.Driver { return sqlDriver{} }

type sqlDriver struct{}

func (sqlDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("detest: use Sim.DB")
}

// sqlConn is a connection. It holds the open detest transaction, if any.
type sqlConn struct {
	db *DB
	tx *Tx
	// lastInsertID is MySQL's LAST_INSERT_ID() of the connection: the first
	// AUTO_INCREMENT value its latest insert that generated one generated.
	lastInsertID int64
	// lockTimeout is a lock timeout set for the session (SET lock_timeout,
	// SET innodb_lock_wait_timeout), which every later transaction of the
	// connection starts with.
	lockTimeout bool
	// noAutoZero is the session's NO_AUTO_VALUE_ON_ZERO, and noFKChecks its
	// FOREIGN_KEY_CHECKS=0.
	noAutoZero, noFKChecks bool
	// lastRun is the run that used the connection last, whose session state
	// lastInsertID and lockTimeout are.
	lastRun *run
}

func (c *sqlConn) Prepare(query string) (driver.Stmt, error) {
	return &sqlStmt{c: c, query: query}, nil
}
func (c *sqlConn) Close() error { return nil }
func (c *sqlConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *sqlConn) Ping(context.Context) error {
	return nil
}

func (c *sqlConn) BeginTx(ctx context.Context, opts driver.TxOptions) (_ driver.Tx, err error) {
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
	p := c.current()
	tx := c.db.newTx(p)
	// The session's settings hold for a transaction it begins, inside a
	// process or not.
	tx.block, tx.iso, tx.lockTimeout, tx.noAutoZero, tx.noFKChecks = true, iso, c.lockTimeout, c.noAutoZero, c.noFKChecks
	if p == nil {
		tx.atomic = true
		c.tx = tx
		return &sqlTx{c: c}, nil
	}
	p.txs = append(p.txs, tx)
	c.tx = tx
	p.yieldf("%s: begin", c.db.name)
	return &sqlTx{c: c}, nil
}

// ExecContext and QueryContext return database errors as the server's Errors
// option converts them, the type the production code's driver returns.
func (c *sqlConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (_ driver.Result, err error) {
	defer recoverRunOver(&err)
	rows, affected, err := c.run(query, args)
	if err != nil {
		return nil, c.db.kind.Convert(err)
	}
	if c.db.kind.InnoDB() {
		return innodbResult{affected: affected, lastID: rows.lastID}, nil
	}
	return driver.RowsAffected(affected), nil
}

type sqlTx struct{ c *sqlConn }

func (t *sqlTx) Commit() (err error) {
	defer recoverRunOver(&err)
	tx := t.c.tx
	t.c.tx = nil
	if tx == nil {
		return nil
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
	}
	return nil
}

func (t *sqlTx) Rollback() (err error) {
	defer recoverRunOver(&err)
	tx := t.c.tx
	t.c.tx = nil
	if tx == nil {
		return nil
	}
	if tx.p != nil {
		defer tx.p.forgetTxUnlessOver(tx)
		tx.p.yieldf("%s: rollback", tx.db.name)
	}
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
	defer recoverRunOver(&err)
	rows, _, err := c.run(query, args)
	if err != nil {
		return nil, c.db.kind.Convert(err)
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
	return tx, true
}

func (c *sqlConn) run(query string, named []driver.NamedValue) (*sqlRows, int64, error) {
	rows, affected, err := c.runQuery(query, named)
	if u, ok := errors.AsType[*sqlir.ErrUnsupportedSQL](err); ok {
		c.db.s.refuse(u)
	}
	return rows, affected, err
}

func (c *sqlConn) runQuery(query string, named []driver.NamedValue) (*sqlRows, int64, error) {
	args := make([]driver.Value, len(named))
	for i, nv := range named {
		args[i] = nv.Value
	}
	if p := c.current(); p != nil {
		p.syncOutside()
	}
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
			r, n, err := c.exec(&parsedStatement{query: query, stmt: st}, args)
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
	return c.exec(stmt, args)
}

// exec runs one parsed statement on the connection.
func (c *sqlConn) exec(stmt *parsedStatement, args []driver.Value) (*sqlRows, int64, error) {
	query := stmt.query
	tx, auto := c.statementTx()
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
	if set, ok := stmt.stmt.(*sqlir.SetStmt); ok && err == nil && (set.Name == "lock_timeout" || set.Name == "all") && !set.Local {
		// A session setting outlives the statement's transaction. MySQL's
		// takes effect at once; Postgres's, run in a transaction, only
		// when the transaction commits.
		if c.db.kind.InnoDB() || auto {
			c.lockTimeout = tx.lockTimeout
		} else {
			v := tx.lockTimeout
			tx.pendingLockTimeout = &v
		}
	}
	if set, ok := stmt.stmt.(*sqlir.SetStmt); ok && err == nil {
		switch set.Name {
		case "no_auto_value_on_zero":
			c.noAutoZero = tx.noAutoZero
		case "foreign_key_checks":
			c.noFKChecks = tx.noFKChecks
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
func (r *sqlRows) Close() error { return nil }
func (r *sqlRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}
