package barcode

import "testing"

func TestCheckDigit(t *testing.T) {
	good := []string{"4006381333931", "036000291452", "96385074", "00012345678905", "5901234123457"}
	for _, g := range good {
		if !CheckDigitValid(g) {
			t.Errorf("%s should be valid", g)
		}
	}
	bad := []string{"4006381333932", "036000291453", "96385075", "5901234123458", "1", "", "12a4"}
	for _, b := range bad {
		if CheckDigitValid(b) {
			t.Errorf("%s should be invalid", b)
		}
	}
}

func TestWithCheckDigit(t *testing.T) {
	if got := WithCheckDigit("400638133393"); got != "4006381333931" {
		t.Fatalf("got %s", got)
	}
}

func TestCanonical(t *testing.T) {
	cases := []struct {
		in, want string
		err      error
	}{
		{"4006381333931", "04006381333931", nil},
		{" 4006 3813 33931 ", "04006381333931", nil},
		{"036000291452", "00036000291452", nil},  // UPC-A
		{"0036000291452", "00036000291452", nil}, // same product as EAN-13
		{"96385074", "00000096385074", nil},      // EAN-8
		{"4006381333932", "", ErrBadCheckDigit},  // misread
		{"4011", "4011", nil},                    // produce PLU
		{"sugar-1kg", "SUGAR-1KG", nil},          // internal
		{"", "", ErrEmpty},                       // nothing
		{"   ", "", ErrEmpty},                    // spaces
		{"a b/c", "", ErrBadChars},               // slash
		{"12345678901234567890123456789012345", "", ErrTooLong},
	}
	for _, c := range cases {
		got, err := Canonical(c.in)
		if err != c.err || got != c.want {
			t.Errorf("Canonical(%q) = %q, %v; want %q, %v", c.in, got, err, c.want, c.err)
		}
	}
}

func TestDisplay(t *testing.T) {
	cases := map[string]string{
		"04006381333931": "4006381333931",
		"00036000291452": "0036000291452",
		"00000096385074": "96385074",
		"14006381333938": "14006381333938",
		"SUGAR-1KG":      "SUGAR-1KG",
		"4011":           "4011",
	}
	for in, want := range cases {
		if got := Display(in); got != want {
			t.Errorf("Display(%s) = %s want %s", in, got, want)
		}
	}
}
