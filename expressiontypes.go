package detest

import (
	"slices"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
)

// expressionType uses only types the statement or its declared columns tell
// us. It never evaluates an argument while resolving a function signature.
func expressionType(e sqlir.Expr, column func(*sqlir.ColumnRef) string) string {
	return expressionTypeIn(e, column, nil)
}

// expressionTypeIn is expressionType with subquery, when given, typing a
// scalar subquery in its own scope, which expressionType reads in the scope
// around it.
func expressionTypeIn(e sqlir.Expr, column func(*sqlir.ColumnRef) string, subquery func(*sqlir.SubQuery) string) string {
	return typer{column: column, subquery: subquery}.of(e)
}

// typer types expressions as expressionTypeIn does, with base, when given,
// reducing a domain to its base type where Postgres resolves an operator or
// a function, so that 1::tiny + 0 over a smallint is an integer rather than
// a tiny. A cast, a column, a scalar subquery and branches all of one domain
// keep the domain. With untypedText, branches or arguments that are all
// string literals, parameters or NULLs are text, which Postgres resolves
// them to, as in min('1') or COALESCE('1', NULL), where without it they are
// left untyped for the caller, such as branchTypes, to resolve.
type typer struct {
	column      func(*sqlir.ColumnRef) string
	subquery    func(*sqlir.SubQuery) string
	base        func(string) string
	untypedText bool
}

func (t typer) reduce(typ string) string {
	if t.base == nil {
		return typ
	}
	return t.base(typ)
}

