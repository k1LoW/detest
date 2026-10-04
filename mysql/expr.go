package mysql

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	"github.com/pingcap/tidb/pkg/parser/test_driver"
	"github.com/pingcap/tidb/pkg/parser/types"
)

var binaryOps = map[opcode.Op]string{
	opcode.LogicAnd: "AND", opcode.LogicOr: "OR",
	opcode.EQ: "=", opcode.NE: "<>", opcode.LT: "<", opcode.LE: "<=", opcode.GT: ">", opcode.GE: ">=",
	opcode.Plus: "+", opcode.Minus: "-", opcode.Mul: "*", opcode.Mod: "%",
}

func (c *conv) expr(n ast.ExprNode) (sqlir.Expr, error) {
	switch e := n.(type) {
	case nil:
		return nil, nil
	case *test_driver.ParamMarkerExpr:
		return &sqlir.Param{Index: c.params[e]}, nil
	case *test_driver.ValueExpr:
		if d, ok := e.GetValue().(*test_driver.MyDecimal); ok {
			if f, err := strconv.ParseFloat(d.String(), 64); err == nil && math.Abs(f) >= 1<<53 {
				// A float64 cannot tell such decimals apart.
				return nil, c.unsupported("a DECIMAL literal of 2^53 or more")
			}
		}
		if u, ok := e.GetValue().(uint64); ok && u > math.MaxInt64 && !c.inLimit {
			// It would become a float64, which cannot tell such values
			// apart; in LIMIT, where MySQL's idiom for no limit uses one,
			// any value that large is past every row.
			return nil, c.unsupported("an integer literal above 9223372036854775807")
		}
		return &sqlir.Const{Value: value(e.GetValue())}, nil
	case *ast.ParenthesesExpr:
		return c.expr(e.Expr)
	case *ast.ColumnNameExpr:
		table := e.Name.Table.L
		if table != "" && table == c.rowAlias {
			table = "excluded"
		}
		return &sqlir.ColumnRef{Table: table, Column: e.Name.Name.L}, nil
	case *ast.ValuesExpr:
		// VALUES(col) in ON DUPLICATE KEY UPDATE is the proposed row's column.
		return &sqlir.ColumnRef{Table: "excluded", Column: e.Column.Name.Name.L}, nil
	case *ast.DefaultExpr:
		return &sqlir.Default{}, nil
	case *ast.BinaryOperationExpr:
		l, err := c.expr(e.L)
		if err != nil {
			return nil, err
		}
		r, err := c.expr(e.R)
		if err != nil {
			return nil, err
		}
		switch e.Op {
		case opcode.LogicAnd, opcode.LogicOr:
			return &sqlir.BinaryExpr{Op: binaryOps[e.Op], L: truthy(l), R: truthy(r)}, nil
		case opcode.Div:
			// MySQL's / divides exactly, where Postgres's divides integers.
			return &sqlir.BinaryExpr{Op: "/", L: &sqlir.FuncCall{Name: "mysql_double", Args: []sqlir.Expr{l}}, R: r}, nil
		case opcode.NullEQ:
			return nullSafeEqual(l, r), nil
		case opcode.LogicXor:
			// XOR compares truth values, so 2 XOR 3 is false.
			return &sqlir.BinaryExpr{Op: "<>", L: truthy(l), R: truthy(r)}, nil
		}
		if op, ok := binaryOps[e.Op]; ok {
			return &sqlir.BinaryExpr{Op: op, L: l, R: r}, nil
		}
		return nil, c.unsupported("operator " + e.Op.String())
	case *ast.UnaryOperationExpr:
		x, err := c.expr(e.V)
		if err != nil {
			return nil, err
		}
		switch e.Op {
		case opcode.Not, opcode.Not2:
			return &sqlir.UnaryExpr{Op: "NOT", X: truthy(x)}, nil
		case opcode.Minus:
			// As 0 - x, so a string takes MySQL's conversion to a number.
			return &sqlir.BinaryExpr{Op: "-", L: &sqlir.Const{Value: int64(0)}, R: x}, nil
		case opcode.Plus:
			if k, ok := x.(*sqlir.Const); ok {
				if _, num := k.Value.(string); !num {
					return x, nil
				}
			}
			// MySQL's unary + converts a string to a number, which the
			// executor's operators do not.
			return nil, c.unsupported("unary + of a value other than a number")
		}
		return nil, c.unsupported("operator " + e.Op.String())
	case *ast.IsNullExpr:
		x, err := c.expr(e.Expr)
		if err != nil {
			return nil, err
		}
		return &sqlir.IsNull{X: x, Not: e.Not}, nil
	case *ast.IsTruthExpr:
		x, err := c.expr(e.Expr)
		if err != nil {
			return nil, err
		}
		// x IS [NOT] TRUE | FALSE is never NULL.
		cond := truthy(x)
		if e.True == 0 {
			cond = &sqlir.UnaryExpr{Op: "NOT", X: cond}
		}
		yes, no := sqlir.Expr(&sqlir.Const{Value: true}), sqlir.Expr(&sqlir.Const{Value: false})
		if e.Not {
			yes, no = no, yes
		}
		return &sqlir.CaseExpr{Whens: []sqlir.CaseWhen{{When: cond, Then: yes}}, Else: no}, nil
	case *ast.BetweenExpr:
		x, err := c.expr(e.Expr)
		if err != nil {
			return nil, err
		}
		lo, err := c.expr(e.Left)
		if err != nil {
			return nil, err
		}
		hi, err := c.expr(e.Right)
		if err != nil {
			return nil, err
		}
		var out sqlir.Expr = &sqlir.BinaryExpr{Op: "AND", L: &sqlir.BinaryExpr{Op: ">=", L: x, R: lo}, R: &sqlir.BinaryExpr{Op: "<=", L: x, R: hi}}
		if e.Not {
			out = &sqlir.UnaryExpr{Op: "NOT", X: out}
		}
		return out, nil
	case *ast.PatternLikeOrIlikeExpr:
		x, err := c.expr(e.Expr)
		if err != nil {
			return nil, err
		}
		p, err := c.expr(e.Pattern)
		if err != nil {
			return nil, err
		}
		if e.Escape != '\\' {
			return nil, c.unsupported("LIKE with an ESCAPE other than backslash")
		}
		if !e.IsLike {
			return nil, c.unsupported("ILIKE, which MySQL does not have")
		}
		op := "LIKE"
		if e.Not {
			op = "NOT " + op
		}
		return &sqlir.BinaryExpr{Op: op, L: x, R: p}, nil
	case *ast.PatternInExpr:
		x, err := c.expr(e.Expr)
		if err != nil {
			return nil, err
		}
		out := &sqlir.InExpr{X: x, Not: e.Not}
		if e.Sel != nil {
			if out.Sub, err = c.query1(e.Sel); err != nil {
				return nil, err
			}
			return out, nil
		}
		for _, it := range e.List {
			v, err := c.expr(it)
			if err != nil {
				return nil, err
			}
			out.List = append(out.List, v)
		}
		return out, nil
	case *ast.RowExpr:
		out := &sqlir.RowExpr{}
		for _, it := range e.Values {
			v, err := c.expr(it)
			if err != nil {
				return nil, err
			}
			out.Items = append(out.Items, v)
		}
		return out, nil
	case *ast.CaseExpr:
		out := &sqlir.CaseExpr{}
		var err error
		if out.Arg, err = c.expr(e.Value); err != nil {
			return nil, err
		}
		for _, w := range e.WhenClauses {
			when, err := c.expr(w.Expr)
			if err != nil {
				return nil, err
			}
			if e.Value == nil {
				when = truthy(when) // a searched CASE tests each WHEN as a condition
			}
			then, err := c.expr(w.Result)
			if err != nil {
				return nil, err
			}
			out.Whens = append(out.Whens, sqlir.CaseWhen{When: when, Then: then})
		}
		if out.Else, err = c.expr(e.ElseClause); err != nil {
			return nil, err
		}
		return out, nil
	case *ast.SubqueryExpr:
		sel, err := c.query1(e.Query)
		if err != nil {
			return nil, err
		}
		if e.Exists {
			return &sqlir.Exists{Select: sel}, nil
		}
		return &sqlir.SubQuery{Select: sel}, nil
	case *ast.ExistsSubqueryExpr:
		sel, err := c.query1(e.Sel)
		if err != nil {
			return nil, err
		}
		return &sqlir.Exists{Select: sel, Not: e.Not}, nil
	case *ast.CompareSubqueryExpr:
		if e.Op != opcode.EQ || e.All {
			return nil, c.unsupported("comparison with ANY or ALL")
		}
		x, err := c.expr(e.L)
		if err != nil {
			return nil, err
		}
		sel, err := c.query1(e.R)
		if err != nil {
			return nil, err
		}
		return &sqlir.InExpr{X: x, Sub: sel}, nil // = ANY is IN
	case *ast.FuncCastExpr:
		x, err := c.expr(e.Expr)
		if err != nil {
			return nil, err
		}
		t, ok := castType(e.Tp)
		if !ok {
			return nil, c.unsupported("CAST to " + strings.ToUpper(e.Tp.String()))
		}
		if t == "int8" {
			if mysql.HasUnsignedFlag(e.Tp.GetFlag()) {
				// A negative value wraps past the int64 values detest keeps.
				return nil, c.unsupported("CAST to UNSIGNED")
			}
			// MySQL's SIGNED converts strings and fractions its own way,
			// not as the executor's Postgres casts do.
			return &sqlir.FuncCall{Name: "mysql_signed", Args: []sqlir.Expr{x}}, nil
		}
		if t == "float8" {
			// A string converts to the number it starts with, as MySQL's
			// arithmetic converts it, not as the executor's casts do.
			return &sqlir.FuncCall{Name: "mysql_double", Args: []sqlir.Expr{x}}, nil
		}
		return &sqlir.Cast{X: x, Type: t}, nil
	case *ast.AggregateFuncExpr:
		return c.aggregate(e)
	case *ast.WindowFuncExpr:
		return c.window(e)
	case *ast.FuncCallExpr:
		return c.funcCall(e)
	}
	return nil, c.unsupported(fmt.Sprintf("expression %T", n))
}

