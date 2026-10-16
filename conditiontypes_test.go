package detest

import (
	"errors"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

func TestNonBooleanConditionRefused(t *testing.T) {
	for _, rows := range []bool{false, true} {
		s := newSim(t)
		db, _ := s.DB("app", postgres.New())
		mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, s text, b bool, j jsonb)`)
		if rows {
			mustExec(t, db, `INSERT INTO t VALUES (1, 't', true, 'true')`)
		}
		for _, q := range []string{
			`SELECT 1 FROM t WHERE s`,
			`SELECT NOT s FROM t WHERE false`,
			`SELECT 1 FROM t WHERE id`,
			`SELECT 1 FROM t WHERE -id`,
			`SELECT 1 FROM t WHERE j`,
			`SELECT 1 FROM t WHERE b AND s`,
			`SELECT 1 FROM t WHERE s OR b`,
			`SELECT b AND (SELECT 't') FROM t`,
			`SELECT CASE WHEN s THEN 1 END FROM t`,
			`SELECT count(*) FROM t HAVING count(*)`,
			`SELECT 1 FROM t a JOIN t c ON a.s`,
			`SELECT 1 FROM (SELECT 't' AS v) x WHERE x.v`,
			`SELECT 1 FROM t WHERE coalesce(s, '')`,
			`SELECT 1 FROM t WHERE coalesce('t', NULL)`,
			`SELECT 1 FROM t WHERE CASE WHEN true THEN 't' END`,
			`SELECT 1 FROM t WHERE (SELECT s FROM t LIMIT 1)`,
			`SELECT 1 FROM t WHERE NULL::text`,
			`SELECT 1 FROM t WHERE s COLLATE "C"`,
			`SELECT 1 FROM t WHERE 'a' || 'b'`,
			`UPDATE t SET b = true WHERE s`,
			`DELETE FROM t WHERE s`,
			`INSERT INTO t VALUES (1, 't') ON CONFLICT (id) DO UPDATE SET s = 'x' WHERE t.s`,
		} {
			if _, err := db.Exec(q); !errors.As(err, new(*ErrUnsupportedSQL)) {
				t.Errorf("%s (rows %v): got %v, want ErrUnsupportedSQL", q, rows, err)
			}
		}
	}
}

func TestNonBooleanConditionOfADomain(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE DOMAIN flag AS bool`)
	mustExec(t, db, `CREATE DOMAIN label AS text`)
	mustExec(t, db, `CREATE TABLE t (id int PRIMARY KEY, f flag, l label)`)
	if _, err := db.Exec(`SELECT 1 FROM t WHERE f AND NOT f::flag`); err != nil {
		t.Errorf("a domain over boolean: got %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM t WHERE l`); !errors.As(err, new(*ErrUnsupportedSQL)) {
		t.Errorf("a domain over text: got %v, want ErrUnsupportedSQL", err)
	}
}

// CheckSQL has no schema, so a type it does not know may be a domain over
// boolean.
func TestCheckSQLNonBooleanCondition(t *testing.T) {
	if err := CheckSQL(postgres.New(), `SELECT id FROM t WHERE b::flag`); err != nil {
		t.Errorf("a cast to a type CheckSQL does not know: got %v", err)
	}
	for _, q := range []string{
		`SELECT 1 WHERE 1`,
		`SELECT 1 FROM t WHERE b::text`,
		`SELECT 1 FROM t WHERE NOT b::uuid`,
	} {
		if err := CheckSQL(postgres.New(), q); !errors.As(err, new(*ErrUnsupportedSQL)) {
			t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
		}
	}
}
