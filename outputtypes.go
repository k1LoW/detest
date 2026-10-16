package detest

import (
	"maps"
	"slices"

	"github.com/k1LoW/detest/internal/sqlir"
)

// certainType is the number type of e that the statement shows without a
// guess, which the output goes by to hand a numeric to the driver as its
// text, or "". It never takes a type from the other operand or branch, as
// the checks' typing does to have something to check against: a float8
// read back as numeric text would scan into an integer where the drivers'
// float64 does not.
func (c *columnChecker) certainType(e sqlir.Expr, sc *colScope) string {
	db := c.x.tx.db
	switch e := e.(type) {
	case *sqlir.Cast:
		if b := db.baseType(e.Type); numberRank[b] > 0 {
			return b
		}
	case *sqlir.Const:
		switch v := e.Value.(type) {
		case int64:
			if v >= -1<<31 && v < 1<<31 {
				return "int4"
			}
			return "int8"
		case float64:
			return "numeric"
		}
	case *sqlir.UnaryExpr:
		if e.Op == "-" {
			return c.certainType(e.X, sc)
		}
	case *sqlir.ColumnRef:
		return db.baseType(sc.certainColumn(e))
	case *sqlir.BinaryExpr:
		switch e.Op {
		case "+", "-", "*", "/", "%":
			l, r := c.certainType(e.L, sc), c.certainType(e.R, sc)
			if numberRank[l] == 0 || numberRank[r] == 0 {
				return ""
			}
			return widerNumber(l, r)
		}
	case *sqlir.WindowFunc:
		return c.certainType(e.Func, sc)
	case *sqlir.SubQuery:
		if t := c.certainOutput(e.Select); len(t) == 1 {
			return t[0]
		}
	case *sqlir.CaseExpr:
		return c.certainBranches(caseBranches(e), sc)
	case *sqlir.FuncCall:
		switch e.Name {
		case "extract":
			return "numeric"
		case "count":
			return "int8"
		case "sum", "avg":
			if len(e.Args) != 1 {
				return ""
			}
			typ := c.certainType(e.Args[0], sc)
			switch {
			case numberCategory(typ) == numberCategory("float8"):
				return "float8"
			case e.Name == "avg" && numberRank[typ] > 0, typ == "int8", typ == "numeric":
				return "numeric"
			case numberRank[typ] > 0:
				return "int8"
			}
		case "min", "max", "abs", "coalesce", "greatest", "least":
			return c.certainBranches(e.Args, sc)
		case "lag", "lead":
			// The value and the default, not the offset.
			return c.certainBranches(branchArgs(e), sc)
		case "first_value", "last_value", "nth_value":
			if len(e.Args) > 0 {
				return c.certainType(e.Args[0], sc)
			}
		case "nullif":
			if len(e.Args) != 2 {
				return ""
			}
			l := c.certainType(e.Args[0], sc)
			if numberRank[l] == 0 {
				return ""
			}
			if takesOtherType(e.Args[1]) {
				return l
			}
			r := c.certainType(e.Args[1], sc)
			if numberRank[r] == 0 {
				return ""
			}
			// The = it compares by promotes the first argument to the
			// second's category where no = takes the pair.
			if lc, rc := numberCategory(l), numberCategory(r); rc > lc {
				if rc == numberCategory("numeric") {
					return "numeric"
				}
				return "float8"
			}
			return l
		case "floor", "ceil", "ceiling", "round", "power", "pow":
			if e.Name == "round" && len(e.Args) == 2 {
				return "numeric"
			}
			kind := ""
			for _, a := range e.Args {
				if p, ok := a.(*sqlir.Param); ok && c.paramCount[p.Index] > 1 {
					// Postgres types it at the use it meets first, whose
					// form may be another than this call's.
					return ""
				}
				if takesOtherType(a) {
					continue // it takes the form the other arguments pick
				}
				switch typ := c.certainType(a, sc); {
				case numberCategory(typ) == numberCategory("float8"):
					return "float8"
				case typ == "numeric":
					kind = "numeric"
				case numberRank[typ] > 0:
					if kind == "" {
						kind = "float8"
					}
				default:
					return ""
				}
			}
			if kind == "" && len(e.Args) > 0 {
				return "float8" // untyped arguments alone take the preferred form
			}
			return kind
		}
	}
	return ""
}

// certainBranches is the number type branches of CASE, COALESCE and the like
// meet at when every one but an untyped literal, parameter or NULL, which
// takes the others' type, is certain, and "" otherwise.
func (c *columnChecker) certainBranches(exprs []sqlir.Expr, sc *colScope) string {
	out := ""
	for _, e := range exprs {
		if takesOtherType(e) {
			continue
		}
		typ := c.certainType(e, sc)
		if numberRank[typ] == 0 {
			return ""
		}
		out = widerNumber(out, typ)
	}
	return out
}

// widerNumber is the number type two numbers meet at, as Postgres widens
// an integer to a numeric and either to a float.
func widerNumber(a, b string) string {
	if numberRank[b] > numberRank[a] {
		return b
	}
	return a
}

// certainColumn is the certain type of the column r, by the item it
// resolves to as columnType resolves it: a table's declared type, or a
// derived table's certain one, and "" for any other item or a name it
// cannot resolve to one column.
func (sc *colScope) certainColumn(r *sqlir.ColumnRef) string {
	for s := sc; s != nil; s = s.outer {
		found, typ := 0, ""
		for alias, cols := range s.items {
			if r.Table != "" && alias != r.Table {
				continue
			}
			if has, _ := cols.has(r.Column); has {
				found++
				typ = s.certain[alias][r.Column]
			} else if cols == nil {
				return ""
			}
		}
		if found == 1 {
			return typ
		}
		if found > 1 {
			return ""
		}
		if r.Table == "excluded" && s.hasExcluded {
			return s.certain["excluded"][r.Column]
		}
	}
	return ""
}

