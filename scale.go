package detest

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// scaledNum is a Postgres numeric with the scale it reads back with, which
// Postgres keeps in each value: 1.50 is written with two places and 1.5
// with one, though they are equal. v is the int64 or float64 detest
// computes with, and every helper that reads a value (derefValue, toFloat,
// integer, keyString) sees v, so that a numeric computes and compares as
// before. scale is -1 where detest cannot tell it, and reading such a value
// back is refused.
//
// The scale rides on the value rather than on the expression's type, as a
// column of numeric without a scale keeps the one each value was written
// with, and sum, min, max and the branches of COALESCE and CASE take theirs
// from the rows they reach.
type scaledNum struct {
	v     any
	scale int
}

func (n scaledNum) String() string { return fmt.Sprint(n.v) }

// unscaled is v without its scale.
func unscaled(v any) any {
	if n, ok := v.(scaledNum); ok {
		return n.v
	}
	return v
}

// withScale is the number v as a numeric of the given scale, on Postgres.
// NULL, a value that is no number and a numRange are left as they are: a
// range is refused wherever its value shows.
func (x *sqlExec) withScale(v any, scale int) any {
	if x.tx.db.kind.InnoDB() || derefValue(v) == nil || isRange(v) || !isNumber(v) {
		return v
	}
	if scale < -1 {
		scale = -1
	}
	v = derefValue(v)
	if n, ok := v.(int64); ok && scale != 0 && math.Abs(float64(n)) < 1<<53 {
		// As a float, the text casts, || and a row's text refuse it as
		// they refuse any numeric with places, rather than write the
		// integer without them.
		v = float64(n)
	}
	return scaledNum{v, scale}
}

// operand kinds of numeric arithmetic, by the value an operand gave.
const (
	opUntyped = iota // NULL, text, or a parameter, whose type the other operand gives
	opInt
	opNumeric
	opFloat
)

// operandScale is the kind of the value v of e as an operand of numeric
// arithmetic, and the scale it has as a numeric. A float that is not a
// parameter is a float8, which makes the arithmetic float arithmetic.
func operandScale(e sqlir.Expr, v any) (int, int) {
	switch n := v.(type) {
	case scaledNum:
		return opNumeric, n.scale
	case numRange:
		return opNumeric, -1
	}
	switch n := derefValue(v).(type) {
	case nil:
		return opUntyped, 0
	case string:
		return opUntyped, textScale(n)
	case []byte:
		return opUntyped, textScale(string(n))
	case float64, float32:
		f, _ := toFloat(n)
		if _, ok := e.(*sqlir.Param); ok {
			return opUntyped, floatScale(f)
		}
		return opFloat, floatScale(f)
	}
	if _, ok := integer(derefValue(v)); ok {
		return opInt, 0
	}
	return opFloat, -1
}

// textScale is the scale numeric input gives the text s: the digits after
// the point, less the exponent, and -1 for text that is no decimal number.
func textScale(s string) int {
	s = strings.TrimSpace(s)
	if !decimalText.MatchString(s) {
		return -1
	}
	exp := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil {
			return -1
		}
		exp, s = e, s[:i]
	}
	frac := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		frac = len(s) - i - 1
	}
	return max(frac-exp, 0)
}

// floatScale is the scale of the float f as a numeric: the places of the
// shortest text that reads as f, which pq and pgx send for a float64
// parameter. A float8 converts to numeric with 15 significant digits
// instead, which a float of more digits does not keep, so its scale is
// unknown.
func floatScale(f float64) int {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return -1
	}
	mant := strconv.FormatFloat(f, 'e', -1, 64)
	if i := strings.IndexByte(mant, 'e'); i >= 0 {
		mant = mant[:i]
	}
	if len(strings.TrimLeft(strings.ReplaceAll(mant, ".", ""), "-")) > 15 {
		return -1
	}
	return textScale(strconv.FormatFloat(f, 'f', -1, 64))
}

