package detest

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
)

// This file evaluates the expressions of detest's SQL IR against the rows an
// executing statement has at hand: literals, columns, parameters, operators,
// CASE, subqueries, and the scalar functions detest runs. The casts and the
// number conversions are in sqlcast.go, and the statements that call eval
// are in sqlexec.go.

type errUnknownExpr struct{ what string }

func (e errUnknownExpr) Error() string { return "detest: cannot evaluate SQL expression: " + e.what }

// inUniqueIndex reports whether row has an entry in one of the table's
// unique indexes, so that an equal row is decided by that index.
func (x *sqlExec) inUniqueIndex(table string, row Row) bool {
	def := x.tx.db.defs[table]
	if def == nil {
		return false
	}
	for i := range def.uniques {
		if _, ok, err := x.uniqueValues(table, &def.uniques[i], row); err == nil && ok {
			return true
		}
	}
	return false
}

// unsupportedExpr turns an expression detest cannot evaluate into
// ErrUnsupportedSQL, naming where in the statement it stood. A written value
// or a sort key the application observes cannot be stood in for: a
// placeholder value would be read back as the column's, and a dropped sort
// key would return other rows under LIMIT. Any other error passes through.
func (x *sqlExec) unsupportedExpr(err error, where string) error {
	if u, ok := errors.AsType[errUnknownExpr](err); ok {
		return x.unsupported("an expression " + where + " detest cannot evaluate (" + u.what + ")")
	}
	return err
}

func (x *sqlExec) evalBool(e sqlir.Expr, en *env) (bool, error) {
	v, err := x.eval(e, en)
	if err != nil {
		return false, err
	}
	b, _ := derefValue(v).(bool)
	return b, nil
}

func (x *sqlExec) eval(e sqlir.Expr, en *env) (any, error) {
	v, err := x.evalRaw(e, en)
	if _, ok := v.(numRange); ok && err == nil {
		return nil, x.errInexact()
	}
	return v, err
}

