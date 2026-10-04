package mysqlerr_test

import (
	"errors"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"

	"github.com/k1LoW/detest"
	"github.com/k1LoW/detest/mysql"
	"github.com/k1LoW/detest/mysql/mysqlerr"
)

type user struct {
	ID    int64 `gorm:"primaryKey;autoIncrement"`
	Email string
}

// Production code that branches on *mysql.MySQLError, or on the errors GORM
// translates it into, takes the same branch on detest's store; GORM reads
// the generated ids from LastInsertId.
func TestConvert(t *testing.T) {
	detest.Explore(t, func(t *testing.T, s *detest.Sim) {
		sqlDB, _ := s.DB("app", mysql.New(mysql.Errors(mysqlerr.Convert), mysql.Collation("utf8mb4_bin")))
		if _, err := sqlDB.Exec("CREATE TABLE users (id BIGINT AUTO_INCREMENT PRIMARY KEY, email VARCHAR(100) NOT NULL, UNIQUE KEY uk_email (email))"); err != nil {
			t.Fatal(err)
		}
		if _, err := sqlDB.Exec("INSERT INTO users (email) VALUES ('a@x')"); err != nil {
			t.Fatal(err)
		}
		_, err := sqlDB.Exec("INSERT INTO users (email) VALUES ('a@x')")
		var myErr *gomysql.MySQLError
		if !errors.As(err, &myErr) || myErr.Number != 1062 || string(myErr.SQLState[:]) != "23000" {
			t.Errorf("duplicate entry: %#v", err)
		}

		gdb, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}),
			&gorm.Config{DisableAutomaticPing: true, Logger: glogger.Discard, TranslateError: true})
		if err != nil {
			t.Fatal(err)
		}
		// The failed insert above used up id 2, as it does in MySQL.
		u := user{Email: "b@x"}
		if err := gdb.Create(&u).Error; err != nil || u.ID != 3 {
			t.Errorf("GORM create: id %d, %v", u.ID, err)
		}
		if err := gdb.Create(&user{Email: "a@x"}).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
			t.Errorf("GORM duplicate: %v", err)
		}
		var got user
		if err := gdb.Where("email = ?", "b@x").First(&got).Error; err != nil || got.ID != 3 {
			t.Errorf("GORM first: %+v, %v", got, err)
		}
	})
}
