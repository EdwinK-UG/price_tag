package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"pricetag/internal/barcode"
	"pricetag/internal/store"
)

type env struct {
	t    *testing.T
	srv  *Server
	ts   *httptest.Server
	dir  string
	base string // http://host
}

func newEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{DataDir: dir, OperatorToken: "operator-token-for-tests-123456", MaxSSEPerIP: 100}
	for _, m := range mutate {
		m(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); srv.Shutdown() })
	st, err := store.Create(dir, "shop", store.DefaultSettings("Test Mart"), "owner@example.com", "Olive", "owner-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.reg.Adopt(st); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, srv: srv, ts: ts, dir: dir, base: ts.URL}
}

func code(body string) string { c, _ := barcode.Canonical(barcode.WithCheckDigit(body)); return c }
func raw(body string) string  { return barcode.WithCheckDigit(body) }

type client struct {
	e *env
	h *http.Client
}

func (e *env) client() *client {
	return &client{e: e, h: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *client) do(method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	c.e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, c.e.base+path, rd)
	if _, isStr := body.(string); body != nil && !isStr {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.h.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res, out
}

func (c *client) get(path string) (*http.Response, []byte) { return c.do("GET", path, nil, nil) }

// admin returns a client with a valid owner session cookie.
func (e *env) adminClient(email, pw string) *client {
	e.t.Helper()
	c := e.client()
	jar := &cookieJar{}
	c.h.Jar = jar
	res, b := c.do("POST", "/api/v1/s/shop/admin/login", map[string]string{"email": email, "password": pw}, map[string]string{"X-PT-CSRF": "1"})
	if res.StatusCode != 200 {
		e.t.Fatalf("login failed: %d %s", res.StatusCode, b)
	}
	return c
}

type cookieJar struct {
	mu sync.Mutex
	m  map[string]*http.Cookie
}

func (j *cookieJar) SetCookies(u *urlT, cs []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.m == nil {
		j.m = map[string]*http.Cookie{}
	}
	for _, c := range cs {
		if c.MaxAge < 0 {
			delete(j.m, c.Name)
		} else {
			j.m[c.Name] = c
		}
	}
}
func (j *cookieJar) Cookies(u *urlT) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []*http.Cookie
	for _, c := range j.m {
		out = append(out, c)
	}
	return out
}

func (c *client) post(path string, body any) (*http.Response, []byte) {
	return c.do("POST", path, body, map[string]string{"X-PT-CSRF": "1"})
}
func (c *client) put(path string, body any) (*http.Response, []byte) {
	return c.do("PUT", path, body, map[string]string{"X-PT-CSRF": "1"})
}

func decode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("bad json %q: %v", b, err)
	}
}

func seed(t *testing.T, e *env) (a, b string) {
	t.Helper()
	st, rel, _ := e.srv.reg.Acquire("shop")
	defer rel()
	a, _, _ = st.UpsertProduct(context.Background(), "t", store.ProductInput{Code: raw("600123456789"), Name: "Sugar", Size: "1 kg", Price: 5500})
	b, _, _ = st.UpsertProduct(context.Background(), "t", store.ProductInput{Code: raw("600123456796"), Name: "Rice", Size: "1 kg", Price: 4000})
	return
}

// ---- SSE helper ------------------------------------------------------------------

type frame struct{ id, event, data string }

func sse(t *testing.T, url string, hdr map[string]string) (<-chan frame, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		cancel()
		t.Fatalf("sse status %d: %s", res.StatusCode, b)
	}
	ch := make(chan frame, 64)
	go func() {
		defer close(ch)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var f frame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f.event != "" {
					ch <- f
				}
				f = frame{}
			case strings.HasPrefix(line, "id: "):
				f.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				f.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				f.data = line[6:]
			}
		}
	}()
	return ch, cancel
}

func next(t *testing.T, ch <-chan frame, d time.Duration) frame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return f
	case <-time.After(d):
		t.Fatal("timed out waiting for SSE frame")
	}
	return frame{}
}

// ---- tests --------------------------------------------------------------------------

