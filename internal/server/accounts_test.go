package server

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"pricetag/internal/platform"
	"pricetag/internal/store"
)

var sixDigits = regexp.MustCompile(`\b(\d{6})\b`)

// lastDev waits for the delivery worker and returns the newest recorded
// (development-mode) message to `to`.
func lastDev(t *testing.T, e *env, to string) platform.DevMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range e.srv.Platform().DevMessages() {
			if m.To == to {
				return m
			}
		}
		e.srv.Platform().Kick()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no message to %s", to)
	return platform.DevMessage{}
}

func shopper(e *env) *client {
	c := e.client()
	c.h.Jar = &cookieJar{}
	return c
}

func signIn(t *testing.T, e *env, c *client, slug, contact, via string) map[string]any {
	t.Helper()
	res, body := c.post("/api/v1/s/"+slug+"/account/start", map[string]string{"contact": contact, "via": via})
	if res.StatusCode != 200 {
		t.Fatalf("start: %d %s", res.StatusCode, body)
	}
	var st struct {
		Contact platform.Contact `json:"contact"`
	}
	decode(t, body, &st)
	code := sixDigits.FindStringSubmatch(lastDev(t, e, st.Contact.Value).Text)[1]
	res, body = c.post("/api/v1/s/"+slug+"/account/verify", map[string]string{"contact": contact, "code": code})
	if res.StatusCode != 200 {
		t.Fatalf("verify: %d %s", res.StatusCode, body)
	}
	var out map[string]any
	decode(t, body, &out)
	return out
}

func TestShopSignupApprovalAndOwnerSetupLink(t *testing.T) {
	e := newEnv(t)
	pub := e.client()
	app := map[string]string{"shop_name": "Fresh Mart", "slug": "fresh-mart", "owner_name": "Ann", "owner_email": "ann@fresh.example", "owner_phone": "0772123456", "town": "Mbarara"}
	if res, _ := pub.do("POST", "/api/v1/signup", app, nil); res.StatusCode != 403 {
		t.Fatalf("sign-up without CSRF header: %d", res.StatusCode)
	}
	if res, body := pub.post("/api/v1/signup", app); res.StatusCode != 201 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	app2 := map[string]string{"shop_name": "Taken", "slug": "shop", "owner_name": "B", "owner_email": "b@x.example"}
	if res, _ := pub.post("/api/v1/signup", app2); res.StatusCode != 409 {
		t.Fatalf("existing shop address: %d", res.StatusCode)
	}
	bot := map[string]string{"shop_name": "Spam", "slug": "spam-shop", "owner_name": "x", "owner_email": "x@x.example", "website": "http://spam"}
	pub.post("/api/v1/signup", bot)

	op := map[string]string{"Authorization": "Bearer operator-token-for-tests-123456"}
	res, body := pub.do("GET", "/api/v1/operator/applications", nil, op)
	var apps struct{ Applications []platform.Application }
	decode(t, body, &apps)
	if res.StatusCode != 200 || len(apps.Applications) != 1 || apps.Applications[0].Status != "pending" {
		t.Fatalf("honeypot sign-up must be dropped: %s", body)
	}
	if res, _ := pub.do("POST", "/api/v1/operator/applications/1/approve", nil, nil); res.StatusCode != 401 {
		t.Fatal("approval without operator token")
	}
	res, body = pub.do("POST", "/api/v1/operator/applications/1/approve", nil, op)
	var ap struct {
		Slug     string `json:"slug"`
		SetupURL string `json:"setup_url"`
	}
	decode(t, body, &ap)
	if res.StatusCode != 201 || !strings.Contains(ap.SetupURL, "/s/fresh-mart/admin/#setup=pw_") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	welcome := lastDev(t, e, "ann@fresh.example")
	if !strings.Contains(welcome.Text, ap.SetupURL) {
		t.Fatalf("welcome e-mail lacks the setup link: %s", welcome.Text)
	}
	if res, _ := pub.do("POST", "/api/v1/operator/applications/1/approve", nil, op); res.StatusCode != 409 {
		t.Fatal("approving twice")
	}
	tok := ap.SetupURL[strings.Index(ap.SetupURL, "#setup=")+7:]
	owner := shopper(e)
	if res, body := owner.post("/api/v1/s/fresh-mart/admin/setup", map[string]string{"token": tok, "password": "my-own-pass-1"}); res.StatusCode != 200 {
		t.Fatalf("setup: %d %s", res.StatusCode, body)
	}
	if res, _ := owner.get("/api/v1/s/fresh-mart/admin/stats"); res.StatusCode != 200 {
		t.Fatal("setup link must sign the owner in")
	}
	if res, _ := e.client().post("/api/v1/s/fresh-mart/admin/setup", map[string]string{"token": tok, "password": "another-pass-1"}); res.StatusCode != 400 {
		t.Fatal("setup link reused")
	}
	e.adminClient2(t, "fresh-mart", "ann@fresh.example", "my-own-pass-1")
}

