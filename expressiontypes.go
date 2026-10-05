package detest

import "github.com/k1LoW/detest/internal/sqlir"

// expressionType uses only types the statement or its declared columns tell
// us. It never evaluates an argument while resolving a function signature.
func expressionType(e sqlir.Expr, column func(*sqlir.ColumnRef) string) string {
	switch e := e.(type) {
	case *sqlir.Cast:
		return e.Type
	case *sqlir.ColumnRef:
		if column != nil {
			return column(e)
		}
	case *sqlir.Const:
		switch v := e.Value.(type) {
		case int64:
			if v >= -1<<31 && v < 1<<31 {
				return "int4"
			}
			return "int8"
		case float64:
			return "numeric"
		case bool:
			return "bool"
		}
	case *sqlir.UnaryExpr:
		if e.Op == "NOT" {
			return "bool"
		}
		if e.Op == "-" {
			return expressionType(e.X, column)
		}
	case *sqlir.BinaryExpr:
		switch e.Op {
		case "||":
			return "text"
		case "+", "-", "*", "/", "%":
			l, r := expressionType(e.L, column), expressionType(e.R, column)
			for _, typ := range []string{"float8", "float4", "numeric", "int8"} {
				if l == typ || r == typ {
					return typ
				}
			}
			if l != "" {
				return l
			}
			return r
		case "=", "<>", "!=", "<", "<=", ">", ">=", "AND", "OR", "LIKE", "ILIKE", "NOT LIKE", "NOT ILIKE":
			return "bool"
		}
	case *sqlir.FuncCall:
		switch e.Name {
		case "floor", "ceil", "ceiling", "round", "power", "pow", "avg":
			return "numeric"
		case "pg_try_advisory_xact_lock":
			return "bool"
		case "pg_advisory_xact_lock":
			return "void"
		case "make_interval":
			return "interval"
		case "hashtext":
			return "int4"
		case "sum":
			if len(e.Args) > 0 {
				switch typ := expressionType(e.Args[0], column); typ {
				case "int", "int2", "int4", "integer", "smallint":
					return "int8"
				case "int8", "bigint":
					return "numeric"
				default:
					return typ
				}
			}
		case "min", "max", "abs", "coalesce", "greatest", "least", "nullif":
			for _, arg := range e.Args {
				if typ := expressionType(arg, column); typ != "" {
					return typ
				}
			}
		case "lower", "upper", "left", "concat":
			return "text"
		case "length", "char_length", "octet_length":
			return "int4"
		case "nextval", "setval", "count", "row_number", "rank", "dense_rank":
			return "int8"
		case "lag", "lead", "first_value", "last_value":
			if len(e.Args) > 0 {
				return expressionType(e.Args[0], column)
			}
		case "random":
			return "float8"
		case "now", "clock_timestamp", "transaction_timestamp", "statement_timestamp", "current_timestamp":
			return "timestamptz"
		case "gen_random_uuid", "uuid_generate_v4":
			return "uuid"
		}
	case *sqlir.WindowFunc:
		return expressionType(e.Func, column)
	case *sqlir.CaseExpr:
		for _, branch := range caseBranches(e) {
			if typ := expressionType(branch, column); typ != "" {
				return typ
			}
		}
	case *sqlir.SubQuery:
		if e.Select != nil && len(e.Select.Targets) == 1 {
			return expressionType(e.Select.Targets[0].Expr, column)
		}
	case *sqlir.Exists, *sqlir.InExpr, *sqlir.IsNull:
		return "bool"
	}
	return ""
}

func textArgumentMismatch(f *sqlir.FuncCall, column func(*sqlir.ColumnRef) string) string {
	if f.Name == "left" && len(f.Args) == 2 {
		switch typ := expressionType(f.Args[1], column); typ {
		case "", "unresolved column type", "int", "int2", "int4", "integer", "smallint":
		default:
			return "left with a " + typ + " length argument"
		}
	}
	switch f.Name {
	case "lower", "upper", "left", "length", "char_length", "octet_length", "hashtext":
		if len(f.Args) > 0 {
			typ := expressionType(f.Args[0], column)
			if typ != "" && typ != "unresolved column type" && typ != "text" && typ != "varchar" && typ != "bpchar" {
				return f.Name + " with a " + typ + " argument"
			}
		}
	}
	return ""
}

func concatTypeMismatch(b *sqlir.BinaryExpr, column func(*sqlir.ColumnRef) string) string {
	if b.Op != "||" {
		return ""
	}
	for _, e := range []sqlir.Expr{b.L, b.R} {
		switch typ := expressionType(e, column); typ {
		case "", "text", "varchar", "bpchar", "bool", "boolean", "int", "int2", "int4", "int8", "integer", "smallint", "bigint":
		default:
			return "|| with a " + typ + " operand"
		}
	}
	return ""
}