func TestPublicAPI(t *testing.T) {
	e := newEnv(t)
	a, _ := seed(t, e)
	c := e.client()

	res, body := c.get("/api/v1/s/shop/items/" + raw("600123456789"))
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	var it store.Item
	decode(t, body, &it)
	if it.Code != a || it.Price != 5500 || it.Name != "Sugar" {
		t.Fatalf("%+v", it)
	}
	if res.Header.Get("X-PT-V") != "2" || res.Header.Get("ETag") == "" || res.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("headers %v", res.Header)
	}
	if len(body) > 300 {
		t.Fatalf("lookup response is %d bytes; keep it tiny", len(body))
	}
	// conditional request => 304 with no body
	res, body = c.do("GET", "/api/v1/s/shop/items/"+raw("600123456789"), nil, map[string]string{"If-None-Match": res.Header.Get("ETag")})
	if res.StatusCode != 304 || len(body) != 0 {
		t.Fatalf("want 304, got %d (%d bytes)", res.StatusCode, len(body))
	}
	// UPC-A and EAN-13 spellings reach the same product
	res, _ = c.get("/api/v1/s/shop/items/" + strings.TrimPrefix(raw("600123456789"), "0"))
	if res.StatusCode != 200 {
		t.Fatal("leading-zero variant")
	}
	if res, _ = c.get("/api/v1/s/shop/items/" + raw("600000000001")); res.StatusCode != 404 {
		t.Fatalf("unknown product: %d", res.StatusCode)
	}
	if res, _ = c.get("/api/v1/s/shop/items/4006381333932"); res.StatusCode != 400 {
		t.Fatalf("bad check digit: %d", res.StatusCode)
	}
	if res, _ = c.get("/api/v1/s/nosuch/items/" + raw("600123456789")); res.StatusCode != 404 {
		t.Fatalf("unknown store: %d", res.StatusCode)
	}
	for _, evil := range []string{"/api/v1/s/..%2f..%2fetc/home", "/api/v1/s/%2e%2e/home", "/api/v1/s/SHOP/home", "/api/v1/s/shop.db/home"} {
		if res, _ = c.get(evil); res.StatusCode == 200 {
			t.Fatalf("%s must not resolve", evil)
		}
	}

	res, body = c.get("/api/v1/s/shop/search?q=sug")
	var sr struct{ Items []store.Item }
	decode(t, body, &sr)
	if res.StatusCode != 200 || len(sr.Items) != 1 {
		t.Fatalf("search %s", body)
	}

	res, body = c.get("/api/v1/s/shop/home")
	var hd store.HomeData
	decode(t, body, &hd)
	if res.StatusCode != 200 || hd.Store.Name != "Test Mart" || hd.Store.Currency != "UGX" || hd.Promos == nil {
		t.Fatalf("home %s", body)
	}
	etag := res.Header.Get("ETag")
	if res, _ = c.do("GET", "/api/v1/s/shop/home", nil, map[string]string{"If-None-Match": etag}); res.StatusCode != 304 {
		t.Fatalf("home 304: %d", res.StatusCode)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("API responses need nosniff")
	}
	if res.Header.Get("Content-Security-Policy") != "" {
		t.Error("API responses should stay lean: CSP belongs on HTML pages only")
	}
	for _, page := range []string{"/s/shop/", "/s/shop/admin/", "/"} {
		pr, _ := c.get(page)
		for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options"} {
			if pr.Header.Get(h) == "" {
				t.Errorf("%s: missing security header %s", page, h)
			}
		}
	}
}

