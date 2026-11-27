package store

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func enableCheckout(t testing.TB, s *Store, ttl int, mode string) {
	t.Helper()
	set := s.Settings()
	set.CheckoutEnabled = true
	if ttl > 0 {
		set.CheckoutTTLMin = ttl
	}
	if mode != "" {
		set.CheckoutDefaultMode = mode
	}
	if err := s.UpdateSettings(ctx, "owner@example.com", set); err != nil {
		t.Fatal(err)
	}
}

// Two processes (a server and a CLI command, say) opening the same old file at
// the same moment must both succeed: the second must not re-run a migration.
func TestConcurrentOpensMigrateOnce(t *testing.T) {
	for round := 0; round < 5; round++ {
		dir := t.TempDir()
		w, r, err := openDB(DBPath(dir, "legacy"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Exec(schemaSQL); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Exec(`INSERT INTO meta(key,value) VALUES('schema_version','1'),('v','0'),('home_rev','0'),('purged_before','0'),('settings','{"name":"Old Shop","currency":"UGX","decimals":0,"tz_offset":180}'),('created_at','1700000000')`); err != nil {
			t.Fatal(err)
		}
		w.Close()
		r.Close()
		const n = 4
		errs := make(chan error, n)
		stores := make(chan *Store, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			go func() {
				<-start
				s, err := Open(dir, "legacy")
				errs <- err
				stores <- s
			}()
		}
		close(start)
		for i := 0; i < n; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: concurrent open failed: %v", round, err)
			}
			if s := <-stores; s != nil {
				var sv string
				s.r.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&sv)
				if sv != strconv.Itoa(schemaVersion) {
					t.Fatalf("schema_version %q", sv)
				}
				s.Close()
			}
		}
	}
}

