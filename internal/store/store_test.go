package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pricetag/internal/barcode"
)

var ctx = context.Background()

func newStore(t testing.TB) *Store {
	t.Helper()
	s, err := Create(t.TempDir(), "shop", DefaultSettings("Test Mart"), "owner@example.com", "Olive Owner", "owner-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func code(body string) string { return barcode.WithCheckDigit(body) }

func put(t testing.TB, s *Store, c, name string, price int64) string {
	t.Helper()
	got, _, err := s.UpsertProduct(ctx, "t", ProductInput{Code: c, Name: name, Price: price})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCreateAndLookup(t *testing.T) {
	s := newStore(t)
	if s.Version() != 0 {
		t.Fatal("fresh store must be version 0")
	}
	c := put(t, s, code("600123456789"), "Sugar", 5500)
	if s.Version() != 1 {
		t.Fatalf("v=%d", s.Version())
	}
	it, err := s.Lookup(ctx, c)
	if err != nil || it.Name != "Sugar" || it.Price != 5500 || it.Rev != 1 {
		t.Fatalf("lookup: %+v %v", it, err)
	}
	if _, err := s.Lookup(ctx, "00000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if _, err := Create(filepath.Dir(s.path), "shop", DefaultSettings("x"), "a@b.co", "n", "password1"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: %v", err)
	}
}

func TestUnchangedWriteDoesNotBumpVersion(t *testing.T) {
	s := newStore(t)
	c := code("600123456789")
	put(t, s, c, "Sugar", 5500)
	v := s.Version()
	_, st, err := s.UpsertProduct(ctx, "t", ProductInput{Code: c, Name: "  Sugar ", Price: 5500})
	if err != nil || st != StatusUnchanged {
		t.Fatalf("st=%s err=%v", st, err)
	}
	if s.Version() != v {
		t.Fatal("version bumped for a no-op")
	}
}

func TestPriceChangePublishesEventAndHistory(t *testing.T) {
	s := newStore(t)
	c := put(t, s, code("600123456789"), "Sugar", 5500)
	sub := s.Hub.Subscribe(0)
	defer s.Hub.Unsubscribe(sub)
	if _, err := s.SetPrice(ctx, "olive", c, 6000); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-sub.C:
		if f.V != 2 {
			t.Fatalf("frame v=%d", f.V)
		}
		txt := string(f.Data)
		if !strings.HasPrefix(txt, "id: 2\nevent: change\ndata: ") || !strings.HasSuffix(txt, "\n\n") {
			t.Fatalf("bad frame %q", txt)
		}
		var ev Event
		payload := strings.TrimSuffix(strings.SplitN(txt, "data: ", 2)[1], "\n\n")
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Prev != 1 || ev.V != 2 || len(ev.Items) != 1 || ev.Items[0].Price != 6000 || ev.Items[0].Rev != 2 {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	h, err := s.ListHistory(ctx, "product", c, 0, 10)
	if err != nil || len(h) != 2 {
		t.Fatalf("history %v %v", h, err)
	}
	if h[0].Actor != "olive" || *h[0].OldPrice != 5500 || *h[0].NewPrice != 6000 {
		t.Fatalf("history[0] = %+v", h[0])
	}
	if _, err := s.SetPrice(ctx, "olive", code("600000000001"), 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown product: %v", err)
	}
}

func TestValidation(t *testing.T) {
	s := newStore(t)
	bad := []ProductInput{
		{Code: "4006381333932", Name: "x", Price: 1}, // bad check digit
		{Code: code("600123456789"), Name: "", Price: 1},
		{Code: code("600123456789"), Name: "x", Price: -5},
		{Code: code("600123456789"), Name: strings.Repeat("a", 201), Price: 1},
		{Code: "", Name: "x", Price: 1},
	}
	for i, in := range bad {
		_, _, err := s.UpsertProduct(ctx, "t", in)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("case %d: want validation error, got %v", i, err)
		}
	}
	if s.Version() != 0 {
		t.Fatal("failed writes must not bump version")
	}
}

func TestDeleteAndRestore(t *testing.T) {
	s := newStore(t)
	c := put(t, s, code("600123456789"), "Sugar", 5500)
	if err := s.DeleteProduct(ctx, "t", c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, c); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted product still visible")
	}
	if err := s.DeleteProduct(ctx, "t", c); !errors.Is(err, ErrNotFound) {
		t.Fatal("double delete")
	}
	if _, st, _ := s.UpsertProduct(ctx, "t", ProductInput{Code: c, Name: "Sugar", Price: 5600}); st != StatusRestored {
		t.Fatalf("status %s", st)
	}
	if it, err := s.Lookup(ctx, c); err != nil || it.Price != 5600 {
		t.Fatal("restore failed")
	}
}

func TestPromotionLifecycle(t *testing.T) {
	s := newStore(t)
	a := put(t, s, code("600123456789"), "Sugar 1kg", 5500)
	b := put(t, s, code("600123456796"), "Rice 1kg", 4000)
	now := time.Now().Unix()
	in := PromoInput{Title: "Weekend deal", StartsAt: now - 60, EndsAt: now + 3600, Enabled: true,
		Items: []PromoItemInput{{Code: a, Price: 4800}, {Code: b, Price: 3500}}}

	if _, err := s.CreatePromo(ctx, "t", PromoInput{Title: "bad", StartsAt: now, EndsAt: now, Enabled: true}); err == nil {
		t.Fatal("end must be after start")
	}
	bad := in
	bad.Items = []PromoItemInput{{Code: a, Price: 5500}}
	if _, err := s.CreatePromo(ctx, "t", bad); err == nil || !strings.Contains(err.Error(), "lower") {
		t.Fatalf("offer price not lower must fail: %v", err)
	}
	bad.Items = []PromoItemInput{{Code: code("600000000007"), Price: 1}}
	if _, err := s.CreatePromo(ctx, "t", bad); err == nil {
		t.Fatal("unknown product must fail")
	}

	sub := s.Hub.Subscribe(0)
	defer s.Hub.Unsubscribe(sub)
	id, err := s.CreatePromo(ctx, "t", in)
	if err != nil {
		t.Fatal(err)
	}
	f := <-sub.C
	var ev Event
	json.Unmarshal([]byte(strings.TrimSuffix(strings.SplitN(string(f.Data), "data: ", 2)[1], "\n\n")), &ev)
	if !ev.Home || len(ev.Items) != 2 || len(ev.Items[0].Promos) != 1 {
		t.Fatalf("promo event %+v", ev)
	}
	it, _ := s.Lookup(ctx, a)
	if len(it.Promos) != 1 || it.Promos[0].Price != 4800 || it.Promos[0].Title != "Weekend deal" {
		t.Fatalf("item promos %+v", it.Promos)
	}

	home, err := s.Home(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hd HomeData
	json.Unmarshal(home.JSON, &hd)
	if len(hd.Promos) != 1 || len(hd.Promos[0].Items) != 2 || hd.Store.Name != "Test Mart" {
		t.Fatalf("home %s", home.JSON)
	}
	// biggest discount first: sugar 12.7% vs rice 12.5%
	if hd.Promos[0].Items[0].Code != a {
		t.Fatalf("home ordering: %+v", hd.Promos[0].Items)
	}

	// Edit: drop rice. Rice must get an event so phones stop showing the offer.
	in.Items = in.Items[:1]
	in.Title = "Sugar only"
	if err := s.UpdatePromo(ctx, "t", id, in); err != nil {
		t.Fatal(err)
	}
	ev2 := <-sub.C
	if !strings.Contains(string(ev2.Data), `"code":"`+b+`"`) {
		t.Fatal("removed item not included in event")
	}
	if it, _ := s.Lookup(ctx, b); len(it.Promos) != 0 {
		t.Fatal("rice should have no promo now")
	}

	ps, _ := s.ListPromos(ctx)
	if len(ps) != 1 || ps[0].Status != "live" || ps[0].ItemCount != 1 {
		t.Fatalf("list %+v", ps)
	}
	if err := s.DeletePromo(ctx, "t", id); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.Lookup(ctx, a); len(it.Promos) != 0 {
		t.Fatal("promo still attached after delete")
	}
	if err := s.DeletePromo(ctx, "t", id); !errors.Is(err, ErrNotFound) {
		t.Fatal("double delete")
	}
}

func TestExpiredPromoNotServed(t *testing.T) {
	s := newStore(t)
	a := put(t, s, code("600123456789"), "Sugar", 5500)
	now := time.Now().Unix()
	if _, err := s.CreatePromo(ctx, "t", PromoInput{Title: "Old", StartsAt: now - 7200, EndsAt: now - 3600, Enabled: true, Items: []PromoItemInput{{Code: a, Price: 100}}}); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.Lookup(ctx, a); len(it.Promos) != 0 {
		t.Fatal("expired offer served")
	}
	hd, _ := s.Home(ctx)
	if strings.Contains(string(hd.JSON), `"Old"`) {
		t.Fatal("expired offer on home")
	}
	// Scheduled (future) offers are delivered so phones can switch over on time.
	if _, err := s.CreatePromo(ctx, "t", PromoInput{Title: "Soon", StartsAt: now + 600, EndsAt: now + 1200, Enabled: true, Items: []PromoItemInput{{Code: a, Price: 100}}}); err != nil {
		t.Fatal(err)
	}
	it, _ := s.Lookup(ctx, a)
	if len(it.Promos) != 1 || it.Promos[0].From != now+600 {
		t.Fatalf("scheduled offer missing: %+v", it.Promos)
	}
}

func TestCatchUp(t *testing.T) {
	s := newStore(t)
	a := put(t, s, code("600123456789"), "Sugar", 5500) // v1
	b := put(t, s, code("600123456796"), "Rice", 4000)  // v2
	if ev, _ := s.CatchUp(ctx, 2); ev != nil {
		t.Fatal("current client needs nothing")
	}
	s.SetPrice(ctx, "t", a, 5600) // v3
	s.DeleteProduct(ctx, "t", b)  // v4
	ev, err := s.CatchUp(ctx, 2)
	if err != nil || ev == nil || ev.Reset {
		t.Fatalf("%+v %v", ev, err)
	}
	if ev.Prev != 2 || ev.V != 4 || len(ev.Items) != 1 || ev.Items[0].Price != 5600 || len(ev.Gone) != 1 || ev.Gone[0] != b {
		t.Fatalf("catch-up %+v", ev)
	}
	if ev, _ := s.CatchUp(ctx, 999); ev == nil || !ev.Reset {
		t.Fatal("cursor from the future must reset")
	}
	if ev, _ := s.CatchUp(ctx, -1); ev == nil || !ev.Reset {
		t.Fatal("negative cursor must reset")
	}
}

func TestBulkImportAndResetEvent(t *testing.T) {
	s := newStore(t)
	var rows []ImportRow
	for i := 0; i < 200; i++ {
		rows = append(rows, ImportRow{Row: i + 2, In: ProductInput{Code: code(fmt.Sprintf("2000000%05d", i)), Name: fmt.Sprintf("Item %d", i), Price: int64(1000 + i)}})
	}
	sub := s.Hub.Subscribe(0)
	defer s.Hub.Unsubscribe(sub)
	res, err := s.ImportProducts(ctx, "feed", rows, false)
	if err != nil || res.Created != 200 || len(res.Errors) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if s.Version() != 1 {
		t.Fatalf("one import must be one version, got %d", s.Version())
	}
	f := <-sub.C
	if !strings.Contains(string(f.Data), `"reset":true`) || strings.Contains(string(f.Data), `"items"`) {
		t.Fatalf("large commit should send a reset hint only: %s", f.Data)
	}
	// Same file again: nothing changes, no version, no event.
	res, _ = s.ImportProducts(ctx, "feed", rows, false)
	if res.Unchanged != 200 || res.Created != 0 || s.Version() != 1 {
		t.Fatalf("idempotent import failed: %+v v=%d", res, s.Version())
	}
	select {
	case <-sub.C:
		t.Fatal("unexpected event for no-op import")
	default:
	}
	// Change 3 prices: small commit => inline items.
	for i := 0; i < 3; i++ {
		rows[i].In.Price += 50
	}
	res, _ = s.ImportProducts(ctx, "feed", rows, false)
	if res.Updated != 3 || res.Unchanged != 197 {
		t.Fatalf("%+v", res)
	}
	f = <-sub.C
	if strings.Contains(string(f.Data), `"reset"`) || strings.Count(string(f.Data), `"rev":2`) != 3 {
		t.Fatalf("small import should be inline: %s", f.Data)
	}
}

func TestImportDryRunDuplicatesAndErrors(t *testing.T) {
	s := newStore(t)
	c := code("600123456789")
	rows := []ImportRow{
		{Row: 2, In: ProductInput{Code: c, Name: "First", Price: 100}},
		{Row: 3, In: ProductInput{Code: c, Name: "Second", Price: 200}},
		{Row: 4, In: ProductInput{Code: "123", Name: "", Price: 5}},
		{Row: 5, In: ProductInput{Code: "4006381333932", Name: "Bad check", Price: 5}},
	}
	res, err := s.ImportProducts(ctx, "t", rows, true)
	if err != nil || !res.DryRun || res.Created != 1 || len(res.Errors) != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	if s.Version() != 0 {
		t.Fatal("dry run changed the store")
	}
	canon, _ := barcode.Canonical(c)
	if _, err := s.Lookup(ctx, canon); err == nil {
		t.Fatal("dry run saved a product")
	}
	res, err = s.ImportProducts(ctx, "t", rows, false)
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.Lookup(ctx, canon)
	if err != nil {
		t.Fatalf("lookup after import: %v (res %+v)", err, res)
	}
	if res.Created != 1 || it.Name != "Second" || it.Price != 200 {
		t.Fatalf("last duplicate must win: %+v %+v", res, it)
	}
}

func TestParseCSV(t *testing.T) {
	file := "\ufeffBarcode;Item Description;Selling Price;Pack\n" +
		"6001234567893;Sugar;\"UGX 5,500\";1 kg\n" +
		"\n" +
		"6001234567800;Rice;4000.00;\n" +
		"bad;Thing;12.5;\n"
	rows, errs, err := ParseCSV(strings.NewReader(file), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].In.Price != 5500 || rows[0].In.Size != "1 kg" || rows[1].In.Price != 4000 || rows[0].Row != 2 || rows[1].Row != 4 {
		t.Fatalf("rows %+v", rows)
	}
	if len(errs) != 1 || errs[0].Row != 5 {
		t.Fatalf("errs %+v", errs)
	}
	if _, _, err := ParseCSV(strings.NewReader("code,name\n1,2\n"), 0); err == nil || !strings.Contains(err.Error(), "price") {
		t.Fatalf("missing column: %v", err)
	}
	if _, _, err := ParseCSV(strings.NewReader(""), 0); err == nil {
		t.Fatal("empty file")
	}
	tabbed := "sku\tname\tprice\n4011\tBananas\t3000\n"
	rows, _, err = ParseCSV(strings.NewReader(tabbed), 0)
	if err != nil || len(rows) != 1 || rows[0].In.Code != "4011" {
		t.Fatalf("tab separated: %+v %v", rows, err)
	}
}

func TestExportRoundTripAndFormulaGuard(t *testing.T) {
	s := newStore(t)
	a := put(t, s, code("600123456789"), "=HYPERLINK(\"x\")", 5500)
	put(t, s, code("600123456796"), "Rice, 1kg \"premium\"", 4000)
	var buf bytes.Buffer
	if err := s.ExportCSV(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "'=HYPERLINK") {
		t.Fatalf("formula not guarded:\n%s", buf.String())
	}
	rows, errs, err := ParseCSV(&buf, 0)
	if err != nil || len(errs) != 0 || len(rows) != 2 {
		t.Fatalf("%v %v %v", rows, errs, err)
	}
	s2 := newStore(t)
	res, _ := s2.ImportProducts(ctx, "t", rows, false)
	if res.Created != 2 {
		t.Fatalf("%+v", res)
	}
	it, err := s2.Lookup(ctx, a)
	if err != nil || it.Name != `=HYPERLINK("x")` {
		t.Fatalf("round trip lost data: %+v %v", it, err)
	}
}

func TestMoney(t *testing.T) {
	ok := map[string]int64{"12500": 12500, "12,500": 12500, "UGX 12,500": 12500, "12500.00": 12500, "0": 0, " 7 ": 7, "Shs 1,000,000": 1000000}
	for in, want := range ok {
		if got, err := ParseMoney(in, 0); err != nil || got != want {
			t.Errorf("ParseMoney(%q,0) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "12.5", "-5", "1.2.3", "1e5", "(5)", "99999999999999999999"} {
		if _, err := ParseMoney(in, 0); err == nil {
			t.Errorf("ParseMoney(%q) should fail", in)
		}
	}
	if got, _ := ParseMoney("3.5", 2); got != 350 {
		t.Errorf("3.5 -> %d", got)
	}
	if FormatMoney(350, 2) != "3.50" || FormatMoney(5, 2) != "0.05" || FormatMoney(12500, 0) != "12500" {
		t.Error("FormatMoney")
	}
}

func TestSearch(t *testing.T) {
	s := newStore(t)
	put(t, s, code("600123456789"), "Sugar 1kg", 5500)
	put(t, s, code("600123456796"), "Brown Sugar 500g", 3000)
	put(t, s, code("600123456802"), "100% Juice", 2000)
	got, _ := s.Search(ctx, "sug", 10)
	if len(got) != 2 || got[0].Name != "Sugar 1kg" { // prefix match ranks first
		t.Fatalf("%+v", got)
	}
	if got, _ := s.Search(ctx, "%", 10); len(got) != 0 {
		t.Fatalf("LIKE wildcard must be escaped, got %d results", len(got))
	}
	if got, _ := s.Search(ctx, "100%", 10); len(got) != 1 {
		t.Fatalf("literal percent search: %d", len(got))
	}
	if got, _ := s.Search(ctx, "a", 10); got != nil {
		t.Fatal("1-char search must be refused")
	}
}

func TestUsersSessionsAndLastOwner(t *testing.T) {
	s := newStore(t)
	if _, err := s.Authenticate(ctx, "owner@example.com", "wrong-pass"); !errors.Is(err, ErrBadLogin) {
		t.Fatal("wrong password accepted")
	}
	if _, err := s.Authenticate(ctx, "nobody@example.com", "whatever12"); !errors.Is(err, ErrBadLogin) {
		t.Fatal("unknown user")
	}
	owner, err := s.Authenticate(ctx, "OWNER@example.com", "owner-pass-123")
	if err != nil || owner.Role != "owner" {
		t.Fatalf("%v %v", owner, err)
	}
	tok, _, err := s.CreateSession(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := s.SessionUser(ctx, tok); err != nil || u.ID != owner.ID {
		t.Fatalf("session: %v %v", u, err)
	}
	if _, err := s.SessionUser(ctx, tok+"x"); !errors.Is(err, ErrNoSession) {
		t.Fatal("forged session accepted")
	}
	s.DeleteSession(ctx, tok)
	if _, err := s.SessionUser(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatal("logout did not work")
	}

	ed, err := s.CreateUser("clerk@example.com", "Clerk", "editor", "clerk-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("Clerk@Example.com", "Dup", "editor", "clerk-pass-1"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate e-mail: %v", err)
	}
	if _, err := s.CreateUser("not-an-email", "X", "editor", "clerk-pass-1"); err == nil {
		t.Fatal("bad email")
	}
	if _, err := s.CreateUser("x@example.com", "X", "editor", "short"); err == nil {
		t.Fatal("weak password")
	}
	etok, _, _ := s.CreateSession(ctx, ed.ID)
	dis := true
	if err := s.UpdateUser(ctx, "owner", ed.ID, UserPatch{Disabled: &dis}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(ctx, etok); !errors.Is(err, ErrNoSession) {
		t.Fatal("disabling must sign the user out")
	}
	if _, err := s.Authenticate(ctx, "clerk@example.com", "clerk-pass-1"); !errors.Is(err, ErrBadLogin) {
		t.Fatal("disabled user logged in")
	}
	role := "editor"
	if err := s.UpdateUser(ctx, "owner", owner.ID, UserPatch{Role: &role}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting last owner: %v", err)
	}
	if err := s.UpdateUser(ctx, "owner", owner.ID, UserPatch{Disabled: &dis}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("disabling last owner: %v", err)
	}
	pw := "brand-new-pass"
	if err := s.UpdateUser(ctx, "owner", owner.ID, UserPatch{Password: &pw}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "owner@example.com", pw); err != nil {
		t.Fatal("new password rejected")
	}
}

func TestExpiredSession(t *testing.T) {
	s := newStore(t)
	u, _ := s.Authenticate(ctx, "owner@example.com", "owner-pass-123")
	tok, _, _ := s.CreateSession(ctx, u.ID)
	s.w.Exec(`UPDATE sessions SET expires_at=?`, time.Now().Unix()-1)
	if _, err := s.SessionUser(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatal("expired session accepted")
	}
}

func TestAPITokens(t *testing.T) {
	s := newStore(t)
	tok, id, err := s.CreateToken(ctx, "owner", "POS nightly")
	if err != nil || !strings.HasPrefix(tok, "ptk_") {
		t.Fatal(err)
	}
	if _, name, err := s.VerifyToken(ctx, tok); err != nil || name != "POS nightly" {
		t.Fatal(err)
	}
	if _, _, err := s.VerifyToken(ctx, tok+"a"); err == nil {
		t.Fatal("forged token")
	}
	if _, _, err := s.VerifyToken(ctx, "ptk_"); err == nil {
		t.Fatal("empty token")
	}
	list, _ := s.ListTokens(ctx)
	if len(list) != 1 || list[0].LastUsed == nil {
		t.Fatalf("%+v", list)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), tok) {
		t.Fatal("token leaked in listing")
	}
	if err := s.RevokeToken(ctx, "owner", id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifyToken(ctx, tok); err == nil {
		t.Fatal("revoked token still valid")
	}
}

func TestStreamCatalog(t *testing.T) {
	s := newStore(t)
	a := put(t, s, code("600123456789"), "Sugar", 5500)
	b := put(t, s, code("600123456796"), "Rice", 4000)
	read := func(since int64) (lines []map[string]any) {
		err := s.StreamCatalog(ctx, since, func(v any) error {
			raw, _ := json.Marshal(v)
			var m map[string]any
			json.Unmarshal(raw, &m)
			lines = append(lines, m)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	l := read(0)
	if len(l) != 4 || l[0]["full"] != true || l[0]["total"] != float64(2) || l[0]["v"] != float64(2) || l[3]["end"] != true || l[3]["n"] != float64(2) {
		t.Fatalf("full: %v", l)
	}
	s.SetPrice(ctx, "t", a, 5600)
	s.DeleteProduct(ctx, "t", b)
	l = read(2)
	if len(l) != 4 || l[0]["full"] != false || l[0]["v"] != float64(4) {
		t.Fatalf("delta header: %v", l)
	}
	var sawItem, sawGone bool
	for _, m := range l[1:3] {
		if m["code"] == a && m["price"] == float64(5600) {
			sawItem = true
		}
		if m["gone"] == b {
			sawGone = true
		}
	}
	if !sawItem || !sawGone {
		t.Fatalf("delta lines: %v", l)
	}
	if l := read(99); l[0]["full"] != true {
		t.Fatal("future cursor must fall back to full")
	}
}

func TestCompactForcesResync(t *testing.T) {
	s := newStore(t)
	a := put(t, s, code("600123456789"), "Sugar", 5500)
	s.DeleteProduct(ctx, "t", a)
	s.w.Exec(`UPDATE products SET updated_at=?`, time.Now().Add(-100*24*time.Hour).Unix())
	if err := s.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	s.r.QueryRow(`SELECT COUNT(*) FROM products`).Scan(&n)
	if n != 0 {
		t.Fatal("tombstone not purged")
	}
	if ev, _ := s.CatchUp(ctx, 1); ev == nil || !ev.Reset {
		t.Fatal("clients older than the purge must resync")
	}
	if ev, _ := s.CatchUp(ctx, s.Version()); ev != nil {
		t.Fatal("current clients are fine")
	}
}

func TestSettingsLock(t *testing.T) {
	s := newStore(t)
	set := s.Settings()
	set.Name = "Renamed Mart"
	set.Announcement = "Open late on Fridays"
	if err := s.UpdateSettings(ctx, "o", set); err != nil {
		t.Fatal(err)
	}
	if s.Settings().Name != "Renamed Mart" {
		t.Fatal("not applied")
	}
	put(t, s, code("600123456789"), "Sugar", 5500)
	set.Decimals = 2
	var ve *ValidationError
	if err := s.UpdateSettings(ctx, "o", set); !errors.As(err, &ve) || ve.Code != "currency_locked" {
		t.Fatalf("decimals must lock once products exist: %v", err)
	}
	set.Decimals = 0
	set.TZOffset = 5000
	if err := s.UpdateSettings(ctx, "o", set); err == nil {
		t.Fatal("bad tz")
	}
}

func TestReopenKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(dir, "reopen", DefaultSettings("Reopen"), "o@example.com", "O", "password-1")
	if err != nil {
		t.Fatal(err)
	}
	c := put(t, s, code("600123456789"), "Sugar", 5500)
	s.Close()
	s2, err := Open(dir, "reopen")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Version() != 1 || s2.Settings().Name != "Reopen" {
		t.Fatal("state lost")
	}
	if it, err := s2.Lookup(ctx, c); err != nil || it.Price != 5500 {
		t.Fatal("data lost")
	}
	if _, err := Open(dir, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing store")
	}
	if _, err := Open(dir, "../etc"); !errors.Is(err, ErrNotFound) {
		t.Fatal("path traversal")
	}
}

func TestBackup(t *testing.T) {
	s := newStore(t)
	c := put(t, s, code("600123456789"), "Sugar", 5500)
	dst := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, dst); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, dst); err == nil {
		t.Fatal("must not overwrite")
	}
	// backups must not carry live sessions
	if _, err := s.Authenticate(ctx, "owner@example.com", "owner-pass-123"); err != nil {
		t.Fatal(err)
	}
	u, _ := s.Authenticate(ctx, "owner@example.com", "owner-pass-123")
	s.CreateSession(ctx, u.ID)
	dst2 := filepath.Join(t.TempDir(), "withsession.db")
	if err := s.Backup(ctx, dst2); err != nil {
		t.Fatal(err)
	}
	dir2 := filepath.Dir(dst2)
	os.Rename(dst2, filepath.Join(dir2, "wscopy.db"))
	ws, err := Open(dir2, "wscopy")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	ws.r.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	ws.Close()
	if n != 0 {
		t.Fatalf("backup contains %d live sessions", n)
	}
	dir := filepath.Dir(dst)
	os.Rename(dst, filepath.Join(dir, "copy.db"))
	b, err := Open(dir, "copy")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if it, err := b.Lookup(ctx, c); err != nil || it.Name != "Sugar" {
		t.Fatal("backup unusable")
	}
}

func TestSlugs(t *testing.T) {
	for _, ok := range []string{"demo", "my-store-1", "abc"} {
		if !ValidSlug(ok) {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"", "a", "ab", "-abc", "abc-", "ABC", "a_b", "a/b", "../x", "admin", "api", "static", strings.Repeat("a", 41)} {
		if ValidSlug(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestConcurrentWritersGetOrderedVersions(t *testing.T) {
	s := newStore(t)
	sub := s.Hub.Subscribe(0)
	defer s.Hub.Unsubscribe(sub)
	const n = 40
	got := make(chan int64, n)
	go func() {
		for f := range sub.C {
			got <- f.V
		}
		close(got)
	}()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.UpsertProduct(ctx, "t", ProductInput{Code: code(fmt.Sprintf("2100000%05d", i)), Name: "P", Price: int64(i + 1)})
		}(i)
	}
	// concurrent readers must never fail while writes happen
	stop := make(chan struct{})
	var rg sync.WaitGroup
	for r := 0; r < 4; r++ {
		rg.Add(1)
		go func() {
			defer rg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := s.Home(ctx); err != nil {
						t.Error(err)
						return
					}
					s.Search(ctx, "P", 5)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	rg.Wait()
	if s.Version() != n {
		t.Fatalf("version %d != %d", s.Version(), n)
	}
	var last int64
	for i := 0; i < n; i++ {
		select {
		case v := <-got:
			if v != last+1 {
				t.Fatalf("events out of order: %d after %d", v, last)
			}
			last = v
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d events arrived", i, n)
		}
	}
}

func TestSecondProcessCannotDuplicateVersions(t *testing.T) {
	dir := t.TempDir()
	a, _ := Create(dir, "twin", DefaultSettings("Twin"), "o@example.com", "O", "password-1")
	defer a.Close()
	b, err := Open(dir, "twin") // simulates a CLI process on the same file
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	put(t, a, code("600123456789"), "A", 1)
	put(t, b, code("600123456796"), "B", 2)
	put(t, a, code("600123456802"), "C", 3)
	var distinct int
	a.r.QueryRow(`SELECT COUNT(DISTINCT rev) FROM products`).Scan(&distinct)
	if distinct != 3 || a.w == nil {
		t.Fatalf("revs not unique: %d", distinct)
	}
}

func TestPromoChangesBumpProductRevForCatchUpAndCatalog(t *testing.T) {
	s := newStore(t)
	a := put(t, s, code("600123456789"), "Sugar", 5500)
	b := put(t, s, code("600123456796"), "Rice", 4000)
	v0 := s.Version()
	it0, _ := s.Lookup(ctx, a)

	now := time.Now().Unix()
	pid, err := s.CreatePromo(ctx, "t", PromoInput{
		Title: "Promo", StartsAt: now - 60, EndsAt: now + 3600, Enabled: true,
		Items: []PromoItemInput{{Code: a, Price: 4800}},
	})
	if err != nil {
		t.Fatal(err)
	}
	it1, _ := s.Lookup(ctx, a)
	if it1.Rev <= it0.Rev {
		t.Fatalf("CreatePromo did not bump product rev: %d -> %d", it0.Rev, it1.Rev)
	}
	ev, err := s.CatchUp(ctx, v0)
	if err != nil || ev == nil || len(ev.Items) != 1 || ev.Items[0].Code != a || len(ev.Items[0].Promos) != 1 {
		t.Fatalf("CatchUp missed created promo: %+v %v", ev, err)
	}
	var deltaCodes []string
	if err := s.StreamCatalog(ctx, v0, func(v any) error {
		if it, ok := v.(*Item); ok {
			deltaCodes = append(deltaCodes, it.Code)
		}
		return nil
	}); err != nil || len(deltaCodes) != 1 || deltaCodes[0] != a {
		t.Fatalf("StreamCatalog delta missed created promo: %v %v", deltaCodes, err)
	}

	// Update promo to move from a to b: both a (removed) and b (added) must appear in CatchUp.
	v1 := s.Version()
	if err := s.UpdatePromo(ctx, "t", pid, PromoInput{
		Title: "Promo2", StartsAt: now - 60, EndsAt: now + 3600, Enabled: true,
		Items: []PromoItemInput{{Code: b, Price: 3500}},
	}); err != nil {
		t.Fatal(err)
	}
	ev2, err := s.CatchUp(ctx, v1)
	if err != nil || ev2 == nil || len(ev2.Items) != 2 {
		t.Fatalf("CatchUp missed updated promo items: %+v %v", ev2, err)
	}

	// Delete promo: b must appear in CatchUp with no promos.
	v2 := s.Version()
	if err := s.DeletePromo(ctx, "t", pid); err != nil {
		t.Fatal(err)
	}
	ev3, err := s.CatchUp(ctx, v2)
	if err != nil || ev3 == nil || len(ev3.Items) != 1 || ev3.Items[0].Code != b || len(ev3.Items[0].Promos) != 0 {
		t.Fatalf("CatchUp missed deleted promo: %+v %v", ev3, err)
	}
}

func BenchmarkLookup(b *testing.B) {
	s := newStore(b)
	var rows []ImportRow
	for i := 0; i < 50000; i++ {
		rows = append(rows, ImportRow{Row: i, In: ProductInput{Code: code(fmt.Sprintf("2200000%05d", i)), Name: fmt.Sprintf("Item %d", i), Price: int64(i + 1)}})
	}
	if _, err := s.ImportProducts(ctx, "b", rows, false); err != nil {
		b.Fatal(err)
	}
	c, _ := barcode.Canonical(code("220000012345"))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := s.Lookup(ctx, c); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// `make demo` uses a RELATIVE data directory ("./data"); every other test used
// absolute temp dirs, so a broken relative-path URI went unnoticed.
func TestRelativeDataDirectoryAndBackupPath(t *testing.T) {
	t.Chdir(t.TempDir())
	s, err := Create("./data", "rel", DefaultSettings("Relative"), "o@example.com", "O", "password-1")
	if err != nil {
		t.Fatalf("create with relative dir: %v", err)
	}
	c := put(t, s, code("600123456789"), "Sugar", 5500)
	if err := s.Backup(ctx, "rel-backup.db"); err != nil { // relative backup target, as `store backup` defaults to
		t.Fatalf("backup to relative path: %v", err)
	}
	s.Close()
	again, err := Open("data", "rel")
	if err != nil {
		t.Fatalf("reopen with relative dir: %v", err)
	}
	defer again.Close()
	if it, err := again.Lookup(ctx, c); err != nil || it.Price != 5500 {
		t.Fatalf("data lost: %+v %v", it, err)
	}
	if err := os.Rename("rel-backup.db", "data/relcopy.db"); err != nil {
		t.Fatal(err)
	}
	cp, err := Open("data", "relcopy")
	if err != nil {
		t.Fatalf("open relative backup: %v", err)
	}
	cp.Close()
}
