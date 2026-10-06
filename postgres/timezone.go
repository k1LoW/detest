package postgres

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	pg "github.com/pganalyze/pg_query_go/v6"
)

// utcZones are the names of UTC among Postgres's time zones, which Postgres
// reads whatever their case.
var utcZones = map[string]bool{
	"utc": true, "etc/utc": true, "uct": true, "etc/uct": true, "gmt": true, "etc/gmt": true,
	"universal": true, "etc/universal": true, "zulu": true, "etc/zulu": true,
}

// loadZone reads an IANA time zone name, as Postgres reads the TimeZone
// setting, with time.LoadLocation.
//
// A name of UTC is UTC in any case, as in Postgres. Another name is read as
// written: Postgres reads 'asia/tokyo' too, and tzdata on a case-sensitive
// file system does not. A POSIX zone such as 'UTC+9' or '+09:00', whose
// offset Postgres takes with the sign reversed, and abbreviations, which
// Postgres refuses as a setting, are not read either, nor "Local", which Go
// reads as the machine's zone and Postgres refuses.
func loadZone(name string) (*time.Location, error) {
	if utcZones[strings.ToLower(name)] {
		return time.UTC, nil
	}
	if name == "" || name == "Local" || strings.ContainsAny(name, "+-0123456789") && !strings.Contains(name, "/") {
		return nil, fmt.Errorf("detest: time zone %q is not an IANA time zone name detest reads", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("detest: time zone %q: %w", name, err)
	}
	return loc, nil
}

// timeZoneSet reads SET TIME ZONE and SET timezone: the zone named, an offset
// in hours (SET TIME ZONE 9), or nil for the server's TimeZone, which LOCAL,
// DEFAULT and RESET return to. ok is false for a statement setting
// something else.
func (c *pgConv) timeZoneSet(v *pg.VariableSetStmt) (zone *time.Location, ok bool, err error) {
	if !strings.EqualFold(v.Name, "timezone") {
		return nil, false, nil
	}
	switch v.Kind {
	case pg.VariableSetKind_VAR_SET_DEFAULT, pg.VariableSetKind_VAR_RESET, pg.VariableSetKind_VAR_SET_CURRENT:
		if v.Kind == pg.VariableSetKind_VAR_SET_CURRENT {
			return nil, true, c.unsupported("SET TIME ZONE FROM CURRENT")
		}
		return nil, true, nil
	case pg.VariableSetKind_VAR_SET_VALUE:
	default:
		return nil, true, c.unsupported("this form of SET TIME ZONE")
	}
	if len(v.Args) != 1 || v.Args[0].GetAConst() == nil {
		// SET TIME ZONE INTERVAL '+09:00' HOUR TO MINUTE, which Postgres
		// reads, is not.
		return nil, true, c.unsupported("SET TIME ZONE to an interval or a list")
	}
	k := v.Args[0].GetAConst()
	hours, isNum := 0.0, false
	switch {
	case k.GetIval() != nil:
		hours, isNum = float64(k.GetIval().Ival), true
	case k.GetFval() != nil:
		f, err := parseFloat(k.GetFval().Fval)
		if err != nil {
			return nil, true, c.unsupported("SET TIME ZONE to this number")
		}
		hours, isNum = f, true
	case k.GetSval() != nil:
		loc, err := loadZone(k.GetSval().Sval)
		if err != nil {
			return nil, true, c.unsupported(fmt.Sprintf("SET TIME ZONE to %q, which detest does not read as an IANA time zone name", k.GetSval().Sval))
		}
		return loc, true, nil
	}
	if !isNum {
		return nil, true, c.unsupported("SET TIME ZONE to this value")
	}
	// An offset in hours east of UTC, which Postgres takes to the second.
	secs := math.Round(hours * 3600)
	if secs == 0 {
		return time.UTC, true, nil
	}
	return time.FixedZone(offsetName(int(secs)), int(secs)), true, nil
}

func offsetName(secs int) string {
	sign := "+"
	if secs < 0 {
		sign, secs = "-", -secs
	}
	return fmt.Sprintf("%s%02d:%02d", sign, secs/3600, secs/60%60)
}

func parseFloat(s string) (float64, error) {
	var f float64
	if _, err := fmt.Sscan(s, &f); err != nil {
		return 0, err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("not a finite number")
	}
	return f, nil
}