func (t typer) of(e sqlir.Expr) string {
	column, subquery := t.column, t.subquery
	switch e := e.(type) {
	case *sqlir.Cast:
		return e.Type
	case *sqlir.Collate:
		if untypedExpr(e.X) || isNullConst(e.X) || untypedBranch(e.X) {
			// COLLATE resolves a string literal, a parameter, a NULL or a
			// CASE or COALESCE of those to text.
			return "text"
		}
		return t.of(e.X)
	case *sqlir.ColumnRef:
		if column != nil {
			return column(e)
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
		case bool:
			return "bool"
		}
	case *sqlir.UnaryExpr:
		if e.Op == "NOT" {
			return "bool"
		}
		if e.Op == "-" {
			if typ := t.reduce(t.of(e.X)); !collatableType(typ) {
				return typ
			}
			return ""
		}
	case *sqlir.BinaryExpr:
		switch e.Op {
		case "||":
			return "text"
		case "+", "-", "*", "/", "%":
			l, r := t.reduce(t.of(e.L)), t.reduce(t.of(e.R))
			if l == ambiguousDomain || r == ambiguousDomain {
				// The operator depends on which of the domains it is,
				// which the fallbacks below would guess from the other
				// operand.
				return ambiguousDomain
			}
			// A date moved by an interval is a timestamp.
			if l == "date" && r == "interval" && (e.Op == "+" || e.Op == "-") || l == "interval" && r == "date" && e.Op == "+" {
				return "timestamp"
			}
			isTime := func(t string) bool { return t == "timestamp" || t == "timestamptz" }
			switch {
			case isTime(l) && isTime(r) && e.Op == "-":
				return "interval"
			case l == "interval" && isTime(r) && e.Op == "+":
				return r
			case (l == "interval" || r == "interval") && e.Op == "*":
				return "interval"
			}
			if numberRank[l] > 0 && numberRank[r] > 0 {
				// The operator takes the wider of two numbers, as
				// smallint + integer is an integer.
				if numberRank[r] > numberRank[l] {
					return r
				}
				return l
			}
			for _, typ := range []string{"float8", "float4", "numeric", "int8"} {
				if l == typ || r == typ {
					return typ
				}
			}
			res := l
			if res == "" {
				res = r
			}
			if collatableType(res) {
				// No arithmetic operator yields a character type, and the
				// one that takes one, jsonb - text, yields jsonb, as
				// '{}' - s does.
				return ""
			}
			return res
		case "=", "<>", "!=", "<", "<=", ">", ">=", "AND", "OR", "LIKE", "ILIKE", "NOT LIKE", "NOT ILIKE":
			return "bool"
		}
	case *sqlir.FuncCall:
		switch e.Name {
		case "floor", "ceil", "ceiling", "round", "power", "pow":
			if e.Name == "round" && len(e.Args) == 2 {
				return "numeric" // round(numeric, int) has no float form
			}
			// Postgres has a numeric and a float8 form of each, and takes
			// the float8 one for an integer, as its preferred number type.
			kind := ""
			for _, a := range e.Args {
				if untypedExpr(a) || isNullConst(a) {
					continue // it takes the form the other arguments pick
				}
				switch typ := t.reduce(t.of(a)); {
				case numberCategory(typ) == numberCategory("float8"):
					return "float8"
				case typ == "numeric" || typ == "decimal":
					kind = "numeric"
				case numberRank[typ] > 0:
					if kind == "" {
						kind = "float8"
					}
				default:
					// A type the check does not know, or one no form takes,
					// which Postgres fails; the checks of the arguments go
					// by numeric, as they did before the forms were told
					// apart.
					kind = "numeric"
				}
			}
			if kind == "" && len(e.Args) > 0 {
				// Untyped arguments alone take float8, the preferred
				// number type.
				return "float8"
			}
			return kind
		case "avg":
			if len(e.Args) == 0 {
				break
			}
			switch typ := t.reduce(t.of(e.Args[0])); {
			case numberCategory(typ) == numberCategory("float8"):
				return "float8"
			}
			return "numeric"
		case "pg_try_advisory_xact_lock":
			return "bool"
		case "pg_advisory_xact_lock":
			return "void"
		case "make_interval":
			return "interval"
		case "hashtext":
			return "int4"
		case "sum":
			if len(e.Args) > 0 {
				switch typ := t.reduce(t.of(e.Args[0])); typ {
				case "int", "int2", "int4", "integer", "smallint":
					return "int8"
				case "int8", "bigint":
					return "numeric"
				default:
					return typ
				}
			}
		case "min", "max", "abs":
			// abs(NULL) takes float8 too, but only where untyped values are
			// resolved: elsewhere it stays unknown, so that the branch
			// checks refuse a COALESCE of an integer with it, which
			// Postgres computes as a float8 and the run as an integer.
			if e.Name == "abs" && len(e.Args) == 1 && (untypedExpr(e.Args[0]) || t.untypedText && isNullConst(e.Args[0])) {
				return "float8" // the preferred number type's form
			}
			return t.reduce(t.common(e.Args))
		case "coalesce", "greatest", "least":
			return t.common(e.Args)
		case "nullif":
			// NULLIF returns its first argument as the = it is compared by
			// takes it: a timestamp compared with a timestamptz, or an
			// integer with another, by a cross-type = stays as it is, while
			// an integer compared with a numeric, or either with a float,
			// is promoted, as no = takes the pair.
			if len(e.Args) == 0 {
				break
			}
			l := t.reduce(t.of(e.Args[0]))
			if len(e.Args) != 2 {
				return l
			}
			r := t.reduce(t.of(e.Args[1]))
			if l == "" {
				// Literals and NULLs alone resolve as COALESCE's do.
				return t.reduce(t.common(e.Args))
			}
			if lc, rc := numberCategory(l), numberCategory(r); lc > 0 && rc > lc {
				if rc == numberCategory("numeric") {
					return "numeric"
				}
				return "float8"
			}
			return l
		case "lower", "upper", "left", "concat":
			return "text"
		case "length", "char_length", "octet_length":
			return "int4"
		case "nextval", "setval", "count", "row_number", "rank", "dense_rank":
			return "int8"
		case "lag", "lead":
			// The value and the default resolve to one type.
			if args := branchArgs(e); len(args) > 0 {
				return t.common(args)
			}
			if len(e.Args) > 0 {
				return t.of(e.Args[0])
			}
		case "first_value", "last_value":
			if len(e.Args) > 0 {
				return t.common(e.Args[:1])
			}
		case "random":
			return "float8"
		case "now", "clock_timestamp", "transaction_timestamp", "statement_timestamp", "current_timestamp":
			return "timestamptz"
		case "localtimestamp":
			return "timestamp"
		case "current_date":
			return "date"
		case "extract":
			return "numeric"
		case "date_part":
			return "float8"
		case "date_trunc":
			// A timestamp's stays a timestamp and an interval's an
			// interval, and a date's becomes a timestamptz, as any other's.
			if len(e.Args) == 2 {
				if typ := t.reduce(t.of(e.Args[1])); typ == "timestamp" || typ == "interval" {
					return typ
				}
			}
			return "timestamptz"
		case "gen_random_uuid", "uuid_generate_v4":
			return "uuid"
		}
	case *sqlir.WindowFunc:
		return t.of(e.Func)
	case *sqlir.CaseExpr:
		return t.common(caseBranches(e))
	case *sqlir.SubQuery:
		if subquery != nil {
			return subquery(e)
		}
		if e.Select != nil && len(e.Select.Targets) == 1 {
			return t.of(e.Select.Targets[0].Expr)
		}
	case *sqlir.Exists, *sqlir.InExpr, *sqlir.ArrayCmp, *sqlir.IsNull:
		return "bool"
	}
	return ""
}