func (e *env) adminClient2(t *testing.T, slug, email, pw string) *client {
	c := shopper(e)
	if res, b := c.post("/api/v1/s/"+slug+"/admin/login", map[string]string{"email": email, "password": pw}); res.StatusCode != 200 {
		t.Fatalf("login %s: %d %s", slug, res.StatusCode, b)
	}
	return c
}

func TestShopperAccountJoinPrivacyAndMessages(t *testing.T) {
	e := newEnv(t)
	seed(t, e)
	st2, err := store.Create(e.dir, "rival", store.DefaultSettings("Rival"), "r@rival.example", "R", "rival-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Registry().Adopt(st2)

	c := shopper(e)
	if res, _ := c.do("POST", "/api/v1/s/shop/account/start", map[string]string{"contact": "jane@mail.example"}, nil); res.StatusCode != 403 {
		t.Fatal("account writes need the CSRF header")
	}
	if res, b := c.post("/api/v1/s/shop/account/start", map[string]string{"contact": "0772123456", "via": "sms"}); res.StatusCode != 409 || !strings.Contains(string(b), "channel_unavailable") {
		t.Fatalf("SMS without a cap: %d %s", res.StatusCode, b)
	}
	out := signIn(t, e, c, "shop", "jane@mail.example", "")
	if out["created"] != true || out["member"] != nil {
		t.Fatalf("%v", out)
	}
	owner := e.adminClient("owner@example.com", "owner-pass-123")
	_, body := owner.get("/api/v1/s/shop/admin/customers")
	if strings.Contains(string(body), "jane@") {
		t.Fatal("a shop must not see a shopper who has not joined")
	}
	if res, _ := c.post("/api/v1/s/shop/account/profile", map[string]string{"name": "Jane", "channel": "email"}); res.StatusCode != 200 {
		t.Fatal("profile")
	}
	if res, _ := c.post("/api/v1/s/shop/account/join", map[string]bool{"marketing": true}); res.StatusCode != 200 {
		t.Fatal("join")
	}
	_, body = owner.get("/api/v1/s/shop/admin/customers")
	if !strings.Contains(string(body), "jane@mail.example") || !strings.Contains(string(body), `"Jane"`) {
		t.Fatalf("joined shopper missing: %s", body)
	}
	rival := e.adminClient2(t, "rival", "r@rival.example", "rival-pass-1")
	if _, b := rival.get("/api/v1/s/rival/admin/customers"); strings.Contains(string(b), "jane@") {
		t.Fatal("another shop must not see her")
	}
	// Same account, another shop: no new account, nothing shared until she joins there.
	if res, b := c.get("/api/v1/s/rival/account"); res.StatusCode != 200 || !strings.Contains(string(b), `"member":null`) || !strings.Contains(string(b), "jane@mail.example") {
		t.Fatalf("signed in everywhere, member nowhere else: %s", b)
	}
	// Editors cannot read the customer list
	owner.post("/api/v1/s/shop/admin/users", map[string]string{"email": "ed@example.com", "name": "Ed", "role": "editor", "password": "editor-pass-1"})
	ed := e.adminClient2(t, "shop", "ed@example.com", "editor-pass-1")
	if res, _ := ed.get("/api/v1/s/shop/admin/customers"); res.StatusCode != 403 {
		t.Fatal("editor read customers")
	}

	// Owner messages customers who opted in
	res, body := owner.post("/api/v1/s/shop/admin/messages", map[string]any{"text": "Sugar 4,900 this weekend", "dry_run": true})
	var plan platform.BroadcastPlan
	decode(t, body, &plan)
	if res.StatusCode != 200 || plan.Audience != 1 || plan.ByChannel["email"] != 1 || !plan.Fits {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	res, body = owner.post("/api/v1/s/shop/admin/messages", map[string]any{"text": "Sugar 4,900 this weekend"})
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	msg := lastDev(t, e, "jane@mail.example")
	if !strings.Contains(msg.Text, "Sugar 4,900") || !strings.Contains(msg.Text, "/u/") {
		t.Fatalf("%+v", msg)
	}
	link := msg.Text[strings.Index(msg.Text, "/u/"):]
	if res, b := e.client().get(strings.TrimSpace(link)); res.StatusCode != 200 || !strings.Contains(string(b), "will not send you offers") {
		t.Fatalf("unsubscribe: %d %s", res.StatusCode, b)
	}
	if _, b := c.get("/api/v1/s/shop/account"); !strings.Contains(string(b), `"marketing":false`) {
		t.Fatalf("still opted in: %s", b)
	}
	// Leaving removes the shop's access
	c.post("/api/v1/s/shop/account/leave", nil)
	if _, b := owner.get("/api/v1/s/shop/admin/customers"); strings.Contains(string(b), "jane@") {
		t.Fatal("left, still visible")
	}
	// Delete account
	c.post("/api/v1/s/shop/account/join", map[string]bool{"marketing": false})
	if res, _ := c.post("/api/v1/s/shop/account/delete", nil); res.StatusCode != 200 {
		t.Fatal("delete")
	}
	if _, b := c.get("/api/v1/s/shop/account"); !strings.Contains(string(b), `"shopper":null`) {
		t.Fatal("deleted account still signed in")
	}
	if _, b := owner.get("/api/v1/s/shop/admin/customers"); strings.Contains(string(b), "jane@") {
		t.Fatal("deleted account still visible to the shop")
	}
}

func TestPhoneSignInUsesShopAllowanceAndWrongCodes(t *testing.T) {
	e := newEnv(t)
	e.srv.Platform().SetLimits(context.Background(), "shop", platform.Limits{SMSMonthCap: 5, WAMonthCap: 5})
	c := shopper(e)
	out := signIn(t, e, c, "shop", "0772 123 456", "whatsapp")
	sh := out["shopper"].(map[string]any)
	if sh["phone"] != "+256772123456" {
		t.Fatalf("%v", out)
	}
	if m := lastDev(t, e, "+256772123456"); m.Channel != "whatsapp" {
		t.Fatalf("code went by %s", m.Channel)
	}
	u, _ := e.srv.Platform().Usage(context.Background(), "shop")
	if u.WAUsed != 1 {
		t.Fatalf("WhatsApp sign-in code must count against the shop: %+v", u)
	}
	c2 := shopper(e)
	c2.post("/api/v1/s/shop/account/start", map[string]string{"contact": "x@y.example"})
	if res, _ := c2.post("/api/v1/s/shop/account/verify", map[string]string{"contact": "x@y.example", "code": "000000"}); res.StatusCode != 400 {
		t.Fatal("wrong code")
	}
}

func TestForgotPasswordDoesNotRevealAccounts(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	r1, b1 := c.post("/api/v1/s/shop/admin/forgot", map[string]string{"email": "owner@example.com"})
	r2, b2 := c.post("/api/v1/s/shop/admin/forgot", map[string]string{"email": "nobody@example.com"})
	if r1.StatusCode != 200 || r2.StatusCode != 200 || string(b1) != string(b2) {
		t.Fatal("answers must be identical")
	}
	m := lastDev(t, e, "owner@example.com")
	i := strings.Index(m.Text, "#setup=")
	if i < 0 {
		t.Fatalf("%s", m.Text)
	}
	tok := strings.Fields(m.Text[i+7:])[0]
	o := shopper(e)
	if res, b := o.post("/api/v1/s/shop/admin/setup", map[string]string{"token": tok, "password": "reset-pass-123"}); res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	e.adminClient("owner@example.com", "reset-pass-123")
}

func TestOperatorShopsAndLimits(t *testing.T) {
	e := newEnv(t)
	op := map[string]string{"Authorization": "Bearer operator-token-for-tests-123456"}
	c := e.client()
	if res, _ := c.do("PUT", "/api/v1/operator/shops/shop/limits", map[string]int{"sms_month_cap": 300, "wa_month_cap": 50}, op); res.StatusCode != 200 {
		t.Fatal("set limits")
	}
	_, b := c.do("GET", "/api/v1/operator/shops", nil, op)
	if !strings.Contains(string(b), `"sms_cap":300`) || !strings.Contains(string(b), `"name":"Test Mart"`) {
		t.Fatalf("%s", b)
	}
	if res, _ := c.do("PUT", "/api/v1/operator/shops/nosuch/limits", map[string]int{"sms_month_cap": 1}, op); res.StatusCode != 404 {
		t.Fatal("unknown shop")
	}
	if _, b := c.do("GET", "/api/v1/operator/status", nil, op); !strings.Contains(string(b), "development (not delivered)") {
		t.Fatalf("%s", b)
	}
}
