package mysql_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/mysql/mysqlerr"
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	myOnce  sync.Once
	myCtr   *tcmysql.MySQLContainer
	myAdmin *sql.DB
	myCfg   *gomysql.Config
	myErr   error
	myDBs   atomic.Int64
)

func start() error {
	myOnce.Do(func() {
		ctx := context.Background()
		// The module waits 60 seconds for the server, which a machine busy
		// with other test runs can take longer than to start it.
		// testcontainers' own reaper container gets a fixed 60 seconds to
		// start, so a start that fails is tried again.
		for attempt := range 3 {
			myCtr, myErr = tcmysql.Run(ctx, "mysql:8.4", tcmysql.WithUsername("root"), tcmysql.WithPassword("detest"),
				testcontainers.WithWaitStrategyAndDeadline(10*time.Minute, wait.ForLog("port: 3306  MySQL Community Server"), wait.ForListeningPort("3306/tcp")))
			if myErr == nil {
				break
			}
			if myCtr != nil {
				_ = myCtr.Terminate(ctx)
			}
			myErr = fmt.Errorf("start mysql (attempt %d): %w", attempt+1, myErr)
		}
		if myErr != nil {
			return
		}
		var dsn string
		if dsn, myErr = myCtr.ConnectionString(ctx, "parseTime=true"); myErr != nil {
			return
		}
		if myCfg, myErr = gomysql.ParseDSN(dsn); myErr != nil {
			return
		}
		myAdmin, myErr = sql.Open("mysql", dsn)
	})
	return myErr
}

func TestMain(m *testing.M) {
	m.Run()
	if myAdmin != nil {
		_ = myAdmin.Close()
	}
	if myCtr != nil {
		_ = myCtr.Terminate(context.Background())
	}
}

type backend struct{}

// Server converts detest's errors into go-sql-driver's, so that both sides
// report MySQL's error numbers, which tell apart errors that share a
// SQLSTATE, such as a duplicate key and a missing parent row (23000).
func (backend) Server() detest.Server { return mysql.New(mysql.Errors(mysqlerr.Convert)) }

func (backend) Open(t *testing.T) *sql.DB {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	if err := start(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("case_%d", myDBs.Add(1))
	if _, err := myAdmin.ExecContext(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg := myCfg.Clone()
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (backend) SessionID(ctx context.Context, conn *sql.Conn) (int64, error) {
	var id int64
	err := conn.QueryRowContext(ctx, `SELECT CONNECTION_ID()`).Scan(&id)
	return id, err
}

// Blockers reads InnoDB's lock waits from performance_schema and maps them to
// the connection ids CONNECTION_ID() returns. The blocking transaction's
// thread is taken from its table lock, which it always takes itself: the
// blocking thread a wait reports names the waiter for an insert's lock,
// which the waiter converts from an implicit one.
func (backend) Blockers(ctx context.Context, db *sql.DB, sessions []int64) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	for _, id := range sessions {
		out[id] = nil
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT rt.PROCESSLIST_ID, bt.PROCESSLIST_ID
		FROM performance_schema.data_lock_waits w
		JOIN performance_schema.threads rt ON rt.THREAD_ID = w.REQUESTING_THREAD_ID
		JOIN performance_schema.data_locks bl ON bl.ENGINE_TRANSACTION_ID = w.BLOCKING_ENGINE_TRANSACTION_ID AND bl.LOCK_TYPE = 'TABLE'
		JOIN performance_schema.threads bt ON bt.THREAD_ID = bl.THREAD_ID`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var waiting, blocking int64
		if err := rows.Scan(&waiting, &blocking); err != nil {
			return nil, err
		}
		if _, ok := out[waiting]; ok && !slices.Contains(out[waiting], blocking) {
			out[waiting] = append(out[waiting], blocking)
		}
	}
	return out, rows.Err()
}

func (backend) Outcome(err error) (string, bool) {
	if e, ok := errors.AsType[*gomysql.MySQLError](err); ok {
		return fmt.Sprintf("ERROR %d", e.Number), true
	}
	return "", false
}
