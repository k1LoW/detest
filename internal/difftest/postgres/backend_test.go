package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/db/postgres"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	pgOnce  sync.Once
	pgCtr   *tcpostgres.PostgresContainer
	pgAdmin *sql.DB
	pgURL   *url.URL
	pgErr   error
	pgDBs   atomic.Int64
)

func start() error {
	pgOnce.Do(func() {
		ctx := context.Background()
		// BasicWaitStrategies waits 60 seconds for the server, which a
		// machine busy with other test runs can take longer than to start it.
		// testcontainers' own reaper container gets a fixed 60 seconds to
		// start, so a start that fails is tried again.
		for attempt := range 3 {
			// The Debian image, as applications run Postgres on glibc rather
			// than on Alpine's musl, with the C collation detest gives by
			// default. Its own default, en_US.utf8, orders text otherwise.
			// LC_CTYPE stays en_US.UTF-8 so that lower and upper map
			// characters beyond ASCII, and the encoding stays UTF8, which
			// --lc-collate=C alone would turn into SQL_ASCII.
			pgCtr, pgErr = tcpostgres.Run(ctx, "postgres:18",
				testcontainers.WithEnv(map[string]string{"POSTGRES_INITDB_ARGS": "--encoding=UTF8 --lc-collate=C --lc-ctype=en_US.UTF-8"}),
				testcontainers.WithWaitStrategyAndDeadline(10*time.Minute,
					wait.ForLog("database system is ready to accept connections").WithOccurrence(2), wait.ForListeningPort("5432/tcp")))
			if pgErr == nil {
				break
			}
			if pgCtr != nil {
				_ = pgCtr.Terminate(ctx)
			}
			pgErr = fmt.Errorf("start postgres (attempt %d): %w", attempt+1, pgErr)
		}
		if pgErr != nil {
			return
		}
		var dsn string
		if dsn, pgErr = pgCtr.ConnectionString(ctx, "sslmode=disable"); pgErr != nil {
			return
		}
		if pgURL, pgErr = url.Parse(dsn); pgErr != nil {
			return
		}
		if pgAdmin, pgErr = sql.Open("pgx", dsn); pgErr != nil {
			return
		}
		pgErr = checkCollation(ctx, pgAdmin)
	})
	return pgErr
}

// checkCollation fails when the server orders text other than as C does,
// which detest's default collation is, so that a change of image or locale
// does not make every case compare against another order.
func checkCollation(ctx context.Context, db *sql.DB) error {
	var collate string
	var upperFirst bool
	if err := db.QueryRowContext(ctx, `SELECT datcollate, 'B' < 'a' FROM pg_database WHERE datname = current_database()`).Scan(&collate, &upperFirst); err != nil {
		return err
	}
	if collate != "C" || !upperFirst {
		return fmt.Errorf("the server's collation is %q, which does not order text as C does", collate)
	}
	return nil
}

func TestMain(m *testing.M) {
	m.Run()
	if pgAdmin != nil {
		_ = pgAdmin.Close()
	}
	if pgCtr != nil {
		_ = pgCtr.Terminate(context.Background())
	}
}

type backend struct{}

func (backend) Server() detest.Server { return postgres.New() }

func (backend) Open(t *testing.T) *sql.DB {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	if err := start(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("case_%d", pgDBs.Add(1))
	if _, err := pgAdmin.ExecContext(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u := *pgURL
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (backend) SessionID(ctx context.Context, conn *sql.Conn) (int64, error) {
	var pid int64
	err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	return pid, err
}

func (backend) Blockers(ctx context.Context, db *sql.DB, sessions []int64) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	for _, id := range sessions {
		rows, err := db.QueryContext(ctx, `SELECT unnest(pg_blocking_pids($1))`, id)
		if err != nil {
			return nil, err
		}
		out[id] = nil
		for rows.Next() {
			var pid int64
			if err := rows.Scan(&pid); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[id] = append(out[id], pid)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (backend) Outcome(err error) (string, bool) {
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return "COMMIT ROLLED BACK", true
	}
	if e, ok := errors.AsType[*pgconn.PgError](err); ok {
		return "ERROR " + e.Code, true
	}
	return "", false
}