// arithScaled is out, the result of the arithmetic e on the operands l and
// r, as a numeric of the scale Postgres gives it: the larger of the
// operands' for + - and %, their sum for *, and select_div_scale's for /.
// Arithmetic on integers alone or on a float is left as it is.
func (x *sqlExec) arithScaled(e *sqlir.BinaryExpr, l, r, out any) any {
	switch e.Op {
	case "+", "-", "*", "/", "%":
	default:
		return out
	}
	if x.tx.db.kind.InnoDB() || derefValue(out) == nil || isRange(out) {
		return out
	}
	lk, ls := operandScale(e.L, l)
	rk, rs := operandScale(e.R, r)
	if lk == opFloat || rk == opFloat || (lk != opNumeric && rk != opNumeric) {
		return out
	}
	if ls < 0 || rs < 0 {
		return x.withScale(out, -1)
	}
	switch e.Op {
	case "*":
		return x.withScale(out, ls+rs)
	case "/":
		a, aok := exactRat(l)
		b, bok := exactRat(r)
		if !aok || !bok {
			return x.withScale(out, -1)
		}
		return x.withScale(out, divScale(a, b, ls, rs))
	}
	return x.withScale(out, max(ls, rs))
}

// divScale is the scale Postgres gives the quotient of a and b, of the
// scales as and bs (select_div_scale): at least 16 significant digits,
// counted from the quotient's estimated weight in base-10000 digits, and no
// fewer places than either operand has.
func divScale(a, b *big.Rat, as, bs int) int {
	w1, d1 := baseDigit(a)
	w2, d2 := baseDigit(b)
	qweight := w1 - w2
	if d1 <= d2 {
		qweight--
	}
	return min(max(16-qweight*4, as, bs, 0), 1000)
}

// baseDigit is the weight and the value of the first nonzero digit of q in
// base 10000, as Postgres stores a numeric, and 0, 0 for zero.
func baseDigit(q *big.Rat) (int, int64) {
	if q.Sign() == 0 {
		return 0, 0
	}
	a := new(big.Rat).Abs(q)
	if ip := new(big.Int).Quo(a.Num(), a.Denom()); ip.Sign() > 0 {
		s := ip.String()
		w := (len(s) - 1) / 4
		d, _ := strconv.ParseInt(s[:len(s)-w*4], 10, 64)
		return w, d
	}
	unit := big.NewRat(10000, 1)
	for w := -1; ; w-- {
		a.Mul(a, unit)
		if g := new(big.Int).Quo(a.Num(), a.Denom()); g.Sign() > 0 {
			return w, g.Int64()
		}
	}
}

// castScaled is out, the cast c gave of in, as a numeric of the scale the
// cast gives it: the type modifier's, or the input's.
func (x *sqlExec) castScaled(c *sqlir.Cast, in, out any) (any, error) {
	if x.tx.db.kind.InnoDB() || x.tx.db.baseType(c.Type) != "numeric" || derefValue(out) == nil {
		return out, nil
	}
	if c.Precision > 0 {
		n, err := scaledNumeric(derefValue(out), numLimit{precision: c.Precision, scale: c.Scale})
		if err != nil {
			return nil, x.tx.db.kind.Error(sqlir.NumericValueOutOfRange, "numeric field overflow", "", "", "")
		}
		return x.withScale(n, c.Scale), nil
	}
	_, s := operandScale(c.X, in)
	return x.withScale(out, s), nil
}

// funcScaled is out, what the function f gave for the arguments args, as a
// numeric of the scale Postgres gives it, where an argument is a numeric or
// the function returns one. A function of a numeric this does not know
// gives a numeric of unknown scale.
func (x *sqlExec) funcScaled(f *sqlir.FuncCall, args []any, out any) any {
	if x.tx.db.kind.InnoDB() || derefValue(out) == nil || isRange(out) || !isNumber(out) {
		return out
	}
	numericArg := false
	for _, a := range args {
		if _, ok := a.(scaledNum); ok {
			numericArg = true
		}
	}
	switch f.Name {
	case "coalesce", "greatest", "least", "nullif":
		return x.branchScaled(f, f.Args, args, out)
	case "abs":
		if n, ok := args[0].(scaledNum); ok {
			return x.withScale(out, n.scale)
		}
		return out
	case "round", "trunc":
		if len(args) == 2 {
			if k, _ := operandScale(f.Args[0], args[0]); k == opFloat {
				return out
			}
			n, ok := integer(derefValue(args[1]))
			if !ok {
				return x.withScale(out, -1)
			}
			return x.withScale(out, int(max(min(n, 1000), 0)))
		}
		if numericArg {
			return x.withScale(out, 0)
		}
		return out
	case "floor", "ceil", "ceiling":
		if numericArg {
			return x.withScale(out, 0)
		}
		return out
	}
	if numericArg {
		return x.withScale(out, -1)
	}
	return out
}

