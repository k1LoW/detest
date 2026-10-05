package detest

import (
	"math"
	"math/big"
	"strconv"

	"github.com/k1LoW/detest/internal/sqlir"
)

// numRange is the value of arithmetic on fractional numbers that the server
// may give otherwise than detest's float64s do. A DECIMAL or numeric value is
// kept as a float64, where the server computes with it exactly: 0.1 + 0.2 is
// 0.3 there and 0.30000000000000004 here. A value does not say whether its
// column is a DECIMAL or a DOUBLE, which the server computes with floats as
// detest does, nor, for MySQL, how many places a quotient is rounded to,
// four more than the dividend's scale. So such a result is kept as the range
// of values the server may give, lo to hi, holding flt, the result computed
// with float64s. A comparison, more arithmetic, or a write to a column that
// rounds to a scale goes on when every value in the range gives the same
// outcome, and any other use is refused. Carrying exact decimals through the
// engine instead would need the operand types, which values do not keep.
type numRange struct {
	lo, hi *big.Rat
	flt    float64
}

// errInexact refuses a numRange where the outcome would depend on which of
// its values the server gives.
func (x *sqlExec) errInexact() error {
	return x.unsupported("a DECIMAL or numeric result detest cannot compute exactly, used where the difference shows")
}

func isRange(v any) bool {
	_, ok := v.(numRange)
	return ok
}

// exactRat is v as the exact decimal a float64 prints as.
func exactRat(v any) (*big.Rat, bool) {
	v = derefValue(v)
	if n, ok := integer(v); ok {
		return new(big.Rat).SetInt64(n), true
	}
	f, ok := v.(float64)
	if !ok || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, false
	}
	return new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
}

// numBounds is v as a range: a number is itself, read as the decimal it
// prints as and as its float64.
func numBounds(v any) (numRange, bool) {
	if r, ok := v.(numRange); ok {
		return r, true
	}
	q, ok := exactRat(v)
	if !ok {
		return numRange{}, false
	}
	// A DOUBLE value takes part in float arithmetic as its exact binary
	// value, and in decimal arithmetic as the decimal it prints as.
	f, _ := toFloat(derefValue(v))
	return numRange{minRat(q, ratOf(f)), maxRat(q, ratOf(f)), f}, true
}

// rangeValue is the range lo to hi widened to hold flt, or flt itself when
// every reading gives it. The float stands for the decimal it prints as, so
// a single value is flt only when that decimal is the value: one with more
// digits than a float prints, though it rounds to flt, stays a range.
func rangeValue(lo, hi *big.Rat, flt float64) any {
	if math.IsInf(flt, 0) || math.IsNaN(flt) {
		return flt
	}
	if lo.Cmp(hi) == 0 {
		if q, ok := exactRat(flt); ok && q.Cmp(lo) == 0 {
			return flt
		}
	}
	// A later operation may take any value as the DOUBLE it converts to, as
	// mixing a DECIMAL with a DOUBLE does, so the range holds the floats of
	// its bounds too, which bound the floats of the values between them.
	fl, _ := lo.Float64()
	fh, _ := hi.Float64()
	return numRange{minRat(lo, ratOf(flt), ratOf(fl)), maxRat(hi, ratOf(flt), ratOf(fh)), flt}
}

// ratOf is the exact value of the float64 f.
func ratOf(f float64) *big.Rat { return new(big.Rat).SetFloat64(f) }

func minRat(r *big.Rat, more ...*big.Rat) *big.Rat {
	for _, m := range more {
		if m.Cmp(r) < 0 {
			r = m
		}
	}
	return r
}

func maxRat(r *big.Rat, more ...*big.Rat) *big.Rat {
	for _, m := range more {
		if m.Cmp(r) > 0 {
			r = m
		}
	}
	return r
}

// arithValue is the value of the arithmetic e computed as v from the plain
// numbers l and r: v, or a numRange when the server may give another value.
func (x *sqlExec) arithValue(e *sqlir.BinaryExpr, l, r, v any) any {
	switch e.Op {
	case "+", "-", "*", "/", "%":
	default:
		return v
	}
	f, ok := v.(float64)
	kind := exprNumberKind(e)
	if !ok || kind == floatKind {
		return v
	}
	a, aok := exactRat(l)
	b, bok := exactRat(r)
	if !aok || !bok || (b.Sign() == 0 && (e.Op == "/" || e.Op == "%")) {
		return v
	}
	q := new(big.Rat)
	switch e.Op {
	case "+":
		q.Add(a, b)
	case "-":
		q.Sub(a, b)
	case "*":
		q.Mul(a, b)
	case "/":
		q.Quo(a, b)
	case "%":
		q.Quo(a, b)
		q.Sub(a, new(big.Rat).Mul(b, new(big.Rat).SetInt(new(big.Int).Quo(q.Num(), q.Denom()))))
	}
	lo, hi := q, q
	if e.Op == "/" {
		lo, hi = x.quotientBounds(q)
	}
	if kind == numericKind {
		// The operands are decimals by their form, so the float result is
		// no value the server gives.
		f, _ = q.Float64()
	}
	return rangeValue(lo, hi, f)
}

