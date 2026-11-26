package store

import (
	"strings"
	"testing"
)

func FuzzParseMoney(f *testing.F) {
	for _, s := range []string{"12500", "UGX 12,500", "3.50", "", "-5", "1e5", "99999999999999999999", "١٢٣", "1.2.3", "\x00"} {
		f.Add(s, 0)
		f.Add(s, 2)
	}
	f.Fuzz(func(t *testing.T, s string, dec int) {
		dec = ((dec % 4) + 4) % 4
		v, err := ParseMoney(s, dec)
		if err != nil {
			return
		}
		if v < 0 || v > MaxPrice {
			t.Fatalf("out of range %d from %q", v, s)
		}
		// what we format must parse back to the same number
		back, err := ParseMoney(FormatMoney(v, dec), dec)
		if err != nil || back != v {
			t.Fatalf("round trip %q -> %d -> %q -> %d (%v)", s, v, FormatMoney(v, dec), back, err)
		}
	})
}

func FuzzParseCSV(f *testing.F) {
	f.Add("code,name,price\n4006381333931,Sugar,5500\n")
	f.Add("\ufeffBarcode;Item;Selling Price\n1;a;\"1,000\"\n")
	f.Add("a\tb\tc\n")
	f.Add("code,name,price\n\"unterminated,x,1\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, in string) {
		rows, errs, err := ParseCSV(strings.NewReader(in), 0)
		if err != nil {
			return
		}
		for _, r := range rows {
			if r.Row < 2 || r.In.Price < 0 {
				t.Fatalf("bad row %+v", r)
			}
		}
		for _, e := range errs {
			if e.Message == "" {
				t.Fatal("error without message")
			}
		}
	})
}

func FuzzNormalizeCheckoutCode(f *testing.F) {
	for _, s := range []string{"", "K7M9P2", "k7m-9p2", " k7m 9p2 ", "000000", "OOOOOO", "111111", "IIIIII", "LLLLLL", "ABCDE", "ABCDEFG", "K7M9P2\x00", strings.Repeat("A", 100)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		norm, ok := NormalizeCheckoutCode(raw)
		if !ok {
			if norm != "" {
				t.Fatalf("invalid code returned non-empty %q", norm)
			}
			return
		}
		if len(norm) != checkoutCodeLen {
			t.Fatalf("len(%q) != %d", norm, checkoutCodeLen)
		}
		for i := 0; i < len(norm); i++ {
			c := norm[i]
			if strings.ContainsRune("0O1IL", rune(c)) || !strings.ContainsRune(checkoutAlphabet, rune(c)) {
				t.Fatalf("forbidden char %q in %q", c, norm)
			}
		}
		// Must be idempotent.
		again, ok2 := NormalizeCheckoutCode(norm)
		if !ok2 || again != norm {
			t.Fatalf("not idempotent: %q -> %q (%v)", norm, again, ok2)
		}
	})
}
