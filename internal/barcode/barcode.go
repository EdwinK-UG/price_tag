// Package barcode normalises the codes that shoppers scan and store owners type.
//
// Retail barcodes (EAN-8, UPC-A, EAN-13, GTIN-14) are all the same GS1
// number space, so we store them as 14-digit GTINs. That way a UPC-A scan and
// the EAN-13 form of the same product (leading 0) hit the same row. Anything
// else (a shop's own short codes, produce PLUs, "SUGAR-1KG") is kept as an
// "internal" code, upper-cased.
package barcode

import (
	"errors"
	"strings"
)

// MaxLen is the longest code we accept.
const MaxLen = 32

var (
	ErrEmpty         = errors.New("empty code")
	ErrTooLong       = errors.New("code too long")
	ErrBadChars      = errors.New("code may only contain letters, digits, '.', '-' and '_'")
	ErrBadCheckDigit = errors.New("check digit is wrong: the barcode was misread or mistyped")
)

// CheckDigitValid reports whether a GS1 number (GTIN-8/12/13/14) has a correct
// trailing check digit. The input must be all digits and at least 2 long.
func CheckDigitValid(d string) bool {
	n := len(d)
	if n < 2 {
		return false
	}
	sum := 0
	for i := 0; i < n-1; i++ {
		c := d[n-2-i]
		if c < '0' || c > '9' {
			return false
		}
		v := int(c - '0')
		if i%2 == 0 {
			sum += 3 * v
		} else {
			sum += v
		}
	}
	last := d[n-1]
	if last < '0' || last > '9' {
		return false
	}
	return (10-sum%10)%10 == int(last-'0')
}

// WithCheckDigit appends the GS1 check digit to a digit string (used by tests
// and the demo data generator).
func WithCheckDigit(body string) string {
	sum := 0
	for i := 0; i < len(body); i++ {
		v := int(body[len(body)-1-i] - '0')
		if i%2 == 0 {
			sum += 3 * v
		} else {
			sum += v
		}
	}
	return body + string(rune('0'+(10-sum%10)%10))
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// Canonical returns the storage/lookup form of a code. GTIN-shaped codes
// (8, 12, 13 or 14 digits) must have a valid check digit; that catches most
// camera misreads before they ever reach the network.
func Canonical(raw string) (string, error) {
	s := strings.ReplaceAll(strings.TrimSpace(raw), " ", "")
	if s == "" {
		return "", ErrEmpty
	}
	if len(s) > MaxLen {
		return "", ErrTooLong
	}
	if allDigits(s) {
		switch len(s) {
		case 8, 12, 13, 14:
			if !CheckDigitValid(s) {
				return "", ErrBadCheckDigit
			}
			return strings.Repeat("0", 14-len(s)) + s, nil
		}
		return s, nil // numeric internal code, e.g. a produce PLU
	}
	s = strings.ToUpper(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_'
		if !ok {
			return "", ErrBadChars
		}
	}
	return s, nil
}

// Display turns a canonical code back into the form printed on the pack:
// 14-digit GTINs are shown as EAN-13 (or EAN-8 when they were padded from 8).
func Display(canon string) string {
	if len(canon) == 14 && allDigits(canon) {
		if strings.HasPrefix(canon, "000000") {
			return canon[6:]
		}
		if canon[0] == '0' {
			return canon[1:]
		}
	}
	return canon
}
