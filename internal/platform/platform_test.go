package platform

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

func newP(t *testing.T, s Senders) *Platform {
	t.Helper()
	p, err := Open(t.TempDir(), Config{Senders: s, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// recorder is a Sender that keeps what it was asked to send.
type recorder struct {
	mu   sync.Mutex
	msgs []OutMessage
	fail int
}

func (r *recorder) Name() string { return "test" }
func (r *recorder) Send(_ context.Context, m OutMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail > 0 {
		r.fail--
		return errors.New("provider down")
	}
	r.msgs = append(r.msgs, m)
	return nil
}
func (r *recorder) last(t *testing.T) OutMessage {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.msgs) == 0 {
		t.Fatal("nothing sent")
	}
	return r.msgs[len(r.msgs)-1]
}

var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

func codeFrom(t *testing.T, m OutMessage) string {
	t.Helper()
	c := codeRe.FindStringSubmatch(m.Text)
	if c == nil {
		t.Fatalf("no code in %q", m.Text)
	}
	return c[1]
}

func TestNormalizeContact(t *testing.T) {
	p := newP(t, Senders{})
	ok := map[string]Contact{
		"0772 123 456":     {"phone", "+256772123456"},
		"+256 772-123456":  {"phone", "+256772123456"},
		"256772123456":     {"phone", "+256772123456"},
		"772123456":        {"phone", "+256772123456"},
		"00254712345678":   {"phone", "+254712345678"},
		"  Jane@Mail.COM ": {"email", "jane@mail.com"},
	}
	for in, want := range ok {
		if got, err := p.NormalizeContact(in); err != nil || got != want {
			t.Errorf("%q -> %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "12", "jane@", "077x123456", "+0123456789"} {
		if _, err := p.NormalizeContact(in); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func TestSignInByEmailCreatesOneAccountAndCodeRules(t *testing.T) {
	em := &recorder{}
	p := newP(t, Senders{Email: em})
	c, _ := p.NormalizeContact("jane@mail.com")
	if err := p.StartSignIn(ctx, "shop", "Fresh Mart", c, "sms"); err != nil { // e-mail always goes by e-mail
		t.Fatal(err)
	}
	p.deliverDue(ctx)
	m := em.last(t)
	if m.Channel != "email" || m.To != "jane@mail.com" || !strings.Contains(m.Text, "Fresh Mart") {
		t.Fatalf("%+v", m)
	}
	code := codeFrom(t, m)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if _, _, err := p.VerifySignIn(ctx, c, wrong, 0); !errors.Is(err, ErrBadCode) {
		t.Fatal("wrong code accepted")
	}
	sh, created, err := p.VerifySignIn(ctx, c, code, 0)
	if err != nil || !created || sh.Email != "jane@mail.com" || sh.Channel != "email" {
		t.Fatalf("%+v %v %v", sh, created, err)
	}
	if _, _, err := p.VerifySignIn(ctx, c, code, 0); !errors.Is(err, ErrBadCode) {
		t.Fatal("a code must work only once")
	}
	// signing in again (another shop, another day) finds the SAME account
	p.StartSignIn(ctx, "other", "Other Shop", c, "")
	p.deliverDue(ctx)
	again, created, err := p.VerifySignIn(ctx, c, codeFrom(t, em.last(t)), 0)
	if err != nil || created || again.ID != sh.ID {
		t.Fatalf("one account across shops: %+v %v %v", again, created, err)
	}
}

func TestCodeLimits(t *testing.T) {
	em := &recorder{}
	p := newP(t, Senders{Email: em})
	c, _ := p.NormalizeContact("a@b.co")
	for i := 0; i < otpPerWindow; i++ {
		if err := p.StartSignIn(ctx, "shop", "", c, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.StartSignIn(ctx, "shop", "", c, ""); !errors.Is(err, ErrTooMany) {
		t.Fatalf("4th code in 15 min: %v", err)
	}
	p.deliverDue(ctx)
	code := codeFrom(t, em.last(t))
	for i := 0; i < otpMaxAttempts; i++ {
		p.VerifySignIn(ctx, c, "999999", 0)
	}
	if _, _, err := p.VerifySignIn(ctx, c, code, 0); !errors.Is(err, ErrBadCode) {
		t.Fatal("after 5 wrong tries even the right code must fail")
	}
	// expiry
	p2 := newP(t, Senders{Email: em})
	p2.StartSignIn(ctx, "shop", "", c, "")
	p2.deliverDue(ctx)
	code = codeFrom(t, em.last(t))
	p2.now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	if _, _, err := p2.VerifySignIn(ctx, c, code, 0); !errors.Is(err, ErrBadCode) {
		t.Fatal("expired code accepted")
	}
}

func TestPhoneCodesAreBilledToTheShopAndCapped(t *testing.T) {
	sms, wa := &recorder{}, &recorder{}
	p := newP(t, Senders{SMS: sms, WhatsApp: wa})
	c, _ := p.NormalizeContact("0772123456")
	if err := p.StartSignIn(ctx, "shop", "", c, "sms"); !errors.Is(err, ErrChannelUnavailable) {
		t.Fatalf("SMS must be off until the operator sets a cap: %v", err)
	}
	if err := p.StartSignIn(ctx, "shop", "", c, "carrier-pigeon"); err == nil {
		t.Fatal("bad channel accepted")
	}
	p.SetLimits(ctx, "shop", Limits{SMSMonthCap: 1, WAMonthCap: 5})
	if err := p.StartSignIn(ctx, "shop", "", c, "sms"); err != nil {
		t.Fatal(err)
	}
	c2, _ := p.NormalizeContact("0772000111")
	if err := p.StartSignIn(ctx, "shop", "", c2, "sms"); !errors.Is(err, ErrChannelUnavailable) {
		t.Fatalf("cap of 1 SMS: %v", err)
	}
	if err := p.StartSignIn(ctx, "shop", "", c2, "whatsapp"); err != nil {
		t.Fatal(err)
	}
	p.deliverDue(ctx)
	if sms.last(t).To != "+256772123456" || wa.last(t).To != "+256772000111" {
		t.Fatal("wrong recipients")
	}
	u, _ := p.Usage(ctx, "shop")
	if u.SMSUsed != 1 || u.WAUsed != 1 || u.SMSQueued != 0 {
		t.Fatalf("usage %+v", u)
	}
	if u2, _ := p.Usage(ctx, "other"); u2.SMSUsed != 0 {
		t.Fatal("usage leaked to another shop")
	}
}

func TestAddContactToExistingAccountAndConflicts(t *testing.T) {
	em, sms := &recorder{}, &recorder{}
	p := newP(t, Senders{Email: em, SMS: sms})
	p.SetLimits(ctx, "shop", Limits{SMSMonthCap: 10})
	ce, _ := p.NormalizeContact("x@y.co")
	p.StartSignIn(ctx, "shop", "", ce, "")
	p.deliverDue(ctx)
	me, _, _ := p.VerifySignIn(ctx, ce, codeFrom(t, em.last(t)), 0)
	cp, _ := p.NormalizeContact("0772123456")
	p.StartSignIn(ctx, "shop", "", cp, "sms")
	p.deliverDue(ctx)
	sh, created, err := p.VerifySignIn(ctx, cp, codeFrom(t, sms.last(t)), me.ID)
	if err != nil || created || sh.ID != me.ID || sh.Phone != "+256772123456" || sh.Email != "x@y.co" {
		t.Fatalf("attach phone: %+v %v %v", sh, created, err)
	}
	if _, err := p.UpdateProfile(ctx, me.ID, "  Jane   Doe ", "whatsapp"); err != nil {
		t.Fatal(err)
	}
	// another account cannot take that phone
	c3, _ := p.NormalizeContact("z@y.co")
	p.StartSignIn(ctx, "shop", "", c3, "")
	p.deliverDue(ctx)
	other, _, _ := p.VerifySignIn(ctx, c3, codeFrom(t, em.last(t)), 0)
	p.StartSignIn(ctx, "shop", "", cp, "sms")
	p.deliverDue(ctx)
	if _, _, err := p.VerifySignIn(ctx, cp, codeFrom(t, sms.last(t)), other.ID); !errors.Is(err, ErrContactInUse) {
		t.Fatalf("stealing a phone: %v", err)
	}
	if _, err := p.UpdateProfile(ctx, other.ID, "", "sms"); err == nil {
		t.Fatal("SMS channel without a phone must be refused")
	}
}

func signedIn(t *testing.T, p *Platform, em *recorder, email string) *Shopper {
	t.Helper()
	c, _ := p.NormalizeContact(email)
	if err := p.StartSignIn(ctx, "", "", c, ""); err != nil {
		t.Fatal(err)
	}
	p.deliverDue(ctx)
	sh, _, err := p.VerifySignIn(ctx, c, codeFrom(t, em.last(t)), 0)
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

func TestConsentPerShopAndDeletion(t *testing.T) {
	em := &recorder{}
	p := newP(t, Senders{Email: em})
	sh := signedIn(t, p, em, "j@x.co")
	if ms, _ := p.Members(ctx, "fresh"); len(ms) != 0 {
		t.Fatal("a shop must see nobody before they join")
	}
	p.Join(ctx, sh.ID, "fresh", false)
	ms, _ := p.Members(ctx, "fresh")
	if len(ms) != 1 || ms[0].Contact != "j@x.co" || ms[0].Marketing {
		t.Fatalf("%+v", ms)
	}
	if other, _ := p.Members(ctx, "rival"); len(other) != 0 {
		t.Fatal("joining one shop must not reveal the shopper to another")
	}
	if err := p.SetMarketing(ctx, sh.ID, "rival", true); !errors.Is(err, ErrNotMember) {
		t.Fatal("opt-in without membership")
	}
	p.Leave(ctx, sh.ID, "fresh")
	if ms, _ := p.Members(ctx, "fresh"); len(ms) != 0 {
		t.Fatal("leaving must remove access")
	}
	p.Join(ctx, sh.ID, "fresh", true)
	tok, _, _ := p.CreateSession(ctx, sh.ID)
	if got, _ := p.SessionShopper(ctx, tok); got == nil || got.ID != sh.ID {
		t.Fatal("session")
	}
	p.DeleteShopper(ctx, sh.ID)
	if got, _ := p.SessionShopper(ctx, tok); got != nil {
		t.Fatal("deleted account still signed in")
	}
	if ms, _ := p.Members(ctx, "fresh"); len(ms) != 0 {
		t.Fatal("deleted account still visible to the shop")
	}
}

func TestBroadcastRespectsOptInChannelsCapsAndUnsubscribe(t *testing.T) {
	em, sms := &recorder{}, &recorder{}
	p := newP(t, Senders{Email: em, SMS: sms})
	a := signedIn(t, p, em, "a@x.co") // e-mail, opted in
	b := signedIn(t, p, em, "b@x.co") // joined, NOT opted in
	cph, _ := p.NormalizeContact("0772123456")
	p.SetLimits(ctx, "fresh", Limits{SMSMonthCap: 1})
	p.StartSignIn(ctx, "fresh", "", cph, "sms") // uses the only SMS this month
	p.deliverDue(ctx)
	c, _, _ := p.VerifySignIn(ctx, cph, codeFrom(t, sms.last(t)), 0)
	p.Join(ctx, a.ID, "fresh", true)
	p.Join(ctx, b.ID, "fresh", false)
	p.Join(ctx, c.ID, "fresh", true)

	plan, err := p.Broadcast(ctx, "fresh", "Fresh Mart", "owner", "Sugar 4,900 this weekend!", "https://x.test", true)
	if err != nil || plan.Audience != 2 || plan.ByChannel["email"] != 1 || plan.ByChannel["sms"] != 1 || plan.SMSUnits != 1 || plan.SMSRemaining != 0 || plan.Fits {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if _, err := p.Broadcast(ctx, "fresh", "Fresh Mart", "owner", "x", "https://x.test", false); err == nil {
		t.Fatal("over the SMS cap must send nothing")
	}
	p.SetLimits(ctx, "fresh", Limits{SMSMonthCap: 5})
	plan, err = p.Broadcast(ctx, "fresh", "Fresh Mart", "owner", "Sugar 4,900 this weekend!", "https://x.test", false)
	if err != nil || plan.Queued != 2 {
		t.Fatalf("%+v %v", plan, err)
	}
	p.deliverDue(ctx)
	mail := em.last(t)
	if mail.To != "a@x.co" || !strings.Contains(mail.Text, "Sugar 4,900") || !strings.Contains(mail.Text, "https://x.test/u/") {
		t.Fatalf("%+v", mail)
	}
	if sms.last(t).To != "+256772123456" {
		t.Fatal("SMS member not messaged")
	}
	// the unsubscribe link works, and only for that shop
	link := mail.Text[strings.Index(mail.Text, "/u/")+3:]
	if slug, err := p.Unsubscribe(ctx, strings.TrimSpace(link)); err != nil || slug != "fresh" {
		t.Fatalf("unsubscribe %v %v", slug, err)
	}
	if m, _ := p.Membership(ctx, a.ID, "fresh"); m == nil || m.Marketing {
		t.Fatal("still opted in after unsubscribing")
	}
	if _, err := p.Unsubscribe(ctx, strings.Replace(link, "fresh", "rival", 1)); err == nil {
		t.Fatal("tampered unsubscribe token accepted")
	}
	list, _ := p.Broadcasts(ctx, "fresh")
	if len(list) != 1 || list[0].Delivered != 2 {
		t.Fatalf("%+v", list)
	}
	for i := 0; i < MaxBroadcastsPerDay; i++ {
		p.Broadcast(ctx, "fresh", "Fresh Mart", "owner", "again", "https://x.test", false)
	}
	if _, err := p.Broadcast(ctx, "fresh", "Fresh Mart", "owner", "too many", "https://x.test", false); err == nil {
		t.Fatal("daily limit not enforced")
	}
}

func TestRetriesAndDevMode(t *testing.T) {
	em := &recorder{fail: 2}
	p := newP(t, Senders{Email: em})
	p.QueueEmail(ctx, "shop", "welcome", "o@x.co", "Welcome", "Hello")
	p.deliverDue(ctx)
	p.now = func() time.Time { return time.Now().Add(5 * time.Minute) }
	p.deliverDue(ctx)
	p.now = func() time.Time { return time.Now().Add(15 * time.Minute) }
	p.deliverDue(ctx)
	if em.last(t).Subject != "Welcome" {
		t.Fatal("not retried")
	}
	out, _ := p.Outbox(ctx, 10)
	if out[0].Status != "sent" || out[0].Attempts != 3 {
		t.Fatalf("%+v", out[0])
	}
	// no provider = development mode: recorded, visible, not delivered
	d := newP(t, Senders{})
	c, _ := d.NormalizeContact("q@x.co")
	d.StartSignIn(ctx, "shop", "", c, "")
	d.deliverDue(ctx)
	dev := d.DevMessages()
	if len(dev) != 1 || !codeRe.MatchString(dev[0].Text) {
		t.Fatalf("%+v", dev)
	}
	if d.Delivering()["email"] != "development (not delivered)" {
		t.Fatal(d.Delivering())
	}
}

func TestOutboxHidesRealSignInCodes(t *testing.T) {
	em := &recorder{}
	p := newP(t, Senders{Email: em})
	c, _ := p.NormalizeContact("q@x.co")
	p.StartSignIn(ctx, "shop", "", c, "")
	p.deliverDue(ctx)
	out, _ := p.Outbox(ctx, 5)
	if codeRe.MatchString(out[0].Text) {
		t.Fatal("a real sign-in code is visible in the operator outbox")
	}
}

func TestSMSSegments(t *testing.T) {
	if SMSSegments(strings.Repeat("a", 160)) != 1 || SMSSegments(strings.Repeat("a", 161)) != 2 || SMSSegments(strings.Repeat("é", 71)) != 2 {
		t.Fatal("segments")
	}
}

func TestApplications(t *testing.T) {
	p := newP(t, Senders{})
	valid := func(s string) bool { return regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`).MatchString(s) }
	id, err := p.Submit(ctx, Application{ShopName: " Fresh  Mart ", Slug: "Fresh-Mart", OwnerName: "Ann", OwnerEmail: "Ann@Fresh.co", OwnerPhone: "0772123456"}, valid)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := p.Application(ctx, id)
	if a.ShopName != "Fresh Mart" || a.Slug != "fresh-mart" || a.OwnerEmail != "ann@fresh.co" || a.OwnerPhone != "+256772123456" || a.Status != "pending" {
		t.Fatalf("%+v", a)
	}
	if _, err := p.Submit(ctx, Application{ShopName: "X", Slug: "../x", OwnerName: "A", OwnerEmail: "a@b.co"}, valid); err == nil {
		t.Fatal("bad slug")
	}
	if err := p.Decide(ctx, id, true, "fresh-mart", ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Decide(ctx, id, false, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("deciding twice")
	}
}

// The provider adapters are checked against fake servers that speak the
// documented request formats. (Not verified against the real services.)
func TestAfricasTalkingAndWhatsAppRequests(t *testing.T) {
	var got url.Values
	var gotKey string
	at := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got, gotKey = r.PostForm, r.Header.Get("apiKey")
		if r.URL.Path != "/version1/messaging" {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `{"SMSMessageData":{"Recipients":[{"status":"Success","statusCode":101}]}}`)
	}))
	defer at.Close()
	s := &AfricasTalkingSender{Username: "sandbox", APIKey: "k1", From: "PRICETAG", BaseURL: at.URL}
	if err := s.Send(ctx, OutMessage{Channel: "sms", To: "+256772123456", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got.Get("to") != "+256772123456" || got.Get("message") != "hi" || got.Get("username") != "sandbox" || got.Get("from") != "PRICETAG" || gotKey != "k1" {
		t.Fatalf("%v %s", got, gotKey)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"SMSMessageData":{"Recipients":[{"status":"InsufficientBalance","statusCode":405}]}}`)
	}))
	defer bad.Close()
	if err := (&AfricasTalkingSender{BaseURL: bad.URL}).Send(ctx, OutMessage{To: "+1", Text: "x"}); err == nil {
		t.Fatal("provider refusal must be an error (so it is retried/reported)")
	}

	var body, auth, path string
	waSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, auth, path = string(b), r.Header.Get("Authorization"), r.URL.Path
		io.WriteString(w, `{"messages":[{"id":"wamid.1"}]}`)
	}))
	defer waSrv.Close()
	wa := &WhatsAppSender{Token: "t", PhoneNumberID: "123", Template: "pricetag_message", BaseURL: waSrv.URL}
	if err := wa.Send(ctx, OutMessage{To: "+256772123456", Text: "line one\nline two"}); err != nil {
		t.Fatal(err)
	}
	if path != "/123/messages" || auth != "Bearer t" || !strings.Contains(body, `"to":"256772123456"`) || !strings.Contains(body, `"name":"pricetag_message"`) || !strings.Contains(body, `line one line two`) {
		t.Fatalf("%s %s %s", path, auth, body)
	}
}
