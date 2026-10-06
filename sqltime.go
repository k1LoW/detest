package detest

import (
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode"

	"github.com/k1LoW/detest/internal/sqlir"
)

// This file reads Postgres's interval text and computes the date and time
// functions detest runs: CURRENT_DATE, a cast to date, date_trunc, extract
// and date_part. They compute on a clock, a timestamp's or a date's, and a
// timestamptz's is the clock it shows in the session's TimeZone (sqltz.go).

// intervalUnit is a unit of interval text: the field of the interval it
// counts in, months, days or microseconds, and how many of them it is.
type intervalUnit struct {
	name  string
	field byte // 'M', 'D' or 'U'
	per   int64
}

// intervalUnits are the units of interval text, by the names Postgres
// accepts for them.
var intervalUnits = func() map[string]intervalUnit {
	m := map[string]intervalUnit{}
	for _, u := range []struct {
		unit  intervalUnit
		names string
	}{
		{intervalUnit{"microsecond", 'U', 1}, "microsecond microseconds microsecon us usec usecs usecond useconds"},
		{intervalUnit{"millisecond", 'U', 1e3}, "millisecond milliseconds millisecon ms msec msecs msecond mseconds"},
		{intervalUnit{"second", 'U', 1e6}, "second seconds s sec secs"},
		{intervalUnit{"minute", 'U', 60e6}, "minute minutes m min mins"},
		{intervalUnit{"hour", 'U', 3600e6}, "hour hours h hr hrs"},
		{intervalUnit{"day", 'D', 1}, "day days d"},
		{intervalUnit{"week", 'D', 7}, "week weeks w"},
		{intervalUnit{"month", 'M', 1}, "month months mon mons"},
		{intervalUnit{"year", 'M', 12}, "year years y yr yrs"},
		{intervalUnit{"decade", 'M', 120}, "decade decades dec decs"},
		{intervalUnit{"century", 'M', 1200}, "century centuries c cent"},
		{intervalUnit{"millennium", 'M', 12000}, "millennium millennia mil mils"},
	} {
		for n := range strings.FieldsSeq(u.names) {
			m[n] = u.unit
		}
	}
	return m
}()

// intervalError is why interval text was not read: malformed is text Postgres
// refuses too (22007), and otherwise it is a form detest does not read.
type intervalError struct {
	what      string
	malformed bool
}

