package detest

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// This file runs x = ANY (a) and x <> ALL (a) over an array given as a value:
// a parameter bound to a Go slice or to array text such as pq.Array sends, or
// a string literal. Each element stands in the comparison as the scalar it
// is, a parameter or a string literal, so it is typed against x by the rules
// x = $1 already follows.

// arrayParam is a Go slice bound to a parameter, its elements converted as
// database/sql converts a scalar argument.
type arrayParam []any

// CheckNamedValue keeps a Go slice bound to a Postgres parameter, which
// database/sql's own conversion refuses, as pgx's stdlib driver keeps it for
// x = ANY ($1). lib/pq refuses it there and takes pq.Array, whose array text
// reaches detest as a string either way. Nothing else changes: any other
// value, a driver.Valuer such as pq.Array or a []byte included, takes
// database/sql's conversion.
func (c *sqlConn) CheckNamedValue(nv *driver.NamedValue) error {
	if c.db.kind.InnoDB() {
		return driver.ErrSkip
	}
	if _, ok := nv.Value.(driver.Valuer); ok {
		return driver.ErrSkip
	}
	rv := reflect.ValueOf(nv.Value)
	if rv.Kind() != reflect.Slice || rv.Type().Elem().Kind() == reflect.Uint8 {
		return driver.ErrSkip
	}
	if rv.IsNil() {
		nv.Value = nil // pgx sends a nil slice as NULL
		return nil
	}
	elems := make(arrayParam, rv.Len())
	for i := range elems {
		v, err := driver.DefaultParameterConverter.ConvertValue(rv.Index(i).Interface())
		if err != nil {
			return fmt.Errorf("detest: element %d of argument $%d: %w", i, nv.Ordinal, err)
		}
		elems[i] = v
	}
	nv.Value = elems
	return nil
}

// arrayElems are the elements of the array a comparison reads, each as the
// expression it stands as against x and its value.
type arrayElems struct {
	exprs []sqlir.Expr
	vals  []any
	null  bool // the array is NULL
}

// arrayElems reads the array of c once per statement. Postgres reads it
// before the statement runs, so a malformed array, or an element its cast
// refuses, fails the statement whatever rows it meets; checkArrays reads
// every one up front for that.
func (x *sqlExec) arrayElems(c *sqlir.ArrayCmp) (*arrayElems, error) {
	if a, ok := x.arrays[c]; ok {
		return a, nil
	}
	a, err := x.readArray(c)
	if err != nil {
		return nil, err
	}
	if x.arrays == nil {
		x.arrays = map[*sqlir.ArrayCmp]*arrayElems{}
	}
	x.arrays[c] = a
	return a, nil
}

// checkArrays also refuses a Go slice bound to a parameter that is not the
// array of an = ANY or <> ALL, as pgx fails to send it before the statement
// runs. Evaluation would find it only where it reaches the parameter, which
// WHERE false AND id = $1 never does.
func (x *sqlExec) checkArrays(stmt sqlir.Statement) error {
	arrays := map[*sqlir.Param]bool{}
	for _, c := range sqlir.ArrayCmps(stmt) {
		if p, ok := c.Array.(*sqlir.Param); ok {
			arrays[p] = true
		}
		if _, err := x.arrayElems(c); err != nil {
			return err
		}
	}
	for _, p := range sqlir.Params(stmt) {
		if p.Index < 0 || p.Index >= len(x.args) || arrays[p] {
			continue
		}
		if _, ok := x.args[p.Index].(arrayParam); ok {
			return x.unsupported("an array parameter anywhere but the right side of = ANY or <> ALL")
		}
	}
	return nil
}

func (x *sqlExec) readArray(c *sqlir.ArrayCmp) (*arrayElems, error) {
	var raw any
	switch a := c.Array.(type) {
	case *sqlir.Param:
		if a.Index < 0 || a.Index >= len(x.args) {
			return nil, x.tx.db.kind.Error(sqlir.UndefinedParameter, fmt.Sprintf("there is no parameter $%d", a.Index+1), "", "", "")
		}
		raw = x.args[a.Index]
	case *sqlir.Const:
		raw = a.Value
	}
	out := &arrayElems{}
	switch v := raw.(type) {
	case nil:
		out.null = true
		return out, nil
	case arrayParam:
		for _, el := range v {
			if c.ElemType == "" {
				out.exprs = append(out.exprs, &sqlir.Param{})
				out.vals = append(out.vals, el)
				continue
			}
			if !castableElem(el, c.ElemType) {
				return nil, x.unsupported(fmt.Sprintf("a %T element of an array parameter cast to %s[]", el, c.ElemType))
			}
			if err := out.cast(x, el, c.ElemType); err != nil {
				return nil, err
			}
		}
		return out, nil
	case []byte:
		raw = string(v)
	}
	s, ok := raw.(string)
	if !ok {
		return nil, x.unsupported(fmt.Sprintf("an array parameter holding a %T", raw))
	}
	elems, err := x.parseArray(s)
	if err != nil {
		return nil, err
	}
	for _, el := range elems {
		var v any
		if el != nil {
			v = *el
		}
		if c.ElemType != "" {
			if err := out.cast(x, v, c.ElemType); err != nil {
				return nil, err
			}
			continue
		}
		// An element of array text is read by x's type's input, as a string
		// literal compared with x is.
		out.exprs = append(out.exprs, &sqlir.Const{Value: v})
		out.vals = append(out.vals, v)
	}
	return out, nil
}

