package store

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func setAudit(t testing.TB, s *Store, enabled bool, rate int) {
	t.Helper()
	set := s.Settings()
	set.CheckoutEnabled = true
	set.AuditEnabled = enabled
	set.AuditRate = rate
	if err := s.UpdateSettings(ctx, "owner@example.com", set); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaMigrationV2ToV3(t *testing.T) {
	dir := t.TempDir()
	path := DBPath(dir, "v2shop")
	w, r, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(schemaSQL + schemaV2SQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('schema_version','2'),('v','1'),('home_rev','0'),('purged_before','0'),('settings','{"name":"V2 Shop","currency":"UGX","decimals":0,"tz_offset":180,"checkout_enabled":true,"checkout_ttl_min":180,"checkout_default_mode":"tick"}'),('created_at','1700000000')`); err != nil {
		t.Fatal(err)
	}
	sugar := code("600123456789")
	if _, err := tx.Exec(`INSERT INTO products(code,name,size,category,price,deleted,rev,updated_at) VALUES(?,?,?,?,?,0,1,?)`, "0"+sugar, "Sugar", "1 kg", "Pantry", 5500, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO checkouts(code,created_at,expires_at,status) VALUES('K7M9P2',?,?,'open')`, now, now+3600); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO checkout_items(code,seq,barcode,qty,unit_price) VALUES('K7M9P2',0,?,2,5500)`, "0"+sugar); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	r.Close()

	s, err := Open(dir, "v2shop")
	if err != nil {
		t.Fatalf("open v2 store: %v", err)
	}
	defer s.Close()

	var sv string
	if err := s.r.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&sv); err != nil || sv != "3" {
		t.Fatalf("schema_version = %q, err=%v", sv, err)
	}
	co, err := s.GetCheckoutForCashier(ctx, "K7M9P2")
	if err != nil || co.AuditRecheck || co.CurrentTotal != 11000 {
		t.Fatalf("migrated checkout = %+v, err=%v", co, err)
	}
	// Can verify with a mismatch on migrated v2 -> v3 store.
	if _, err := s.VerifyCheckout(ctx, "clerk@example.com", "K7M9P2", VerifyCheckoutInput{
		Mode: CheckoutModeTick,
		Mismatch: &CheckoutMismatchInput{
			ExtraItems: []MismatchItemInput{{Description: "Unscanned pen", Qty: 1}},
			Note:       "found in basket",
		},
	}); err != nil {
		t.Fatalf("verify with mismatch after v2->v3: %v", err)
	}
}

func TestAuditRecheckDecisionStabilityAndHiddenFromShopper(t *testing.T) {
	s := newStore(t)
	a := put(t, s, code("600123456789"), "Sugar", 5500)

	// Audit disabled -> 0% flagged even if audit_rate is 100.
	setAudit(t, s, false, 100)
	for i := 0; i < 10; i++ {
		cc, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: a, Qty: 1}}, "")
		if err != nil {
			t.Fatal(err)
		}
		co, err := s.GetCheckoutForCashier(ctx, cc.Code)
		if err != nil || co.AuditRecheck {
			t.Fatalf("disabled audit flagged %s: %+v %v", cc.Code, co, err)
		}
	}

	// Audit enabled at 100% -> always flagged, never exposed in shopper JSON, never re-rolled.
	setAudit(t, s, true, 100)
	cc, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: a, Qty: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	rawJSON, _ := json.Marshal(cc)
	if strings.Contains(string(rawJSON), "audit") || strings.Contains(string(rawJSON), "recheck") {
		t.Fatalf("shopper CheckoutCreated must not expose audit flag: %s", rawJSON)
	}
	// Multiple lookups cannot re-roll the decision (even if audit is subsequently turned off).
	setAudit(t, s, false, 0)
	for i := 0; i < 5; i++ {
		co, err := s.GetCheckoutForCashier(ctx, cc.Code)
		if err != nil || !co.AuditRecheck {
			t.Fatalf("lookup %d changed stored audit_recheck: %+v %v", i, co, err)
		}
	}
	// Re-tapping checkout with prevCode keeps the same code and stored flag.
	reused, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: a, Qty: 1}}, cc.Code)
	if err != nil || !reused.Reused || reused.Code != cc.Code {
		t.Fatalf("expected reuse: %+v %v", reused, err)
	}
	co, _ := s.GetCheckoutForCashier(ctx, reused.Code)
	if !co.AuditRecheck {
		t.Fatal("reusing unchanged cart lost audit_recheck flag")
	}

	// Audit enabled at 50% -> probabilistic mix of flagged and unflagged over 80 checkouts.
	setAudit(t, s, true, 50)
	flagged := 0
	for i := 0; i < 80; i++ {
		c, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: a, Qty: 1}}, "")
		if err != nil {
			t.Fatal(err)
		}
		v, err := s.GetCheckoutForCashier(ctx, c.Code)
		if err != nil {
			t.Fatal(err)
		}
		if v.AuditRecheck {
			flagged++
		}
	}
	if flagged == 0 || flagged == 80 {
		t.Fatalf("expected mix at 50%% rate over 80 trials, got %d flagged", flagged)
	}
}

func TestAuditRecheckRequiresRescanAndLogsMismatch(t *testing.T) {
	s := newStore(t)
	sugar := put(t, s, code("600123456789"), "Sugar", 5500)
	rice := put(t, s, code("600123456796"), "Rice", 4000)
	setAudit(t, s, true, 100)

	cc, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: sugar, Qty: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}

	// Must reject verify without RescanConfirmed=true.
	_, err = s.VerifyCheckout(ctx, "clerk@example.com", cc.Code, VerifyCheckoutInput{Mode: CheckoutModeTick})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "rescan_required" {
		t.Fatalf("expected rescan_required validation error, got %v", err)
	}

	// With RescanConfirmed=true, forces mode="rescan" even if "tick" was sent, and saves mismatch + history.
	ver, err := s.VerifyCheckout(ctx, "clerk@example.com", cc.Code, VerifyCheckoutInput{
		Mode:            CheckoutModeTick,
		RescanConfirmed: true,
		Mismatch: &CheckoutMismatchInput{
			ExtraItems:   []MismatchItemInput{{Barcode: rice, Qty: 2}, {Description: "Loose bread", Qty: 1}},
			MissingItems: []MismatchItemInput{{Barcode: sugar, Qty: 1}},
			Note:         "2 bags of rice not scanned",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ver.AuditRecheck || ver.VerifyMode != CheckoutModeRescan || !ver.MismatchLogged {
		t.Fatalf("unexpected verify result: %+v", ver)
	}

	hist, err := s.ListHistory(ctx, "checkout", cc.Code, 0, 5)
	if err != nil || len(hist) != 1 {
		t.Fatalf("history = %+v %v", hist, err)
	}
	if !strings.Contains(hist[0].Summary, "spot-check") || !strings.Contains(hist[0].Summary, "MISMATCH: 3 extra, 1 missing") {
		t.Fatalf("unexpected audit trail summary: %q", hist[0].Summary)
	}
}

func TestAuditReportMathTimezoneAndCSVFormulaGuard(t *testing.T) {
	s := newStore(t)
	sugar := put(t, s, code("600123456789"), "Sugar", 5500)
	rice := put(t, s, code("600123456796"), "Rice", 4000)

	// 1. Flagged checkout #1 with mismatch (2x Rice + 1x free-text formula payload).
	setAudit(t, s, true, 100)
	c1, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: sugar, Qty: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyCheckout(ctx, "clerk@example.com", c1.Code, VerifyCheckoutInput{
		RescanConfirmed: true,
		Mismatch: &CheckoutMismatchInput{
			ExtraItems: []MismatchItemInput{
				{Barcode: rice, Qty: 2},
				{Description: "=CMD('calc')", Qty: 1},
			},
			MissingItems: []MismatchItemInput{{Barcode: sugar, Qty: 1}},
			Note:         "+HYPERLINK(\"http://bad\")",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// 2. Flagged checkout #2 with NO mismatch.
	c2, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: sugar, Qty: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyCheckout(ctx, "clerk@example.com", c2.Code, VerifyCheckoutInput{RescanConfirmed: true}); err != nil {
		t.Fatal(err)
	}

	// 3. Unflagged checkout #3 with NO mismatch.
	setAudit(t, s, false, 0)
	c3, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: sugar, Qty: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyCheckout(ctx, "clerk@example.com", c3.Code, VerifyCheckoutInput{Mode: CheckoutModeTick}); err != nil {
		t.Fatal(err)
	}

	// 4. Unflagged checkout #4 WITH a cashier-spotted mismatch (1x Sugar).
	c4, err := s.CreateCheckout(ctx, []CheckoutItemInput{{Code: rice, Qty: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyCheckout(ctx, "clerk@example.com", c4.Code, VerifyCheckoutInput{
		Mode: CheckoutModeTick,
		Mismatch: &CheckoutMismatchInput{
			ExtraItems: []MismatchItemInput{{Barcode: sugar, Qty: 1}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Change Rice price AFTER verification to verify AuditReport uses CURRENT prices!
	if _, err := s.SetPrice(ctx, "owner@example.com", rice, 4500); err != nil {
		t.Fatal(err)
	}

	fromL, toL, fromU, toU, err := ParseAuditDateRange("", "", s.Settings().TZOffset, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.AuditReport(ctx, fromL, toL, fromU, toU, 500)
	if err != nil {
		t.Fatal(err)
	}
	// 4 total verified checkouts, 2 rechecked, 2 total mismatches, 1 rechecked mismatch -> 50.0% among rechecked.
	// Estimated missed value = (2 * 4500 current Rice) + (1 * 5500 current Sugar) = 14,500 UGX; 1 unpriced extra item.
	if rep.TotalCheckouts != 4 || rep.RecheckedCheckouts != 2 || rep.MismatchCheckouts != 2 || rep.RecheckedMismatchCheckouts != 1 {
		t.Fatalf("unexpected counts: %+v", rep)
	}
	if rep.MismatchRatePct != 50.0 {
		t.Fatalf("mismatch_rate_pct = %v, want 50.0", rep.MismatchRatePct)
	}
	if rep.EstimatedMissedValue != 14500 || rep.UnpricedExtraCount != 1 {
		t.Fatalf("missed value = %d (unpriced %d), want 14500 (unpriced 1)", rep.EstimatedMissedValue, rep.UnpricedExtraCount)
	}
	if len(rep.Mismatches) != 2 {
		t.Fatalf("len(mismatches) = %d, want 2", len(rep.Mismatches))
	}

	// Date range outside today returns 0 checkouts.
	oldFromL, oldToL, oldFromU, oldToU, err := ParseAuditDateRange("2020-01-01", "2020-01-31", 180, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	emptyRep, err := s.AuditReport(ctx, oldFromL, oldToL, oldFromU, oldToU, 500)
	if err != nil || emptyRep.TotalCheckouts != 0 || emptyRep.EstimatedMissedValue != 0 || emptyRep.MismatchRatePct != 0 {
		t.Fatalf("old range should be empty: %+v %v", emptyRep, err)
	}

	// CSV export formula-injection guard check.
	var buf bytes.Buffer
	if err := s.ExportAuditReportCSV(ctx, fromL, toL, fromU, toU, &buf); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parse audit csv: %v", err)
	}
	if len(recs) != 5 { // header + 3 items on c1 + 1 item on c4
		t.Fatalf("expected 5 CSV rows, got %d:\n%s", len(recs), buf.String())
	}
	foundGuardedFormula := false
	for _, row := range recs[1:] {
		if strings.HasPrefix(row[7], "'=CMD") && strings.HasPrefix(row[11], "'+HYPERLINK") {
			foundGuardedFormula = true
		}
	}
	if !foundGuardedFormula {
		t.Fatalf("CSV formula guard missing:\n%s", buf.String())
	}
}