// evalRaw is eval, giving a numRange as it is, for a caller that resolves it.
func (x *sqlExec) evalRaw(e sqlir.Expr, en *env) (any, error) {
	switch v := e.(type) {
	case nil:
		return true, nil
	case *sqlir.ColumnRef:
		val, _ := en.lookup(v.Table, v.Column)
		return val, nil
	case *sqlir.Param:
		if v.Index < 0 || v.Index >= len(x.args) {
			return nil, x.tx.db.kind.Error(sqlir.UndefinedParameter, fmt.Sprintf("there is no parameter $%d", v.Index+1), "", "", "")
		}
		if _, ok := x.args[v.Index].(arrayParam); ok {
			return nil, x.unsupported("an array parameter anywhere but the right side of = ANY or <> ALL")
		}
		return x.args[v.Index], nil
	case *sqlir.Const:
		return v.Value, nil
	case *sqlir.WindowFunc:
		for cur := en; cur != nil; cur = cur.outer {
			if val, ok := cur.win[v]; ok {
				return val, nil
			}
		}
		return nil, x.unsupported("window function outside the select list, ORDER BY or DISTINCT ON")
	case *sqlir.Default:
		return nil, nil
	case *sqlir.Unconverted:
		return nil, errUnknownExpr{"an expression detest could not convert"}
	case *sqlir.Cast:
		val, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		if err := x.byteaCast(v, val); err != nil {
			return nil, err
		}
		if err := x.paramTextCast(v, val); err != nil {
			return nil, err
		}
		if err := x.boolCastSource(v); err != nil {
			return nil, err
		}
		return x.cast(x.halfToInteger(v, paramBool(v, val)), v.Type)
	case *sqlir.UnaryExpr:
		val, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		switch v.Op {
		case "NOT":
			b, ok := derefValue(val).(bool)
			if !ok {
				return nil, nil // NOT NULL is NULL
			}
			return !b, nil
		case "-":
			if n, ok := integer(derefValue(val)); ok {
				if n == math.MinInt64 {
					return nil, x.tx.db.kind.Error(sqlir.NumericValueOutOfRange, "bigint out of range", "", "", "")
				}
				return -n, nil
			}
			if f, ok := toFloat(derefValue(val)); ok {
				return -f, nil
			}
		}
		return nil, errUnknownExpr{"unary " + v.Op}
	case *sqlir.BinaryExpr:
		switch v.Op {
		case "AND", "OR":
			// Three-valued: false decides AND and true decides OR, whatever
			// the other side; otherwise a NULL side makes the result NULL.
			decides := v.Op == "OR"
			l, err := x.eval(v.L, en)
			if err != nil {
				return nil, err
			}
			lb, lok := derefValue(l).(bool)
			if lok && lb == decides {
				return decides, nil
			}
			r, err := x.eval(v.R, en)
			if err != nil {
				return nil, err
			}
			rb, rok := derefValue(r).(bool)
			switch {
			case rok && rb == decides:
				return decides, nil
			case !lok || !rok:
				return nil, nil
			}
			return !decides, nil
		}
		l, err := x.evalRaw(v.L, en)
		if err != nil {
			return nil, err
		}
		r, err := x.evalRaw(v.R, en)
		if err != nil {
			return nil, err
		}
		if isRange(l) || isRange(r) {
			return x.rangeBinary(v.Op, l, r)
		}
		switch v.Op {
		case "=", "<>", "!=", "<", "<=", ">", ">=":
			lr, lok := v.L.(*sqlir.RowExpr)
			rr, rok := v.R.(*sqlir.RowExpr)
			if lok || rok {
				if !lok || !rok || len(lr.Items) != len(rr.Items) {
					return nil, x.unsupported("row comparison")
				}
				return x.compareRows(v.Op, lr.Items, l, rr.Items, r)
			}
			if l, r, err = x.untypedPair(v.L, l, v.R, r); err != nil {
				return nil, err
			}
		}
		if v.Op == "||" {
			l, r = paramText(v.L, l), paramText(v.R, r)
		}
		res, err := x.binary(v.Op, l, r)
		if err != nil {
			return nil, err
		}
		return x.arithValue(v, l, r, res), nil
	case *sqlir.IsNull:
		val, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		isNull := derefValue(val) == nil
		if v.Not {
			return !isNull, nil
		}
		return isNull, nil
	case *sqlir.InExpr:
		l, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		in, sawNull := false, false
		if v.Sub != nil {
			cols, rows, err := x.evalSelect(v.Sub, en)
			if err != nil {
				return nil, err
			}
			var lhs []any
			lhsExprs := []sqlir.Expr{v.X}
			if row, ok := v.X.(*sqlir.RowExpr); ok {
				if lhs, ok = l.([]any); !ok {
					return nil, x.unsupported("row comparison")
				}
				lhsExprs = row.Items
			} else {
				lhs = []any{l}
			}
			if len(cols) != len(lhs) {
				return nil, x.unsupported("IN (subquery) whose columns do not match the left side")
			}
			for _, r := range rows {
				vals := make([]any, len(cols))
				for i, c := range cols {
					vals[i] = r[c]
				}
				eq, err := x.compareRows("=", lhsExprs, lhs, make([]sqlir.Expr, len(cols)), vals)
				if err != nil {
					return nil, err
				}
				if eq == nil {
					sawNull = true
				} else if b, _ := eq.(bool); b {
					in = true
					break
				}
			}
		} else {
			lr, isRow := v.X.(*sqlir.RowExpr)
			for _, it := range v.List {
				val, err := x.eval(it, en)
				if err != nil {
					return nil, err
				}
				if isRow {
					ir, ok := it.(*sqlir.RowExpr)
					if !ok || len(ir.Items) != len(lr.Items) {
						return nil, x.unsupported("row comparison")
					}
					eq, err := x.compareRows("=", lr.Items, l, ir.Items, val)
					if err != nil {
						return nil, err
					}
					if eq == nil {
						sawNull = true
					} else if b, _ := eq.(bool); b {
						in = true
						break
					}
					continue
				}
				if _, ok := it.(*sqlir.RowExpr); ok {
					return nil, x.unsupported("row comparison")
				}
				if _, row := derefValue(val).([]any); row {
					return nil, x.unsupported("IN of a value among rows")
				}
				// Typed first, as a NULL of text compared with a number is
				// refused before any value is seen.
				lt, vt, err := x.untypedPair(v.X, l, it, val)
				if err != nil {
					return nil, err
				}
				if derefValue(val) == nil || derefValue(l) == nil {
					sawNull = true
					continue // NULL equals nothing, itself included
				}
				if equalValues(x.comparable(lt, vt)) {
					in = true
					break
				}
			}
		}
		// x IN (...) is NULL for a NULL x, or when it matches nothing and
		// the list holds a NULL; NOT IN negates only a known answer. A
		// subquery with no rows is false whatever x is.
		if !in && (sawNull || v.Sub == nil && derefValue(l) == nil) {
			return nil, nil
		}
		if v.Not {
			return !in, nil
		}
		return in, nil
	case *sqlir.ArrayCmp:
		l, err := x.eval(v.X, en)
		if err != nil {
			return nil, err
		}
		arr, err := x.arrayElems(v)
		if err != nil {
			return nil, err
		}
		if arr.null {
			return nil, nil
		}
		// Every element is typed against x before any is compared, as
		// Postgres reads the whole array as x's type first, so one that does
		// not read as it fails whatever the others match.
		ls, rs := make([]any, len(arr.vals)), make([]any, len(arr.vals))
		for i := range arr.vals {
			li, ri, err := x.untypedPair(v.X, l, arr.exprs[i], arr.vals[i])
			if err != nil {
				return nil, err
			}
			ls[i], rs[i] = li, ri
		}
		// = ANY is true on a match and <> ALL false; without one, a NULL on
		// either side makes it NULL. An empty array decides it whatever x is.
		sawNull := false
		for i := range ls {
			if derefValue(ls[i]) == nil || derefValue(rs[i]) == nil {
				sawNull = true
				continue
			}
			if equalValues(x.comparable(ls[i], rs[i])) {
				return !v.All, nil
			}
		}
		if sawNull {
			return nil, nil
		}
		return v.All, nil
	case *sqlir.Exists:
		_, rows, err := x.evalSelect(v.Select, en)
		if err != nil {
			return nil, err
		}
		if v.Not {
			return len(rows) == 0, nil
		}
		return len(rows) > 0, nil
	case *sqlir.SubQuery:
		cols, rows, err := x.evalSelect(v.Select, en)
		if err != nil {
			return nil, err
		}
		if len(cols) != 1 {
			return nil, x.unsupported("a scalar subquery of other than one column")
		}
		if len(rows) > 1 {
			return nil, x.tx.db.kind.Error(sqlir.CardinalityViolation, "more than one row returned by a subquery used as an expression", "", "", "")
		}
		if len(rows) == 0 {
			return nil, nil
		}
		return rows[0][cols[0]], nil
	case *sqlir.RowExpr:
		vals := make([]any, len(v.Items))
		for i, it := range v.Items {
			val, err := x.eval(it, en)
			if err != nil {
				return nil, err
			}
			vals[i] = val
		}
		return vals, nil
	case *sqlir.CaseExpr:
		var arg any
		if v.Arg != nil {
			a, err := x.eval(v.Arg, en)
			if err != nil {
				return nil, err
			}
			arg = a
		}
		for _, w := range v.Whens {
			cond, err := x.eval(w.When, en)
			if err != nil {
				return nil, err
			}
			hit := false
			if v.Arg != nil {
				// SQL equality, where NULL matches nothing.
				at, ct, err := x.untypedPair(v.Arg, arg, w.When, cond)
				if err != nil {
					return nil, err
				}
				hit = derefValue(at) != nil && derefValue(ct) != nil && equalValues(x.comparable(at, ct))
			} else {
				hit, _ = derefValue(cond).(bool)
			}
			if hit {
				out, err := x.eval(w.Then, en)
				if err != nil {
					return nil, err
				}
				return x.branchValue(caseBranches(v), x.columnBranches(caseBranches(v), en), out)
			}
		}
		if v.Else != nil {
			out, err := x.eval(v.Else, en)
			if err != nil {
				return nil, err
			}
			return x.branchValue(caseBranches(v), x.columnBranches(caseBranches(v), en), out)
		}
		return nil, nil
	case *sqlir.FuncCall:
		if !knownFunc(v.Name, x.tx.db.kind.InnoDB()) {
			// Before the arguments, which may have effects (nextval),
			// where the server resolves the function first and runs none
			// of them.
			return nil, errUnknownExpr{v.Name + "(...)"}
		}
		if err := x.checkArity(v); err != nil {
			return nil, err
		}
		if v.Name == "mysql_nullif" && volatile(v.Args[0]) {
			// MySQL evaluates the first argument again to return it, which
			// a function such as UUID() answers with another value.
			return nil, x.unsupported("NULLIF of a function that returns another value each time")
		}
		args := make([]any, len(v.Args))
		for i, a := range v.Args {
			val, err := x.eval(a, en)
			if err != nil {
				return nil, err
			}
			args[i] = val
		}
		if v.Name == "nullif" {
			var err error
			if args[0], args[1], err = x.untypedPair(v.Args[0], args[0], v.Args[1], args[1]); err != nil {
				return nil, err
			}
		}
		if v.Name == "round" && len(args) == 1 {
			if out, ok, err := x.roundHalf(v.Args[0], args[0]); ok || err != nil {
				return out, err
			}
		}
		out, err := x.callFunc(v.Name, args)
		switch v.Name {
		case "coalesce", "greatest", "least", "nullif":
			if err == nil {
				out, err = x.branchValue(v.Args, args, out)
			}
		}
		return out, err
	}
	return nil, errUnknownExpr{fmt.Sprintf("%T", e)}
}