func TestLiveUpdateOverSSE(t *testing.T) {
	e := newEnv(t)
	a, _ := seed(t, e)
	admin := e.adminClient("owner@example.com", "owner-pass-123")
	ch, stop := sse(t, e.base+"/api/v1/s/shop/events", nil)
	defer stop()
	hello := next(t, ch, 2*time.Second)
	if hello.event != "hello" || !strings.Contains(hello.data, `"v":2`) {
		t.Fatalf("hello %+v", hello)
	}

	start := time.Now()
	res, body := admin.put("/api/v1/s/shop/admin/products/"+raw("600123456789")+"/price", map[string]int64{"price": 6100})
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	f := next(t, ch, 2*time.Second)
	lat := time.Since(start)
	var ev store.Event
	decode(t, []byte(f.data), &ev)
	if f.event != "change" || f.id != "3" || ev.Prev != 2 || ev.V != 3 || len(ev.Items) != 1 || ev.Items[0].Code != a || ev.Items[0].Price != 6100 {
		t.Fatalf("event %+v", f)
	}
	t.Logf("price edit -> phone event: %v (includes the HTTP edit round trip)", lat)
	if lat > 500*time.Millisecond {
		t.Errorf("update took %v", lat)
	}

	// An unchanged edit produces no event and no version bump.
	admin.put("/api/v1/s/shop/admin/products/"+raw("600123456789")+"/price", map[string]int64{"price": 6100})
	select {
	case f := <-ch:
		if f.event == "change" {
			t.Fatalf("unexpected event %+v", f)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

func TestSSECatchUpAfterOffline(t *testing.T) {
	e := newEnv(t)
	seed(t, e) // v2
	admin := e.adminClient("owner@example.com", "owner-pass-123")
	admin.put("/api/v1/s/shop/admin/products/"+raw("600123456789")+"/price", map[string]int64{"price": 6100}) // v3
	admin.put("/api/v1/s/shop/admin/products/"+raw("600123456796")+"/price", map[string]int64{"price": 4200}) // v4

	// Phone last saw v2: query param form
	ch, stop := sse(t, e.base+"/api/v1/s/shop/events?since=2", nil)
	next(t, ch, 2*time.Second) // hello
	f := next(t, ch, 2*time.Second)
	var ev store.Event
	decode(t, []byte(f.data), &ev)
	if ev.Prev != 2 || ev.V != 4 || len(ev.Items) != 2 || ev.Reset {
		t.Fatalf("catch-up %s", f.data)
	}
	stop()
	// EventSource's automatic reconnect sends Last-Event-ID instead
	ch, stop = sse(t, e.base+"/api/v1/s/shop/events", map[string]string{"Last-Event-ID": "3"})
	defer stop()
	next(t, ch, 2*time.Second)
	f = next(t, ch, 2*time.Second)
	decode(t, []byte(f.data), &ev)
	if ev.Prev != 3 || len(ev.Items) != 1 || ev.Items[0].Price != 4200 {
		t.Fatalf("header catch-up %s", f.data)
	}
	// Already current: nothing extra
	ch2, stop2 := sse(t, e.base+"/api/v1/s/shop/events?since=4", nil)
	defer stop2()
	next(t, ch2, 2*time.Second)
	select {
	case f := <-ch2:
		t.Fatalf("current client got %+v", f)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestSSELimitsAndShutdown(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxSSEPerIP = 2 })
	_, s1 := sse(t, e.base+"/api/v1/s/shop/events", nil)
	_, s2 := sse(t, e.base+"/api/v1/s/shop/events", nil)
	defer s1()
	defer s2()
	time.Sleep(50 * time.Millisecond)
	res, err := http.Get(e.base + "/api/v1/s/shop/events")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 429 {
		t.Fatalf("third stream from one IP: %d", res.StatusCode)
	}
	s1()
	time.Sleep(100 * time.Millisecond)
	res, _ = http.Get(e.base + "/api/v1/s/shop/events?since=0")
	if res.StatusCode != 200 {
		t.Fatalf("slot not released: %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestAdminAuthAndCSRF(t *testing.T) {
	e := newEnv(t)
	seed(t, e)
	c := e.client()
	if res, b := c.get("/api/v1/s/shop/admin/me"); res.StatusCode != 200 || !strings.Contains(string(b), `"user":null`) {
		t.Fatalf("signed-out me should be 200 with a null user: %d %s", res.StatusCode, b)
	}
	if res, _ := c.get("/api/v1/s/shop/admin/stats"); res.StatusCode != 401 {
		t.Fatalf("unauthenticated admin endpoint: %d", res.StatusCode)
	}
	if res, _ := c.put("/api/v1/s/shop/admin/products/"+raw("600123456789")+"/price", map[string]int64{"price": 1}); res.StatusCode != 401 {
		t.Fatalf("unauthenticated write: %d", res.StatusCode)
	}
	if res, _ := c.post("/api/v1/s/shop/admin/login", map[string]string{"email": "owner@example.com", "password": "nope-nope"}); res.StatusCode != 401 {
		t.Fatalf("wrong password: %d", res.StatusCode)
	}
	// login without the CSRF header is refused
	if res, _ := c.do("POST", "/api/v1/s/shop/admin/login", map[string]string{"email": "owner@example.com", "password": "owner-pass-123"}, nil); res.StatusCode != 403 {
		t.Fatalf("login without csrf header: %d", res.StatusCode)
	}
	admin := e.adminClient("owner@example.com", "owner-pass-123")
	if res, _ := admin.get("/api/v1/s/shop/admin/me"); res.StatusCode != 200 {
		t.Fatal("me")
	}
	// session cookie flags
	res, _ := e.client().do("POST", "/api/v1/s/shop/admin/login", map[string]string{"email": "owner@example.com", "password": "owner-pass-123"}, map[string]string{"X-PT-CSRF": "1"})
	ck := res.Header.Get("Set-Cookie")
	if !strings.Contains(ck, "HttpOnly") || !strings.Contains(ck, "SameSite=Strict") {
		t.Fatalf("cookie flags: %s", ck)
	}
	// writes need the header and a same-origin Origin
	if res, _ := admin.do("PUT", "/api/v1/s/shop/admin/products/"+raw("600123456789")+"/price", map[string]int64{"price": 9}, nil); res.StatusCode != 403 {
		t.Fatalf("missing csrf header: %d", res.StatusCode)
	}
	if res, _ := admin.do("PUT", "/api/v1/s/shop/admin/products/"+raw("600123456789")+"/price", map[string]int64{"price": 9},
		map[string]string{"X-PT-CSRF": "1", "Origin": "https://evil.example"}); res.StatusCode != 403 {
		t.Fatalf("cross-origin write: %d", res.StatusCode)
	}
	if res, _ := admin.do("PUT", "/api/v1/s/shop/admin/products/"+raw("600123456789")+"/price", map[string]int64{"price": 9},
		map[string]string{"X-PT-CSRF": "1", "Origin": e.base}); res.StatusCode != 200 {
		t.Fatalf("same-origin write: %d", res.StatusCode)
	}
	// logout kills the session server-side
	admin.post("/api/v1/s/shop/admin/logout", nil)
	if _, b := admin.get("/api/v1/s/shop/admin/me"); !strings.Contains(string(b), `"user":null`) {
		t.Fatalf("session survived logout: %s", b)
	}
	if res, _ := admin.get("/api/v1/s/shop/admin/stats"); res.StatusCode != 401 {
		t.Fatal("admin API still open after logout")
	}
}

func TestEditorCannotDoOwnerThings(t *testing.T) {
	e := newEnv(t)
	seed(t, e)
	owner := e.adminClient("owner@example.com", "owner-pass-123")
	res, body := owner.post("/api/v1/s/shop/admin/users", map[string]string{"email": "clerk@example.com", "name": "Clerk", "role": "editor"})
	if res.StatusCode != 201 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	var cu struct {
		Password string
	}
	decode(t, body, &cu)
	if len(cu.Password) < 10 {
		t.Fatal("generated password not returned")
	}
	clerk := e.adminClient("clerk@example.com", cu.Password)
	if res, _ := clerk.put("/api/v1/s/shop/admin/products/"+raw("600123456789")+"/price", map[string]int64{"price": 5000}); res.StatusCode != 200 {
		t.Fatalf("editor should edit prices: %d", res.StatusCode)
	}
	for _, p := range []string{"/api/v1/s/shop/admin/users", "/api/v1/s/shop/admin/tokens", "/api/v1/s/shop/admin/export.db"} {
		if res, _ := clerk.get(p); res.StatusCode != 403 {
			t.Errorf("editor GET %s: %d", p, res.StatusCode)
		}
	}
	if res, _ := clerk.put("/api/v1/s/shop/admin/settings", map[string]any{"name": "Hacked", "currency": "UGX", "decimals": 0, "tz_offset": 180}); res.StatusCode != 403 {
		t.Fatalf("editor changing settings: %d", res.StatusCode)
	}
	// history shows who changed the price
	res, body = owner.get("/api/v1/s/shop/admin/history?kind=product&ref=" + raw("600123456789"))
	if !strings.Contains(string(body), "clerk@example.com") {
		t.Fatalf("audit trail missing actor: %s", body)
	}
}

func TestPromotionShowsOnHomeLive(t *testing.T) {
	e := newEnv(t)
	a, _ := seed(t, e)
	admin := e.adminClient("owner@example.com", "owner-pass-123")
	ch, stop := sse(t, e.base+"/api/v1/s/shop/events", nil)
	defer stop()
	next(t, ch, 2*time.Second)
	now := time.Now().Unix()
	res, body := admin.post("/api/v1/s/shop/admin/promos", map[string]any{
		"title": "Weekend special", "text": "While stocks last", "starts_at": now - 10, "ends_at": now + 7200, "enabled": true,
		"items": []map[string]any{{"code": raw("600123456789"), "price": 4900}},
	})
	if res.StatusCode != 201 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	f := next(t, ch, 2*time.Second)
	var ev store.Event
	decode(t, []byte(f.data), &ev)
	if !ev.Home || len(ev.Items) != 1 || len(ev.Items[0].Promos) != 1 || ev.Items[0].Promos[0].Price != 4900 {
		t.Fatalf("event %s", f.data)
	}
	_, hb := e.client().get("/api/v1/s/shop/home")
	var hd store.HomeData
	decode(t, hb, &hd)
	if len(hd.Promos) != 1 || hd.Promos[0].Title != "Weekend special" || hd.Promos[0].Items[0].Code != a || hd.Promos[0].Items[0].PromoPrice != 4900 {
		t.Fatalf("home %s", hb)
	}
	// offer price must be lower than normal price
	res, body = admin.post("/api/v1/s/shop/admin/promos", map[string]any{
		"title": "Oops", "starts_at": now, "ends_at": now + 60, "enabled": true,
		"items": []map[string]any{{"code": raw("600123456789"), "price": 9999}},
	})
	if res.StatusCode != 400 || !strings.Contains(string(body), "promo_price_not_lower") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestIngestCSVAndJSON(t *testing.T) {
	e := newEnv(t)
	owner := e.adminClient("owner@example.com", "owner-pass-123")
	res, body := owner.post("/api/v1/s/shop/admin/tokens", map[string]string{"name": "POS nightly"})
	if res.StatusCode != 201 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	var tk struct{ Token string }
	decode(t, body, &tk)
	auth := map[string]string{"Authorization": "Bearer " + tk.Token, "Content-Type": "text/csv"}
	csv := "Barcode,Description,Selling Price,Pack\n" + raw("600123456789") + ",Sugar,\"5,500\",1 kg\n" + raw("600123456796") + ",Rice,4000,1 kg\n4006381333932,Misread,1,\n"
	c := e.client()

	if res, _ := c.do("POST", "/api/v1/s/shop/ingest", csv, map[string]string{"Content-Type": "text/csv"}); res.StatusCode != 401 {
		t.Fatalf("no token: %d", res.StatusCode)
	}
	if res, _ := c.do("POST", "/api/v1/s/shop/ingest", csv, map[string]string{"Authorization": "Bearer ptk_wrong", "Content-Type": "text/csv"}); res.StatusCode != 401 {
		t.Fatalf("wrong token: %d", res.StatusCode)
	}
	res, body = c.do("POST", "/api/v1/s/shop/ingest?dry_run=1", csv, auth)
	var r1 store.ImportResult
	decode(t, body, &r1)
	if res.StatusCode != 200 || !r1.DryRun || r1.Created != 2 || len(r1.Errors) != 1 || r1.Version != 0 {
		t.Fatalf("dry run %d %s", res.StatusCode, body)
	}
	res, body = c.do("POST", "/api/v1/s/shop/ingest", csv, auth)
	decode(t, body, &r1)
	if r1.Created != 2 || r1.Version != 1 {
		t.Fatalf("import %s", body)
	}
	// identical file again: no new version => nothing pushed to phones
	_, body = c.do("POST", "/api/v1/s/shop/ingest", csv, auth)
	var r2 store.ImportResult
	decode(t, body, &r2)
	if r2.Unchanged != 2 || r2.Version != 1 {
		t.Fatalf("idempotency %s", body)
	}
	// JSON form
	js := fmt.Sprintf(`{"products":[{"code":%q,"name":"Sugar","size":"1 kg","price":5900}]}`, raw("600123456789"))
	_, body = c.do("POST", "/api/v1/s/shop/ingest", js, map[string]string{"Authorization": "Bearer " + tk.Token, "Content-Type": "application/json"})
	var r3 store.ImportResult
	decode(t, body, &r3)
	if r3.Updated != 1 || r3.Version != 2 {
		t.Fatalf("json import %s", body)
	}
	_, body = c.get("/api/v1/s/shop/items/" + raw("600123456789"))
	if !strings.Contains(string(body), `"price":5900`) {
		t.Fatalf("price not applied: %s", body)
	}
	// revoked tokens stop working immediately
	var list struct{ Tokens []store.APIToken }
	_, lb := owner.get("/api/v1/s/shop/admin/tokens")
	decode(t, lb, &list)
	if strings.Contains(string(lb), tk.Token) {
		t.Fatal("token leaked")
	}
	owner.do("DELETE", fmt.Sprintf("/api/v1/s/shop/admin/tokens/%d", list.Tokens[0].ID), nil, map[string]string{"X-PT-CSRF": "1"})
	if res, _ := c.do("POST", "/api/v1/s/shop/ingest", csv, auth); res.StatusCode != 401 {
		t.Fatalf("revoked token: %d", res.StatusCode)
	}
}

func TestImportBodyLimitAndBadFiles(t *testing.T) {
	e := newEnv(t)
	owner := e.adminClient("owner@example.com", "owner-pass-123")
	res, body := owner.do("POST", "/api/v1/s/shop/admin/import", "name,price\nSugar,1\n", map[string]string{"X-PT-CSRF": "1", "Content-Type": "text/csv"})
	if res.StatusCode != 400 || !strings.Contains(string(body), "missing_column") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	res, _ = owner.do("POST", "/api/v1/s/shop/admin/import", "", map[string]string{"X-PT-CSRF": "1", "Content-Type": "text/csv"})
	if res.StatusCode != 400 {
		t.Fatalf("empty file: %d", res.StatusCode)
	}
}

func TestExports(t *testing.T) {
	e := newEnv(t)
	seed(t, e)
	owner := e.adminClient("owner@example.com", "owner-pass-123")
	res, body := owner.get("/api/v1/s/shop/admin/export.csv")
	if res.StatusCode != 200 || !strings.HasPrefix(string(body), "code,name,size,category,price") || !strings.Contains(string(body), "Sugar") {
		t.Fatalf("csv %d %s", res.StatusCode, body)
	}
	res, body = owner.get("/api/v1/s/shop/admin/export.db")
	if res.StatusCode != 200 || !bytes.HasPrefix(body, []byte("SQLite format 3")) {
		t.Fatalf("db export %d (%d bytes)", res.StatusCode, len(body))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		left := ""
		ents, _ := readDir(e.dir)
		for _, n := range ents {
			if strings.HasPrefix(n, ".export-") {
				left = n
			}
		}
		if left == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("temp export dir left behind: %s", left)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOperatorAPI(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	auth := map[string]string{"Authorization": "Bearer operator-token-for-tests-123456"}
	if res, _ := c.do("GET", "/api/v1/operator/stores", nil, nil); res.StatusCode != 401 {
		t.Fatalf("no token: %d", res.StatusCode)
	}
	if res, _ := c.do("GET", "/api/v1/operator/stores", nil, map[string]string{"Authorization": "Bearer nope"}); res.StatusCode != 401 {
		t.Fatalf("bad token: %d", res.StatusCode)
	}
	res, body := c.do("POST", "/api/v1/operator/stores", map[string]any{"slug": "fresh-mart", "name": "Fresh Mart", "owner_email": "boss@fresh.example"}, auth)
	var out struct {
		Slug          string `json:"slug"`
		OwnerPassword string `json:"owner_password"`
	}
	decode(t, body, &out)
	if res.StatusCode != 201 || out.OwnerPassword == "" {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if res, _ := c.do("POST", "/api/v1/operator/stores", map[string]any{"slug": "fresh-mart", "name": "Dup", "owner_email": "x@y.example"}, auth); res.StatusCode != 409 {
		t.Fatalf("duplicate slug: %d", res.StatusCode)
	}
	if res, _ := c.do("POST", "/api/v1/operator/stores", map[string]any{"slug": "../evil", "name": "Evil", "owner_email": "x@y.example"}, auth); res.StatusCode != 400 {
		t.Fatalf("bad slug: %d", res.StatusCode)
	}
	if res, _ := c.get("/api/v1/s/fresh-mart/home"); res.StatusCode != 200 {
		t.Fatalf("new store should serve immediately: %d", res.StatusCode)
	}
	// the new owner can sign in to THEIR store, and not to another one
	cc := e.client()
	cc.h.Jar = &cookieJar{}
	if res, b := cc.post("/api/v1/s/fresh-mart/admin/login", map[string]string{"email": "boss@fresh.example", "password": out.OwnerPassword}); res.StatusCode != 200 {
		t.Fatalf("owner login: %d %s", res.StatusCode, b)
	}
	if res, _ := cc.get("/api/v1/s/shop/admin/stats"); res.StatusCode != 401 {
		t.Fatal("session for one store must not work on another")
	}
	_, lb := c.do("GET", "/api/v1/operator/stores", nil, auth)
	if !strings.Contains(string(lb), "fresh-mart") || !strings.Contains(string(lb), "shop") {
		t.Fatalf("list %s", lb)
	}
}

func TestOperatorDisabledWithoutToken(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.OperatorToken = "" })
	if res, _ := e.client().do("GET", "/api/v1/operator/stores", nil, map[string]string{"Authorization": "Bearer "}); res.StatusCode != 404 {
		t.Fatalf("operator API must be off by default: %d", res.StatusCode)
	}
}

func TestRateLimits(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.PublicRPS = 1; c.PublicBurst = 5 })
	c := e.client()
	var limited int
	for i := 0; i < 12; i++ {
		if res, _ := c.get("/api/v1/s/shop/home"); res.StatusCode == 429 {
			limited++
			if res.Header.Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
		}
	}
	if limited == 0 {
		t.Fatal("public limiter never triggered")
	}
	e2 := newEnv(t)
	c2 := e2.client()
	var got429 bool
	for i := 0; i < 14; i++ {
		res, _ := c2.post("/api/v1/s/shop/admin/login", map[string]string{"email": fmt.Sprintf("guess%d@example.com", i), "password": "wrong-password"})
		if res.StatusCode == 429 {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("login brute force not throttled")
	}
}

func TestPerAccountThrottle(t *testing.T) {
	e := newEnv(t)
	var got429 bool
	for i := 0; i < 12; i++ {
		c := e.client() // fresh client but same IP: IP limiter is looser than account limiter
		res, _ := c.post("/api/v1/s/shop/admin/login", map[string]string{"email": "owner@example.com", "password": "wrong-password"})
		if res.StatusCode == 429 {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("per-account guessing not throttled")
	}
}

func TestSuccessfulLoginsDoNotCountAgainstThrottle(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 25; i++ { // a shop with many devices signing in
		c := e.client()
		if res, b := c.post("/api/v1/s/shop/admin/login", map[string]string{"email": "owner@example.com", "password": "owner-pass-123"}); res.StatusCode != 200 {
			t.Fatalf("login %d: %d %s", i, res.StatusCode, b)
		}
	}
}

func TestCatalogStream(t *testing.T) {
	e := newEnv(t)
	seed(t, e)
	req, _ := http.NewRequest("GET", e.base+"/api/v1/s/shop/catalog", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "gzip" || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/x-ndjson") {
		t.Fatalf("headers %v", res.Header)
	}
	zr, _ := gzip.NewReader(res.Body)
	sc := bufio.NewScanner(zr)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 4 || !strings.Contains(lines[0], `"full":true`) || !strings.Contains(lines[3], `"end":true`) {
		t.Fatalf("catalog: %v", lines)
	}
}

func TestFullCatalogIsCachedPerVersion(t *testing.T) {
	e := newEnv(t)
	seed(t, e)
	st, rel, _ := e.srv.reg.Acquire("shop")
	defer rel()
	a, err := st.FullCatalogGzip(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := st.FullCatalogGzip(context.Background())
	if &a[0] != &b[0] {
		t.Fatal("same version must reuse the built catalogue")
	}
	st.SetPrice(context.Background(), "t", code("600123456789"), 5600)
	c, _ := st.FullCatalogGzip(context.Background())
	if &a[0] == &c[0] {
		t.Fatal("a new version must rebuild")
	}
	zr, _ := gzip.NewReader(bytes.NewReader(c))
	body, _ := io.ReadAll(zr)
	if !bytes.Contains(body, []byte(`"price":5600`)) {
		t.Fatalf("stale catalogue: %s", body)
	}
}

func TestStaticAndPages(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	for path, want := range map[string]int{"/": 200, "/healthz": 200, "/s/shop/": 200, "/s/shop/admin/": 200, "/s/shop/test-sheet": 200,
		"/s/nosuch/": 404, "/s/shop/manifest.webmanifest": 200, "/sw.js": 200, "/static/shopper/nope.js": 404, "/api/v1/zzz": 404} {
		if res, _ := c.get(path); res.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, res.StatusCode, want)
		}
	}
	if res, _ := c.get("/static/../etc/passwd"); res.StatusCode == 200 {
		t.Error("path traversal served a file")
	}
	req, _ := http.NewRequest("GET", e.base+"/s/shop/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, _ := http.DefaultTransport.RoundTrip(req)
	res.Body.Close()
	if res.Header.Get("ETag") == "" {
		t.Error("static pages need ETags")
	}
	if res, _ := c.get("/sw.js"); res.Header.Get("Service-Worker-Allowed") != "/" {
		t.Error("service worker scope header")
	}
}

func TestManyConcurrentLookups(t *testing.T) {
	e := newEnv(t)
	seed(t, e)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl := &http.Client{Timeout: 5 * time.Second}
			for i := 0; i < 40; i++ {
				res, err := cl.Get(e.base + "/api/v1/s/shop/items/" + raw("600123456789"))
				if err != nil {
					errs <- err.Error()
					return
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode != 200 && res.StatusCode != 429 {
					errs <- fmt.Sprint(res.StatusCode)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for m := range errs {
		t.Error(m)
	}
}

func TestPromoChangesInvalidateItemETag(t *testing.T) {
	e := newEnv(t)
	a, _ := seed(t, e)
	c := e.client()
	adm := e.adminClient("owner@example.com", "owner-pass-123")

	res, _ := c.get("/api/v1/s/shop/items/" + a)
	etag0 := res.Header.Get("ETag")
	if res.StatusCode != 200 || etag0 == "" {
		t.Fatalf("initial lookup: %d %q", res.StatusCode, etag0)
	}

	now := time.Now().Unix()
	pr, pb := adm.post("/api/v1/s/shop/admin/promos", map[string]any{
		"title": "Deal", "starts_at": now - 60, "ends_at": now + 3600, "enabled": true,
		"items": []map[string]any{{"code": a, "price": 4900}},
	})
	if pr.StatusCode != 201 {
		t.Fatalf("create promo: %d %s", pr.StatusCode, pb)
	}
	var pid struct{ ID int64 }
	decode(t, pb, &pid)

	// Revalidating with old ETag must return 200 (not stale 304) with the new offer.
	res, body := c.do("GET", "/api/v1/s/shop/items/"+a, nil, map[string]string{"If-None-Match": etag0})
	if res.StatusCode != 200 {
		t.Fatalf("want 200 after promo create, got %d", res.StatusCode)
	}
	var it store.Item
	decode(t, body, &it)
	if len(it.Promos) != 1 || it.Promos[0].Price != 4900 {
		t.Fatalf("expected promo on item: %+v", it)
	}
	etag1 := res.Header.Get("ETag")

	// Deleting promo must also invalidate ETag.
	dr, _ := adm.do("DELETE", fmt.Sprintf("/api/v1/s/shop/admin/promos/%d", pid.ID), nil, map[string]string{"X-PT-CSRF": "1"})
	if dr.StatusCode != 200 {
		t.Fatalf("delete promo: %d", dr.StatusCode)
	}
	res, body = c.do("GET", "/api/v1/s/shop/items/"+a, nil, map[string]string{"If-None-Match": etag1})
	if res.StatusCode != 200 {
		t.Fatalf("want 200 after promo delete, got %d", res.StatusCode)
	}
	it = store.Item{}
	decode(t, body, &it)
	if len(it.Promos) != 0 {
		t.Fatalf("expected promo removed: %+v", it)
	}
}
