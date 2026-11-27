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

	// A used code cannot be reused, and answers exactly like an unknown code.
	_, missBody := cashier.get("/api/v1/s/shop/admin/checkouts/ZZZZZZ")
	if res, b := cashier.get("/api/v1/s/shop/admin/checkouts/" + cc.Code); res.StatusCode != 404 || !bytes.Equal(b, missBody) {
		t.Fatalf("re-lookup verified: want the same 404 as an unknown code, got %d %s", res.StatusCode, b)
	}
	if res, b := cashier.post("/api/v1/s/shop/admin/checkouts/"+cc.Code+"/verify", map[string]string{"mode": "tick"}); res.StatusCode != 404 || !bytes.Equal(b, missBody) {
		t.Fatalf("re-verify: want the same 404 as an unknown code, got %d %s", res.StatusCode, b)
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

// One client must not be able to fill the shop-wide open-code cap and block
// checkout for every other shopper.
func TestCheckoutCodesPerClientAreCapped(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.CheckoutCodesPerIP = 3 })
	a, b := seed(t, e)
	owner := e.adminClient("owner@example.com", "owner-pass-123")
	enableShopCheckout(t, owner, "tick")
	pub := e.client()
	create := func(qty int, prev string) (int, store.CheckoutCreated) {
		res, body := pub.do("POST", "/api/v1/s/shop/checkouts", map[string]any{"items": []map[string]any{{"code": a, "qty": qty}}, "prev_code": prev}, nil)
		var cc store.CheckoutCreated
		if res.StatusCode < 300 {
			decode(t, body, &cc)
		}
		return res.StatusCode, cc
	}
	var last store.CheckoutCreated
	for q := 1; q <= 3; q++ {
		st, cc := create(q, "")
		if st != 201 {
			t.Fatalf("code %d: status %d", q, st)
		}
		last = cc
	}
	// Re-tapping Checkout on an unchanged cart reuses the code and costs nothing.
	if st, cc := create(3, last.Code); st != 200 || !cc.Reused || cc.Code != last.Code {
		t.Fatalf("reuse: %d %+v", st, cc)
	}
	if st, _ := create(9, ""); st != 429 {
		t.Fatalf("4th new code from one client: want 429, got %d", st)
	}
	// Invalid carts (rejected before a code exists) don't eat the allowance either.
	e2 := newEnv(t, func(c *Config) { c.CheckoutCodesPerIP = 1 })
	a2, _ := seed(t, e2)
	enableShopCheckout(t, e2.adminClient("owner@example.com", "owner-pass-123"), "tick")
	p2 := e2.client()
	if res, _ := p2.do("POST", "/api/v1/s/shop/checkouts", map[string]any{"items": []map[string]any{}}, nil); res.StatusCode != 400 {
		t.Fatalf("empty cart: %d", res.StatusCode)
	}
	if res, body := p2.do("POST", "/api/v1/s/shop/checkouts", map[string]any{"items": []map[string]any{{"code": a2, "qty": 1}}}, nil); res.StatusCode != 201 {
		t.Fatalf("first real code after a rejected cart: %d %s", res.StatusCode, body)
	}
	_ = b
}