// untypedPair resolves the operands of a comparison as Postgres resolves an
// untyped string literal or a parameter: to the type of the other operand
// when that is a number, so '01' = 1 holds. Any other text compared with a
// number is unsupported, as Postgres has no operator for it and fails the
// statement, which detest cannot do before it reaches a row.
func (x *sqlExec) untypedPair(le sqlir.Expr, l any, re sqlir.Expr, r any) (any, any, error) {
	if x.tx.db.kind.InnoDB() {
		return l, r, nil // MySQL compares a string with a number as the number (mysqlOperands)
	}
	l, r = paramText(le, l), paramText(re, r)
	// Rows are compared pair by pair in compareRows; one reaching here comes
	// from a context that does not, such as HAVING, CASE or NULLIF.
	if _, ok := l.([]any); ok {
		return nil, nil, x.unsupported("row comparison")
	}
	if _, ok := r.([]any); ok {
		return nil, nil, x.unsupported("row comparison")
	}
	// A cast to text is text by its type even when its value is NULL, which
	// Postgres refuses to compare with a number before any value is seen.
	// A parameter there takes the text type, as it does against any text.
	_, lParam := le.(*sqlir.Param)
	_, rParam := re.(*sqlir.Param)
	if textCast(le) && isNumber(r) && !rParam || isNumber(l) && !lParam && textCast(re) {
		return nil, nil, x.unsupported("a comparison of text with a number")
	}
	// A parameter compared with text is sent as text, and the text a driver
	// formats a float, a boolean or a time as is not modeled, unlike an
	// integer's.
	if unmodeledTextParam(le, l, r) || unmodeledTextParam(re, r, l) {
		return nil, nil, x.unsupported("a parameter other than text or an integer compared with text")
	}
	// A char(n) compares without its padding, so 'abc ' equals the 'abc' a
	// char(3) holds, where text compares the space. The value does not tell
	// the two columns apart, so a literal ending in a space is refused
	// against text.
	if trailingSpaceLiteral(le) && isText(r) || trailingSpaceLiteral(re) && isText(l) {
		return nil, nil, x.unsupported("a string literal ending in a space compared with text, which a char(n) column compares without it")
	}
	// With neither side typed, as in $1 = '01', Postgres compares text, so a
	// number the parameter holds is compared as the text it is sent as.
	if untypedExpr(le) && untypedExpr(re) {
		return asText(l), asText(r), nil
	}
	l, err := x.untyped(le, l, r)
	if err != nil {
		return nil, nil, err
	}
	r, err = x.untyped(re, r, l)
	if err != nil {
		return nil, nil, err
	}
	_, lp := le.(*sqlir.Param)
	_, rp := re.(*sqlir.Param)
	// A parameter compared with text takes the text type, so only a value of
	// another expression makes the comparison an error.
	if !lp && !rp && (isText(l) && isNumber(r) || isNumber(l) && isText(r)) {
		return nil, nil, x.unsupported("a comparison of text with a number")
	}
	// Boolean and timestamp columns hold their own values, so text against
	// one comes from a text expression, which Postgres has no operator for.
	if !lp && !rp && (isText(l) && isOther(r) || isOther(l) && isText(r)) {
		return nil, nil, x.unsupported("a comparison of text with a boolean or a time")
	}
	// Postgres sorts NaN above every number, which the comparisons do not.
	if isNaN(l) || isNaN(r) {
		return nil, nil, x.unsupported("a comparison with NaN")
	}
	// Postgres has no operator comparing a number with a boolean or a time
	// either, and a parameter holding one fails to be sent as a number.
	if isNumber(l) && isOther(r) || isOther(l) && isNumber(r) {
		return nil, nil, x.unsupported("a comparison of a number with a value of another type")
	}
	return l, r, nil
}

// compareRows compares two rows as Postgres does: = and <> pair by pair in
// three-valued logic, and an ordering by the first pair that is not equal,
// which is NULL when that pair holds a NULL.
func (x *sqlExec) compareRows(op string, le []sqlir.Expr, lv any, re []sqlir.Expr, rv any) (any, error) {
	l, lok := lv.([]any)
	r, rok := rv.([]any)
	if !lok || !rok || len(l) != len(le) || len(r) != len(re) {
		return nil, x.unsupported("row comparison")
	}
	eq := op == "=" || op == "<>" || op == "!="
	if !eq && x.tx.db.kind.InnoDB() {
		return nil, x.unsupported("ordered row comparison")
	}
	// Every pair is typed before any is compared, as Postgres resolves the
	// operators of all the pairs when it plans the statement, so a text
	// against a number in a later pair fails whatever the first one holds.
	ls, rs := make([]any, len(l)), make([]any, len(r))
	for i := range l {
		li, ri, err := x.untypedPair(le[i], l[i], re[i], r[i])
		if err != nil {
			return nil, err
		}
		ls[i], rs[i] = x.comparable(li, ri)
	}
	sawNull := false
	for i := range l {
		li, ri := ls[i], rs[i]
		if derefValue(li) == nil || derefValue(ri) == nil {
			if !eq {
				return nil, nil
			}
			sawNull = true
			continue
		}
		if equalValues(li, ri) {
			continue
		}
		if eq {
			return op != "=", nil
		}
		return x.binary(op, li, ri)
	}
	if sawNull {
		return nil, nil
	}
	return op == "=" || op == "<=" || op == ">=", nil
}

// parseBool reads text as Postgres's boolean input does: true, yes, on or 1,
// false, no, off or 0, or a prefix of a word that tells it apart, in any
// case and with surrounding spaces.
func parseBool(s string) (bool, bool) {
	t := strings.ToLower(strings.TrimSpace(s))
	switch {
	case t == "":
		return false, false
	case strings.HasPrefix("true", t), strings.HasPrefix("yes", t), t == "on", t == "1":
		return true, true
	case strings.HasPrefix("false", t), strings.HasPrefix("no", t), len(t) >= 2 && strings.HasPrefix("off", t), t == "0":
		return false, true
	}
	return false, false
}

// paramText returns a parameter's bytes as the text they are, so they compare
// and order as text rather than as a byte slice.
func paramText(e sqlir.Expr, v any) any {
	if _, ok := e.(*sqlir.Param); ok {
		if b, ok := derefValue(v).([]byte); ok {
			return string(b)
		}
	}
	return v
}

func textCast(e sqlir.Expr) bool {
	c, ok := e.(*sqlir.Cast)
	return ok && (c.Type == "text" || c.Type == "varchar" || c.Type == "bpchar")
}

func unmodeledTextParam(e sqlir.Expr, v, other any) bool {
	if _, ok := e.(*sqlir.Param); !ok || derefValue(v) == nil || isText(v) || !isText(other) {
		return false
	}
	_, isInt := integer(derefValue(v))
	return !isInt
}

func untypedExpr(e sqlir.Expr) bool {
	switch e := e.(type) {
	case *sqlir.Param:
		return true
	case *sqlir.Const:
		_, ok := e.Value.(string)
		return ok
	}
	return false
}

func asText(v any) any {
	if isNumber(v) {
		return fmt.Sprint(derefValue(v))
	}
	return v
}

func isNaN(v any) bool {
	f, ok := toFloat(derefValue(v))
	return ok && math.IsNaN(f)
}

// isOther reports whether v is a value that is neither NULL, text nor a
// number, such as a boolean or a time.
func isOther(v any) bool {
	return derefValue(v) != nil && !isText(v) && !isNumber(v)
}

func isText(v any) bool {
	switch derefValue(v).(type) {
	case string, []byte:
		return true
	}
	return false
}

func trailingSpaceLiteral(e sqlir.Expr) bool {
	k, ok := e.(*sqlir.Const)
	if !ok {
		return false
	}
	s, ok := k.Value.(string)
	return ok && strings.HasSuffix(s, " ")
}

