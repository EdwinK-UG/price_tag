package barcode

import "testing"

// Canonical must never panic and must be idempotent: feeding its own output
// back in must give the same answer (otherwise lookups and stored keys could diverge).
func FuzzCanonical(f *testing.F) {
	for _, s := range []string{"4006381333931", "036000291452", "96385074", "sugar-1kg", "", "   ", "a b/c", "0000000000000", "\x00\xff", "１２３４５６７８"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		c, err := Canonical(in)
		if err != nil {
			return
		}
		again, err2 := Canonical(c)
		if err2 != nil || again != c {
			t.Fatalf("not idempotent: %q -> %q -> %q (%v)", in, c, again, err2)
		}
		if len(c) == 0 || len(c) > MaxLen {
			t.Fatalf("bad length for %q: %q", in, c)
		}
		if d := Display(c); d == "" {
			t.Fatalf("empty display for %q", c)
		}
	})
}
