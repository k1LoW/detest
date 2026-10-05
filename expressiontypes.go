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
		switch e.Value.(type) {
		case int64:
			return "int8"
		case float64:
			return "numeric"
		case bool:
			return "bool"
		}
	case *sqlir.UnaryExpr:
		if e.Op == "-" {
			return expressionType(e.X, column)
		}
	case *sqlir.BinaryExpr:
		switch e.Op {
		case "||":
			return "text"
		case "+", "-", "*", "/", "%":
			if typ := expressionType(e.L, column); typ != "" {
				return typ
			}
			return expressionType(e.R, column)
		case "=", "<>", "!=", "<", "<=", ">", ">=", "AND", "OR":
			return "bool"
		}
	case *sqlir.FuncCall:
		switch e.Name {
		case "min", "max", "sum", "abs", "coalesce", "greatest", "least", "nullif":
			for _, arg := range e.Args {
				if typ := expressionType(arg, column); typ != "" {
					return typ
				}
			}
		case "lower", "upper", "left", "concat":
			return "text"
		case "length", "char_length", "octet_length", "nextval", "count":
			return "int8"
		case "random":
			return "float8"
		case "now", "clock_timestamp", "transaction_timestamp", "statement_timestamp", "current_timestamp":
			return "timestamptz"
		case "gen_random_uuid", "uuid_generate_v4":
			return "uuid"
		}
	}
	return ""
}

func textArgumentMismatch(f *sqlir.FuncCall, column func(*sqlir.ColumnRef) string) string {
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
