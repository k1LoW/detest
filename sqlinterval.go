package detest

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// pgInterval is a Postgres interval, which keeps its months, its days and
// its time apart: a month is a calendar month when added to a time, a day a
// calendar day, and only the time is a fixed length. They meet only when
// intervals are compared, where a month counts 30 days and a day 24 hours.
type pgInterval struct {
	months, days, micros int64
}

const (
	usecsPerDay  = int64(86400e6)
	daysPerMonth = 30
)

// String writes iv as Postgres prints an interval in its default style:
// 1 year 2 mons 3 days 04:05:06.5, with the time left out when it is zero
// and something else is not.
func (iv pgInterval) String() string {
	var parts []string
	// A field after a negative one carries its sign, as "-1 days +02:00:00".
	before := false
	plus := func(positive bool) string {
		if before && positive {
			return "+"
		}
		return ""
	}
	unit := func(n int64, name string) {
		if n == 0 {
			return
		}
		s := ""
		if n != 1 {
			s = "s"
		}
		parts = append(parts, fmt.Sprintf("%s%d %s%s", plus(n > 0), n, name, s))
		before = n < 0
	}
	unit(iv.months/12, "year")
	unit(iv.months%12, "mon")
	unit(iv.days, "day")
	if iv.micros != 0 || len(parts) == 0 {
		us := iv.micros
		sign := plus(us > 0)
		if us < 0 {
			sign, us = "-", -us
		}
		clock := fmt.Sprintf("%s%02d:%02d:%02d", sign, us/3600e6, us/60e6%60, us/1e6%60)
		if frac := us % 1e6; frac != 0 {
			clock += strings.TrimRight(fmt.Sprintf(".%06d", frac), "0")
		}
		parts = append(parts, clock)
	}
	return strings.Join(parts, " ")
}

// span is the length Postgres compares intervals by.
func (iv pgInterval) span() int64 {
	return (iv.months*daysPerMonth+iv.days)*usecsPerDay + iv.micros
}

func (iv pgInterval) neg() pgInterval {
	return pgInterval{-iv.months, -iv.days, -iv.micros}
}

func (iv pgInterval) add(o pgInterval) pgInterval {
	return pgInterval{iv.months + o.months, iv.days + o.days, iv.micros + o.micros}
}

// addToTime is t + iv as Postgres computes it: the months first, keeping
// the day of the month unless the month is shorter, then the days, then the
// time. A day is 24 hours, as in the session's TimeZone, UTC.
func (iv pgInterval) addToTime(t time.Time) time.Time {
	if iv.months != 0 {
		y, m, d := t.Date()
		total := int64(y)*12 + int64(m-1) + iv.months
		ny, nm := int(total/12), time.Month(total%12+1) // years are positive for times applications hold
		if last := time.Date(ny, nm+1, 0, 0, 0, 0, 0, time.UTC).Day(); d > last {
			d = last
		}
		hh, mm, ss := t.Clock()
		t = time.Date(ny, nm, d, hh, mm, ss, t.Nanosecond(), t.Location())
	}
	return t.AddDate(0, 0, int(iv.days)).Add(time.Duration(iv.micros) * time.Microsecond)
}

// timeDifference is a - b as Postgres gives it: the difference in time,
// whole days of it moved to days (justify_hours).
func timeDifference(a, b time.Time) pgInterval {
	us := a.Sub(b).Round(time.Microsecond).Microseconds()
	return pgInterval{days: us / usecsPerDay, micros: us % usecsPerDay}
}