// cond converts an expression in a place that tests it as a condition.
func (c *conv) cond(n ast.ExprNode) (sqlir.Expr, error) {
	e, err := c.expr(n)
	if err != nil || e == nil {
		return e, err
	}
	return truthy(e), nil
}

// truthy makes e a truth value as MySQL tests one: NULL stays NULL, a number
// is true unless 0, a string by the number it starts with. MySQL has no
// separate boolean type, so WHERE 1 selects every row, where the executor
// takes only a boolean as true. An expression that already yields a boolean
// is left as it is.
func truthy(e sqlir.Expr) sqlir.Expr {
	if isBoolean(e) {
		return e
	}
	return &sqlir.FuncCall{Name: "mysql_truth", Args: []sqlir.Expr{e}}
}

func isBoolean(e sqlir.Expr) bool {
	switch v := e.(type) {
	case *sqlir.BinaryExpr:
		switch v.Op {
		case "=", "<>", "<", "<=", ">", ">=", "<=>", "AND", "OR", "LIKE", "ILIKE", "NOT LIKE", "NOT ILIKE":
			return true
		}
	case *sqlir.UnaryExpr:
		return v.Op == "NOT"
	case *sqlir.InExpr, *sqlir.IsNull, *sqlir.Exists:
		return true
	case *sqlir.Const:
		_, ok := v.Value.(bool)
		return ok
	case *sqlir.CaseExpr:
		for _, w := range v.Whens {
			if !isBoolean(w.Then) {
				return false
			}
		}
		return v.Else != nil && isBoolean(v.Else)
	case *sqlir.FuncCall:
		return v.Name == "mysql_truth"
	}
	return false
}

