package sqlir

import "reflect"

// ColumnRefs returns the column references in e, in the order they appear.
// It walks the expression by reflection, so it needs no case for each kind
// of node.
func ColumnRefs(e Expr) []*ColumnRef {
	var out []*ColumnRef
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return
			}
			if c, ok := reflect.TypeAssert[*ColumnRef](v); ok {
				out = append(out, c)
				return
			}
			walk(v.Elem())
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(e))
	return out
}

// Exprs returns e and every expression under it, subqueries included.
func Exprs(e Expr) []Expr {
	var out []Expr
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return
			}
			if x, ok := reflect.TypeAssert[Expr](v); ok {
				out = append(out, x)
			}
			walk(v.Elem())
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(e))
	return out
}

// CloneExpr returns a deep copy of e. A parsed statement is shared by every
// database that runs the same query, so an expression kept in one
// database's schema is copied before that database changes it. Like
// ColumnRefs it walks by reflection, and it copies the exported fields;
// unexported ones are shallow-copied with their struct.
func CloneExpr(e Expr) Expr {
	if e == nil {
		return nil
	}
	out, _ := reflect.TypeAssert[Expr](deepCopy(reflect.ValueOf(&e).Elem()))
	return out
}

func deepCopy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		out := reflect.New(v.Type()).Elem()
		if !v.IsNil() {
			out.Set(deepCopy(v.Elem()))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(deepCopy(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				out.Field(i).Set(deepCopy(v.Field(i)))
			}
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(deepCopy(v.Index(i)))
		}
		return out
	}
	return v
}
