package detest

import (
	"strings"
	"sync"
	"testing"

	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"

	"github.com/k1LoW/detest/db/postgres"
	"github.com/k1LoW/detest/db/seam/gormpool"
)

// A goroutine that commits a GORM transaction its process began stops the
// exploration through gormpool too, as on the *sql.DB: where it commits is
// seen by every other process, and only the process that began the
// transaction is explored committing it.
func TestSeamGoroutineCommitStops(t *testing.T) {
	res, _ := exploreBubble(t, func(t *testing.T, s *Sim) {
		db, _ := s.DB("app", postgres.New())
		gdb, err := gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: gormpool.New(db)}),
			&gorm.Config{DisableAutomaticPing: true, Logger: glogger.Discard})
		if err != nil {
			t.Fatal(err)
		}
		s.Manual("pod", 1, func(p *Proc) error {
			tx := gdb.WithContext(p.Context()).Begin()
			defer tx.Rollback()
			var wg sync.WaitGroup
			wg.Go(func() { tx.Commit() })
			wg.Wait()
			return nil
		})
	}, nil, nil, 0)
	if res.Fatal == nil || !strings.Contains(res.Fatal.Error(), "ran COMMIT on it") {
		t.Fatalf("want the exploration stopped for a goroutine's commit, got:\n%s", res.report())
	}
}