// common is the type Postgres resolves exprs to together, as the arguments
// of COALESCE or the branches of CASE: the first one known, except that
// dates and timestamps meet at the widest of them, a timestamptz over a
// timestamp over a date, and numbers at the one the others cast to
// implicitly, a float8 over a float4 over a numeric over the integers from
// the widest down.
func (t typer) common(exprs []sqlir.Expr) string {
	if t.untypedText && len(exprs) > 0 && !slices.ContainsFunc(exprs, func(e sqlir.Expr) bool { return !untypedExpr(e) && !isNullConst(e) }) {
		return "text"
	}
	types := make([]string, len(exprs))
	// Branches all of one type keep it, a domain included, while branches
	// of several meet at their base types.
	same, mixed := "", false
	for i, e := range exprs {
		types[i] = t.of(e)
		if types[i] != "" {
			mixed = mixed || same != "" && types[i] != same
			same = types[i]
		}
	}
	if !mixed {
		return same
	}
	first, best := "", ""
	var rank map[string]int
	for _, typ := range types {
		typ = t.reduce(typ)
		if typ == "" {
			continue
		}
		if first == "" {
			first = typ
			for _, r := range []map[string]int{timeRank, numberRank} {
				if r[typ] > 0 {
					rank = r
				}
			}
		}
		if rank[typ] > rank[best] {
			best = typ
		}
	}
	if rank != nil && best != "" {
		return best
	}
	return first
}

var (
	timeRank   = map[string]int{"date": 1, "timestamp": 2, "timestamptz": 3}
	numberRank = map[string]int{
		"int2": 1, "smallint": 1, "int4": 2, "int": 2, "integer": 2, "int8": 3, "bigint": 3,
		"numeric": 4, "decimal": 4, "float4": 5, "real": 5, "float8": 6, "double precision": 6,
	}
)

// numberCategory groups the number types Postgres compares by a cross-type
// =: integers 1, numeric 2 and floats 3, 0 for any other type.
func numberCategory(typ string) int {
	switch r := numberRank[typ]; {
	case r == 0:
		return 0
	case r <= numberRank["int8"]:
		return 1
	case r == numberRank["numeric"]:
		return 2
	default:
		return 3
	}
}

