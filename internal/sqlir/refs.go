package sqlir

import "reflect"

// ColumnRefs returns the column references in e, in the order they appear.
func ColumnRefs(e Expr) []*ColumnRef { return find[*ColumnRef](e) }

// FuncCalls returns the function calls in e, in the order they appear.
func FuncCalls(e Expr) []*FuncCall { return find[*FuncCall](e) }

// BlockFuncCalls returns the function calls of e's own query block: not
// those in a subquery, which belong to its block, nor the function a window
// computes, which is no aggregate of the block. A window's arguments,
// partition and order are the block's.
func BlockFuncCalls(e Expr) []*FuncCall {
	var out []*FuncCall
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			switch n := v.Interface().(type) {
			case *SelectStmt:
				return
			case *WindowFunc:
				walk(reflect.ValueOf(n.Func.Args))
				walk(reflect.ValueOf(n.Partition))
				walk(reflect.ValueOf(n.Order))
				return
			case *FuncCall:
				out = append(out, n)
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

// find returns the nodes of type T in e. It walks the expression by
// reflection, so it needs no case for each kind of node.
func find[T any](e Expr) []T {
	var out []T
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			// Matched at the pointer only, as an interface holding it is
			// walked into the pointer and would match it twice.
			if c, ok := reflect.TypeAssert[T](v); ok {
				out = append(out, c)
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

// TableNames returns the names the FROM items under n read, CTEs and views
// included, in the order they appear.
func TableNames(n any) []string {
	var out []string
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			if t, ok := reflect.TypeAssert[TableRef](v); ok && t.Name != "" {
				out = append(out, t.Name)
			}
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
	walk(reflect.ValueOf(n))
	return out
}
