package detest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
	"github.com/k1LoW/detest/internal/sqlir"
)

// A function that returns another value each time is no bound of a locked
// range, as the WHERE tests another value.
func TestVolatileIsNoBound(t *testing.T) {
	for e, want := range map[sqlir.Expr]bool{
		&sqlir.FuncCall{Name: "gen_random_uuid"}:                                                     true,
		&sqlir.BinaryExpr{Op: "||", L: &sqlir.Const{Value: "a"}, R: &sqlir.FuncCall{Name: "random"}}: true,
		&sqlir.FuncCall{Name: "now"}:                                                                 false,
		&sqlir.Const{Value: 1}:                                                                       false,
	} {
		if got := volatile(e); got != want {
			t.Errorf("volatile(%#v) = %v, want %v", e, got, want)
		}
	}
}

// Postgres reads a CTE's rows on demand, so one that locks rows or has
// effects runs only as far as its readers ask, where detest runs it in
// full. Such a CTE is accepted only where every row of it is read.
func TestPartlyReadCTEWithEffects(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE SEQUENCE sq`)
	mustExec(t, db, `CREATE TABLE u (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `INSERT INTO u VALUES (1, 10), (2, 20)`)
	mustExec(t, db, `CREATE TABLE e (id int PRIMARY KEY)`)
	mustExec(t, db, `CREATE VIEW lv AS WITH d AS (SELECT id FROM u FOR UPDATE) SELECT id FROM d LIMIT 1`)
	mustExec(t, db, `CREATE VIEW sv AS SELECT nextval('sq') AS v FROM u`)
	mustExec(t, db, `CREATE TABLE w (id int PRIMARY KEY, n bigint)`)
	mustExec(t, db, `INSERT INTO w VALUES (1, NULL)`)
	for _, q := range []string{
		`WITH c AS (SELECT nextval('sq') AS v FROM u) SELECT coalesce(NULL::bigint, (SELECT v FROM c LIMIT 1))`,
		`WITH c AS (SELECT nextval('sq') AS v FROM u) SELECT (SELECT v FROM c LIMIT 1)`,
		`WITH c AS (SELECT nextval('sq') AS v FROM u) SELECT v FROM c LIMIT 1`,
		`WITH c AS (SELECT nextval('sq') AS v FROM u) SELECT EXISTS (SELECT v FROM c)`,
		`WITH c AS (SELECT nextval('sq') AS v FROM u) SELECT c.v FROM u JOIN c ON true`,
		`WITH c AS (SELECT nextval('sq') AS v FROM u), d AS (SELECT v FROM c LIMIT 1) SELECT v FROM d`,
		`WITH c AS (SELECT n FROM u FOR UPDATE) SELECT n FROM c LIMIT 1`,
		`SELECT (WITH c AS (SELECT n FROM u FOR UPDATE) SELECT count(*) FROM c)`,
		`WITH c AS (SELECT n FROM u FOR UPDATE) SELECT n FROM c UNION ALL SELECT 1 LIMIT 1`,
		// A reader may ask for no row, which Postgres then never runs.
		`WITH c AS (SELECT nextval('sq') AS v) SELECT coalesce(NULL::bigint, (SELECT v FROM c LIMIT 0))`,
		`WITH c AS (SELECT nextval('sq') AS v) SELECT coalesce(NULL::bigint, (SELECT v FROM c WHERE false))`,
		`WITH c AS (SELECT nextval('sq') AS v FROM u) SELECT v FROM c WHERE false`,
		`WITH c AS (SELECT nextval('sq') AS v FROM u) SELECT v FROM c WHERE 1 = 0 AND v > 0`,
		// Postgres skips the rest of a join when one side has no rows,
		// which side its planner's choice.
		`WITH c AS (SELECT nextval('sq') AS v) SELECT c.v FROM u JOIN c ON true`,
		// A condition on a query around the reader, or on a subquery,
		// reads none of the reader's rows.
		`WITH c AS (SELECT nextval('sq') AS v) SELECT coalesce(NULL::bigint, (SELECT v FROM c WHERE u.n = 1)) FROM u`,
		`WITH c AS (SELECT nextval('sq') AS v) SELECT coalesce(NULL::bigint, (SELECT v FROM c WHERE (SELECT count(id) FROM e) > 0))`,
		`WITH c AS (SELECT nextval('sq') AS v FROM u) SELECT v FROM c WHERE (SELECT count(id) FROM e) > 0`,
		// A derived table or a set operation's arm is read as far as the
		// query around it asks.
		`WITH c AS (SELECT nextval('sq') AS v) SELECT 1 UNION ALL SELECT v FROM c LIMIT 1`,
		`WITH c AS (SELECT nextval('sq') AS v) SELECT * FROM e JOIN (SELECT v FROM c) s ON true`,
		`WITH c AS (SELECT nextval('sq') AS v) SELECT coalesce(NULL::bigint, (SELECT 1 UNION ALL SELECT v FROM c LIMIT 1))`,
		// Postgres reaches a subquery as the rows decide, and never the
		// select list of an EXISTS, however few rows the CTE has.
		`WITH c AS (SELECT nextval('sq') AS v) SELECT (SELECT v FROM c) + (SELECT v FROM c)`,
		`WITH c AS (SELECT nextval('sq') AS v) SELECT EXISTS (SELECT coalesce(NULL::bigint, (SELECT v FROM c)))`,
		// So does a view's, whose rows a statement may read in part.
		`SELECT id FROM lv`,
		// A write's CTE has an effect through the view it reads too.
		`WITH c AS (SELECT v FROM sv) UPDATE w SET n = coalesce(n, (SELECT v FROM c LIMIT 1))`,
		`WITH c AS (SELECT (SELECT id FROM lv) AS n) SELECT coalesce(NULL::int, (SELECT n FROM c))`,
	} {
		if _, err := db.Query(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
	q := `WITH c AS (SELECT nextval('sq') AS v) SELECT coalesce(NULL::bigint, (SELECT v FROM c LIMIT $1))`
	if _, err := db.Query(q, 0); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
	}
	for _, tt := range []struct {
		q    string
		want []string
	}{
		{`WITH c AS (SELECT nextval('sq') AS v FROM u) SELECT v FROM c ORDER BY v`, []string{"1", "2"}},
		{`WITH c AS (SELECT nextval('sq') AS v FROM u), d AS (SELECT v FROM c) SELECT count(*) FROM d`, []string{"2"}},
		{`WITH c AS (SELECT n FROM u FOR UPDATE) SELECT n FROM c ORDER BY n`, []string{"10", "20"}},
		{`WITH c AS (SELECT n FROM u FOR UPDATE) SELECT n FROM c WHERE n > 10`, []string{"20"}},
		{`SELECT nextval('sq')`, []string{"5"}},
	} {
		if got := rowsOf(t, db, tt.q); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.q, got, tt.want)
		}
	}
}