// nullSafeEqual is a <=> b: true when both are NULL or equal, never NULL.
func nullSafeEqual(l, r sqlir.Expr) sqlir.Expr {
	return &sqlir.BinaryExpr{Op: "<=>", L: l, R: r}
}

// value converts a literal to the Go types the executor works with.
func value(v any) any {
	switch v := v.(type) {
	case uint64:
		if v <= 1<<63-1 {
			return int64(v)
		}
		return float64(v)
	case int64, float64, string, nil:
		return v
	case []byte:
		return string(v)
	case *test_driver.MyDecimal:
		f, err := strconv.ParseFloat(v.String(), 64)
		if err != nil {
			return v.String()
		}
		return f
	}
	return fmt.Sprint(v)
}

// castType is the executor's type for the CAST targets it converts to as
// MySQL does. The others are refused: FLOAT rounds to single precision,
// DECIMAL to its scale, CHAR(n) truncates, BINARY pads and compares bytes,
// and the temporal types and JSON compare by their own rules, so carrying
// any of them out as another type would match different rows.
func castType(t *types.FieldType) (string, bool) {
	switch strings.ToLower(types.TypeToStr(t.GetType(), t.GetCharset())) {
	case "bigint":
		return "int8", true
	case "double":
		return "float8", true
	case "var_string":
		return "text", t.GetFlen() == types.UnspecifiedLength && t.GetCharset() != "binary"
	}
	return "", false
}

