package detest

import (
	"fmt"
	"time"

	"github.com/k1LoW/detest/internal/sqlir"
)

// instant is a Postgres timestamptz, a point in time. A timestamp and a
// date are a wall clock, which a time.Time in UTC holds. Postgres converts
// between the two in the session's TimeZone, so a value has to say which it
// is: a timestamp compared with a timestamptz, cast to one or written to a
// timestamptz column is read as the clock in that zone, and a timestamptz's
// fields, its date and the days and months added to it are read there too.
// MySQL has no such type and keeps every time a time.Time.
type instant struct{ time.Time }

func newInstant(t time.Time) instant { return instant{t.UTC()} }

// wallIn is the clock t shows in loc, as a timestamp holds it.
func wallIn(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), time.UTC)
}

// instantAt is the instant at which loc's clock shows w, as Postgres reads
// a timestamp as a timestamptz. A clock time a zone skips at a change of
// offset is read with the offset before the change, and one it shows twice
// with the offset after, as Postgres's DetermineTimeZoneOffset does; Go's
// time.Date picks the other way in both.
func instantAt(w time.Time, loc *time.Location) instant {
	naive := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC)
	offset := func(t time.Time) int { _, off := t.In(loc).Zone(); return off }
	// The offsets a day and a half before and after: no zone changes its
	// offset twice within that.
	before, after := offset(naive.Add(-36*time.Hour)), offset(naive.Add(36*time.Hour))
	at := func(off int) time.Time { return naive.Add(-time.Duration(off) * time.Second) }
	if before == after {
		return newInstant(at(before))
	}
	b, a := at(before), at(after)
	validB, validA := offset(b) == before, offset(a) == after
	switch {
	case validB && !validA:
		return newInstant(b)
	case validA && !validB:
		return newInstant(a)
	case !validA && !validB:
		return newInstant(b) // skipped: the offset before the change
	}
	return newInstant(a) // shown twice: the offset after the change
}

// dateIn is the date of t in loc at midnight, as a date holds it.
func dateIn(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}

// publicRow is r as the code under test reads it through the transaction
// API and State: a timestamptz is a time.Time, as before detest told it from
// a timestamp. r is copied only when it holds one.
func publicRow(r Row) Row {
	var out Row
	for k, v := range r {
		if t, ok := v.(instant); ok {
			if out == nil {
				out = r.clone()
			}
			out[k] = t.Time
		}
	}
	if out == nil {
		return r
	}
	return out
}

// publicRows is publicRow of each of rows.
func publicRows(rows []Row) []Row {
	for i, r := range rows {
		rows[i] = publicRow(r)
	}
	return rows
}

// publicPred is pred applied to the public form of a row (publicRow).
func publicPred(pred func(Row) bool) func(Row) bool {
	if pred == nil {
		return nil
	}
	return func(r Row) bool { return pred(publicRow(r)) }
}

// typedRow is row as the transaction API writes it to table: a time.Time
// for a Postgres timestamptz column is the instant it is.
func (db *DB) typedRow(table string, row Row) Row {
	var out Row
	for k, v := range row {
		if w, changed := db.typedValue(table, k, v); changed {
			if out == nil {
				out = row.clone()
			}
			out[k] = w
		}
	}
	if out == nil {
		return row
	}
	return out
}

// typedValue is v as the transaction API writes it to table's column col
// (typedRow), and whether that differs from v.
func (db *DB) typedValue(table, col string, v any) (any, bool) {
	t, ok := v.(time.Time)
	if !ok || db.kind.InnoDB() {
		return v, false
	}
	if def := db.defs[db.resolve(table)]; def != nil && def.types[col] == "timestamptz" {
		return newInstant(t), true
	}
	return v, false
}

// zoneSetting is a session TimeZone a transaction set, nil for the
// server's.
type zoneSetting struct{ loc *time.Location }

// zone is the session's TimeZone the transaction runs in: the one SET TIME
// ZONE gave it, or the server's.
func (tx *Tx) zone() *time.Location {
	if tx.timeZone != nil {
		return tx.timeZone
	}
	return tx.db.kind.TimeZone()
}

// asInstant is v as a timestamptz: an instant as it is, and a timestamp's or
// a date's clock read in the session's TimeZone. ok is false for a value of
// no time type.
func (x *sqlExec) asInstant(v any) (instant, bool) {
	switch t := derefValue(v).(type) {
	case instant:
		return t, true
	case time.Time:
		return instantAt(t, x.tx.zone()), true
	}
	return instant{}, false
}

// timeOperands converts a timestamp or a date compared with a timestamptz to
// a timestamptz, as Postgres compares them. Any other pair is left as it is.
func (x *sqlExec) timeOperands(l, r any) (any, any) {
	_, li := derefValue(l).(instant)
	_, ri := derefValue(r).(instant)
	if li == ri {
		return l, r
	}
	if _, ok := derefValue(l).(time.Time); ok {
		l, _ = x.asInstant(l)
	}
	if _, ok := derefValue(r).(time.Time); ok {
		r, _ = x.asInstant(r)
	}
	return l, r
}