// parseInterval reads Postgres's interval text in its own style: numbers
// with units, such as '2 days 3 hours' or '10min', a time such as '1:30' or
// '02:03:04.5', a number alone as seconds, a leading '@' and a trailing
// 'ago'. A unit given twice is malformed, as in Postgres. ISO 8601 ('P1D'),
// a fraction of a year that is no whole number of months and a fraction of
// a microsecond, both of which Postgres rounds, are not read.
func parseInterval(s string) (pgInterval, *intervalError) {
	fields := strings.Fields(strings.ToLower(s))
	if len(fields) > 0 && fields[0] == "@" {
		fields = fields[1:]
	}
	ago := false
	if len(fields) > 0 && fields[len(fields)-1] == "ago" {
		ago, fields = true, fields[:len(fields)-1]
	}
	if len(fields) == 0 {
		return pgInterval{}, &intervalError{"empty interval text", true}
	}
	months, days, us := new(big.Rat), new(big.Rat), new(big.Rat)
	seen := map[string]bool{}
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if strings.Contains(f, ":") {
			d, err := parseClock(f)
			if err != nil {
				return pgInterval{}, err
			}
			for _, u := range []string{"hour", "minute", "second"} {
				if seen[u] {
					// Postgres fails a time with an hour, a minute or a second
					// given apart, and a second time, as malformed.
					return pgInterval{}, &intervalError{"a time with an hour, minute or second unit", true}
				}
				seen[u] = true
			}
			us.Add(us, d)
			continue
		}
		if (f == "+" || f == "-") && i+1 < len(fields) && startsNumber(fields[i+1]) {
			fields[i+1] = f + fields[i+1] // '+ 1 day' is '+1 day'
			continue
		}
		num, unitName := splitNumber(f)
		if num == "" {
			// ISO 8601 (P1D) and infinity are interval text Postgres reads;
			// any other word without a number is malformed.
			if f == "infinity" || f == "-infinity" || f == "+infinity" || isoInterval(f) {
				return pgInterval{}, &intervalError{fmt.Sprintf("interval text %q", s), false}
			}
			return pgInterval{}, &intervalError{fmt.Sprintf("interval text %q", s), true}
		}
		if strings.TrimLeft(num, "+-") == "." {
			num = "0" // Postgres reads a point alone as zero
		}
		n, ok := new(big.Rat).SetString(num)
		if !ok {
			// num holds a sign, digits and points only, so it is malformed,
			// such as 1.2.3.
			return pgInterval{}, &intervalError{fmt.Sprintf("the number %q in interval text", num), true}
		}
		if unitName == "" && i+1 < len(fields) && !startsNumber(fields[i+1]) {
			i++
			unitName = fields[i]
		}
		if unitName == "" {
			unitName = "second" // a number alone is seconds
		}
		u, ok := intervalUnits[unitName]
		if !ok {
			// A number in exponent form, such as 1e3, is malformed too, and
			// so is a quarter, which Postgres has no interval unit for.
			return pgInterval{}, &intervalError{fmt.Sprintf("the interval unit %q", unitName), true}
		}
		if seen[u.name] {
			return pgInterval{}, &intervalError{"a unit given twice", true}
		}
		seen[u.name] = true
		v := new(big.Rat).Mul(n, new(big.Rat).SetInt64(u.per))
		if u.field == 'M' && u.name != "month" && !v.IsInt() {
			// Postgres rounds a fraction of a year to whole months.
			return pgInterval{}, &intervalError{"a fraction of a " + u.name + " that is no whole number of months", false}
		}
		// Each unit's fraction is carried down as Postgres reads the unit,
		// a fraction of a month to days at 30 a month and a fraction of a
		// day to time at 24 hours, so '1 year -0.5 months' is 12 months and
		// -15 days rather than 11 months and 15 days.
		if u.field == 'M' {
			whole, frac := splitRat(v)
			months.Add(months, new(big.Rat).SetInt(whole))
			v = frac.Mul(frac, big.NewRat(daysPerMonth, 1))
		}
		if u.field != 'U' {
			whole, frac := splitRat(v)
			days.Add(days, new(big.Rat).SetInt(whole))
			v = frac.Mul(frac, new(big.Rat).SetInt64(usecsPerDay))
		}
		us.Add(us, v)
	}
	if !us.IsInt() {
		return pgInterval{}, &intervalError{"an interval with a fraction of a microsecond", false}
	}
	m, d := months.Num(), days.Num()
	if !us.Num().IsInt64() || !m.IsInt64() || !d.IsInt64() {
		return pgInterval{}, &intervalError{"an interval beyond the range detest reads", false}
	}
	iv := pgInterval{m.Int64(), d.Int64(), us.Num().Int64()}
	if ago {
		iv = iv.neg()
	}
	return iv, nil
}

// splitRat is r's whole part, toward zero, and the fraction left.
func splitRat(r *big.Rat) (*big.Int, *big.Rat) {
	whole := new(big.Int).Quo(r.Num(), r.Denom())
	return whole, new(big.Rat).Sub(r, new(big.Rat).SetInt(whole))
}

// parseClock reads [+|-]h:m[:s[.f]] in microseconds.
func parseClock(f string) (*big.Rat, *intervalError) {
	neg := strings.HasPrefix(f, "-")
	if neg || strings.HasPrefix(f, "+") {
		f = f[1:] // one sign: another is malformed
	}
	parts := strings.Split(f, ":")
	if len(parts) > 3 {
		return nil, &intervalError{"a time of more than three parts", true}
	}
	units := []int64{3600e6, 60e6, 1e6}
	if len(parts) == 2 && strings.Contains(parts[1], ".") {
		units = units[1:] // m:s.f, as Postgres reads '1:2.5'
	}
	total := new(big.Rat)
	for i, p := range parts {
		if p == "" || strings.ContainsAny(p, "+-eE/") {
			return nil, &intervalError{fmt.Sprintf("the time %q in interval text", f), true}
		}
		if i < len(parts)-1 && strings.Contains(p, ".") {
			// A fraction before a colon, as in 1.5:00, is malformed.
			return nil, &intervalError{fmt.Sprintf("the time %q in interval text", f), true}
		}
		n, ok := new(big.Rat).SetString(p)
		if !ok {
			return nil, &intervalError{fmt.Sprintf("the time %q in interval text", f), true}
		}
		unit := units[i]
		total.Add(total, new(big.Rat).Mul(n, new(big.Rat).SetInt64(unit)))
	}
	if neg {
		total.Neg(total)
	}
	return total, nil
}