// numberBranchKind is "integer" or "numeric" when every branch among exprs
// that has a type of its own is a number of that kind, a numeric or a float
// making them all numeric, as Postgres resolves them; "" otherwise. A NULL,
// a string literal or a parameter takes that type, which branchValue cannot
// tell from the values: a NULL numeric column leaves an integer literal an
// integer there, and text beside an integer column stays text. mismatch
// reports a number beside a branch of a type other than a number, which
// Postgres fails with 42804 whatever the rows, and detest would run on a
// row whose values do not show it.
func numberBranchKind(exprs []sqlir.Expr, t typer) (kind string, mismatch bool) {
	other, unknown := false, false
	for _, e := range exprs {
		if k, ok := e.(*sqlir.Const); ok && k.Value == nil || untypedExpr(e) {
			continue
		}
		if untypedBranch(e) {
			// A CASE or COALESCE of those only is text already.
			other = true
			continue
		}
		typ := t.of(e)
		switch r := numberRank[typ]; {
		case r >= numberRank["numeric"]:
			kind = "numeric"
		case r > 0:
			if kind == "" {
				kind = "integer"
			}
		case typ == "" || typ == "unresolved column type":
			unknown = true
		default:
			other = true
		}
	}
	if kind != "" && other {
		return "", true
	}
	if unknown || other {
		return "", false
	}
	return kind, false
}

func textArgumentMismatch(f *sqlir.FuncCall, t typer) string {
	if f.Name == "left" && len(f.Args) == 2 {
		switch typ := t.reduce(t.of(f.Args[1])); typ {
		case "", "unresolved column type", "int", "int2", "int4", "integer", "smallint":
		default:
			return "left with a " + typ + " length argument"
		}
	}
	switch f.Name {
	case "lower", "upper", "left", "length", "char_length", "octet_length", "hashtext":
		if len(f.Args) > 0 {
			typ := t.reduce(t.of(f.Args[0]))
			if typ != "" && typ != "unresolved column type" && typ != "text" && typ != "varchar" && typ != "bpchar" {
				return f.Name + " with a " + typ + " argument"
			}
		}
	case "pg_advisory_xact_lock", "pg_try_advisory_xact_lock":
		// The one-bigint and two-integer overloads are the only signatures
		// Postgres defines; it rejects any other key type before locking,
		// where callFunc would otherwise key the lock on the value as given.
		for _, a := range f.Args {
			switch typ := t.reduce(t.of(a)); typ {
			case "", "unresolved column type", "int", "int2", "int4", "int8", "integer", "bigint", "smallint":
			default:
				return f.Name + " with a " + typ + " key argument"
			}
		}
	case "date_trunc", "extract", "date_part":
		if len(f.Args) != 2 {
			break
		}
		switch typ := t.reduce(t.of(f.Args[0])); typ {
		case "", "unresolved column type", "text", "varchar":
		default:
			return f.Name + " with a " + typ + " unit"
		}
		// unit is "" when it is given at run time, and unknownUnit for a
		// constant Postgres does not take, which fails with 22023 whatever
		// the source, so it is left to the run.
		const unknownUnit = "?"
		unit := ""
		if k, ok := f.Args[0].(*sqlir.Const); ok {
			if s, ok := k.Value.(string); ok {
				unit = unknownUnit
				u, known := timeUnits[strings.ToLower(s)]
				if f.Name != "date_trunc" {
					u, known = extractUnit(s)
				}
				if known {
					unit = u
				}
			}
		}
		// Postgres picks the overload by the source's type, and one it does
		// not know, a parameter or a string literal, matches several of them
		// (42725), whatever value is bound.
		if untypedExpr(f.Args[1]) {
			return f.Name + " of a parameter or a string literal of no type"
		}
		switch typ := t.reduce(t.of(f.Args[1])); typ {
		case "", "unresolved column type", "timestamp", "timestamptz":
		case "date":
			// extract from a date fails for a unit of the time of day, which
			// the value, a time at midnight, does not tell.
			if f.Name == "extract" {
				switch unit {
				case "", "microseconds", "milliseconds", "second", "minute", "hour", "timezone", "timezone_hour", "timezone_minute":
					return "extract of a time-of-day unit, or a unit given at run time, from a date"
				}
			}
		case "interval":
			// Units of a calendar position Postgres refuses for an interval,
			// which has none.
			switch unit {
			case "week":
				if f.Name == "date_trunc" {
					return "date_trunc of an interval to week"
				}
			case "dow", "isodow", "doy", "isoyear", "julian", "timezone", "timezone_hour", "timezone_minute":
				return f.Name + " of " + unit + " from an interval"
			}
		default:
			return f.Name + " of a " + typ
		}
	case "abs", "floor", "ceil", "ceiling", "power", "pow":
		// Postgres has no signature of these for a non-numeric argument
		// and rejects the call before evaluating any of them, where
		// callFunc would otherwise evaluate a side-effecting argument
		// (nextval) and only then find it cannot convert the result.
		for _, a := range f.Args {
			switch typ := t.reduce(t.of(a)); typ {
			case "", "unresolved column type", "int", "int2", "int4", "int8", "integer", "bigint", "smallint", "numeric", "float4", "float8", "real", "double precision":
			default:
				return f.Name + " with a " + typ + " argument"
			}
		}
	}
	return ""
}