// instantArith is the arithmetic of a timestamptz: one moved by an
// interval, and the difference of two times one of which is a timestamptz,
// whose other side Postgres reads as a timestamptz too. ok is false when
// neither side is a timestamptz.
func (x *sqlExec) instantArith(op string, l, r any) (v any, ok bool) {
	l, r = derefValue(l), derefValue(r)
	li, lok := l.(instant)
	ri, rok := r.(instant)
	if !lok && !rok {
		return nil, false
	}
	loc := x.tx.zone()
	switch {
	case lok:
		switch rv := r.(type) {
		case pgInterval:
			switch op {
			case "+":
				return rv.addToInstant(li, loc), true
			case "-":
				return rv.neg().addToInstant(li, loc), true
			}
		case time.Time, instant:
			if op == "-" {
				b, _ := x.asInstant(rv)
				return timeDifference(li.Time, b.Time), true
			}
		}
	case rok:
		switch lv := l.(type) {
		case pgInterval:
			if op == "+" {
				return lv.addToInstant(ri, loc), true
			}
		case time.Time:
			if op == "-" {
				a, _ := x.asInstant(lv)
				return timeDifference(a.Time, ri.Time), true
			}
		}
	}
	return nil, false
}

// castTime is a Postgres cast to timestamptz, timestamp or date, which
// convert between a timestamptz and a clock in the session's TimeZone. ok
// is false for a value castValue casts instead.
func (x *sqlExec) castTime(v any, typ string) (out any, ok bool) {
	loc := x.tx.zone()
	switch t := derefValue(v).(type) {
	case instant:
		switch typ {
		case "timestamptz":
			return t, true
		case "timestamp":
			return wallIn(t.Time, loc), true
		case "date":
			return dateIn(t.Time, loc), true
		}
	case time.Time:
		if typ == "timestamptz" {
			return instantAt(t, loc), true
		}
	case string:
		if typ != "timestamptz" {
			return nil, false
		}
		tm, hasZone, parsed := pgParseTime(t)
		if !parsed {
			return t, true // text in a form detest does not read stays text, as before
		}
		if hasZone {
			return newInstant(tm), true
		}
		return instantAt(tm, loc), true
	}
	return nil, false
}

// timeOf is v as extract and date_trunc read it: a timestamptz's clock in
// the session's TimeZone, and a clock as it is.
func (x *sqlExec) timeOf(v any) (time.Time, bool) {
	switch t := derefValue(v).(type) {
	case instant:
		return wallIn(t.Time, x.tx.zone()), true
	case time.Time:
		return t, true
	}
	return time.Time{}, false
}

// sourceType is the type the column check resolved for e, or the one e's
// own form tells.
func (x *sqlExec) sourceType(e sqlir.Expr) string {
	if typ, ok := x.exprTypes[e]; ok {
		return typ
	}
	return expressionType(e, nil)
}

// noteType records the type the column check resolved for e, for a run to
// convert e's value by (exprTypes).
func (x *sqlExec) noteType(e sqlir.Expr, typ string) {
	if typ != "timestamptz" && typ != "timestamp" && typ != "date" {
		return
	}
	if x.exprTypes == nil {
		x.exprTypes = map[sqlir.Expr]string{}
	}
	x.exprTypes[e] = typ
}

// toTimeType converts v to typ, the type Postgres resolves the branches of
// CASE or COALESCE or the arguments of GREATEST or LEAST to, where one is a
// timestamp or a date and another a timestamptz.
//
// A string literal or a parameter among them is read as typ too, and text
// detest does not read as one is refused.
func (x *sqlExec) toTimeType(v any, typ string) (any, error) {
	switch t := derefValue(v).(type) {
	case time.Time:
		if typ == "timestamptz" {
			return instantAt(t, x.tx.zone()), nil
		}
	case instant:
		if typ == "timestamp" {
			return wallIn(t.Time, x.tx.zone()), nil
		}
	case string:
		if typ == "" {
			return v, nil
		}
		out, err := x.cast(t, typ)
		if err != nil {
			return nil, err
		}
		if _, text := out.(string); text {
			return nil, x.unsupported(fmt.Sprintf("the %s text %q, in a format detest does not parse", typ, t))
		}
		return out, nil
	}
	return v, nil
}

// timeArgs converts the arguments of a call as Postgres resolves them before
// it runs the function: those of COALESCE, GREATEST and LEAST to the type
// they resolve to together, and a date given to date_trunc to the
// timestamptz Postgres casts it to.
func (x *sqlExec) timeArgs(f *sqlir.FuncCall, args []any) error {
	switch f.Name {
	case "coalesce", "greatest", "least":
		if typ := x.exprTypes[f]; typ != "" {
			for i := range args {
				v, err := x.toTimeType(args[i], typ)
				if err != nil {
					return err
				}
				args[i] = v
			}
		}
	case "date_trunc":
		if len(f.Args) == 2 && !x.tx.db.kind.InnoDB() && x.sourceType(f.Args[1]) == "date" {
			if t, ok := x.asInstant(args[1]); ok {
				args[1] = t
			}
		}
	}
	return nil
}