func TestCatalogueDownloadsFromOneSharedIP(t *testing.T) {
	e := newEnv(t)
	seed(t, e)
	c := e.client()
	for i := 0; i < 15; i++ { // e.g. fifteen shoppers on the shop's Wi-Fi saving prices for offline use
		if res, _ := c.do("GET", "/api/v1/s/shop/catalog", nil, map[string]string{"Accept-Encoding": "gzip"}); res.StatusCode != 200 {
			t.Fatalf("download %d from one IP: %d", i+1, res.StatusCode)
		}
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

func TestAuditModeRecheckMismatchAndOwnerReportAPI(t *testing.T) {
	e := newEnv(t)
	a, b := seed(t, e) // a=5500, b=4000
	pub := e.client()
	owner := e.adminClient("owner@example.com", "owner-pass-123")

	res, body := owner.post("/api/v1/s/shop/admin/users", map[string]string{
		"email": "cashier@example.com", "name": "Cashier", "role": "editor", "password": "cashier-pass-123",
	})
	if res.StatusCode != 201 {
		t.Fatalf("create editor: %d %s", res.StatusCode, body)
	}
	cashier := e.adminClient("cashier@example.com", "cashier-pass-123")

	// Reject out-of-range audit_rate.
	res, _ = owner.put("/api/v1/s/shop/admin/settings", map[string]any{
		"name": "Test Mart", "currency": "UGX", "decimals": 0, "tz_offset": 180,
		"checkout_enabled": true, "checkout_ttl_min": 180, "checkout_default_mode": "tick",
		"audit_enabled": true, "audit_rate": 105,
	})
	if res.StatusCode != 400 {
		t.Fatalf("audit_rate=105 want 400, got %d", res.StatusCode)
	}

	// Enable checkout + 100% audit rate.
	res, body = owner.put("/api/v1/s/shop/admin/settings", map[string]any{
		"name": "Test Mart", "currency": "UGX", "decimals": 0, "tz_offset": 180,
		"checkout_enabled": true, "checkout_ttl_min": 180, "checkout_default_mode": "tick",
		"audit_enabled": true, "audit_rate": 100,
	})
	if res.StatusCode != 200 {
		t.Fatalf("enable audit: %d %s", res.StatusCode, body)
	}

	// Public /home must NOT reveal audit_enabled or audit_rate to shoppers.
	res, homeBody := pub.get("/api/v1/s/shop/home")
	if res.StatusCode != 200 || bytes.Contains(homeBody, []byte("audit")) {
		t.Fatalf("home leaked audit settings: %s", homeBody)
	}

	// Shopper creates checkout code -> response must NOT reveal audit_recheck.
	res, body = pub.do("POST", "/api/v1/s/shop/checkouts", map[string]any{
		"items": []map[string]any{{"code": a, "qty": 1}},
	}, nil)
	if res.StatusCode != 201 || bytes.Contains(body, []byte("audit")) || bytes.Contains(body, []byte("recheck")) {
		t.Fatalf("create checkout: %d %s", res.StatusCode, body)
	}
	var cc store.CheckoutCreated
	decode(t, body, &cc)

	// Cashier sees audit_recheck=true; repeated lookups return the same flag.
	res, body = cashier.get("/api/v1/s/shop/admin/checkouts/" + cc.Code)
	if res.StatusCode != 200 {
		t.Fatalf("lookup: %d %s", res.StatusCode, body)
	}
	var view store.CashierCheckout
	decode(t, body, &view)
	if !view.AuditRecheck {
		t.Fatalf("expected audit_recheck=true: %+v", view)
	}

	// Verifying flagged order without rescan_confirmed=true is rejected (400).
	res, body = cashier.post("/api/v1/s/shop/admin/checkouts/"+cc.Code+"/verify", map[string]any{"mode": "tick"})
	if res.StatusCode != 400 || !bytes.Contains(body, []byte("rescan_required")) {
		t.Fatalf("verify without rescan_confirmed: want 400 rescan_required, got %d %s", res.StatusCode, body)
	}

	// Verify with rescan_confirmed=true and a logged mismatch (1x extra product b + formula note).
	res, body = cashier.post("/api/v1/s/shop/admin/checkouts/"+cc.Code+"/verify", map[string]any{
		"mode":             "tick", // server must force "rescan"
		"rescan_confirmed": true,
		"mismatch": map[string]any{
			"extra_items": []map[string]any{{"barcode": b, "qty": 2}},
			"note":        "=1+1 unscanned rice",
		},
	})
	if res.StatusCode != 200 {
		t.Fatalf("verify with mismatch: %d %s", res.StatusCode, body)
	}
	var ver store.CheckoutVerified
	decode(t, body, &ver)
	if !ver.AuditRecheck || ver.VerifyMode != "rescan" || !ver.MismatchLogged {
		t.Fatalf("unexpected verify response: %+v", ver)
	}

	// Editor is forbidden (403) from Audit Report JSON and CSV.
	if r, _ := cashier.get("/api/v1/s/shop/admin/audit-report"); r.StatusCode != 403 {
		t.Fatalf("editor audit-report: want 403, got %d", r.StatusCode)
	}
	if r, _ := cashier.get("/api/v1/s/shop/admin/audit-report.csv"); r.StatusCode != 403 {
		t.Fatalf("editor audit-report.csv: want 403, got %d", r.StatusCode)
	}

	// Owner gets Audit Report JSON with exact totals (2 * 4000 = 8000 UGX, 100.0% mismatch rate).
	res, body = owner.get("/api/v1/s/shop/admin/audit-report")
	if res.StatusCode != 200 {
		t.Fatalf("owner audit-report: %d %s", res.StatusCode, body)
	}
	var rep store.AuditReportData
	decode(t, body, &rep)
	if rep.TotalCheckouts != 1 || rep.RecheckedCheckouts != 1 || rep.MismatchCheckouts != 1 || rep.MismatchRatePct != 100.0 || rep.EstimatedMissedValue != 8000 {
		t.Fatalf("unexpected audit report: %+v", rep)
	}

	// Invalid date range returns 400.
	if r, _ := owner.get("/api/v1/s/shop/admin/audit-report?from=2026-05-10&to=2026-05-01"); r.StatusCode != 400 {
		t.Fatalf("reversed range want 400, got %d", r.StatusCode)
	}

	// Owner downloads formula-guarded CSV.
	res, csvBody := owner.get("/api/v1/s/shop/admin/audit-report.csv")
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("owner audit-report.csv: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if !bytes.Contains(csvBody, []byte(cc.Code)) || !bytes.Contains(csvBody, []byte("'=1+1 unscanned rice")) {
		t.Fatalf("csv missing expected guarded content:\n%s", csvBody)
	}
}

func TestOutOfAllowanceClientCanStillReshowItsCode(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.CheckoutCodesPerIP = 1 })
	a, _ := seed(t, e)
	enableShopCheckout(t, e.adminClient("owner@example.com", "owner-pass-123"), "tick")
	pub := e.client()
	items := map[string]any{"items": []map[string]any{{"code": a, "qty": 2}}}
	res, body := pub.do("POST", "/api/v1/s/shop/checkouts", items, nil)
	if res.StatusCode != 201 {
		t.Fatalf("first: %d %s", res.StatusCode, body)
	}
	var cc store.CheckoutCreated
	decode(t, body, &cc)
	items["prev_code"] = cc.Code
	res, body = pub.do("POST", "/api/v1/s/shop/checkouts", items, nil)
	var again store.CheckoutCreated
	decode(t, body, &again)
	if res.StatusCode != 200 || !again.Reused || again.Code != cc.Code {
		t.Fatalf("re-show existing code while out of allowance: %d %s", res.StatusCode, body)
	}
	items["items"] = []map[string]any{{"code": a, "qty": 3}} // changed cart needs a NEW code
	if res, _ := pub.do("POST", "/api/v1/s/shop/checkouts", items, nil); res.StatusCode != 429 {
		t.Fatalf("new code while out of allowance: want 429, got %d", res.StatusCode)
	}
}