func isTemporal(v any) bool {
	switch derefValue(v).(type) {
	case time.Time, time.Duration:
		return true
	}
	return false
}

func isFloat(v any) bool {
	switch derefValue(v).(type) {
	case float64, float32:
		return true
	}
	return false
}

func isNumber(v any) bool {
	v = derefValue(v)
	if _, ok := v.(time.Duration); ok {
		return false // an interval, though its kind is an integer
	}
	_, ok := toFloat(v)
	return ok
}

func (x *sqlExec) untyped(e sqlir.Expr, v, other any) (any, error) {
	if x.tx.db.kind.InnoDB() {
		return v, nil // MySQL compares a string with a number as the number (mysqlOperands)
	}
	var s string
	switch e := e.(type) {
	case *sqlir.Const:
		k, ok := e.Value.(string)
		if !ok {
			return v, nil
		}
		s = k
	case *sqlir.Param:
		switch k := derefValue(v).(type) {
		case string:
			s = k
		case []byte:
			s = string(k)
		case int64:
			// An integer bound to a parameter Postgres infers as boolean
			// reaches it as the text the driver sends, so active = $1
			// with 1 is true and with 2 fails as boolean input does.
			if _, isBool := derefValue(other).(bool); !isBool {
				return v, nil
			}
			s = strconv.FormatInt(k, 10)
		default:
			return v, nil
		}
	default:
		return v, nil
	}
	switch derefValue(other).(type) {
	case bool:
		b, ok := parseBool(s)
		if !ok {
			return nil, x.tx.db.kind.Error(sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type boolean: %q", s), "", "", "")
		}
		return b, nil
	case time.Time:
		// Postgres reads the text as the other side's timestamp type, in
		// the session's time zone when it has none, and a value does not
		// tell timestamp from timestamptz.
		return nil, x.unsupported("a string literal or parameter compared with a timestamp")
	}
	if !isNumber(other) {
		return v, nil
	}
	if isOtherNumberText(s) {
		return nil, x.unsupported("number text in a form detest does not model, compared with a number")
	}
	t := strings.TrimSpace(s)
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		return n, nil
	}
	if _, err := parseNumber(t); err == nil {
		// Postgres refuses '1.5' for an integer and compares it with a
		// float8 or numeric, but values do not carry their column's type,
		// and a float8 column keeps a whole number written as one as an
		// integer.
		return nil, x.unsupported("a string literal or parameter with a fraction compared with a number")
	}
	typ := "integer"
	if _, ok := derefValue(other).(float64); ok {
		typ = "double precision"
	}
	return nil, x.tx.db.kind.Error(sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type %s: %q", typ, s), "", "", "")
}

func (x *sqlExec) binary(op string, l, r any) (any, error) {
	switch op {
	case "=", "<>", "!=", "<", "<=", ">", ">=", "LIKE", "ILIKE", "NOT LIKE", "NOT ILIKE":
		if derefValue(l) == nil || derefValue(r) == nil {
			return nil, nil // a comparison with NULL is NULL
		}
	}
	switch op {
	case "=", "<>", "!=", "<", "<=", ">", ">=", "<=>":
		l, r = x.comparable(l, r)
	}
	if _, ok := derefValue(l).([]any); ok && (op == "<" || op == "<=" || op == ">" || op == ">=") {
		return nil, x.unsupported("ordered row comparison")
	}
	_, lRow := derefValue(l).([]any)
	if _, rRow := derefValue(r).([]any); rRow && !lRow {
		return nil, x.unsupported("comparison of a value with a row")
	}
	if la, ok := derefValue(l).([]any); ok && op == "<=>" {
		ra, ok := derefValue(r).([]any)
		if !ok || len(la) != len(ra) {
			return nil, x.unsupported("row comparison of different shapes")
		}
		for i := range la {
			eq, err := x.binary("<=>", la[i], ra[i])
			if err != nil {
				return nil, err
			}
			if eq != true {
				return false, nil
			}
		}
		return true, nil
	}
	if la, ok := derefValue(l).([]any); ok && (op == "=" || op == "<>" || op == "!=") {
		ra, ok := derefValue(r).([]any)
		if !ok || len(la) != len(ra) {
			return nil, x.unsupported("row comparison of different shapes")
		}
		eq, unknown := x.rowsEqual(la, ra)
		if unknown {
			return nil, nil
		}
		return eq == (op == "="), nil
	}
	switch op {
	case "<=>":
		// MySQL's null-safe equality, each operand evaluated once.
		if derefValue(l) == nil || derefValue(r) == nil {
			return derefValue(l) == nil && derefValue(r) == nil, nil
		}
		return equalValues(l, r), nil
	case "=":
		return equalValues(l, r), nil
	case "<>", "!=":
		return !equalValues(l, r), nil
	case "<", "<=", ">", ">=":
		c, ok := compareValues(l, r)
		if !ok {
			return false, nil
		}
		switch op {
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		default:
			return c >= 0, nil
		}
	case "LIKE", "ILIKE", "NOT LIKE", "NOT ILIKE":
		m, dangling := likeMatch(l, r, strings.HasSuffix(op, "ILIKE"))
		if dangling && !x.tx.db.kind.InnoDB() {
			return nil, x.unsupported("LIKE pattern ending with an escape, which Postgres refuses")
		}
		if strings.HasPrefix(op, "NOT") {
			return !m, nil
		}
		return m, nil
	case "+", "-", "*", "/", "%":
		if x.tx.db.kind.InnoDB() {
			// MySQL's arithmetic takes a string as the number it starts with.
			l, r = mysqlArithOperand(l), mysqlArithOperand(r)
		}
		v, err := arith(op, l, r)
		if ke := (kindError{}); errors.As(err, &ke) && ke.kind == sqlir.DivisionByZero && x.selectStmt && x.tx.db.kind.InnoDB() {
			// MySQL's strict mode refuses a division by zero in a write
			// only; a SELECT gets NULL and a warning.
			return nil, nil
		}
		if ke := (kindError{}); errors.As(err, &ke) {
			return nil, x.tx.db.kind.Error(ke.kind, ke.msg, "", "", "")
		}
		return v, err
	case "||":
		if derefValue(l) == nil || derefValue(r) == nil {
			return nil, nil // NULL || x is NULL
		}
		// text || anything concatenates, but Postgres has no || without a
		// text operand, as for two numbers or two booleans, and fails the
		// statement.
		if !isText(l) && !isText(r) {
			return nil, errUnknownExpr{"|| without a text operand"}
		}
		// As a cast to text, a timestamp or an interval is written by
		// Postgres's own rules, not Go's, and bytes are a bytea, which ||
		// concatenates as bytes.
		if isTemporal(l) || isTemporal(r) {
			return nil, errUnknownExpr{"|| of a timestamp or an interval"}
		}
		if _, ok := derefValue(l).([]byte); ok {
			return nil, errUnknownExpr{"|| of a bytea"}
		}
		if _, ok := derefValue(r).([]byte); ok {
			return nil, errUnknownExpr{"|| of a bytea"}
		}
		// As a cast to text, the text of a numeric or a float is not the
		// float's.
		if isFloat(l) || isFloat(r) {
			return nil, errUnknownExpr{"|| of a numeric or a float"}
		}
		return fmt.Sprint(derefValue(l)) + fmt.Sprint(derefValue(r)), nil
	}
	return nil, errUnknownExpr{"operator " + op}
}

// arith handles numeric arithmetic and timestamp/interval arithmetic
// (intervals are time.Duration).
func arith(op string, l, r any) (any, error) {
	l, r = derefValue(l), derefValue(r)
	if l == nil || r == nil {
		return nil, nil
	}
	if t, ok := l.(time.Time); ok {
		switch rv := r.(type) {
		case time.Duration:
			if op == "+" {
				return t.Add(rv), nil
			}
			if op == "-" {
				return t.Add(-rv), nil
			}
		case time.Time:
			if op == "-" {
				return t.Sub(rv), nil
			}
		}
		return nil, errUnknownExpr{"timestamp arithmetic " + op}
	}
	if d, ok := l.(time.Duration); ok {
		switch rv := r.(type) {
		case time.Duration:
			if op == "+" {
				return d + rv, nil
			}
			if op == "-" {
				return d - rv, nil
			}
		case time.Time:
			if op == "+" {
				return rv.Add(d), nil
			}
		}
		if f, ok := toFloat(r); ok && op == "*" {
			return time.Duration(float64(d) * f), nil
		}
		return nil, errUnknownExpr{"interval arithmetic " + op}
	}
	if il, ok := integer(l); ok {
		if ir, ok := integer(r); ok {
			return intArith(op, il, ir)
		}
	}
	fl, okl := toFloat(l)
	fr, okr := toFloat(r)
	if !okl || !okr {
		return nil, errUnknownExpr{"arithmetic on non-numeric values"}
	}
	var v float64
	switch op {
	case "+":
		v = fl + fr
	case "-":
		v = fl - fr
	case "*":
		v = fl * fr
	case "/":
		if fr == 0 {
			return nil, kindError{sqlir.DivisionByZero, "division by zero"}
		}
		v = fl / fr
	case "%":
		if fr == 0 {
			return nil, kindError{sqlir.DivisionByZero, "division by zero"}
		}
		v = math.Mod(fl, fr)
	}
	// A float or numeric operand makes a float result even when it is whole,
	// so a later division is not integer division ((1.5 * 2) / 2 is 1.5).
	return v, nil
}

// commonNumber returns v, the result of one of exprs, as the type Postgres
// gives all of them: a float when one of them is a float, as CASE WHEN ...
// THEN 1 ELSE 1.5 END is a numeric, so a later division is not integer
// division. Only literals and casts show their type before they run.
// branchValue is the value one branch of CASE, COALESCE, GREATEST, LEAST or
// NULLIF gives, as the common type of all the branches: a number as
// commonNumber gives it, and text as a number when another branch is typed as
// one, which is how Postgres reads an untyped literal or a parameter there,
// such as the '2' of CASE WHEN ... THEN 1 ELSE '2' END. The number's type is
// the branches' common one, an integer unless one is a numeric or a float by
// its form or, in vals, by the value it holds in this row, and text that
// reads as no value of it fails as its input does. Text of a branch typed as
// text, a column or a cast, has no common type with a number in Postgres,
// which fails the statement whatever row it reads, so it is refused. MySQL
// converts between the two instead.
func (x *sqlExec) branchValue(exprs []sqlir.Expr, vals []any, v any) (any, error) {
	if x.tx.db.kind.InnoDB() {
		return commonNumber(exprs, v), nil
	}
	// A column's type shows only in its value, so a float there makes the
	// integer of another branch a float, as 1 ELSE amount is a numeric; a
	// row where the column is NULL leaves the integer, as the type is not
	// kept.
	if n, ok := integer(derefValue(v)); ok && slices.ContainsFunc(vals, isFloat) {
		return float64(n), nil
	}
	if !slices.ContainsFunc(exprs, numberTyped) {
		return commonNumber(exprs, v), nil
	}
	refuse := x.unsupported("text and a number among the branches of CASE, COALESCE, GREATEST, LEAST or NULLIF")
	float := slices.ContainsFunc(exprs, floatTyped) || slices.ContainsFunc(vals, isFloat)
	// The branches' types are resolved before any is evaluated, so a
	// branch typed as text, or a literal that reads as no number, fails
	// the statement whichever branch a row takes.
	for _, e := range exprs {
		if textCast(e) {
			return nil, refuse
		}
		if k, ok := e.(*sqlir.Const); ok {
			if s, ok := k.Value.(string); ok {
				if _, err := x.branchNumber(s, float); err != nil {
					return nil, err
				}
			}
		}
	}
	s, ok := derefValue(v).(string)
	if !ok {
		return commonNumber(exprs, v), nil
	}
	for _, e := range exprs {
		switch e.(type) {
		case *sqlir.Const, *sqlir.Param:
		default:
			if !numberTyped(e) {
				return nil, refuse
			}
		}
	}
	n, err := x.branchNumber(s, float)
	if err != nil {
		return nil, err
	}
	return commonNumber(exprs, n), nil
}

// columnBranches evaluates the branches of a CASE that are column references,
// whose values tell their columns' types, in the row en; the other branches,
// which Postgres does not evaluate either, are left nil.
func (x *sqlExec) columnBranches(exprs []sqlir.Expr, en *env) []any {
	vals := make([]any, len(exprs))
	for i, e := range exprs {
		if _, ok := e.(*sqlir.ColumnRef); ok {
			vals[i], _ = x.eval(e, en)
		}
	}
	return vals
}

// branchNumber reads a string literal among number branches as the branches'
// type does: an integer, or a numeric when float.
func (x *sqlExec) branchNumber(s string, float bool) (any, error) {
	if isOtherNumberText(s) {
		return nil, x.unsupported("number text in a form detest does not model among the branches of CASE or COALESCE")
	}
	if !float {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, x.tx.db.kind.Error(sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type integer: %q", s), "", "", "")
		}
		return n, nil
	}
	f, err := parseNumber(s)
	if err != nil {
		return nil, x.tx.db.kind.Error(sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type numeric: %q", s), "", "", "")
	}
	return numericValue(f), nil
}

// numberTyped reports whether e is a number by its own form, before any value
// is seen: a number literal, or an expression floatTyped knows.
func numberTyped(e sqlir.Expr) bool {
	if k, ok := e.(*sqlir.Const); ok && isNumber(k.Value) {
		return true
	}
	return floatTyped(e)
}

func commonNumber(exprs []sqlir.Expr, v any) any {
	n, ok := integer(derefValue(v))
	if !ok || !slices.ContainsFunc(exprs, floatTyped) {
		return v
	}
	return float64(n)
}

func floatTyped(e sqlir.Expr) bool {
	switch e := e.(type) {
	case *sqlir.Const:
		_, ok := e.Value.(float64)
		return ok
	case *sqlir.Cast:
		switch e.Type {
		case "numeric", "float8", "float4", "double precision", "real":
			return true
		}
	case *sqlir.UnaryExpr:
		return e.Op == "-" && floatTyped(e.X)
	case *sqlir.BinaryExpr:
		switch e.Op {
		case "+", "-", "*", "/", "%":
			return floatTyped(e.L) || floatTyped(e.R)
		}
	case *sqlir.CaseExpr:
		return slices.ContainsFunc(caseBranches(e), floatTyped)
	case *sqlir.FuncCall:
		switch e.Name {
		case "floor", "ceil", "ceiling", "round", "avg", "power", "pow", "random":
			return true
		case "coalesce", "greatest", "least", "nullif", "abs", "sum", "min", "max":
			return slices.ContainsFunc(e.Args, floatTyped)
		}
	}
	return false
}

func caseBranches(c *sqlir.CaseExpr) []sqlir.Expr {
	out := make([]sqlir.Expr, 0, len(c.Whens)+1)
	for _, w := range c.Whens {
		out = append(out, w.Then)
	}
	if c.Else != nil {
		out = append(out, c.Else)
	}
	return out
}

// numberKind is the number type an expression has by its own form: a
// literal with a point is a numeric, a cast says its type, and arithmetic is
// a float when one operand is. Nothing is known of a column or a function.
type numberKind int

const (
	unknownKind numberKind = iota
	numericKind
	floatKind
)

func exprNumberKind(e sqlir.Expr) numberKind {
	switch e := e.(type) {
	case *sqlir.Const:
		if isNumber(e.Value) {
			return numericKind
		}
	case *sqlir.Cast:
		switch e.Type {
		case "numeric":
			return numericKind
		case "float8", "float4", "double precision", "real":
			return floatKind
		}
	case *sqlir.FuncCall:
		switch e.Name {
		case "mysql_double":
			return floatKind
		case "mysql_dividend":
			return exprNumberKind(e.Args[0])
		}
	case *sqlir.UnaryExpr:
		if e.Op == "-" {
			return exprNumberKind(e.X)
		}
	case *sqlir.BinaryExpr:
		switch e.Op {
		case "+", "-", "*", "/", "%":
			l, r := exprNumberKind(e.L), exprNumberKind(e.R)
			switch {
			case l == floatKind || r == floatKind:
				return floatKind
			case l == numericKind && r == numericKind:
				return numericKind
			}
		}
	}
	return unknownKind
}

// rowsEqual compares two rows of values with SQL equality: unequal if any
// pair differs, unknown if none does but a NULL is among them.
func (x *sqlExec) rowsEqual(a, b []any) (eq, unknown bool) {
	for i := range a {
		l, r := x.comparable(a[i], b[i])
		switch {
		case derefValue(l) == nil || derefValue(r) == nil:
			unknown = true
		case !equalValues(l, r):
			return false, false
		}
	}
	return !unknown, unknown
}

// callFunc evaluates the scalar functions that appear on control paths.
// pg_try_advisory_xact_lock is a non-blocking lock held until the end of the
// transaction, modeled in the DB's lock table.
// strictFuncs are the functions detest evaluates that Postgres declares
// strict, given NULL, they return NULL without being called, with the
// numbers of arguments their signatures take.
var strictFuncs = map[string][]int{
	"lower": {1}, "upper": {1}, "length": {1}, "char_length": {1}, "hashtext": {1},
	"abs": {1}, "floor": {1}, "ceil": {1}, "ceiling": {1}, "round": {1, 2}, "power": {2}, "pow": {2},
	"nextval": {1}, "setval": {2, 3}, "pg_advisory_xact_lock": {1, 2}, "pg_try_advisory_xact_lock": {1, 2},
	"octet_length": {1}, "left": {2}, "mysql_signed": {1}, "mysql_double": {1}, "mysql_dividend": {1},
}

// mysqlNullIfAnyNull are MySQL's functions of any number of arguments that
// are NULL when one is, where the executor's Postgres ones skip NULLs.
var mysqlNullIfAnyNull = map[string]bool{"mysql_concat": true, "mysql_greatest": true, "mysql_least": true}

// roundDecimal is round(x, n) on the decimal x was written as, half away from
// zero as Postgres's numeric rounds: on the float, f*10^n is off by a
// little, so round(-81.865, 2) would come out -81.86.
func roundDecimal(f float64, n int) float64 {
	// Past these, a float64 keeps its value or becomes 0, which also bounds
	// the power of ten below for a scale taken from SQL.
	switch {
	case n > 30:
		return f
	case n < -330:
		return 0
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'f', -1, 64))
	if !ok {
		return f
	}
	scale := new(big.Rat).SetFrac(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(max(n, -n))), nil), big.NewInt(1))
	if n >= 0 {
		r.Mul(r, scale)
	} else {
		r.Quo(r, scale)
	}
	// Half away from zero: add or subtract 1/2, then truncate.
	half := big.NewRat(1, 2)
	if r.Sign() < 0 {
		r.Sub(r, half)
	} else {
		r.Add(r, half)
	}
	q := new(big.Int).Quo(r.Num(), r.Denom())
	r.SetInt(q)
	if n >= 0 {
		r.Quo(r, scale)
	} else {
		r.Mul(r, scale)
	}
	out, _ := r.Float64()
	return out
}

