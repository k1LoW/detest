package pgxerr_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"

	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/postgres"
	"github.com/k1LoW/detest/postgres/pgxerr"
)

type user struct {
	ID    string
	Email string
}

// Production code that branches on *pgconn.PgError, or on the errors GORM
// translates it into, takes the same branch on detest's store.
func TestConvert(t *testing.T) {
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		sqlDB, _ := s.DB("app", postgres.New(postgres.Errors(pgxerr.Convert)))
		if _, err := sqlDB.Exec(`CREATE TABLE users (id text PRIMARY KEY, email text NOT NULL, CONSTRAINT users_email_key UNIQUE (email))`); err != nil {
			t.Fatal(err)
		}
		if _, err := sqlDB.Exec(`INSERT INTO users (id, email) VALUES ('u1', 'a@x')`); err != nil {
			t.Fatal(err)
		}

		_, err := sqlDB.Exec(`INSERT INTO users (id, email) VALUES ('u2', 'a@x')`)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "users_email_key" || pgErr.TableName != "users" {
			t.Errorf("unique violation: %#v", err)
		}
		if _, err := sqlDB.Exec(`INSERT INTO users (email) VALUES ('b@x')`); !errors.As(err, &pgErr) || pgErr.Code != "23502" || pgErr.ColumnName != "id" {
			t.Errorf("not-null violation: %#v", err)
		}

		gdb, err := gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: sqlDB}), &gorm.Config{DisableAutomaticPing: true, Logger: glogger.Discard, TranslateError: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := gdb.Create(&user{ID: "u3", Email: "a@x"}).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
			t.Errorf("GORM: %v", err)
		}
		if _, err := sqlDB.Exec(`CREATE TABLE posts (id text PRIMARY KEY, user_id text REFERENCES users (id))`); err != nil {
			t.Fatal(err)
		}
		if err := gdb.Exec(`INSERT INTO posts (id, user_id) VALUES ('p1', 'nobody')`).Error; !errors.Is(err, gorm.ErrForeignKeyViolated) {
			t.Errorf("GORM foreign key: %v", err)
		}
	})
}
