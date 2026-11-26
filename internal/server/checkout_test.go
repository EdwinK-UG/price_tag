package server

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"pricetag/internal/store"
)

func backdateCheckout(t *testing.T, dbPath, code string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE checkouts SET expires_at=? WHERE code=?`, time.Now().Unix()-10, code); err != nil {
		t.Fatal(err)
	}
}

func enableShopCheckout(t *testing.T, adm *client, mode string) {
	t.Helper()
	res, body := adm.put("/api/v1/s/shop/admin/settings", map[string]any{
		"name": "Test Mart", "currency": "UGX", "decimals": 0, "tz_offset": 180,
		"checkout_enabled": true, "checkout_ttl_min": 180, "checkout_default_mode": mode,
	})
	if res.StatusCode != 200 {
		t.Fatalf("enable checkout: %d %s", res.StatusCode, body)
	}
}

func TestCheckoutAPILifecyclePermissionsAndAudit(t *testing.T) {
	e := newEnv(t)
	a, b := seed(t, e)
	pub := e.client()
	owner := e.adminClient("owner@example.com", "owner-pass-123")

	// Create an editor (cashier) account.
	res, body := owner.post("/api/v1/s/shop/admin/users", map[string]string{
		"email": "cashier@example.com", "name": "Cashier", "role": "editor", "password": "cashier-pass-123",
	})
	if res.StatusCode != 201 {
		t.Fatalf("create editor: %d %s", res.StatusCode, body)
	}
	cashier := e.adminClient("cashier@example.com", "cashier-pass-123")

	// Off by default: /home omits checkout_enabled, POST /checkouts returns 403.
	res, home0 := pub.get("/api/v1/s/shop/home")
	if res.StatusCode != 200 || bytes.Contains(home0, []byte("checkout_enabled")) {
		t.Fatalf("home should omit checkout_enabled when off: %d %s", res.StatusCode, home0)
	}
	res, _ = pub.do("POST", "/api/v1/s/shop/checkouts", map[string]any{
		"items": []map[string]any{{"code": a, "qty": 1}},
	}, nil)
	if res.StatusCode != 403 {
		t.Fatalf("checkout when disabled: want 403, got %d", res.StatusCode)
	}

	// Editor cannot change settings (owner-only).
	res, _ = cashier.put("/api/v1/s/shop/admin/settings", map[string]any{
		"name": "Test Mart", "currency": "UGX", "decimals": 0, "tz_offset": 180, "checkout_enabled": true,
	})
	if res.StatusCode != 403 {
		t.Fatalf("editor put settings: want 403, got %d", res.StatusCode)
	}

	// Owner enables checkout.
	enableShopCheckout(t, owner, "rescan")
	res, home1 := pub.get("/api/v1/s/shop/home")
	if res.StatusCode != 200 || !bytes.Contains(home1, []byte(`"checkout_enabled":true`)) {
		t.Fatalf("home should include checkout_enabled:true: %d %s", res.StatusCode, home1)
	}

	// Shopper creates checkout code.
	res, body = pub.do("POST", "/api/v1/s/shop/checkouts", map[string]any{
		"items": []map[string]any{{"code": a, "qty": 2}, {"code": b, "qty": 1}},
	}, nil)
	if res.StatusCode != 201 {
		t.Fatalf("create checkout: %d %s", res.StatusCode, body)
	}
	var cc store.CheckoutCreated
	decode(t, body, &cc)
	if len(cc.Code) != 6 || cc.Reused || cc.Total != 2*5500+4000 {
		t.Fatalf("bad checkout created: %+v", cc)
	}

	// Re-tapping Checkout on unchanged cart with prev_code returns the exact same code (200).
	res, body = pub.do("POST", "/api/v1/s/shop/checkouts", map[string]any{
		"items": []map[string]any{{"code": b, "qty": 1}, {"code": a, "qty": 2}}, "prev_code": cc.Code,
	}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("re-tap checkout: want 200, got %d %s", res.StatusCode, body)
	}
	var cc2 store.CheckoutCreated
	decode(t, body, &cc2)
	if !cc2.Reused || cc2.Code != cc.Code {
		t.Fatalf("expected reused %s, got %+v", cc.Code, cc2)
	}

	// Unauthenticated cashier lookup fails with 401.
	if res, _ := pub.get("/api/v1/s/shop/admin/checkouts/" + cc.Code); res.StatusCode != 401 {
		t.Fatalf("unauth lookup: want 401, got %d", res.StatusCode)
	}

	// Price changes before cashier scans code -> cashier sees both prices.
	res, _ = owner.put("/api/v1/s/shop/admin/products/"+a+"/price", map[string]int64{"price": 6200})
	if res.StatusCode != 200 {
		t.Fatalf("price edit: %d", res.StatusCode)
	}

	// Editor (cashier) looks up the code (lowercase + hyphen accepted).
	res, body = cashier.get("/api/v1/s/shop/admin/checkouts/" + strings.ToLower(cc.Code[:3]) + "-" + cc.Code[3:])
	if res.StatusCode != 200 {
		t.Fatalf("cashier lookup: %d %s", res.StatusCode, body)
	}
	var view store.CashierCheckout
	decode(t, body, &view)
	if view.DefaultMode != "rescan" || view.SnapshotTotal != 15000 || view.CurrentTotal != 16400 || view.PriceChangedCount != 1 {
		t.Fatalf("unexpected cashier view: %+v", view)
	}

	// Verify without CSRF header is rejected (403).
	res, _ = cashier.do("POST", "/api/v1/s/shop/admin/checkouts/"+cc.Code+"/verify", map[string]string{"mode": "rescan"}, nil)
	if res.StatusCode != 403 {
		t.Fatalf("verify without CSRF: want 403, got %d", res.StatusCode)
	}

	// Cashier verifies the order with CSRF.
	res, body = cashier.post("/api/v1/s/shop/admin/checkouts/"+cc.Code+"/verify", map[string]string{"mode": "rescan"})
	if res.StatusCode != 200 {
		t.Fatalf("verify: %d %s", res.StatusCode, body)
	}
	var ver store.CheckoutVerified
	decode(t, body, &ver)
	if !ver.OK || ver.VerifiedBy != "cashier@example.com" || ver.VerifyMode != "rescan" {
		t.Fatalf("unexpected verify response: %+v", ver)
	}

	// Code cannot be reused on lookup or verify (409).
	if res, _ := cashier.get("/api/v1/s/shop/admin/checkouts/" + cc.Code); res.StatusCode != 409 {
		t.Fatalf("re-lookup verified: want 409, got %d", res.StatusCode)
	}
	if res, _ := cashier.post("/api/v1/s/shop/admin/checkouts/"+cc.Code+"/verify", map[string]string{"mode": "tick"}); res.StatusCode != 409 {
		t.Fatalf("re-verify: want 409, got %d", res.StatusCode)
	}

	// Audit trail records cashier and code.
	res, body = owner.get("/api/v1/s/shop/admin/history?kind=checkout&ref=" + cc.Code)
	if res.StatusCode != 200 || !bytes.Contains(body, []byte("cashier@example.com")) || !bytes.Contains(body, []byte(cc.Code)) {
		t.Fatalf("audit history missing checkout: %d %s", res.StatusCode, body)
	}
}

func TestCheckoutFailedAndExpiredLookIdentical(t *testing.T) {
	e := newEnv(t)
	a, _ := seed(t, e)
	owner := e.adminClient("owner@example.com", "owner-pass-123")
	enableShopCheckout(t, owner, "tick")

	pub := e.client()
	res, body := pub.do("POST", "/api/v1/s/shop/checkouts", map[string]any{
		"items": []map[string]any{{"code": a, "qty": 1}},
	}, nil)
	if res.StatusCode != 201 {
		t.Fatalf("create: %d %s", res.StatusCode, body)
	}
	var cc store.CheckoutCreated
	decode(t, body, &cc)

	// Expire the checkout in SQLite directly.
	st, rel, err := e.srv.reg.Acquire("shop")
	if err != nil {
		t.Fatal(err)
	}
	// Backdate via Cleanup check / direct open handle not exported, so reopen a second store handle or use SQL via DBPath:
	rel()
	_ = st
	// Open a fresh writable SQLite handle to backdate expires_at for this test.
	backdateCheckout(t, store.DBPath(e.dir, "shop"), cc.Code)

	rBad, bBad := owner.get("/api/v1/s/shop/admin/checkouts/000000")   // invalid alphabet
	rMiss, bMiss := owner.get("/api/v1/s/shop/admin/checkouts/ZZZZZZ") // valid syntax, nonexistent
	rExp, bExp := owner.get("/api/v1/s/shop/admin/checkouts/" + cc.Code)
	if rBad.StatusCode != 404 || rMiss.StatusCode != 404 || rExp.StatusCode != 404 {
		t.Fatalf("want 404 for all, got bad=%d miss=%d exp=%d", rBad.StatusCode, rMiss.StatusCode, rExp.StatusCode)
	}
	if !bytes.Equal(bBad, bMiss) || !bytes.Equal(bMiss, bExp) {
		t.Fatalf("failed and expired responses must match:\n bad=%s\n miss=%s\n exp=%s", bBad, bMiss, bExp)
	}
}

func TestCheckoutLookupRateLimitsPerIPAndPerStore(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.TrustProxy = true })
	a, _ := seed(t, e)
	owner := e.adminClient("owner@example.com", "owner-pass-123")
	enableShopCheckout(t, owner, "tick")

	// Create one valid checkout code.
	pub := e.client()
	res, body := pub.do("POST", "/api/v1/s/shop/checkouts", map[string]any{
		"items": []map[string]any{{"code": a, "qty": 1}},
	}, nil)
	if res.StatusCode != 201 {
		t.Fatalf("create: %d %s", res.StatusCode, body)
	}
	var cc store.CheckoutCreated
	decode(t, body, &cc)

	// 25 valid lookups from one IP must ALL succeed (tokens refunded on 200).
	for i := 0; i < 25; i++ {
		r, _ := owner.do("GET", "/api/v1/s/shop/admin/checkouts/"+cc.Code, nil, map[string]string{"X-Forwarded-For": "10.0.0.1"})
		if r.StatusCode != 200 {
			t.Fatalf("valid lookup %d throttled: %d", i, r.StatusCode)
		}
	}

	// Failed lookups from one IP hit per-IP limit (burst 20 -> 21st gets 429).
	got429 := false
	for i := 0; i < 25; i++ {
		r, _ := owner.do("GET", "/api/v1/s/shop/admin/checkouts/ZZZZZZ", nil, map[string]string{"X-Forwarded-For": "10.0.0.2"})
		if r.StatusCode == 429 {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("expected per-IP 429 on failed checkout lookups")
	}

	// Failed lookups spread across distinct IPs hit the per-shop limit (burst 40).
	gotStore429 := false
	for i := 0; i < 30; i++ {
		ip := fmt.Sprintf("10.0.1.%d", i+1)
		r, _ := owner.do("GET", "/api/v1/s/shop/admin/checkouts/YYYYYY", nil, map[string]string{"X-Forwarded-For": ip})
		if r.StatusCode == 429 {
			gotStore429 = true
			break
		}
	}
	if !gotStore429 {
		t.Fatal("expected per-store 429 across distinct IPs")
	}
	_ = context.Background()
}