// checkArity refuses a call to a function detest knows that no signature of
// it takes, before its arguments are evaluated, as Postgres resolves the
// signature when it plans the statement.
// otherArity are the numbers of arguments of functions detest evaluates
// that are not strict. current_timestamp takes the one precision the
// converter passes for CURRENT_TIMESTAMP(p), which SQL cannot call itself.
var otherArity = map[string][]int{
	"now": {0}, "clock_timestamp": {0}, "transaction_timestamp": {0}, "statement_timestamp": {0},
	"current_timestamp": {1}, "random": {0}, "nullif": {2},
	"gen_random_uuid": {0}, "uuid_generate_v4": {0},
}

func (x *sqlExec) checkArity(f *sqlir.FuncCall) error {
	if what := arityMismatch(f); what != "" {
		return x.unsupported(what)
	}
	if !x.tx.db.kind.InnoDB() {
		if what := textArgumentMismatch(f, nil); what != "" {
			return x.unsupported(what)
		}
	}
	return nil
}

// arityMismatch names what is wrong with a call's arguments, or returns ""
// when the call has the arguments its function takes. The statement alone
// decides it, so CheckSQL asks it too.
func arityMismatch(f *sqlir.FuncCall) string {
	if f.Star && f.Name != "count" {
		// now(*): no function but count takes a star.
		return f.Name + "(*)"
	}
	arity, known := strictFuncs[f.Name]
	if !known {
		arity, known = otherArity[f.Name]
	}
	if known && !slices.Contains(arity, len(f.Args)) {
		return fmt.Sprintf("%s with %d arguments", f.Name, len(f.Args))
	}
	// round with a scale exists only for numeric; detest keeps no type for
	// its argument but can see a cast to a float or a call of random.
	// Refusing every argument not proven numeric would refuse round(price,
	// 2) on a numeric column too.
	if f.Name == "round" && len(f.Args) == 2 {
		c, cast := f.Args[0].(*sqlir.Cast)
		r, call := f.Args[0].(*sqlir.FuncCall)
		if cast && (c.Type == "float4" || c.Type == "float8") || call && r.Name == "random" {
			return "round of a float with a scale"
		}
	}
	return ""
}