// isoInterval reports whether f looks like ISO 8601 interval text, such as
// P1D or PT1H: a P followed by a digit or a T.
func isoInterval(f string) bool {
	return len(f) > 1 && f[0] == 'p' && (f[1] == 't' || f[1] >= '0' && f[1] <= '9')
}

// splitNumber splits a field such as "10min" into its number and its unit.
func splitNumber(f string) (num, unit string) {
	i := 0
	if i < len(f) && (f[i] == '+' || f[i] == '-') {
		i++
	}
	start := i
	for i < len(f) && (f[i] >= '0' && f[i] <= '9' || f[i] == '.') {
		i++
	}
	if i == start {
		return "", ""
	}
	return f[:i], f[i:]
}

func startsNumber(f string) bool {
	f = strings.TrimLeft(f, "+-")
	return f != "" && (unicode.IsDigit(rune(f[0])) || f[0] == '.')
}

// timeUnits are the names of the units of date_trunc and extract, as
// Postgres reads them, by the unit each names.
var timeUnits = map[string]string{
	"microsecond": "microseconds", "microseconds": "microseconds", "us": "microseconds", "usec": "microseconds", "usecs": "microseconds", "useconds": "microseconds", "microsecon": "microseconds",
	"millisecond": "milliseconds", "milliseconds": "milliseconds", "ms": "milliseconds", "msec": "milliseconds", "msecs": "milliseconds", "mseconds": "milliseconds", "millisecon": "milliseconds",
	"second": "second", "seconds": "second", "s": "second", "sec": "second", "secs": "second",
	"minute": "minute", "minutes": "minute", "m": "minute", "min": "minute", "mins": "minute",
	"hour": "hour", "hours": "hour", "h": "hour", "hr": "hour", "hrs": "hour",
	"day": "day", "days": "day", "d": "day",
	"week": "week", "weeks": "week", "w": "week",
	"month": "month", "months": "month", "mon": "month", "mons": "month",
	"quarter": "quarter", "qtr": "quarter",
	"year": "year", "years": "year", "y": "year", "yr": "year", "yrs": "year",
	"decade": "decade", "decades": "decade", "dec": "decade", "decs": "decade",
	"century": "century", "centuries": "century", "c": "century", "cent": "century",
	"millennium": "millennium", "millennia": "millennium", "mil": "millennium", "mils": "millennium",
	// extract's alone
	"epoch": "epoch", "dow": "dow", "isodow": "isodow", "doy": "doy", "isoyear": "isoyear",
	"julian": "julian", "timezone": "timezone", "timezone_h": "timezone_hour", "timezone_hour": "timezone_hour", "timezone_m": "timezone_minute", "timezone_minute": "timezone_minute",
}

// extractAliases are spellings extract and date_part take, from Postgres's
// table of special tokens, which date_trunc does not.
var extractAliases = map[string]string{"mm": "minute", "j": "julian", "jd": "julian"}

// extractUnit is the unit extract and date_part read unit as.
func extractUnit(unit string) (string, bool) {
	unit = strings.ToLower(unit)
	if u, ok := timeUnits[unit]; ok {
		return u, true
	}
	u, ok := extractAliases[unit]
	return u, ok
}

// extractOnly are the units extract reads that date_trunc does not.
var extractOnly = map[string]bool{"epoch": true, "dow": true, "isodow": true, "doy": true, "isoyear": true, "julian": true, "timezone": true, "timezone_hour": true, "timezone_minute": true}

// errUnit is Postgres's error for a unit it does not take, which it raises
// whatever the rows. The unit is checked before the source is evaluated,
// so the message names a timestamptz whatever the source is.
func (x *sqlExec) errUnit(unit string) error {
	return x.tx.db.kind.Error(sqlir.InvalidParameterValue, fmt.Sprintf("unit %q not recognized for type timestamp with time zone", unit), "", "", "")
}

