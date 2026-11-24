package store

import (
	"strconv"
	"strings"
)

// ParseMoney reads a human price such as "12,500", "UGX 12500" or "3.50" into
// minor units. Fractions beyond `decimals` are only accepted when they are
// zeros ("12500.00" is fine for UGX, "12500.5" is not).
func ParseMoney(s string, decimals int) (int64, error) {
	var intPart, frac strings.Builder
	seenDot := false
	// Currency labels ("UGX 12,500", "12500 Shs") are allowed only at the ends,
	// so "1e5" is rejected rather than read as 15.
	body := strings.TrimFunc(s, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == ' ' || r == '\u00a0' || r == ':'
	})
	for _, r := range body {
		switch {
		case r >= '0' && r <= '9':
			if seenDot {
				frac.WriteRune(r)
			} else {
				intPart.WriteRune(r)
			}
		case r == '.':
			if seenDot {
				return 0, invalid("price", "price %q has more than one decimal point", s)
			}
			seenDot = true
		case r == ',' || r == ' ' || r == ' ' || r == ' ':
			// thousands separators
		default:
			return 0, invalid("price", "price %q is not a valid amount", s)
		}
	}
	if intPart.Len() == 0 && frac.Len() == 0 {
		return 0, invalid("price", "price is missing")
	}
	f := frac.String()
	if len(f) > decimals {
		if strings.Trim(f[decimals:], "0") != "" {
			return 0, invalid("price", "price %q has more decimal places than the store's currency allows (%d)", s, decimals)
		}
		f = f[:decimals]
	}
	f += strings.Repeat("0", decimals-len(f))
	digits := strings.TrimLeft(intPart.String()+f, "0")
	if digits == "" {
		return 0, nil
	}
	if len(digits) > 15 {
		return 0, invalid("price", "price %q is too large", s)
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > MaxPrice {
		return 0, invalid("price", "price %q is too large", s)
	}
	return n, nil
}

// FormatMoney renders minor units as plain text without separators ("12500",
// "3.50"), suitable for CSV and for round-tripping through ParseMoney.
func FormatMoney(v int64, decimals int) string {
	if decimals == 0 {
		return strconv.FormatInt(v, 10)
	}
	s := strconv.FormatInt(v, 10)
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	return s[:len(s)-decimals] + "." + s[len(s)-decimals:]
}
