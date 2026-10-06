package detest

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
)

// This file converts values between SQL types the way the server does where
// the application can observe the result: CAST, the rounding of a value that
// ends in .5, and the refusals of casts detest cannot reproduce.

// roundHalf is round(v) when v ends in .5, which Postgres rounds away from
// zero for a numeric and to even for a float: round(2.5) is 3 and
// round(2.5::float8) is 2. The value does not tell the two apart, so one of
// a form exprNumberKind does not know is refused. ok is false for any other
// value, which callFunc rounds.
func (x *sqlExec) roundHalf(e sqlir.Expr, v any) (any, bool, error) {
	f, isNum := toFloat(derefValue(v))
	if x.tx.db.kind.InnoDB() || !isNum || math.Abs(f-math.Trunc(f)) != 0.5 {
		return nil, false, nil
	}
	switch exprNumberKind(e) {
	case numericKind:
		return math.Round(f), true, nil
	case floatKind:
		return math.RoundToEven(f), true, nil
	}
	return nil, true, x.unsupported("round of a number ending in .5 whose type detest does not know")
}

// halfToInteger rounds a value ending in .5 before a cast to an integer, by
// the form of the expression cast, as roundHalf does; castInteger refuses the
// value when the form tells nothing.
func (x *sqlExec) halfToInteger(c *sqlir.Cast, v any) any {
	switch c.Type {
	case "int", "int2", "int4", "int8", "bigint", "integer", "smallint":
	default:
		return v
	}
	f, isNum := toFloat(derefValue(v))
	if x.tx.db.kind.InnoDB() || !isNum || !isNumber(derefValue(v)) || math.Abs(f-math.Trunc(f)) != 0.5 {
		return v
	}
	switch exprNumberKind(c.X) {
	case numericKind:
		return math.Round(f)
	case floatKind:
		return math.RoundToEven(f)
	}
	return v
}

// asKindOf returns f as an integer when v is one and as a float otherwise, so
// a function of a float or numeric keeps float arithmetic after it.
func asKindOf(v any, f float64) any {
	if _, ok := integer(derefValue(v)); ok {
		return numeric(f)
	}
	return f
}

func numeric(v float64) any {
	if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
		return int64(v)
	}
	return v
}

// byteaCast refuses a cast of a bytea value to a type other than bytea:
// Postgres has no cast from bytea to a number and writes it as hex text,
// while detest would read its bytes as text. A parameter's bytes are text
// the driver sent, which Postgres reads as the target type.
func (x *sqlExec) byteaCast(c *sqlir.Cast, v any) error {
	if _, ok := derefValue(v).([]byte); !ok || c.Type == "bytea" {
		return nil
	}
	if _, ok := c.X.(*sqlir.Param); ok {
		return nil
	}
	return x.unsupported("a cast of a bytea value to " + c.Type)
}

// boolCastSource refuses a cast to boolean of a value cast to bigint or
// smallint, which Postgres casts to boolean from integer only; the values
// of the three types are alike.
func (x *sqlExec) boolCastSource(c *sqlir.Cast) error {
	if c.Type != "bool" && c.Type != "boolean" {
		return nil
	}
	if inner, ok := c.X.(*sqlir.Cast); ok {
		switch inner.Type {
		case "int2", "int8", "smallint", "bigint":
			return x.unsupported("a cast of a " + inner.Type + " to boolean")
		}
	}
	return nil
}

// paramBool is an integer parameter cast to boolean as the text the driver
// sends it as, so that $1::bool with 2 fails as boolean input does, where
// 2::bool is true through the integer cast.
func paramBool(c *sqlir.Cast, v any) any {
	if _, ok := c.X.(*sqlir.Param); !ok || (c.Type != "bool" && c.Type != "boolean") {
		return v
	}
	if n, ok := integer(derefValue(v)); ok {
		return strconv.FormatInt(n, 10)
	}
	return v
}

// paramDate is a time bound to a parameter cast to date or timestamp as the
// value the driver sends for it (wallClock).
func paramDate(c *sqlir.Cast, v any) any {
	if _, ok := c.X.(*sqlir.Param); !ok {
		return v
	}
	return wallClock(c.Type, v)
}

// wallClock is a time bound for a date or a timestamp as the driver sends it
// once Postgres has typed the parameter: pgx takes the time's own year,
// month, day and clock, and lib/pq sends its text, whose zone a date's and a
// timestamp's input drop. A timestamptz is the instant, which detest keeps
// as it is, and so does a date or a timestamp's own time in UTC.
func wallClock(typ string, v any) any {
	t, ok := derefValue(v).(time.Time)
	if !ok {
		return v
	}
	switch typ {
	case "date":
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case "timestamp":
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	}
	return v
}

