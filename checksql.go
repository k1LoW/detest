package detest

import (
	"fmt"

	"github.com/k1LoW/detest/internal/sqlir"
)

// knownFuncs are the functions callFunc computes, by the names the dialects
// give them. callFunc refuses any other name, and CheckSQL refuses it before
// running the statement, since with no rows to evaluate over the executor
// would never reach the call.
var knownFuncs = map[string]bool{
	"coalesce": true, "nullif": true, "greatest": true, "least": true,
	"left": true, "lower": true, "upper": true, "length": true, "char_length": true, "octet_length": true, "concat": true, "hashtext": true,
	"nextval": true, "setval": true, "gen_random_uuid": true, "uuid_generate_v4": true,
	"now": true, "clock_timestamp": true, "current_timestamp": true, "transaction_timestamp": true, "statement_timestamp": true,
	"random": true, "abs": true, "floor": true, "ceil": true, "ceiling": true, "round": true, "power": true, "pow": true,
	"make_interval": true, "pg_try_advisory_xact_lock": true, "pg_advisory_xact_lock": true,
	// MySQL's, under the executor's names for them (mysql/expr.go).
	"last_insert_id": true, "mysql_concat": true, "mysql_greatest": true, "mysql_least": true, "mysql_nullif": true,
	"mysql_signed": true, "mysql_double": true, "mysql_dividend": true, "mysql_truth": true,
}

// aggregateFuncs are the aggregates the executor folds.
var aggregateFuncs = map[string]bool{"count": true, "sum": true, "min": true, "max": true, "avg": true}

// windowFuncs are the window functions the executor computes, besides the
// aggregates.
var windowFuncs = map[string]bool{
	"row_number": true, "rank": true, "dense_rank": true, "lag": true, "lead": true, "first_value": true, "last_value": true,
}

// knownBinaryOps are the operators the executor's binary evaluates.
var knownBinaryOps = map[string]bool{
	"AND": true, "OR": true,
	"=": true, "<>": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true, "<=>": true,
	"LIKE": true, "ILIKE": true, "NOT LIKE": true, "NOT ILIKE": true,
	"+": true, "-": true, "*": true, "/": true, "%": true, "||": true,
}

// checkStatic refuses the functions and operators in a statement that the
// executor cannot evaluate, wherever they appear. A run reaches only the
// expressions it evaluates, so a statement over a table CheckSQL has no rows
// for would pass with an unknown function in its WHERE and fail in the
// exploration. The schema statements are left alone: an expression in a
// default or a CHECK loads unconverted and is refused by the write that reads
// it, as README's Approximate rules say.
func checkStatic(st sqlir.Statement, query string) error {
	c := &staticCheck{query: query}
	return c.statement(st)
}

type staticCheck struct{ query string }

func (c *staticCheck) refuse(what string) error {
	return unsupported("an expression in the statement detest cannot evaluate ("+what+")", c.query)
}