// mul is iv * f as Postgres's interval_mul computes it, in floats: the
// fraction of the months moves to days at 30 a month, and that of the days
// to the time at 24 hours a day.
func (iv pgInterval) mul(f float64) (pgInterval, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return pgInterval{}, false
	}
	monthsF := float64(iv.months) * f
	months := math.Trunc(monthsF)
	// TSROUND, rint to six places, of both remainders, without which
	// '1 month' * 0.3333333333 comes out a microsecond short of 10 days.
	tsround := func(x float64) float64 { return math.RoundToEven(x*1e6) / 1e6 }
	monthRemainderDays := tsround((monthsF - months) * daysPerMonth)
	daysF := float64(iv.days) * f
	days := math.Trunc(daysF)
	secRemainder := tsround((daysF - days + monthRemainderDays - math.Trunc(monthRemainderDays)) * 86400)
	if math.Abs(secRemainder) >= 86400 {
		days += math.Trunc(secRemainder / 86400)
		secRemainder -= math.Trunc(secRemainder/86400) * 86400
	}
	days += math.Trunc(monthRemainderDays)
	micros := math.RoundToEven(float64(iv.micros)*f + secRemainder*1e6)
	if math.Abs(months) > math.MaxInt32 || math.Abs(days) > math.MaxInt32 || math.Abs(micros) > math.MaxInt64/2 {
		return pgInterval{}, false
	}
	return pgInterval{int64(months), int64(days), int64(micros)}, true
}

// intervalFields are an interval's fields as extract reads them: the years
// and months of its months, its days, and the hours, minutes and microseconds
// of its time, each with the sign of its field.
func (iv pgInterval) fields() (years, months, days, hours, minutes, usecs int64) {
	return iv.months / 12, iv.months % 12, iv.days, iv.micros / 3600e6, iv.micros / 60e6 % 60, iv.micros % 60e6
}

// extract is extract(unit FROM iv) as the decimal Postgres's numeric
// holds, or what explains a unit Postgres does not take from an interval.
func (iv pgInterval) extract(u string) (dec, what string) {
	years, months, days, hours, minutes, usecs := iv.fields()
	switch u {
	case "epoch":
		// A year is 365.25 days and a month 30, as Postgres reads them.
		secs := (iv.months/12)*31557600 + (iv.months%12)*daysPerMonth*86400 + iv.days*86400
		return micros(secs*1e6 + iv.micros), ""
	case "microseconds":
		return fmt.Sprint(usecs), ""
	case "milliseconds":
		return fmt.Sprintf("%s%d.%03d", signOf(usecs), abs(usecs)/1000, abs(usecs)%1000), ""
	case "second":
		return micros(usecs), ""
	case "minute":
		return fmt.Sprint(minutes), ""
	case "hour":
		return fmt.Sprint(hours), ""
	case "day":
		return fmt.Sprint(days), ""
	case "week":
		return fmt.Sprint(days / 7), ""
	case "month":
		return fmt.Sprint(months), ""
	case "quarter":
		return fmt.Sprint(months/3 + 1), ""
	case "year":
		return fmt.Sprint(years), ""
	case "decade":
		return fmt.Sprint(years / 10), ""
	case "century":
		return fmt.Sprint(years / 100), ""
	case "millennium":
		return fmt.Sprint(years / 1000), ""
	}
	return "", "extract of " + u + " from an interval, which Postgres refuses"
}

// trunc is date_trunc(u, iv): the fields below u set to zero. ok is false for
// a unit Postgres does not take for an interval, such as week.
func (iv pgInterval) trunc(u string) (pgInterval, bool) {
	years, months, days, hours, _, usecs := iv.fields()
	clock := func(us int64) pgInterval { return pgInterval{iv.months, iv.days, us} }
	switch u {
	case "microseconds":
		return iv, true
	case "milliseconds":
		return clock(iv.micros - usecs%1000), true
	case "second":
		return clock(iv.micros - usecs%1e6), true
	case "minute":
		return clock(iv.micros - usecs), true
	case "hour":
		return clock(hours * 3600e6), true
	case "day":
		return pgInterval{iv.months, days, 0}, true
	case "month":
		return pgInterval{iv.months, 0, 0}, true
	case "quarter":
		return pgInterval{years*12 + months/3*3, 0, 0}, true
	case "year":
		return pgInterval{years * 12, 0, 0}, true
	case "decade":
		return pgInterval{years / 10 * 120, 0, 0}, true
	case "century":
		return pgInterval{years / 100 * 1200, 0, 0}, true
	case "millennium":
		return pgInterval{years / 1000 * 12000, 0, 0}, true
	}
	return pgInterval{}, false
}

func signOf(n int64) string {
	if n < 0 {
		return "-"
	}
	return ""
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
