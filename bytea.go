package detest

import (
	"fmt"

	"github.com/k1LoW/detest/internal/sqlir"
)

// byteaError is why text is not bytea input, with the kind Postgres fails
// it with: 22023 for the hex format, 22P02 for the escape format.
type byteaError struct {
	kind sqlir.DBErrorKind
	msg  string
}

// parseBytea reads text as Postgres's bytea input does: \x followed by
// pairs of hex digits, which whitespace may separate, or the escape format,
// where \\ is a backslash, \ followed by three octal digits the byte they
// give, and any other character itself.
func parseBytea(s string) ([]byte, *byteaError) {
	if len(s) >= 2 && s[0] == '\\' && s[1] == 'x' {
		out := make([]byte, 0, (len(s)-2)/2)
		for i := 2; i < len(s); {
			switch s[i] {
			case ' ', '\t', '\n', '\r', '\v', '\f':
				i++
				continue
			}
			hi, ok := hexDigit(s[i])
			if !ok {
				return nil, &byteaError{sqlir.InvalidParameterValue, fmt.Sprintf("invalid hexadecimal digit: %q", s[i:i+1])}
			}
			if i+1 >= len(s) {
				return nil, &byteaError{sqlir.InvalidParameterValue, "invalid hexadecimal data: odd number of digits"}
			}
			lo, ok := hexDigit(s[i+1])
			if !ok {
				return nil, &byteaError{sqlir.InvalidParameterValue, fmt.Sprintf("invalid hexadecimal digit: %q", s[i+1:i+2])}
			}
			out = append(out, hi<<4|lo)
			i += 2
		}
		return out, nil
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			out = append(out, s[i])
			i++
			continue
		}
		switch {
		case i+1 < len(s) && s[i+1] == '\\':
			out = append(out, '\\')
			i += 2
		case i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '3' && isOctal(s[i+2]) && isOctal(s[i+3]):
			out = append(out, (s[i+1]-'0')<<6|(s[i+2]-'0')<<3|(s[i+3]-'0'))
			i += 4
		default:
			return nil, &byteaError{sqlir.InvalidTextRepresentation, "invalid input syntax for type bytea"}
		}
	}
	return out, nil
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