// cast appends an element cast to the array's element type, as the cast of
// the scalar converts it.
func (a *arrayElems) cast(x *sqlExec, v any, typ string) error {
	e := &sqlir.Cast{X: &sqlir.Const{Value: v}, Type: typ}
	val, err := x.eval(e, nil)
	if err != nil {
		return err
	}
	a.exprs = append(a.exprs, e)
	a.vals = append(a.vals, val)
	return nil
}

// castableElem reports whether a Go slice's element is of the kind pgx
// encodes as an element of typ[] without converting it: an integer for the
// integer types and a string for the others. pgx converts other kinds, such
// as a float to an integer, by its own rules, which detest does not model.
func castableElem(v any, typ string) bool {
	switch v.(type) {
	case nil:
		return true
	case int64:
		return typ == "int2" || typ == "int4" || typ == "int8"
	case string:
		return typ == "text" || typ == "varchar" || typ == "uuid"
	}
	return false
}

// parseArray reads Postgres's array text: {a,b,"c d",NULL}, with elements
// unquoted or double-quoted, a backslash escaping the next character, and
// space around them dropped. A nil element is NULL. A nested array or
// dimension information ([1:2]={...}) is refused, as ANY flattens one and
// reads the other by rules detest does not model.
func (x *sqlExec) parseArray(s string) ([]*string, error) {
	// The server's DETAIL tells the cases apart; its message is the same for
	// every one.
	malformed := func() error {
		return x.tx.db.kind.Error(sqlir.InvalidTextRepresentation, `malformed array literal: "`+s+`"`, "", "", "")
	}
	i := skipSpace(s, 0)
	if i < len(s) && s[i] == '[' {
		return nil, x.unsupported("array text with dimension information")
	}
	if i >= len(s) || s[i] != '{' {
		return nil, malformed()
	}
	i = skipSpace(s, i+1)
	var out []*string
	if i < len(s) && s[i] == '}' {
		i++
	} else {
		for {
			if i >= len(s) {
				return nil, malformed()
			}
			switch s[i] {
			case '{':
				return nil, x.unsupported("a multidimensional array")
			case ',', '}':
				return nil, malformed()
			}
			var el strings.Builder
			quoted, escaped := s[i] == '"', false
			if quoted {
				i++
				for {
					if i >= len(s) {
						return nil, malformed()
					}
					if s[i] == '\\' && i+1 < len(s) {
						el.WriteByte(s[i+1])
						i += 2
						continue
					}
					if s[i] == '"' {
						i++
						break
					}
					el.WriteByte(s[i])
					i++
				}
				i = skipSpace(s, i)
				if i >= len(s) || s[i] != ',' && s[i] != '}' {
					return nil, malformed()
				}
			} else {
				// Space inside an unquoted element is kept and space after
				// it dropped, unless a backslash escapes it.
				kept := 0
				for i < len(s) && s[i] != ',' && s[i] != '}' {
					switch s[i] {
					case '"':
						return nil, malformed()
					case '{':
						return nil, x.unsupported("a multidimensional array")
					case '\\':
						if i+1 < len(s) {
							el.WriteByte(s[i+1])
							i += 2
							escaped = true
							kept = el.Len()
							continue
						}
					}
					el.WriteByte(s[i])
					if s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
						kept = el.Len()
					}
					i++
				}
				if i >= len(s) {
					return nil, malformed()
				}
				trimmed := el.String()[:kept]
				el.Reset()
				el.WriteString(trimmed)
			}
			if !quoted && !escaped && strings.EqualFold(el.String(), "NULL") {
				out = append(out, nil)
			} else {
				v := el.String()
				out = append(out, &v)
			}
			if s[i] == '}' {
				i++
				break
			}
			i = skipSpace(s, i+1)
			if i < len(s) && s[i] == '}' {
				return nil, malformed()
			}
		}
	}
	if skipSpace(s, i) < len(s) {
		return nil, malformed()
	}
	return out, nil
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}
