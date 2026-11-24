package demo

import (
	"context"
	"testing"

	"pricetag/internal/barcode"
	"pricetag/internal/store"
)

func TestSeedIsComplete(t *testing.T) {
	dir := t.TempDir()
	info, err := Seed(dir, "demo")
	if err != nil || !info.Created {
		t.Fatalf("%+v %v", info, err)
	}
	st, err := store.Open(dir, "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stats, _ := st.Stats(context.Background())
	if stats.Products != len(Items) || stats.ActivePromos != 2 {
		t.Fatalf("%+v", stats)
	}
	// every demo barcode must be a valid EAN-13 in the in-store range
	seen := map[string]bool{}
	for i := range Items {
		c := Code(i)
		if len(c) != 13 || c[0] != '2' || !barcode.CheckDigitValid(c) || seen[c] {
			t.Fatalf("bad demo code %s", c)
		}
		seen[c] = true
	}
	if _, err := st.Authenticate(context.Background(), OwnerEmail, OwnerPassword); err != nil {
		t.Fatal(err)
	}
	again, err := Seed(dir, "demo")
	if err != nil || again.Created {
		t.Fatal("seeding twice must not touch the store")
	}
}