func TestSchemaMigrationV1ToV2(t *testing.T) {
	dir := t.TempDir()
	path := DBPath(dir, "legacy")
	w, r, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	// Build a genuine schema v1 store file (no checkout tables, v1 settings JSON).
	tx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('schema_version','1'),('v','1'),('home_rev','0'),('purged_before','0'),('settings','{"name":"Old Shop","currency":"UGX","decimals":0,"tz_offset":180,"announcement":"Hi"}'),('created_at','1700000000')`); err != nil {
		t.Fatal(err)
	}
	sugar := code("600123456789")
	if _, err := tx.Exec(`INSERT INTO products(code,name,size,category,price,deleted,rev,updated_at) VALUES(?,?,?,?,?,0,1,1700000000)`, "0"+sugar, "Sugar", "1 kg", "Pantry", 5500); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	r.Close()

	// Opening with current build must migrate cleanly from v1 -> v2 without losing data.
	s, err := Open(dir, "legacy")
	if err != nil {
		t.Fatalf("open v1 store: %v", err)
	}
	defer s.Close()

	var sv string
	if err := s.r.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&sv); err != nil || sv != strconv.Itoa(schemaVersion) {
		t.Fatalf("schema_version = %q, err=%v", sv, err)
	}
	set := s.Settings()
	if set.Name != "Old Shop" || set.CheckoutEnabled || set.CheckoutTTLMin != DefaultCheckoutTTLMin || set.CheckoutDefaultMode != CheckoutModeTick || set.AuditEnabled || set.AuditRate != 0 {
		t.Fatalf("migrated settings = %+v", set)
	}
	it, err := s.Lookup(ctx, "0"+sugar)
	if err != nil || it.Name != "Sugar" || it.Price != 5500 {
		t.Fatalf("existing product lost: %+v %v", it, err)
	}
	// Checkout tables work after migration.
	enableCheckout(t, s, 180, CheckoutModeTick)
	cc, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: sugar, Qty: 2}}, "")
	if err != nil || len(cc.Code) != 6 || cc.Total != 11000 {
		t.Fatalf("checkout on migrated store: %+v %v", cc, err)
	}
}

func TestCheckoutCodeAlphabetAndRandomness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := generateCheckoutCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != 6 {
			t.Fatalf("bad length: %q", c)
		}
		if strings.ContainsAny(c, "0O1IL") {
			t.Fatalf("ambiguous char in %q", c)
		}
		if seen[c] {
			t.Fatalf("unexpected collision at %d: %q", i, c)
		}
		seen[c] = true
		norm, ok := NormalizeCheckoutCode(" " + strings.ToLower(c[:3]) + "-" + c[3:] + " ")
		if !ok || norm != c {
			t.Fatalf("NormalizeCheckoutCode failed for %q -> %q (%v)", c, norm, ok)
		}
	}
	for _, bad := range []string{"", "ABCDE", "ABCDEFG", "K7M9P0", "K7M9PO", "K7M9P1", "K7M9PI", "K7M9PL", "K7M9P!"} {
		if _, ok := NormalizeCheckoutCode(bad); ok {
			t.Errorf("accepted invalid code %q", bad)
		}
	}
}

func TestCheckoutLifecycleReusePriceChangeAndVerify(t *testing.T) {
	s := newStore(t)
	a := put(t, s, code("600123456789"), "Sugar", 5500)
	b := put(t, s, code("600123456796"), "Rice", 4000)
	unk := code("590123412345")
	now := time.Now().Unix()
	if _, err := s.CreatePromo(ctx, "t", PromoInput{
		Title: "Deal", StartsAt: now - 60, EndsAt: now + 3600, Enabled: true,
		Items: []PromoItemInput{{Code: a, Price: 4900}},
	}); err != nil {
		t.Fatal(err)
	}

	// Disabled by default.
	if _, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: a, Qty: 1}}, ""); !errors.Is(err, ErrCheckoutDisabled) {
		t.Fatalf("want ErrCheckoutDisabled, got %v", err)
	}
	enableCheckout(t, s, 180, CheckoutModeRescan)

	// Create snapshot with promo item, normal item, duplicate merge, and unknown item.
	cc, err := s.CreateCheckout(ctx, []CheckoutItemInput{
		{Code: b, Qty: 1},
		{Code: a, Qty: 1},
		{Code: a, Qty: 1}, // merges with previous -> qty 2 @ 4900
		{Code: unk, Qty: 1},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if cc.Reused || cc.Total != 2*4900+4000 || cc.UnknownCount != 1 || len(cc.Items) != 3 {
		t.Fatalf("unexpected checkout: %+v", cc)
	}

	// Re-tapping Checkout on an unchanged cart (different input order) with prevCode returns the same code.
	same, err := s.CreateCheckout(ctx, []CheckoutItemInput{
		{Code: unk, Qty: 1},
		{Code: a, Qty: 2},
		{Code: b, Qty: 1},
	}, cc.Code)
	if err != nil || !same.Reused || same.Code != cc.Code {
		t.Fatalf("expected reuse of %s, got %+v %v", cc.Code, same, err)
	}

	// Another shopper with the same items (no prevCode) gets a distinct code.
	other, err := s.CreateCheckout(ctx, []CheckoutItemInput{
		{Code: a, Qty: 2},
		{Code: b, Qty: 1},
		{Code: unk, Qty: 1},
	}, "")
	if err != nil || other.Reused || other.Code == cc.Code {
		t.Fatalf("expected distinct code for another shopper, got %+v %v", other, err)
	}

	// Changing the cart creates a new code.
	changed, err := s.CreateCheckout(ctx, []CheckoutItemInput{
		{Code: a, Qty: 3},
		{Code: b, Qty: 1},
	}, cc.Code)
	if err != nil || changed.Reused || changed.Code == cc.Code {
		t.Fatalf("changed cart must create new code: %+v %v", changed, err)
	}

	// Price changes on server after code creation: cashier sees BOTH snapshot and current prices.
	if _, err := s.SetPrice(ctx, "owner@example.com", b, 4600); err != nil {
		t.Fatal(err)
	}
	view, err := s.GetCheckoutForCashier(ctx, cc.Code)
	if err != nil {
		t.Fatal(err)
	}
	if view.DefaultMode != CheckoutModeRescan || view.SnapshotTotal != 2*4900+4000 || view.CurrentTotal != 2*4900+4600 || view.PriceChangedCount != 1 || view.UnknownCount != 1 {
		t.Fatalf("cashier view = %+v", view)
	}
	var foundB bool
	for _, it := range view.Items {
		if it.Code == b {
			foundB = true
			if !it.PriceChanged || *it.SnapshotPrice != 4000 || *it.CurrentPrice != 4600 {
				t.Fatalf("rice item diff wrong: %+v", it)
			}
		}
	}
	if !foundB {
		t.Fatal("rice missing from cashier view")
	}

	// Verify checkout: marks verified, prevents reuse, writes audit trail.
	ver, err := s.VerifyCheckout(ctx, "clerk@example.com", cc.Code, VerifyCheckoutInput{Mode: CheckoutModeTick})
	if err != nil || !ver.OK || ver.VerifiedBy != "clerk@example.com" || ver.VerifyMode != CheckoutModeTick {
		t.Fatalf("verify = %+v %v", ver, err)
	}
	if _, err := s.GetCheckoutForCashier(ctx, cc.Code); !errors.Is(err, ErrAlreadyVerified) {
		t.Fatalf("re-lookup verified want ErrAlreadyVerified, got %v", err)
	}
	if _, err := s.VerifyCheckout(ctx, "clerk@example.com", cc.Code, VerifyCheckoutInput{Mode: CheckoutModeTick}); !errors.Is(err, ErrAlreadyVerified) {
		t.Fatalf("re-verify want ErrAlreadyVerified, got %v", err)
	}
	hist, err := s.ListHistory(ctx, "checkout", cc.Code, 0, 10)
	if err != nil || len(hist) != 1 || hist[0].Actor != "clerk@example.com" || !strings.Contains(hist[0].Summary, cc.Code) {
		t.Fatalf("checkout audit history = %+v %v", hist, err)
	}
}

func TestCheckoutExpiryAndCleanup(t *testing.T) {
	s := newStore(t)
	a := put(t, s, code("600123456789"), "Sugar", 5500)
	enableCheckout(t, s, 60, CheckoutModeTick)

	openExp, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: a, Qty: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	verifiedOld, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: a, Qty: 2}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyCheckout(ctx, "clerk@example.com", verifiedOld.Code, VerifyCheckoutInput{Mode: CheckoutModeTick}); err != nil {
		t.Fatal(err)
	}

	// Force both codes' expires_at into the past.
	past := time.Now().Unix() - 10
	if _, err := s.w.Exec(`UPDATE checkouts SET expires_at=? WHERE code IN (?,?)`, past, openExp.Code, verifiedOld.Code); err != nil {
		t.Fatal(err)
	}

	// Expired open checkout looks identical to a nonexistent code (ErrNotFound).
	if _, err := s.GetCheckoutForCashier(ctx, openExp.Code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired open lookup: want ErrNotFound, got %v", err)
	}
	if _, err := s.GetCheckoutForCashier(ctx, "ZZZZZZ"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lookup: want ErrNotFound, got %v", err)
	}
	if _, err := s.VerifyCheckout(ctx, "clerk@example.com", openExp.Code, VerifyCheckoutInput{Mode: CheckoutModeTick}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired verify: want ErrNotFound, got %v", err)
	}

	// Cleanup deletes expired open checkout, retains verified order.
	n, err := s.CleanupExpiredCheckouts(ctx)
	if err != nil || n != 1 {
		t.Fatalf("cleanup = %d, %v", n, err)
	}
	var st string
	if err := s.r.QueryRow(`SELECT status FROM checkouts WHERE code=?`, openExp.Code).Scan(&st); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired open checkout still in DB: %q %v", st, err)
	}
	if err := s.r.QueryRow(`SELECT status FROM checkouts WHERE code=?`, verifiedOld.Code).Scan(&st); err != nil || st != "verified" {
		t.Fatalf("verified checkout should be retained: %q %v", st, err)
	}
}