func (x *sqlExec) callFunc(name string, args []any) (any, error) {
	// database/sql lets a string argument come as []byte, which the
	// functions would otherwise print as a list of numbers.
	for i, a := range args {
		if b, ok := derefValue(a).([]byte); ok {
			args[i] = string(b)
		}
	}
	if !knownFunc(name, x.tx.db.kind.InnoDB()) {
		// Refused here so that knownFuncs and mysqlFuncs, which CheckSQL
		// reads, are the one list of what runs; a name the switch below
		// lacks is refused too.
		return nil, errUnknownExpr{name + "(...)"}
	}
	if _, strict := strictFuncs[name]; (strict || mysqlNullIfAnyNull[name]) && slices.ContainsFunc(args, func(a any) bool { return derefValue(a) == nil }) {
		return nil, nil // a strict function of NULL is NULL
	}
	if x.tx.db.kind.InnoDB() {
		switch name {
		case "mysql_greatest", "mysql_least":
			// MySQL compares a mix of numbers and strings as strings, in
			// its own formatting of the numbers, which the executor's
			// pairwise comparison does not follow.
			str := slices.ContainsFunc(args, func(a any) bool { _, ok := derefValue(a).(string); return ok })
			num := slices.ContainsFunc(args, func(a any) bool { _, ok := toFloat(derefValue(a)); return ok })
			if str && num {
				return nil, x.unsupported(strings.ToUpper(strings.TrimPrefix(name, "mysql_")) + " of numbers and strings")
			}
			return x.callFunc(strings.TrimPrefix(name, "mysql_"), args)
		case "mysql_concat":
			// MySQL's, NULL when an argument is (strictFuncs), and
			// otherwise the executor's.
			return x.callFunc(strings.TrimPrefix(name, "mysql_"), args)
		case "abs", "floor", "ceil", "power":
			for i, a := range args {
				args[i] = mysqlArithOperand(a) // a string as the number it converts to
			}
		case "round":
			if _, ok := derefValue(args[0]).(string); ok {
				// The string converts to a DOUBLE, which MySQL rounds half
				// to even, where the executor rounds half away from zero.
				return nil, x.unsupported("ROUND of a string")
			}
		case "mysql_nullif":
			// NULLIF compares as MySQL's = does.
			if derefValue(args[0]) == nil || derefValue(args[1]) == nil {
				return args[0], nil
			}
			if equalValues(x.comparable(args[0], args[1])) {
				return nil, nil
			}
			return args[0], nil
		case "mysql_signed":
			return mysqlSigned(derefValue(args[0])), nil
		case "mysql_double", "mysql_dividend":
			if f, ok := toFloat(derefValue(mysqlArithOperand(args[0]))); ok {
				return f, nil
			}
			if name == "mysql_dividend" {
				return nil, x.unsupported("division of a " + fmt.Sprintf("%T", derefValue(args[0])))
			}
			return nil, x.unsupported("CAST to DOUBLE of a " + fmt.Sprintf("%T", derefValue(args[0])))
		case "mysql_truth":
			return mysqlTruth(derefValue(args[0])), nil
		case "last_insert_id":
			if x.tx.lastInsertID == nil {
				return int64(0), nil
			}
			return *x.tx.lastInsertID, nil
		}
	}
	d := func(i int) any {
		if i < len(args) {
			return derefValue(args[i])
		}
		return nil
	}
	switch name {
	case "coalesce":
		for i := range args {
			if d(i) != nil {
				return args[i], nil
			}
		}
		return nil, nil
	case "nullif":
		if len(args) == 2 && equalValues(args[0], args[1]) {
			return nil, nil
		}
		return args[0], nil
	case "greatest", "least":
		var best any
		for i := range args {
			v := d(i)
			if v == nil {
				continue
			}
			if best == nil {
				best = v
				continue
			}
			c, _ := compareValues(v, best)
			if (name == "greatest" && c > 0) || (name == "least" && c < 0) {
				best = v
			}
		}
		return best, nil
	case "left":
		if d(0) == nil || d(1) == nil {
			return nil, nil
		}
		n, ok := integer(d(1))
		if !ok {
			if !x.tx.db.kind.InnoDB() {
				return nil, x.unsupported("left with a length other than an integer")
			}
			n, _ = integer(mysqlSigned(d(1))) // MySQL converts the length
		}
		r := []rune(fmt.Sprint(d(0)))
		if n < 0 && !x.tx.db.kind.InnoDB() {
			n += int64(len(r)) // Postgres: all but the last -n characters
		}
		if int(n) < len(r) {
			r = r[:max(n, 0)]
		}
		return string(r), nil
	case "lower":
		return strings.ToLower(fmt.Sprint(d(0))), nil
	case "upper":
		return strings.ToUpper(fmt.Sprint(d(0))), nil
	case "length", "char_length":
		return int64(len([]rune(fmt.Sprint(d(0))))), nil
	case "octet_length":
		return int64(len(fmt.Sprint(d(0)))), nil
	case "concat":
		var b strings.Builder
		for i := range args {
			if v := d(i); v != nil {
				fmt.Fprint(&b, v)
			}
		}
		return b.String(), nil
	case "hashtext":
		return fmt.Sprint(d(0)), nil
	case "nextval":
		// A sequence is not transactional: a rolled back nextval stays used.
		if len(args) != 1 {
			return nil, x.unsupported("nextval with other than one argument")
		}
		if name := x.tx.db.seqName(fmt.Sprint(derefValue(args[0]))); x.tx.db.isRelation(name) {
			return nil, x.tx.db.notSequence(name)
		} else if err := x.tx.db.missingSequence(name); err != nil {
			return nil, err
		}
		v, refused := x.tx.db.nextval(fmt.Sprint(derefValue(args[0])))
		if refused != "" {
			return nil, x.unsupported(refused)
		}
		return v, nil
	case "setval":
		if len(args) < 2 || len(args) > 3 {
			return nil, x.unsupported("setval with other than two or three arguments")
		}
		if derefValue(args[0]) == nil || derefValue(args[1]) == nil {
			return nil, nil // strict: NULL in, NULL out
		}
		v, ok := toInt64(derefValue(args[1]))
		if !ok {
			return nil, x.unsupported("setval to a non-integer")
		}
		called := true
		if len(args) == 3 {
			called, _ = derefValue(args[2]).(bool)
		}
		if name := x.tx.db.seqName(fmt.Sprint(derefValue(args[0]))); x.tx.db.isRelation(name) {
			return nil, x.tx.db.notSequence(name)
		} else if err := x.tx.db.missingSequence(name); err != nil {
			return nil, err
		}
		x.tx.db.setval(fmt.Sprint(derefValue(args[0])), v, called)
		return v, nil
	case "gen_random_uuid", "uuid_generate_v4":
		return x.tx.db.newUUID(), nil
	case "now", "clock_timestamp", "current_timestamp", "transaction_timestamp", "statement_timestamp":
		// Postgres fixes now() at the start of the transaction, which a
		// transaction that starts early and commits late depends on.
		ts := x.tx.start
		if x.tx.db.kind.InnoDB() {
			ts = x.start // MySQL's NOW() is the statement's time
		}
		switch name {
		case "clock_timestamp":
			ts = time.Now()
		case "statement_timestamp":
			ts = x.start
		}
		if ts.IsZero() {
			ts = time.Now()
		}
		ts = ts.Truncate(time.Microsecond) // a Postgres timestamp has six fractional digits
		if x.tx.db.kind.InnoDB() {
			// MySQL's NOW(fsp) keeps fsp fractional digits, none without one.
			p := 0.0
			if len(args) > 0 {
				f, ok := toFloat(d(0))
				if !ok || f < 0 || f > 6 || f != math.Trunc(f) {
					return nil, x.unsupported(name + " with a precision other than 0 to 6")
				}
				p = f
			}
			return ts.Truncate(time.Duration(math.Pow10(9 - int(p)))), nil
		}
		if len(args) == 0 {
			return ts, nil
		}
		// Only CURRENT_TIMESTAMP(p) and LOCALTIMESTAMP(p) take an argument,
		// which the converter passes as current_timestamp's.
		p, ok := toFloat(d(0))
		if name != "current_timestamp" || len(args) > 1 || !ok || p < 0 {
			return nil, x.unsupported(name + " with these arguments")
		}
		// Postgres reduces a precision above 6 to 6, with a warning.
		return ts.Round(time.Duration(math.Pow10(9 - int(min(p, 6))))), nil
	case "random":
		return 0.5, nil
	case "abs":
		if f, ok := toFloat(d(0)); ok {
			return asKindOf(d(0), math.Abs(f)), nil
		}
	case "floor":
		if f, ok := toFloat(d(0)); ok {
			return math.Floor(f), nil
		}
	case "ceil", "ceiling":
		if f, ok := toFloat(d(0)); ok {
			return math.Ceil(f), nil
		}
	case "round":
		f, ok := toFloat(d(0))
		if !ok {
			break
		}
		// floor, ceil and round have no integer form in Postgres, so even
		// floor(3) is a numeric, and floor(3) / 2 is 1.5.
		if len(args) == 1 {
			return math.Round(f), nil
		}
		places, ok := toFloat(d(1))
		if !ok {
			break
		}
		if places != math.Trunc(places) || places < math.MinInt32 || places > math.MaxInt32 {
			// round(numeric, integer) is the only signature with two, and
			// its scale is an int4.
			return nil, x.unsupported("round with a scale that is not an integer")
		}
		return roundDecimal(f, int(places)), nil
	case "power", "pow":
		a, oka := toFloat(d(0))
		b, okb := toFloat(d(1))
		if oka && okb {
			return math.Pow(a, b), nil
		}
	case "make_interval":
		// make_interval(secs => x) is the form that appears in backoff SQL; a
		// single argument is taken as seconds.
		if f, ok := toFloat(d(0)); ok {
			return time.Duration(f * float64(time.Second)), nil
		}
	case "pg_try_advisory_xact_lock", "pg_advisory_xact_lock":
		var kb strings.Builder
		for i := range args {
			valuesKey(&kb, d(i))
		}
		lk := lockKey{"__advisory__", kb.String()}
		if name == "pg_try_advisory_xact_lock" && x.tx.heldByOther(lk, lockUpdate) {
			return false, nil
		}
		if err := x.tx.lock(lk); err != nil {
			return nil, err
		}
		if name == "pg_advisory_xact_lock" {
			return nil, nil
		}
		return true, nil
	}
	return nil, errUnknownExpr{name + "(...)"}
}