// branchScaled is out, the value of the branches exprs of e that gave
// vals, as a numeric of the scale of the branch it came from, where the
// branches resolve to a numeric.
func (x *sqlExec) branchScaled(e sqlir.Expr, exprs []sqlir.Expr, vals []any, out any) any {
	if x.tx.db.kind.InnoDB() || derefValue(out) == nil || isRange(out) || !isNumber(out) {
		return out
	}
	numeric := x.numberBranchType(e) == "numeric"
	for i, v := range vals {
		if derefValue(v) == nil {
			continue
		}
		k, _ := operandScale(exprs[i], v)
		if k == opFloat {
			return out
		}
		if k == opNumeric {
			numeric = true
		}
	}
	if !numeric {
		return out
	}
	// Of branches equal in value and apart in scale (1.5 and 1.50), the
	// first is taken, where GREATEST and LEAST keep the one their
	// comparison loop happens to keep. That is not followed, as such a tie
	// needs one value written twice with other places.
	for i, v := range vals {
		if derefValue(v) != nil && sameValue(v, out) {
			_, s := operandScale(exprs[i], v)
			return x.withScale(out, s)
		}
	}
	return x.withScale(out, -1)
}

// aggScaled is out, the aggregate name over the values vals, as a numeric
// of the scale Postgres gives it: sum's is the largest of the values', and
// avg's that of the sum divided by the count. min and max give one of the
// values as it is.
func (x *sqlExec) aggScaled(name string, vals []any, out any) any {
	if (name != "sum" && name != "avg") || x.tx.db.kind.InnoDB() || derefValue(out) == nil || isRange(out) {
		return out
	}
	numeric, scale := name == "avg", 0
	total := new(big.Rat)
	for _, v := range vals {
		k, s := operandScale(nil, v)
		switch k {
		case opFloat:
			return out
		case opNumeric:
			numeric = true
		}
		if s < 0 {
			scale = -1
		} else if scale >= 0 {
			scale = max(scale, s)
		}
		if q, ok := exactRat(v); ok && total != nil {
			total.Add(total, q)
		} else {
			total = nil
		}
	}
	switch {
	case !numeric:
		return out
	case scale < 0 || (name == "avg" && total == nil):
		return x.withScale(out, -1)
	case name == "avg":
		return x.withScale(out, divScale(total, new(big.Rat).SetInt64(int64(len(vals))), scale, 0))
	}
	return x.withScale(out, scale)
}

// storedScaled is n, the number written to the numeric column col of def
// from the value v, as the column keeps it: at the column's scale, or at
// v's where the column has none.
func storedScaled(def *tableDef, col string, v, n any) any {
	if l, ok := def.nums[col]; ok {
		return scaledNum{derefValue(n), l.scale}
	}
	// A float written from no numeric is a parameter, which the drivers
	// send as its shortest text, or a float8, which converts as that text
	// where it has no more than 15 digits.
	_, s := operandScale(&sqlir.Param{}, v)
	return scaledNum{derefValue(n), s}
}

// scaledText is n written with scale places, as Postgres writes a numeric,
// and false when n has more places than that, which a numeric of the scale
// never holds.
func scaledText(n any, scale int) (string, bool) {
	var s string
	switch v := n.(type) {
	case int64:
		s = strconv.FormatInt(v, 10)
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return "", false
		}
		if v == 0 {
			v = 0 // a numeric has no negative zero
		}
		s = strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return "", false
	}
	frac := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		frac = len(s) - i - 1
	}
	if frac > scale {
		return "", false
	}
	if scale > 0 && frac == 0 {
		s += "."
	}
	return s + strings.Repeat("0", scale-frac), true
}

// chosenScaled is out, the value CASE e gave from the branch chosen, which
// gave raw, as a numeric of that branch's scale where the branches resolve
// to a numeric.
func (x *sqlExec) chosenScaled(e sqlir.Expr, chosen sqlir.Expr, raw, out any) any {
	if x.tx.db.kind.InnoDB() || derefValue(out) == nil || isRange(out) || !isNumber(out) {
		return out
	}
	k, s := operandScale(chosen, raw)
	if k == opFloat || (k != opNumeric && x.numberBranchType(e) != "numeric") {
		return out
	}
	return x.withScale(out, s)
}