// dateTrunc is date_trunc(unit, t) of the clock t, as Postgres computes it
// after reading t to microseconds. ok is false for a unit date_trunc does not take.
func dateTrunc(unit string, t time.Time) (time.Time, bool) {
	t = t.UTC().Round(time.Microsecond)
	u, ok := timeUnits[strings.ToLower(unit)]
	if !ok || extractOnly[u] {
		return time.Time{}, false
	}
	y, mo, d := t.Date()
	day := func(y int, mo time.Month, d int) time.Time { return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC) }
	switch u {
	case "microseconds":
		return t, true
	case "milliseconds":
		return t.Truncate(time.Millisecond), true
	case "second":
		return t.Truncate(time.Second), true
	case "minute":
		return t.Truncate(time.Minute), true
	case "hour":
		return t.Truncate(time.Hour), true
	case "day":
		return day(y, mo, d), true
	case "week":
		// The Monday of the ISO week.
		return day(y, mo, d-(int(t.Weekday())+6)%7), true
	case "month":
		return day(y, mo, 1), true
	case "quarter":
		return day(y, mo-(mo-1)%3, 1), true
	case "year":
		return day(y, 1, 1), true
	case "decade":
		return day(y-y%10, 1, 1), true
	case "century":
		return day((y-1)/100*100+1, 1, 1), true
	case "millennium":
		return day((y-1)/1000*1000+1, 1, 1), true
	}
	return time.Time{}, false
}

// extractField is extract(unit FROM v) as the decimal Postgres's numeric
// result holds, for a time or an interval. unknown is a unit Postgres does
// not take, and what a unit or a source detest does not compute.
func extractField(unit string, v any) (dec string, unknown bool, what string) {
	u, ok := extractUnit(unit)
	if !ok {
		return "", true, ""
	}
	switch v := v.(type) {
	case pgInterval:
		dec, what := v.extract(u)
		return dec, false, what
	case time.Time:
		t := v.UTC().Round(time.Microsecond)
		y, mo, d := t.Date()
		usec := int64(t.Nanosecond() / 1000)
		switch u {
		case "microseconds":
			return fmt.Sprint(int64(t.Second())*1e6 + usec), false, ""
		case "milliseconds":
			us := int64(t.Second())*1e6 + usec
			return fmt.Sprintf("%d.%03d", us/1000, us%1000), false, ""
		case "second":
			return micros(int64(t.Second())*1e6 + usec), false, ""
		case "minute":
			return fmt.Sprint(t.Minute()), false, ""
		case "hour":
			return fmt.Sprint(t.Hour()), false, ""
		case "day":
			return fmt.Sprint(d), false, ""
		case "month":
			return fmt.Sprint(int(mo)), false, ""
		case "quarter":
			return fmt.Sprint((int(mo)-1)/3 + 1), false, ""
		case "year":
			return fmt.Sprint(y), false, ""
		case "decade":
			return fmt.Sprint(y / 10), false, ""
		case "century":
			return fmt.Sprint((y + 99) / 100), false, ""
		case "millennium":
			return fmt.Sprint((y + 999) / 1000), false, ""
		case "week":
			_, w := t.ISOWeek()
			return fmt.Sprint(w), false, ""
		case "isoyear":
			iy, _ := t.ISOWeek()
			return fmt.Sprint(iy), false, ""
		case "dow":
			return fmt.Sprint(int(t.Weekday())), false, ""
		case "isodow":
			return fmt.Sprint((int(t.Weekday())+6)%7 + 1), false, ""
		case "doy":
			return fmt.Sprint(t.YearDay()), false, ""
		case "epoch":
			return micros(t.Unix()*1e6 + usec), false, ""
		}
		return "", false, "extract of " + u
	}
	return "", false, fmt.Sprintf("extract from a %T", v)
}

// micros writes a number of microseconds as seconds, with the fraction
// Postgres keeps.
func micros(us int64) string {
	neg := us < 0
	if neg {
		us = -us
	}
	s := fmt.Sprintf("%d.%06d", us/1e6, us%1e6)
	if neg {
		s = "-" + s
	}
	return s
}

// utcDate is the date of the clock t at midnight, as a date compares with a
// timestamp.
func utcDate(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