func (c *staticCheck) statement(st sqlir.Statement) error {
	switch s := st.(type) {
	case *sqlir.SelectStmt:
		return c.selectStmt(s)
	case *sqlir.InsertStmt:
		for _, row := range s.Rows {
			if err := c.exprs(row); err != nil {
				return err
			}
		}
		if err := c.selectStmt(s.Select); err != nil {
			return err
		}
		if oc := s.OnConflict; oc != nil {
			if err := c.exprs(oc.Elems); err != nil {
				return err
			}
			if err := c.expr(oc.InferWhere); err != nil {
				return err
			}
			if err := c.assignments(oc.Set); err != nil {
				return err
			}
			if err := c.expr(oc.Where); err != nil {
				return err
			}
		}
		return c.targets(s.Returning)
	case *sqlir.UpdateStmt:
		if err := c.assignments(s.Set); err != nil {
			return err
		}
		for i := range s.From {
			if err := c.tableRef(&s.From[i]); err != nil {
				return err
			}
		}
		if err := c.expr(s.Where); err != nil {
			return err
		}
		if err := c.orderKeys(s.OrderBy); err != nil {
			return err
		}
		if err := c.expr(s.Limit); err != nil {
			return err
		}
		return c.targets(s.Returning)
	case *sqlir.DeleteStmt:
		for i := range s.Using {
			if err := c.tableRef(&s.Using[i]); err != nil {
				return err
			}
		}
		if err := c.expr(s.Where); err != nil {
			return err
		}
		if err := c.orderKeys(s.OrderBy); err != nil {
			return err
		}
		if err := c.expr(s.Limit); err != nil {
			return err
		}
		return c.targets(s.Returning)
	case *sqlir.CreateTableAsStmt:
		return c.selectStmt(s.Select)
	case *sqlir.Script:
		for _, st := range s.Stmts {
			if err := c.statement(st); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *staticCheck) selectStmt(s *sqlir.SelectStmt) error {
	if s == nil {
		return nil
	}
	for _, cte := range s.With {
		if err := c.selectStmt(cte.Select); err != nil {
			return err
		}
	}
	if err := c.exprs(s.DistinctOn); err != nil {
		return err
	}
	if err := c.selectStmt(s.Larg); err != nil {
		return err
	}
	if err := c.selectStmt(s.Rarg); err != nil {
		return err
	}
	for _, row := range s.Values {
		if err := c.exprs(row); err != nil {
			return err
		}
	}
	if err := c.targets(s.Targets); err != nil {
		return err
	}
	if err := c.tableRef(s.From); err != nil {
		return err
	}
	for i := range s.Joins {
		if err := c.tableRef(&s.Joins[i].Table); err != nil {
			return err
		}
		if err := c.expr(s.Joins[i].On); err != nil {
			return err
		}
	}
	if err := c.expr(s.Where); err != nil {
		return err
	}
	if err := c.exprs(s.GroupBy); err != nil {
		return err
	}
	if err := c.expr(s.Having); err != nil {
		return err
	}
	if err := c.orderKeys(s.OrderBy); err != nil {
		return err
	}
	if err := c.expr(s.Limit); err != nil {
		return err
	}
	return c.expr(s.Offset)
}

func (c *staticCheck) tableRef(t *sqlir.TableRef) error {
	if t == nil {
		return nil
	}
	if err := c.selectStmt(t.Sub); err != nil {
		return err
	}
	if t.Func != nil {
		// The executor runs generate_series in FROM and refuses the other
		// set-returning functions itself, rows or no rows.
		return c.exprs(t.Func.Args)
	}
	return nil
}

func (c *staticCheck) targets(ts []sqlir.Target) error {
	for _, t := range ts {
		if err := c.expr(t.Expr); err != nil {
			return err
		}
	}
	return nil
}

func (c *staticCheck) assignments(as []sqlir.Assignment) error {
	for _, a := range as {
		if err := c.expr(a.Value); err != nil {
			return err
		}
	}
	return nil
}

func (c *staticCheck) orderKeys(ks []sqlir.OrderKey) error {
	for _, k := range ks {
		if err := c.expr(k.Expr); err != nil {
			return err
		}
	}
	return nil
}

func (c *staticCheck) exprs(es []sqlir.Expr) error {
	for _, e := range es {
		if err := c.expr(e); err != nil {
			return err
		}
	}
	return nil
}

func (c *staticCheck) expr(e sqlir.Expr) error {
	switch v := e.(type) {
	case nil, *sqlir.ColumnRef, *sqlir.Param, *sqlir.Const, *sqlir.Default:
		return nil
	case *sqlir.Unconverted:
		return c.refuse("an expression detest could not convert")
	case *sqlir.FuncCall:
		if !knownFuncs[v.Name] && !aggregateFuncs[v.Name] {
			if sqlir.OtherAggregates[v.Name] {
				return unsupported("aggregate "+v.Name, c.query)
			}
			return c.refuse(v.Name + "(...)")
		}
		return c.exprs(v.Args)
	case *sqlir.WindowFunc:
		if !windowFuncs[v.Func.Name] && !aggregateFuncs[v.Func.Name] {
			return unsupported("window function "+v.Func.Name, c.query)
		}
		if err := c.exprs(v.Func.Args); err != nil {
			return err
		}
		if err := c.exprs(v.Partition); err != nil {
			return err
		}
		return c.orderKeys(v.Order)
	case *sqlir.BinaryExpr:
		if !knownBinaryOps[v.Op] {
			return c.refuse("operator " + v.Op)
		}
		if err := c.expr(v.L); err != nil {
			return err
		}
		return c.expr(v.R)
	case *sqlir.UnaryExpr:
		if v.Op != "NOT" && v.Op != "-" {
			return c.refuse("unary " + v.Op)
		}
		return c.expr(v.X)
	case *sqlir.InExpr:
		if err := c.expr(v.X); err != nil {
			return err
		}
		if err := c.exprs(v.List); err != nil {
			return err
		}
		return c.selectStmt(v.Sub)
	case *sqlir.IsNull:
		return c.expr(v.X)
	case *sqlir.SubQuery:
		return c.selectStmt(v.Select)
	case *sqlir.Exists:
		return c.selectStmt(v.Select)
	case *sqlir.RowExpr:
		return c.exprs(v.Items)
	case *sqlir.Cast:
		return c.expr(v.X)
	case *sqlir.CaseExpr:
		if err := c.expr(v.Arg); err != nil {
			return err
		}
		for _, w := range v.Whens {
			if err := c.expr(w.When); err != nil {
				return err
			}
			if err := c.expr(w.Then); err != nil {
				return err
			}
		}
		return c.expr(v.Else)
	}
	return c.refuse(fmt.Sprintf("%T", e))
}