// paramTextCast refuses a cast to text of a parameter holding a value other
// than text or an integer, such as one ANY (ARRAY['x', $1]) makes: the text
// a driver sends for a float, a boolean or a time is not modeled.
func (x *sqlExec) paramTextCast(c *sqlir.Cast, v any) error {
	if _, ok := c.X.(*sqlir.Param); !ok || !textCast(c) || derefValue(v) == nil || isText(v) {
		return nil
	}
	if _, ok := integer(derefValue(v)); ok {
		return nil
	}
	return x.unsupported("a cast to text of a parameter other than text or an integer")
}

// cast is castValue with the error a cast of text that does not read as the
// type raises.
func (x *sqlExec) cast(v any, typ string) (any, error) {
	out, err := castValue(v, typ)
	if ke := (kindError{}); errors.As(err, &ke) {
		return nil, x.tx.db.kind.Error(ke.kind, ke.msg, "", "", "")
	}
	return out, err
}

// castInteger casts v to an integer before the target type's width is
// checked: text as the integer input function reads it, and a fraction
// rounded. nil is a value castValue leaves as it is.
func castInteger(v any) (any, error) {
	if s, ok := v.(string); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if errors.Is(err, strconv.ErrRange) {
			return nil, kindError{sqlir.NumericValueOutOfRange, fmt.Sprintf("value %q is out of range for type bigint", s)}
		}
		if err != nil {
			return nil, kindError{sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type integer: %q", s)}
		}
		return n, nil
	}
	if n, ok := integer(v); ok {
		return n, nil
	}
	if b, ok := v.(bool); ok {
		if b {
			return int64(1), nil
		}
		return int64(0), nil
	}
	// Postgres rounds a numeric half away from zero but a float8 half to
	// even, which the value does not tell apart, so a half is refused.
	if f, ok := toFloat(v); ok && isNumber(v) {
		if math.Abs(f-math.Trunc(f)) == 0.5 {
			return nil, errUnknownExpr{"a cast to an integer of a number ending in .5"}
		}
		if math.IsNaN(f) || math.Abs(f) >= 1<<63 {
			return nil, kindError{sqlir.NumericValueOutOfRange, "bigint out of range"}
		}
		return int64(math.Round(f)), nil
	}
	return nil, nil
}

