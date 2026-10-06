package detest

import (
	"cmp"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func derefValue(v any) any {
	switch v.(type) {
	case nil:
		return nil
	case string, int64, int, bool, float64, []byte:
		return v // the common column types, without reflection
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	return rv.Interface()
}

// equalValues implements SQL equality: a comparison involving NULL is not
// true. Use sameValue where NULLs must compare equal (identity, conflicts).
func equalValues(a, b any) bool {
	a, b = derefValue(a), derefValue(b)
	if a == nil || b == nil {
		return false
	}
	return sameValue(a, b)
}

// sameRow reports whether two rows hold the same values.
// A column missing from a row is NULL, as a row omits the nullable columns
// an insert did not give.
func sameRow(a, b Row) bool {
	for k, v := range a {
		// '1 mon' and '30 days' are equal but move a time to other days,
		// so a row changed from one to the other is a change.
		if x, ok := derefValue(v).(pgInterval); ok {
			if y, ok := derefValue(b[k]).(pgInterval); ok && x != y {
				return false
			}
		}
		if !sameValue(v, b[k]) {
			return false
		}
	}
	for k, w := range b {
		if _, ok := a[k]; !ok && derefValue(w) != nil {
			return false
		}
	}
	return true
}

// sameValue compares two values treating NULL as equal to NULL.
func sameValue(a, b any) bool {
	a, b = derefValue(a), derefValue(b)
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if ta, ok := a.(time.Time); ok {
		if tb, ok := b.(time.Time); ok {
			return ta.Equal(tb)
		}
	}
	if ta, ok := a.(instant); ok {
		if tb, ok := b.(instant); ok {
			return ta.Equal(tb.Time)
		}
	}
	if ia, ok := a.(pgInterval); ok {
		if ib, ok := b.(pgInterval); ok {
			return ia.span() == ib.span()
		}
	}
	if ba, ok := a.([]byte); ok {
		a = string(ba)
	}
	if bb, ok := b.([]byte); ok {
		b = string(bb)
	}
	// Numbers are equal by value, as 1000000 and 1000000.0 format apart.
	if isNumber(a) && isNumber(b) {
		c, _ := compareValues(a, b)
		return c == 0
	}
	if reflect.DeepEqual(a, b) {
		return true
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func compareValues(a, b any) (int, bool) {
	a, b = derefValue(a), derefValue(b)
	if a == nil || b == nil {
		return 0, false
	}
	if ta, ok := a.(time.Time); ok {
		tb, ok := b.(time.Time)
		if !ok {
			return 0, false
		}
		return ta.Compare(tb), true
	}
	if ta, ok := a.(instant); ok {
		tb, ok := b.(instant)
		if !ok {
			return 0, false
		}
		return ta.Compare(tb.Time), true
	}
	if ia, ok := a.(pgInterval); ok {
		ib, ok := b.(pgInterval)
		if !ok {
			return 0, false
		}
		return cmp.Compare(ia.span(), ib.span()), true
	}
	// Postgres takes NaN as equal to NaN and greater than every other
	// number, so ORDER BY and min/max place it as Postgres does.
	if na, nb := isNaNValue(a), isNaNValue(b); na || nb {
		if !isNumber(a) || !isNumber(b) {
			return 0, false
		}
		switch {
		case na && nb:
			return 0, true
		case na:
			return 1, true
		}
		return -1, true
	}
	// Two integers are compared as integers, and an integer with a float
	// exactly, as a float64 cannot tell apart integers above 2^53, such as
	// adjacent snowflake IDs.
	if ia, ok := integer(a); ok {
		if ib, ok := integer(b); ok {
			return cmp.Compare(ia, ib), true
		}
		if fb, ok := b.(float64); ok {
			return new(big.Float).SetInt64(ia).Cmp(big.NewFloat(fb)), true
		}
	}
	if fa, ok := a.(float64); ok {
		if ib, ok := integer(b); ok {
			return big.NewFloat(fa).Cmp(new(big.Float).SetInt64(ib)), true
		}
	}
	fa, oka := toFloat(a)
	fb, okb := toFloat(b)
	if oka && okb {
		switch {
		case fa < fb:
			return -1, true
		case fa > fb:
			return 1, true
		}
		return 0, true
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)), true
}

// isOtherNumberText reports number text in a form Postgres reads for some
// types but detest does not model: a non-decimal integer (0x10, 0o17, 0b101)
// or a hexadecimal float (0x1p2), and digits grouped with underscores (1_000).
// Postgres takes or refuses each by the target type, so detest refuses them
// all rather than answer for one type as for another.
// Text that is not a number at all, such as 'not_a_number', is left to fail
// as invalid input.
var (
	prefixedNumberText   = regexp.MustCompile(`(?i)^\s*[+-]?0[xob][0-9a-f_.p+-]*\s*$`)
	underscoreNumberText = regexp.MustCompile(`(?i)^\s*[+-]?[0-9]+(_[0-9]+)*(\.([0-9]+(_[0-9]+)*)?)?(e[+-]?[0-9]+(_[0-9]+)*)?\s*$`)
)

func isOtherNumberText(v any) bool {
	var s string
	switch t := derefValue(v).(type) {
	case string:
		s = t
	case []byte:
		s = string(t)
	default:
		return false
	}
	return prefixedNumberText.MatchString(s) || strings.Contains(s, "_") && underscoreNumberText.MatchString(s)
}

var (
	decimalText = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?$`)
	specialText = regexp.MustCompile(`(?i)^[+-]?(inf|infinity|nan)$`)
)

// parseNumber reads text in decimal or scientific notation, or as a special
// value, the forms Postgres's float and numeric input both take. The others
// strconv.ParseFloat takes are refused before (isOtherNumberText).
func parseNumber(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if !decimalText.MatchString(s) && !specialText.MatchString(s) {
		return 0, strconv.ErrSyntax
	}
	return strconv.ParseFloat(s, 64)
}

// valueKey is a key equal values share, for grouping, DISTINCT, set
// operations and window partitions: numbers by value whatever their Go type,
// as sameValue compares them, and values of different kinds apart.
func valueKey(v any) string {
	v = derefValue(v)
	switch t := v.(type) {
	case nil:
		return "N"
	case string:
		return "s" + strconv.Quote(t)
	case []byte:
		return "s" + strconv.Quote(string(t))
	case bool:
		if t {
			return "b1"
		}
		return "b0"
	case time.Time:
		return "t" + t.UTC().Format(time.RFC3339Nano)
	case instant:
		return "z" + t.UTC().Format(time.RFC3339Nano)
	case pgInterval:
		// Equal intervals, '1 mon' and '30 days', are one key, as in an
		// index or a DISTINCT.
		return "i" + strconv.FormatInt(t.span(), 10)
	}
	if isNumber(v) {
		if n, ok := integer(v); ok {
			return "n" + strconv.FormatInt(n, 10)
		}
		if f, ok := toFloat(v); ok {
			if math.IsNaN(f) {
				return "nNaN"
			}
			return "n" + keyString(f)
		}
	}
	return fmt.Sprintf("%T%#v", v, v)
}

// valuesKey is valueKey of several values.
func valuesKey(b *strings.Builder, v any) {
	b.WriteString(valueKey(v))
	b.WriteByte(0x1f)
}

func isNaNValue(v any) bool {
	f, ok := toFloat(v)
	return ok && math.IsNaN(f)
}

func toFloat(v any) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}

// likeMatch reports whether v matches a LIKE pattern. A pattern that ends
// with the escape is an error in Postgres (dangling is true) and a literal
// backslash in MySQL.
func likeMatch(v any, pattern any, caseInsensitive bool) (match, dangling bool) {
	v, pattern = derefValue(v), derefValue(pattern)
	if v == nil || pattern == nil {
		return false, false
	}
	var b strings.Builder
	b.WriteString("^")
	escaped := false
	for _, r := range fmt.Sprint(pattern) {
		if escaped {
			// A backslash, the default escape of MySQL and Postgres alike,
			// makes the next character itself, % and _ included.
			b.WriteString(regexp.QuoteMeta(string(r)))
			escaped = false
			continue
		}
		switch r {
		case '\\':
			escaped = true
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	if escaped {
		b.WriteString(regexp.QuoteMeta("\\"))
		dangling = true
	}
	b.WriteString("$")
	expr := b.String()
	if caseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return false, dangling
	}
	return re.MatchString(fmt.Sprint(v)), dangling
}

// valueKind names the kind of a value as the types of a set operation's
// column tell them apart: text, number, boolean, time or other, and nothing
// for NULL.
func valueKind(v any) string {
	v = derefValue(v)
	switch {
	case v == nil:
		return ""
	case isText(v):
		return "text"
	case isNumber(v):
		return "number"
	}
	switch v.(type) {
	case bool:
		return "boolean"
	case time.Time, instant:
		return "time"
	}
	return "other"
}