// quotientBounds bounds the values the server gives for the exact quotient
// q. MySQL rounds a DECIMAL quotient to four places more than the dividend
// has, so q needing more than four is anywhere within half a unit of the
// fourth; Postgres keeps at least 16 significant digits of a numeric
// quotient.
func (x *sqlExec) quotientBounds(q *big.Rat) (*big.Rat, *big.Rat) {
	var d *big.Rat
	if x.tx.db.kind.InnoDB() {
		if scale, ok := decimalScale(q); ok && scale <= 4 {
			return q, q
		}
		d = big.NewRat(5, 100000)
	} else {
		if scale, ok := decimalScale(q); ok && scale <= 15 {
			return q, q
		}
		d = new(big.Rat).Abs(q)
		d.Quo(d, big.NewRat(1e15, 1))
	}
	return new(big.Rat).Sub(q, d), new(big.Rat).Add(q, d)
}

// decimalScale is the number of decimal places q needs, and whether it has
// a decimal form at all, which it has when its denominator divides a power
// of ten.
func decimalScale(q *big.Rat) (int, bool) {
	d := new(big.Int).Set(q.Denom())
	twos, fives := 0, 0
	two, five, rem := big.NewInt(2), big.NewInt(5), new(big.Int)
	for d.BitLen() > 1 {
		if rem.Mod(d, two).Sign() == 0 {
			d.Quo(d, two)
			twos++
			continue
		}
		if rem.Mod(d, five).Sign() == 0 {
			d.Quo(d, five)
			fives++
			continue
		}
		return 0, false
	}
	return max(twos, fives), true
}

// rangeBinary is l op r where one of them is a numRange: more arithmetic
// widens the range, and a comparison gives its outcome when every value in
// the ranges gives the same one.
func (x *sqlExec) rangeBinary(op string, l, r any) (any, error) {
	switch op {
	case "+", "-", "*", "/", "=", "<>", "!=", "<", "<=", ">", ">=":
	default:
		return nil, x.errInexact()
	}
	if derefValue(l) == nil || derefValue(r) == nil {
		return nil, nil
	}
	a, aok := numBounds(l)
	b, bok := numBounds(r)
	if !aok || !bok {
		return nil, x.errInexact()
	}
	switch op {
	case "+":
		return rangeValue(new(big.Rat).Add(a.lo, b.lo), new(big.Rat).Add(a.hi, b.hi), a.flt+b.flt), nil
	case "-":
		return rangeValue(new(big.Rat).Sub(a.lo, b.hi), new(big.Rat).Sub(a.hi, b.lo), a.flt-b.flt), nil
	case "*":
		lo, hi := spread(func(p, q *big.Rat) *big.Rat { return new(big.Rat).Mul(p, q) }, a, b)
		return rangeValue(lo, hi, a.flt*b.flt), nil
	case "/":
		if b.lo.Sign() <= 0 && b.hi.Sign() >= 0 {
			return nil, x.errInexact()
		}
		lo, hi := spread(func(p, q *big.Rat) *big.Rat { return new(big.Rat).Quo(p, q) }, a, b)
		lo, _ = x.quotientBounds(lo)
		_, hi = x.quotientBounds(hi)
		return rangeValue(lo, hi, a.flt/b.flt), nil
	}
	var c int
	switch {
	case a.hi.Cmp(b.lo) < 0:
		c = -1
	case a.lo.Cmp(b.hi) > 0:
		c = 1
	case a.lo.Cmp(a.hi) == 0 && b.lo.Cmp(b.hi) == 0 && a.lo.Cmp(b.lo) == 0:
		c = 0
	default:
		return nil, x.errInexact()
	}
	switch op {
	case "=":
		return c == 0, nil
	case "<>", "!=":
		return c != 0, nil
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	}
	return c >= 0, nil
}

// spread is the least and the greatest of f over the bounds of a and b,
// which bound f over the ranges for a product or a quotient.
func spread(f func(p, q *big.Rat) *big.Rat, a, b numRange) (*big.Rat, *big.Rat) {
	lo, hi := f(a.lo, b.lo), f(a.lo, b.lo)
	for _, v := range []*big.Rat{f(a.lo, b.hi), f(a.hi, b.lo), f(a.hi, b.hi)} {
		if v.Cmp(lo) < 0 {
			lo = v
		}
		if v.Cmp(hi) > 0 {
			hi = v
		}
	}
	return lo, hi
}

// columnValue is v as it is written to the column col of table: a numRange
// is the value every value in it rounds to at the column's scale, and is
// refused when they round apart or the column keeps no scale.
func (x *sqlExec) columnValue(table, col string, v any) (any, error) {
	r, ok := v.(numRange)
	if !ok {
		return v, nil
	}
	def := x.tx.db.defs[table]
	if def == nil {
		return nil, x.errInexact()
	}
	l, scaled := def.nums[col]
	if t := def.types[col]; !scaled || (t != "numeric" && t != "decimal") {
		return nil, x.errInexact()
	}
	lo, errLo := scaledRat(r.lo, l)
	hi, errHi := scaledRat(r.hi, l)
	switch {
	case errLo != nil && errHi != nil && r.lo.Sign() == r.hi.Sign():
		return r.flt, nil // out of range whichever value it is, as the write reports
	case errLo != nil || errHi != nil || lo.Cmp(hi) != 0:
		return nil, x.errInexact()
	}
	f, _ := lo.Float64()
	return f, nil
}