// noteCertain records the certain types of sel's output columns, by
// position where every * of it is over a table the check expands, and by
// name for a select list with a *.
func (c *columnChecker) noteCertain(sel *sqlir.SelectStmt, sc *colScope) {
	if len(sel.Values) > 0 {
		return // noteValuesCertain records it over the rows
	}
	var types []string
	named := map[string]string{}
	seen := map[string]bool{}
	note := func(name, typ string) {
		if seen[name] {
			typ = ""
		}
		seen[name] = true
		named[name] = typ
	}
	positional, namesKnown := true, true
	for _, t := range sel.Targets {
		if !t.Star {
			typ := c.certainType(t.Expr, sc)
			types = append(types, typ)
			note(targetName(t), typ)
			continue
		}
		if cols := c.starColumns(sel, t); cols != nil {
			for _, typ := range cols {
				types = append(types, c.x.tx.db.baseType(typ))
			}
		} else {
			positional = false
		}
		for alias, cols := range sc.items {
			if t.Table != "" && alias != t.Table {
				continue
			}
			if cols == nil {
				// An item of columns the check does not know may give a
				// name another item gives too, which the names below
				// would take for one.
				namesKnown = false
			}
			for name, once := range cols {
				typ := sc.certain[alias][name]
				if !once {
					typ = ""
				}
				note(name, typ)
			}
		}
	}
	if c.outCertain == nil {
		c.outCertain = map[*sqlir.SelectStmt][]string{}
		c.outCertainNamed = map[*sqlir.SelectStmt]map[string]string{}
	}
	if positional {
		c.outCertain[sel] = types
	}
	if !namesKnown {
		named = map[string]string{}
	}
	c.outCertainNamed[sel] = named
}

// certainOutput is the certain types of the output columns of sel by
// position, nil where they are not known by position.
func (c *columnChecker) certainOutput(sel *sqlir.SelectStmt) []string {
	if sel.SetOp != "" && sel.Larg != nil && sel.Rarg != nil {
		l, r := c.certainOutput(sel.Larg), c.certainOutput(sel.Rarg)
		if l == nil || r == nil || len(l) != len(r) {
			return nil
		}
		out := make([]string, len(l))
		for i := range out {
			out[i] = c.certainPair(sel, i, l[i], r[i])
		}
		return out
	}
	return c.outCertain[sel]
}

// noteValuesCertain records the certain types of the columns of a VALUES
// query over its rows in sc, the scope the walk checks them in, where a
// row may read a column of an outer query.
func (c *columnChecker) noteValuesCertain(sel *sqlir.SelectStmt, sc *colScope) {
	out := make([]string, len(sel.Values[0]))
	for i := range out {
		out[i] = c.certainBranches(setOpColumnExprs(sel, i), sc)
	}
	if c.outCertain == nil {
		c.outCertain = map[*sqlir.SelectStmt][]string{}
		c.outCertainNamed = map[*sqlir.SelectStmt]map[string]string{}
	}
	c.outCertain[sel] = out
}

// certainPair is the certain type the i-th columns of a set operation's two
// queries meet at, typed l and r, where a query whose column is an untyped
// literal, parameter or NULL takes the other's.
func (c *columnChecker) certainPair(sel *sqlir.SelectStmt, i int, l, r string) string {
	lu := slices.ContainsFunc(leafColumn(sel.Larg, i), takesOtherType)
	ru := slices.ContainsFunc(leafColumn(sel.Rarg, i), takesOtherType)
	switch {
	case lu && ru:
		return ""
	case lu:
		return r
	case ru:
		return l
	case numberRank[l] == 0 || numberRank[r] == 0:
		return ""
	}
	return widerNumber(l, r)
}

// leafColumn is the expression of the i-th column of sel when it is a
// query of one select list without a *, and nil otherwise.
func leafColumn(sel *sqlir.SelectStmt, i int) []sqlir.Expr {
	if sel == nil || sel.SetOp != "" || len(sel.Values) > 0 || i >= len(sel.Targets) {
		return nil
	}
	if slices.ContainsFunc(sel.Targets, func(t sqlir.Target) bool { return t.Star }) {
		return nil
	}
	return []sqlir.Expr{sel.Targets[i].Expr}
}

// certainItem is the certain types of the columns of a derived table over
// sel, by name, which names renames by position.
func (c *columnChecker) certainItem(sel *sqlir.SelectStmt, names []string) map[string]string {
	out := map[string]string{}
	cols := c.expandedNames(sel)
	if types := c.certainOutput(sel); types != nil && len(cols) == len(types) {
		// names renames the first columns by position, and the rest keep
		// their own.
		cols = slices.Clone(cols)
		copy(cols, names)
		seen := map[string]int{}
		for _, n := range cols {
			seen[n]++
		}
		for i, typ := range types {
			if seen[cols[i]] == 1 {
				out[cols[i]] = typ
			}
		}
		return out
	}
	if len(names) > 0 || sel.SetOp != "" {
		return out
	}
	// A select list with a * has its columns by name.
	maps.Copy(out, c.outCertainNamed[sel])
	return out
}

func (sc *colScope) noteCertain(alias string, types map[string]string) {
	if sc.certain == nil {
		sc.certain = map[string]map[string]string{}
	}
	sc.certain[alias] = types
}

// certainSource is the type of e, an extract's source, that the statement
// shows without a guess: a cast's or a table's declared column's, and ""
// for any other.
func (sc *colScope) certainSource(e sqlir.Expr) string {
	switch e := e.(type) {
	case *sqlir.Cast:
		return e.Type
	case *sqlir.ColumnRef:
		return sc.certainColumn(e)
	}
	return ""
}
