package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/mysql"
	"github.com/k1LoW/detest/db/postgres"
)

// A write whose target is a view is refused on both servers, however the
// view is named, and leaves the table under it as it was. A view read by a
// write is a read and still runs.
func TestWriteThroughViewRefused(t *testing.T) {
	for _, tt := range []struct {
		name    string
		server  Server
		refused []string
		reads   []string
	}{
		{
			name:   "postgres",
			server: postgres.New(),
			refused: []string{
				`UPDATE tv SET n = 5`,
				`UPDATE tv AS x SET n = 5 WHERE x.id = 1`,
				`UPDATE public.tv SET n = 5`,
				`UPDATE TV SET n = 5`,
				`UPDATE "tv" SET n = 5`,
				`UPDATE tv SET n = 5 FROM t WHERE tv.id = t.id`,
				`INSERT INTO tv VALUES (3, 'c', 3)`,
				`INSERT INTO Public.Tv AS x (id, s, n) VALUES (3, 'c', 3) ON CONFLICT (id) DO NOTHING`,
				`INSERT INTO tv SELECT id + 10, s, n FROM t`,
				`DELETE FROM tv WHERE id = 1`,
				`DELETE FROM tv AS x USING t WHERE x.id = t.id`,
				`DELETE FROM PUBLIC.TV`,
				`WITH c AS (SELECT 1 AS id) UPDATE tv SET n = 5 FROM c WHERE tv.id = c.id`,
			},
			reads: []string{
				`UPDATE t SET n = 2 FROM tv WHERE t.id = tv.id`,
				`UPDATE t SET n = (SELECT max(n) FROM tv)`,
				`DELETE FROM t USING tv WHERE t.id = tv.id AND tv.id = 9`,
				`INSERT INTO t SELECT id + 10, s, n FROM tv WHERE false`,
			},
		},
		{
			name:   "mysql",
			server: mysql.New(),
			refused: []string{
				`UPDATE tv SET n = 5`,
				`UPDATE tv x SET n = 5 WHERE x.id = 1`,
				`UPDATE app.tv SET n = 5`,
				"UPDATE `tv` SET n = 5",
				`UPDATE tv JOIN t u ON u.id = tv.id SET tv.n = 5`,
				`INSERT INTO tv VALUES (3, 'c', 3)`,
				`INSERT INTO app.tv (id, s, n) VALUES (3, 'c', 3) ON DUPLICATE KEY UPDATE n = 5`,
				`INSERT IGNORE INTO tv VALUES (1, 'c', 3)`,
				`REPLACE INTO tv VALUES (1, 'c', 3)`,
				`DELETE FROM tv WHERE id = 1`,
				`DELETE FROM app.tv`,
			},
			reads: []string{
				`UPDATE t JOIN tv ON tv.id = t.id SET t.n = 2`,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSim(t)
			db, _ := s.DB("app", tt.server)
			mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, s text, n int)`)
			mustExec(t, db, `CREATE VIEW tv AS SELECT * FROM t`)
			mustExec(t, db, `INSERT INTO t VALUES (1, 'a', 2)`)
			for _, q := range tt.refused {
				if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
					t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
				}
			}
			if got, want := rowsOf(t, db, `SELECT n FROM t ORDER BY id`), []string{"2"}; !reflect.DeepEqual(got, want) {
				t.Errorf("after the refused writes: got %v, want %v", got, want)
			}
			for _, q := range tt.reads {
				mustExec(t, db, q)
			}
		})
	}
}
