package detest

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/k1LoW/detest/mysql"
	"github.com/k1LoW/detest/postgres"
)

// CheckSQL runs against a database with no rows, so an expression over a
// table's rows is never evaluated. A function or an operator detest does not
// know is refused anyway, wherever it stands, since the exploration would
// refuse it on the first row.
func TestCheckSQLRefusesUnknownFunctionsAndOperators(t *testing.T) {
	refused := map[Server][]string{
		postgres.New(): {
			`SELECT date_trunc('day', created_at) FROM t`,
			`SELECT * FROM t WHERE trim(name) <> ''`,
			`SELECT * FROM t WHERE data->>'k' = 'v'`,
			`SELECT * FROM t WHERE name ~ '^x'`,
			`UPDATE t SET data = jsonb_set(data, '{a}', '1') WHERE id = 1`,
			`INSERT INTO t (id, d) VALUES (1, to_char(now(), 'YYYY'))`,
			`SELECT id FROM t ORDER BY extract(epoch FROM created_at)`,
			`SELECT * FROM t JOIN u ON u.k = split_part(t.k, '-', 1)`,
			`WITH c AS (SELECT strpos(name, 'x') AS p FROM t) SELECT p FROM c`,
			`SELECT * FROM t WHERE EXISTS (SELECT 1 FROM u WHERE age(u.at, t.at) > interval '1 day')`,
			`SELECT CASE WHEN a > 1 THEN mod(a, 2) ELSE 0 END FROM t`,
			`SELECT * FROM t WHERE 'x' = ANY(tags)`,
			`SELECT mysql_signed(id) FROM t`,
			`SELECT last_insert_id()`,
			`SELECT * FROM t WHERE a <=> b`,
			`SELECT lower(name, name) FROM t`,
			`SELECT * FROM t WHERE round(random(), 2) > 0.5`,
			`SELECT * FROM t WHERE EXISTS (SELECT 1 FROM unnest(t.tags) AS u(x) WHERE x = 'a')`,
			`SELECT * FROM t CROSS JOIN LATERAL generate_series(1) g`,
		},
		mysql.New(): {
			"SELECT * FROM t WHERE created_at > DATE_SUB(NOW(), INTERVAL 1 DAY)",
			"SELECT DATE(created_at) FROM t",
			"SELECT * FROM t WHERE JSON_EXTRACT(data, '$.a') = 1",
		},
	}
	accepted := map[Server][]string{
		postgres.New(): {
			`SELECT lower(name), length(name), coalesce(n, 0) FROM t WHERE created_at < now() - make_interval(secs => 10)`,
			`SELECT count(*), sum(n), row_number() OVER (PARTITION BY k ORDER BY id) FROM t GROUP BY k`,
			`SELECT * FROM t WHERE id = ANY(ARRAY[1, 2]) AND name LIKE 'x%' AND a BETWEEN 1 AND 2`,
			`UPDATE t SET n = n + 1, updated_at = now() WHERE id = $1 AND version = $2`,
			`SELECT * FROM generate_series(1, 3)`,
		},
		mysql.New(): {
			"SELECT LOWER(name), IFNULL(n, 0), UUID() FROM t WHERE created_at > NOW()",
			"INSERT INTO t (id) VALUES (?) ON DUPLICATE KEY UPDATE n = VALUES(n)",
			"SELECT LAST_INSERT_ID(), CAST(n AS SIGNED) FROM t WHERE a <=> ?",
		},
	}
	for srv, qs := range refused {
		for _, q := range qs {
			if err := CheckSQL(srv, q); !errors.As(err, new(*ErrUnsupportedSQL)) {
				t.Errorf("%s: got %v, want ErrUnsupportedSQL", q, err)
			}
		}
	}
	for srv, qs := range accepted {
		for _, q := range qs {
			if err := CheckSQL(srv, q); err != nil {
				t.Errorf("%s: got %v, want nil", q, err)
			}
		}
	}
}

// knownFuncs and mysqlFuncs are what CheckSQL and callFunc agree on, so
// every name in them must have a case in callFunc's switch, and every case
// must be in one of them. Otherwise CheckSQL would pass a function the run
// refuses, or the reverse.
func TestKnownFuncsMatchCallFunc(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sqlexec.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "callFunc" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, _ := strconv.Unquote(lit.Value)
					cases[name] = true
				}
			}
			return true
		})
	}
	if len(cases) == 0 {
		t.Fatal("no case strings found in callFunc")
	}
	for name := range knownFuncs {
		if !cases[name] {
			t.Errorf("knownFuncs has %q, which callFunc has no case for", name)
		}
		if mysqlFuncs[name] {
			t.Errorf("%q is in knownFuncs and mysqlFuncs both", name)
		}
	}
	for name := range mysqlFuncs {
		if !cases[name] {
			t.Errorf("mysqlFuncs has %q, which callFunc has no case for", name)
		}
	}
	for name := range cases {
		if !knownFuncs[name] && !mysqlFuncs[name] {
			t.Errorf("callFunc has a case for %q, which knownFuncs and mysqlFuncs lack", name)
		}
	}
}