func (c *conv) args(in []ast.ExprNode) ([]sqlir.Expr, error) {
	out := make([]sqlir.Expr, 0, len(in))
	for _, a := range in {
		e, err := c.expr(a)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// funcNames maps the MySQL functions detest runs to the executor's names for
// the same function. Any other is refused, as the executor's other functions
// are Postgres's and would run another dialect's semantics.
var funcNames = map[string]string{
	"ifnull": "coalesce", "coalesce": "coalesce", "nullif": "mysql_nullif",
	"now": "now", "current_timestamp": "now", "localtimestamp": "now", "localtime": "now",
	"uuid": "gen_random_uuid", "length": "octet_length", "char_length": "length", "character_length": "length",
	"lower": "lower", "lcase": "lower", "upper": "upper", "ucase": "upper", "left": "left",
	"abs": "abs", "floor": "floor", "ceil": "ceil", "ceiling": "ceil", "round": "round", "power": "power", "pow": "power",
	"last_insert_id": "last_insert_id", "concat": "mysql_concat", "greatest": "mysql_greatest", "least": "mysql_least",
}

// funcArity is the fewest and the most arguments the executor's function
// takes, -1 for no limit.
var funcArity = map[string][2]int{
	"coalesce": {1, -1}, "mysql_nullif": {2, 2}, "now": {0, 0}, "current_timestamp": {0, 1}, "gen_random_uuid": {0, 0},
	"octet_length": {1, 1}, "length": {1, 1}, "lower": {1, 1}, "upper": {1, 1}, "left": {2, 2},
	"abs": {1, 1}, "floor": {1, 1}, "ceil": {1, 1}, "round": {1, 1}, "power": {2, 2},
	"last_insert_id": {0, 0}, "mysql_concat": {1, -1}, "mysql_greatest": {2, -1}, "mysql_least": {2, -1},
}

func (c *conv) funcCall(e *ast.FuncCallExpr) (sqlir.Expr, error) {
	name := e.FnName.L
	args, err := c.args(e.Args)
	if err != nil {
		return nil, err
	}
	switch name {
	case "if":
		if len(args) != 3 {
			return nil, c.unsupported("IF with other than three arguments")
		}
		return &sqlir.CaseExpr{Whens: []sqlir.CaseWhen{{When: truthy(args[0]), Then: args[1]}}, Else: args[2]}, nil
	case "last_insert_id":
		if len(args) > 0 {
			return nil, c.unsupported("LAST_INSERT_ID with an argument")
		}
	}
	n, ok := funcNames[name]
	if !ok {
		return nil, c.unsupported("function " + name)
	}
	if n == "now" && len(args) == 1 {
		n = "current_timestamp" // the executor's name of the time with a precision
	}
	if a := funcArity[n]; len(args) < a[0] || a[1] >= 0 && len(args) > a[1] {
		// The executor's functions take the arguments they know and ignore
		// the rest, so ROUND(x, d) would round to an integer.
		return nil, c.unsupported(fmt.Sprintf("%s with %d arguments", name, len(args)))
	}
	return &sqlir.FuncCall{Name: n, Args: args}, nil
}

func (c *conv) aggregate(e *ast.AggregateFuncExpr) (sqlir.Expr, error) {
	name := strings.ToLower(e.F)
	switch name {
	case "count", "sum", "min", "max", "avg":
	default:
		return nil, c.unsupported("aggregate " + name)
	}
	if e.Order != nil {
		return nil, c.unsupported("ordered aggregate")
	}
	args, err := c.args(e.Args)
	if err != nil {
		return nil, err
	}
	if len(args) > 1 {
		// The executor aggregates its first argument only.
		return nil, c.unsupported("aggregate " + name + " of more than one argument")
	}
	out := &sqlir.FuncCall{Name: name, Args: args, Distinct: e.Distinct}
	if name == "count" && !e.Distinct && len(args) == 1 {
		if k, ok := args[0].(*sqlir.Const); ok && k.Value != nil {
			out.Star, out.Args = true, nil // COUNT(*) and COUNT(1) count every row
		}
	}
	return out, nil
}

// windowArity is the fewest and the most arguments of the window functions
// the executor computes.
var windowArity = map[string][2]int{
	"row_number": {0, 0}, "rank": {0, 0}, "dense_rank": {0, 0},
	"lag": {1, 3}, "lead": {1, 3}, "first_value": {1, 1}, "last_value": {1, 1},
	"count": {1, 1}, "sum": {1, 1}, "min": {1, 1}, "max": {1, 1}, "avg": {1, 1},
}

func (c *conv) window(e *ast.WindowFuncExpr) (sqlir.Expr, error) {
	if e.Spec.Name.O != "" || e.Spec.Ref.O != "" {
		return nil, c.unsupported("named window")
	}
	args, err := c.args(e.Args)
	if err != nil {
		return nil, err
	}
	name := strings.ToLower(e.Name)
	arity, ok := windowArity[name]
	if !ok {
		return nil, c.unsupported("window function " + name)
	}
	if len(args) < arity[0] || len(args) > arity[1] {
		return nil, c.unsupported(fmt.Sprintf("%s with %d arguments", name, len(args)))
	}
	out := &sqlir.WindowFunc{Func: &sqlir.FuncCall{Name: name, Args: args, Distinct: e.Distinct}}
	if e.Spec.PartitionBy != nil {
		for _, it := range e.Spec.PartitionBy.Items {
			x, err := c.expr(it.Expr)
			if err != nil {
				return nil, err
			}
			out.Partition = append(out.Partition, x)
		}
	}
	if out.Order, err = c.orderBy(e.Spec.OrderBy); err != nil {
		return nil, err
	}
	if f := e.Spec.Frame; f != nil {
		whole := f.Type == ast.Rows && f.Extent.Start.UnBounded && f.Extent.Start.Type == ast.Preceding &&
			f.Extent.End.UnBounded && f.Extent.End.Type == ast.Following
		if !whole {
			return nil, c.unsupported("window frame other than the default or the whole partition")
		}
		out.Whole = true
	}
	return out, nil
}