// aggregateArgumentMismatch refuses sum and avg of an argument Postgres has
// no form of. It resolves the aggregate by the argument's type when it plans
// the statement, so it fails such a call over no rows too, where the
// evaluators see no value to refuse. typed is false where the check has no
// schema, as in CheckSQL, which cannot tell a domain over a number from a
// type no form takes, so only the forms that fail whatever the schema are
// refused there.
func aggregateArgumentMismatch(f *sqlir.FuncCall, t typer, typed bool) string {
	if f.Name != "sum" && f.Name != "avg" || len(f.Args) != 1 {
		return ""
	}
	switch a := f.Args[0]; {
	case untypedBranch(a) || isNullConst(a):
		// A string literal, a parameter or a NULL matches several forms
		// (42725), and a CASE or COALESCE of them is text (42883).
		return f.Name + " of a string literal, a parameter or NULL, or a CASE or COALESCE of them"
	case rowValued(a):
		return f.Name + " of a row"
	case !typed:
		return ""
	}
	switch typ := t.reduce(t.of(f.Args[0])); typ {
	case "", "unresolved column type", "int", "int2", "int4", "int8", "integer", "bigint", "smallint", "numeric", "decimal", "float4", "float8", "real", "double precision", "interval":
		return ""
	default:
		// sum of money exists too, but detest does not model money, so it
		// is refused with the rest.
		return f.Name + " with a " + typ + " argument"
	}
}

// rowValued reports a row, or a CASE, COALESCE, GREATEST, LEAST or NULLIF
// one of whose branches is a row, or a min, max or window function giving
// the row it takes, which the typer gives no type.
func rowValued(e sqlir.Expr) bool {
	switch e := e.(type) {
	case *sqlir.RowExpr:
		return true
	case *sqlir.CaseExpr:
		return slices.ContainsFunc(caseBranches(e), rowValued)
	case *sqlir.WindowFunc:
		return rowValued(e.Func)
	case *sqlir.FuncCall:
		switch e.Name {
		case "min", "max", "first_value", "last_value", "nth_value":
			return len(e.Args) > 0 && rowValued(e.Args[0])
		}
		return slices.ContainsFunc(branchArgs(e), rowValued)
	}
	return false
}

// dateDifferenceMismatch refuses date - date, which Postgres gives as an
// integer of days. A date is a time at midnight to detest, so the difference
// would come out an interval.
func dateDifferenceMismatch(b *sqlir.BinaryExpr, t typer) string {
	if b.Op != "-" {
		return ""
	}
	l, r := t.reduce(t.of(b.L)), t.reduce(t.of(b.R))
	// A parameter or a string literal beside a date is read as a date too.
	if l == "date" && (r == "date" || untypedExpr(b.R)) || r == "date" && untypedExpr(b.L) {
		return "date - date"
	}
	return ""
}