// exprString renders a predicate for the trace with parameter values filled in.
func (x *sqlExec) exprString(e sqlir.Expr) string {
	switch v := e.(type) {
	case nil:
		return "true"
	case *sqlir.ColumnRef:
		if v.Table != "" {
			return v.Table + "." + v.Column
		}
		return v.Column
	case *sqlir.Param:
		if v.Index >= 0 && v.Index < len(x.args) {
			return fmt.Sprint(x.args[v.Index])
		}
		return "?"
	case *sqlir.Const:
		return fmt.Sprint(v.Value)
	case *sqlir.BinaryExpr:
		// An operand that is itself an operation of another operator, or
		// the right one of the same, is grouped, so that (n + 1) * 2 does
		// not read as n + 1 * 2.
		l, r := x.exprString(v.L), x.exprString(v.R)
		if b, ok := v.L.(*sqlir.BinaryExpr); ok && b.Op != v.Op {
			l = "(" + l + ")"
		}
		if _, ok := v.R.(*sqlir.BinaryExpr); ok {
			r = "(" + r + ")"
		}
		return l + " " + strings.ToLower(v.Op) + " " + r
	case *sqlir.UnaryExpr:
		if _, ok := v.X.(*sqlir.BinaryExpr); ok {
			return strings.ToLower(v.Op) + " (" + x.exprString(v.X) + ")"
		}
		return strings.ToLower(v.Op) + " " + x.exprString(v.X)
	case *sqlir.IsNull:
		if v.Not {
			return x.exprString(v.X) + " is not null"
		}
		return x.exprString(v.X) + " is null"
	case *sqlir.InExpr:
		var items []string
		for _, it := range v.List {
			items = append(items, x.exprString(it))
		}
		rhs := "(" + strings.Join(items, ", ") + ")"
		if v.Sub != nil {
			rhs = "(subquery)"
		}
		if v.Not {
			return x.exprString(v.X) + " not in " + rhs
		}
		return x.exprString(v.X) + " in " + rhs
	case *sqlir.ArrayCmp:
		op := " = any "
		if v.All {
			op = " <> all "
		}
		return x.exprString(v.X) + op + "(" + x.exprString(v.Array) + ")"
	case *sqlir.Cast:
		return x.exprString(v.X)
	case *sqlir.FuncCall:
		return v.Name + "(...)"
	case *sqlir.Exists:
		return "exists (subquery)"
	case *sqlir.SubQuery:
		return "(subquery)"
	case *sqlir.RowExpr:
		var items []string
		for _, it := range v.Items {
			items = append(items, x.exprString(it))
		}
		return "(" + strings.Join(items, ", ") + ")"
	}
	return "..."
}

