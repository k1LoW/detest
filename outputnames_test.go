package detest

import (
	"reflect"
	"testing"

	"github.com/k1LoW/detest/db/postgres"
)

// A select list item without an alias is named as Postgres names it, which
// the client reads it under and ORDER BY finds it by.
func TestPostgresOutputColumnNames(t *testing.T) {
	s := newSim(t)
	db, _ := s.DB("app", postgres.New())
	mustExec(t, db, `CREATE TABLE q (id int PRIMARY KEY, n int)`)
	mustExec(t, db, `CREATE TABLE r (k int PRIMARY KEY)`)
	rows, err := db.Query(`SELECT id::text, 'a'::text, CASE WHEN true THEN 'x' ELSE 'y' END, CASE WHEN true THEN 1 ELSE n END,
		coalesce(n, 0), id + 0, current_date, (SELECT 1 AS one), EXISTS (SELECT 1), 1::int, nullif(n, 0), greatest(n, 1),
		(SELECT id FROM q LIMIT 1), (SELECT id::text FROM q LIMIT 1), (SELECT 1 + 1), (SELECT id FROM q UNION SELECT 2 LIMIT 1),
		(VALUES (1)), ((SELECT id FROM q LIMIT 1))::text FROM q`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	// As Postgres 18 names them.
	want := []string{"id", "text", "case", "n", "coalesce", "?column?", "current_date", "one", "exists", "int4", "nullif", "greatest",
		"id", "id", "?column?", "id", "column1", "id"}
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("got %q, want %q", cols, want)
	}
}
