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