// intervalBranchMismatch refuses a value other than an interval or NULL
// beside an interval among the branches of CASE, COALESCE, GREATEST, LEAST,
// NULLIF or the value and default of LAG and LEAD. Postgres reads a string
// literal or a parameter there as an interval, failing with 22007 on text
// that is none whichever branch a row takes, and fails a branch of another
// type with 42804. detest would keep the value as it is, which compares with
// no interval. A column whose type the check cannot resolve, of a * or a
// set operation, may be an interval, so it is refused beside an interval or
// a value of another type too.
//
// subquery, when given, types a scalar subquery in its own scope, which
// expressionType reads in the scope around it.
func intervalBranchMismatch(exprs []sqlir.Expr, column func(*sqlir.ColumnRef) string, subquery func(*sqlir.SubQuery) string) string {
	interval, other, unresolved := false, false, false
	for _, e := range exprs {
		if k, ok := e.(*sqlir.Const); ok && k.Value == nil {
			continue
		}
		typ := expressionTypeIn(e, column, subquery)
		switch {
		case untypedBranch(e):
			other = true
		case typ == "interval":
			interval = true
		case typ == "unresolved column type":
			unresolved = true
		case typ != "":
			other = true
		}
	}
	if interval && other || unresolved && (interval || other) {
		return "a value other than an interval beside an interval, or beside a column of unresolved type, among the branches of CASE, COALESCE, GREATEST, LEAST, NULLIF, LAG or LEAD"
	}
	return ""
}

// untypedBranch reports a string literal or a parameter, or a CASE,
// COALESCE, LAG or LEAD of nothing else or of NULL only, which Postgres
// resolves to text, as in COALESCE('bogus', 'x') and COALESCE(NULL, NULL).
func untypedBranch(e sqlir.Expr) bool {
	var branches []sqlir.Expr
	if w, ok := e.(*sqlir.WindowFunc); ok {
		e = w.Func
	}
	switch e := e.(type) {
	case *sqlir.CaseExpr:
		branches = caseBranches(e)
	case *sqlir.FuncCall:
		if e.Name == "lag" || e.Name == "lead" {
			// The value and the default, without the offset.
			if len(e.Args) > 0 {
				branches = []sqlir.Expr{e.Args[0]}
			}
			if len(e.Args) == 3 {
				branches = append(branches, e.Args[2])
			}
			break
		}
		branches = branchArgs(e)
	default:
		return untypedExpr(e)
	}
	for _, b := range branches {
		if k, ok := b.(*sqlir.Const); ok && k.Value == nil {
			continue
		}
		if !untypedBranch(b) {
			return false
		}
	}
	return len(branches) > 0
}

// branchArgs are the arguments of f that Postgres resolves to one type, for
// intervalBranchMismatch: all of them, or the value and any default of LAG
// and LEAD, whose offset is an integer.
func branchArgs(f *sqlir.FuncCall) []sqlir.Expr {
	switch f.Name {
	case "coalesce", "greatest", "least", "nullif":
		return f.Args
	case "lag", "lead":
		switch len(f.Args) {
		case 1, 2:
			return f.Args[:1]
		case 3:
			return []sqlir.Expr{f.Args[0], f.Args[2]}
		}
	}
	return nil
}

// takesOtherType reports whether e is typed by what stands beside it, as
// Postgres types an untyped literal, a parameter, a NULL, or a CASE or
// COALESCE of those alone.
func takesOtherType(e sqlir.Expr) bool {
	return untypedExpr(e) || isNullConst(e) || untypedBranch(e)
}

func isNullConst(e sqlir.Expr) bool {
	k, ok := e.(*sqlir.Const)
	return ok && k.Value == nil
}

func concatTypeMismatch(b *sqlir.BinaryExpr, t typer) string {
	if b.Op != "||" {
		return ""
	}
	for _, e := range []sqlir.Expr{b.L, b.R} {
		switch typ := t.reduce(t.of(e)); typ {
		case "", "text", "varchar", "bpchar", "bool", "boolean", "int", "int2", "int4", "int8", "integer", "smallint", "bigint":
		default:
			return "|| with a " + typ + " operand"
		}
	}
	return ""
}