func castValue(v any, typ string) (any, error) {
	v = derefValue(v)
	if v == nil {
		return nil, nil
	}
	// Bytes are read as text only by the casts whose input reads text, so
	// $1::bytea keeps its byte slice.
	if b, ok := v.([]byte); ok && typ != "bytea" {
		v = string(b)
	}
	switch typ {
	case "int", "int2", "int4", "int8", "bigint", "integer", "smallint", "numeric", "float8", "float4", "double precision", "real":
		if isOtherNumberText(v) {
			return nil, errUnknownExpr{"a cast of number text in a form detest does not model"}
		}
	}
	switch typ {
	case "int", "int2", "int4", "int8", "bigint", "integer", "smallint":
		// Postgres casts a boolean to integer only, not to bigint or
		// smallint.
		if _, ok := v.(bool); ok && typ != "int" && typ != "int4" && typ != "integer" {
			return nil, errUnknownExpr{"a cast of a boolean to " + typ}
		}
		n, err := castInteger(v)
		if err != nil {
			return nil, err
		}
		if n == nil {
			return nil, errUnknownExpr{fmt.Sprintf("a cast of a %T to an integer", v)}
		}
		lim, name := int64(math.MaxInt64), "bigint"
		switch typ {
		case "int2", "smallint":
			lim, name = math.MaxInt16, "smallint"
		case "int", "int4", "integer":
			lim, name = math.MaxInt32, "integer"
		}
		if i, _ := n.(int64); i > lim || i < -lim-1 {
			return nil, kindError{sqlir.NumericValueOutOfRange, name + " out of range"}
		}
		return n, nil
	case "numeric":
		// As a value written to a numeric column: refused when a float
		// cannot keep it, and in the one representation numerics have.
		if isText(v) {
			if !exactAsFloat(v) {
				return nil, errUnknownExpr{"a cast to numeric with more digits than a float keeps"}
			}
			n, err := columnNumber(v, "numeric")
			if err != nil {
				return nil, kindError{sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type numeric: %q", v)}
			}
			if isNaN(n) {
				return nil, errUnknownExpr{"a cast to NaN"}
			}
			return n, nil
		}
		if isNumber(v) {
			if !exactAsFloat(v) {
				return nil, errUnknownExpr{"a cast to numeric with more digits than a float keeps"}
			}
			if isNaN(v) {
				return nil, errUnknownExpr{"a cast to NaN"}
			}
			return numericValue(v), nil
		}
		return nil, errUnknownExpr{fmt.Sprintf("a cast of a %T to numeric", v)}
	case "float8", "float4", "double precision", "real":
		if s, ok := v.(string); ok {
			f, err := parseNumber(s)
			if errors.Is(err, strconv.ErrRange) {
				return nil, kindError{sqlir.NumericValueOutOfRange, fmt.Sprintf("%q is out of range for type %s", s, typ)}
			}
			if err != nil {
				return nil, kindError{sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type %s: %q", typ, s)}
			}
			// Postgres sorts NaN above every number, which the
			// comparisons and ORDER BY do not.
			if math.IsNaN(f) {
				return nil, errUnknownExpr{"a cast to NaN"}
			}
			return f, nil
		}
		if f, ok := toFloat(v); ok && isNumber(v) {
			if math.IsNaN(f) {
				return nil, errUnknownExpr{"a cast to NaN"}
			}
			return f, nil
		}
		return nil, errUnknownExpr{fmt.Sprintf("a cast of a %T to %s", v, typ)}
	case "text", "varchar", "bpchar":
		// A numeric's text keeps the digits it was written or computed
		// with (1.50, 3.0) and a float's is formatted by Postgres's own
		// rules, neither of which the float detest keeps gives.
		switch x := v.(type) {
		case float64, float32:
			return nil, errUnknownExpr{"a cast of a numeric or a float to text"}
		case time.Time:
			return nil, errUnknownExpr{"a cast of a timestamp to text"}
		case pgInterval:
			return x.String(), nil
		}
		return fmt.Sprint(v), nil
	case "bool", "boolean":
		// Text reads as boolean input does, and an integer is true unless
		// zero; Postgres has no cast from a numeric or a float.
		switch b := v.(type) {
		case bool:
			return b, nil
		case string:
			out, ok := parseBool(b)
			if !ok {
				return nil, kindError{sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type boolean: %q", b)}
			}
			return out, nil
		case int64:
			return b != 0, nil
		}
		return nil, errUnknownExpr{fmt.Sprintf("a cast of a %T to boolean", v)}
	case "uuid":
		// Read as a uuid column stores it, so it equals the column's value
		// however it was written. Postgres has no cast to uuid from a type
		// other than text.
		if u, ok := v.(uuidValue); ok {
			return u, nil
		}
		s, ok := v.(string)
		if !ok {
			return nil, errUnknownExpr{fmt.Sprintf("a cast of a %T to uuid", v)}
		}
		c, ok := canonicalUUID(s)
		if !ok {
			return nil, kindError{sqlir.InvalidTextRepresentation, fmt.Sprintf("invalid input syntax for type uuid: %q", s)}
		}
		return uuidValue(c), nil
	case "interval":
		switch v := v.(type) {
		case pgInterval:
			return v, nil
		case string:
			d, ierr := parseInterval(v)
			if ierr == nil {
				return d, nil
			}
			if ierr.malformed {
				return nil, kindError{sqlir.InvalidDatetimeFormat, fmt.Sprintf("invalid input syntax for type interval: %q", v)}
			}
			return nil, errUnknownExpr{ierr.what}
		}
		return nil, errUnknownExpr{fmt.Sprintf("a cast of a %T to interval", v)}
	case "timestamptz", "timestamp":
		// A time is kept in UTC, the session's TimeZone: an instant as a
		// timestamptz, and the clock a timestamptz shows there as a
		// timestamp. Text is left as it is, as before.
		switch v := v.(type) {
		case time.Time:
			return v.UTC(), nil
		case string:
			// Text without a zone stays text for a timestamptz, as a column
			// write refuses it: the server's TimeZone is not modeled there.
			if tm, hasZone, ok := pgParseTime(v); ok && (typ == "timestamp" || hasZone) {
				if typ == "timestamp" {
					return time.Date(tm.Year(), tm.Month(), tm.Day(), tm.Hour(), tm.Minute(), tm.Second(), tm.Nanosecond(), time.UTC), nil
				}
				return tm.UTC(), nil
			}
		}
	case "date":
		switch v := v.(type) {
		case time.Time:
			return utcDate(v), nil
		case string:
			tm, _, ok := pgParseTime(v)
			if !ok {
				return nil, errUnknownExpr{fmt.Sprintf("the date %q, in a format detest does not parse", v)}
			}
			// The date part as written: a date drops the time and any zone.
			return time.Date(tm.Year(), tm.Month(), tm.Day(), 0, 0, 0, 0, time.UTC), nil
		}
		return nil, errUnknownExpr{fmt.Sprintf("a cast of a %T to date", v)}
	}
	return v, nil
}