// kindError is a database error raised where the server is not at hand, such
// as in arithmetic; eval turns it into the server's DBError.
type kindError struct {
	kind sqlir.DBErrorKind
	msg  string
}

func (e kindError) Error() string { return "detest: " + e.msg }

// intArith is integer arithmetic as Postgres does it on bigint: overflow is an
// error rather than a wrap, and division truncates toward zero.
func intArith(op string, l, r int64) (any, error) {
	overflow := kindError{sqlir.ArithmeticOutOfRange, "bigint out of range"}
	switch op {
	case "+":
		v := l + r
		if (r > 0 && v < l) || (r < 0 && v > l) {
			return nil, overflow
		}
		return v, nil
	case "-":
		v := l - r
		if (r < 0 && v < l) || (r > 0 && v > l) {
			return nil, overflow
		}
		return v, nil
	case "*":
		if l == 0 || r == 0 {
			return int64(0), nil
		}
		v := l * r
		if v/r != l || (l == -1 && r == math.MinInt64) || (r == -1 && l == math.MinInt64) {
			return nil, overflow
		}
		return v, nil
	case "/", "%":
		if r == 0 {
			return nil, kindError{sqlir.DivisionByZero, "division by zero"}
		}
		if l == math.MinInt64 && r == -1 {
			if op == "%" {
				return int64(0), nil
			}
			return nil, overflow
		}
		if op == "/" {
			return l / r, nil
		}
		return l % r, nil
	}
	return nil, errUnknownExpr{"integer operator " + op}
}
