package domain

import (
	"errors"
	"fmt"
	"strings"
)

// NumberFormat describes how a provider writes decimal amounts in major units.
// The format is declared per provider adapter, never sniffed: "1.500" means
// fifteen hundred in one convention and one-and-a-half in the other, and no
// heuristic can tell them apart.
type NumberFormat struct {
	Decimal byte // decimal separator
	Group   byte // thousands separator; 0 means grouping is not allowed
}

var (
	// DotDecimal reads 1500000.00 or 1,500,000.00.
	DotDecimal = NumberFormat{Decimal: '.', Group: ','}
	// CommaDecimal (Indonesian / continental style) reads 1500000,00 or 1.500.000,00.
	CommaDecimal = NumberFormat{Decimal: ',', Group: '.'}
)

// Amount parsing errors.
var (
	ErrAmountSyntax    = errors.New("malformed amount")
	ErrAmountPrecision = errors.New("amount has more decimal places than the currency allows")
	ErrAmountRange     = errors.New("amount out of range")
)

// ParseAmount converts a major-unit decimal string into int64 minor units with
// exp fractional digits. It accepts an optional sign or accounting-style
// parentheses, optional thousands grouping (validated: groups of exactly three
// digits), and fewer fractional digits than exp. It never rounds: extra
// fractional digits are accepted only if they are zeros, otherwise
// ErrAmountPrecision - silently rounding money is how cents go missing.
func ParseAmount(s string, f NumberFormat, exp int) (int64, error) {
	if exp < 0 || exp > 6 {
		return 0, fmt.Errorf("%w: unsupported exponent %d", ErrAmountSyntax, exp)
	}
	s = strings.TrimSpace(s)
	neg := false
	switch {
	case len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')':
		neg, s = true, strings.TrimSpace(s[1:len(s)-1])
	case len(s) > 0 && (s[0] == '-' || s[0] == '+'):
		neg, s = s[0] == '-', s[1:]
	}
	if s == "" {
		return 0, ErrAmountSyntax
	}

	intPart, frac := s, ""
	if i := strings.IndexByte(s, f.Decimal); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
		if frac == "" {
			return 0, fmt.Errorf("%w: %q has a trailing decimal separator", ErrAmountSyntax, s)
		}
	}
	if intPart == "" {
		return 0, fmt.Errorf("%w: %q has no integer digits", ErrAmountSyntax, s)
	}

	var v int64
	var err error
	if v, err = accumulateInt(intPart, f.Group); err != nil {
		return 0, err
	}

	// Fractional part: digits only; anything beyond exp must be zero.
	for i := 0; i < len(frac); i++ {
		c := frac[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: unexpected %q in fraction", ErrAmountSyntax, c)
		}
		if i >= exp && c != '0' {
			return 0, ErrAmountPrecision
		}
	}
	for i := 0; i < exp; i++ {
		d := int64(0)
		if i < len(frac) {
			d = int64(frac[i] - '0')
		}
		if v > (MaxAmount-d)/10 {
			return 0, ErrAmountRange
		}
		v = v*10 + d
	}
	if neg {
		v = -v
	}
	return v, nil
}

// accumulateInt parses the integer part, validating thousands grouping when
// present: first group 1-3 digits, every following group exactly 3.
func accumulateInt(s string, group byte) (int64, error) {
	var v int64
	grouped := group != 0 && strings.IndexByte(s, group) >= 0
	run := 0 // digits since the last separator
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			run++
			if grouped && run > 3 {
				return 0, fmt.Errorf("%w: bad digit grouping in %q", ErrAmountSyntax, s)
			}
			d := int64(c - '0')
			if v > (MaxAmount-d)/10 {
				return 0, ErrAmountRange
			}
			v = v*10 + d
		case grouped && c == group:
			if run == 0 || (i != run && run != 3) {
				// empty group, or a non-leading group that is not 3 digits long
				return 0, fmt.Errorf("%w: bad digit grouping in %q", ErrAmountSyntax, s)
			}
			run = 0
		default:
			return 0, fmt.Errorf("%w: unexpected %q", ErrAmountSyntax, c)
		}
	}
	if grouped && run != 3 {
		return 0, fmt.Errorf("%w: bad digit grouping in %q", ErrAmountSyntax, s)
	}
	return v, nil
}

// FormatAmount renders minor units as a major-unit decimal string in format f.
// With group=true thousands separators are inserted. It is the exact inverse
// of ParseAmount for every value in range (fuzz-tested).
func FormatAmount(minor int64, exp int, f NumberFormat, group bool) string {
	neg := minor < 0
	u := uint64(minor)
	if neg {
		u = uint64(-minor)
	}
	digits := fmt.Sprintf("%0*d", exp+1, u)
	intPart, frac := digits[:len(digits)-exp], digits[len(digits)-exp:]

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	if group && f.Group != 0 {
		lead := len(intPart) % 3
		if lead == 0 {
			lead = 3
		}
		b.WriteString(intPart[:lead])
		for i := lead; i < len(intPart); i += 3 {
			b.WriteByte(f.Group)
			b.WriteString(intPart[i : i+3])
		}
	} else {
		b.WriteString(intPart)
	}
	if exp > 0 {
		b.WriteByte(f.Decimal)
		b.WriteString(frac)
	}
	return b.String()
}

// FormatMoney is the human-readable form used in CLI output: "1,500,000.00 IDR".
func FormatMoney(minor int64, c Currency) string {
	exp := c.Exponent()
	if exp < 0 {
		return fmt.Sprintf("%d %s(minor)", minor, c)
	}
	return FormatAmount(minor, exp, DotDecimal, true) + " " + string(c)
}
