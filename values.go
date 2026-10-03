package detest

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
)

func derefValue(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr {
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
	if ba, ok := a.([]byte); ok {
		a = string(ba)
	}
	if bb, ok := b.([]byte); ok {
		b = string(bb)
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

func likeMatch(v any, pattern any, caseInsensitive bool) bool {
	v, pattern = derefValue(v), derefValue(pattern)
	if v == nil || pattern == nil {
		return false
	}
	var b strings.Builder
	b.WriteString("^")
	for _, r := range fmt.Sprint(pattern) {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	expr := b.String()
	if caseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return false
	}
	return re.MatchString(fmt.Sprint(v))
}
