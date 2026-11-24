package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pricetag/internal/barcode"
	"pricetag/internal/store"
)

func deadline(w http.ResponseWriter, d time.Duration) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
}

func setVersion(w http.ResponseWriter, st *store.Store) {
	w.Header().Set("X-PT-V", strconv.FormatInt(st.Version(), 10))
}

// etagMatch implements If-None-Match for a single weak/strong validator.
func etagMatch(r *http.Request, etag string) bool {
	for _, c := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		c = strings.TrimSpace(c)
		if c != "" && strings.TrimPrefix(c, "W/") == strings.TrimPrefix(etag, "W/") {
			return true
		}
	}
	return false
}

// GET /api/v1/s/{slug}/home
func (s *Server) home(w http.ResponseWriter, r *http.Request, st *store.Store) {
	deadline(w, 15*time.Second)
	snap, err := st.Home(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", snap.ETag)
	w.Header().Set("Cache-Control", "no-cache")
	setVersion(w, st)
	if etagMatch(r, snap.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSONBytes(w, r, 200, snap.JSON)
}

// GET /api/v1/s/{slug}/items/{code}
// The hot path: one indexed read, ~200 bytes out, and a 304 (no body) when the
// phone already holds the current revision.
func (s *Server) item(w http.ResponseWriter, r *http.Request, st *store.Store) {
	deadline(w, 10*time.Second)
	code, err := barcode.Canonical(r.PathValue("code"))
	if err != nil {
		writeErr(w, 400, "bad_code", err.Error())
		return
	}
	setVersion(w, st)
	it, err := st.Lookup(r.Context(), code)
	if err == store.ErrNotFound {
		w.Header().Set("Cache-Control", "no-cache")
		writeErr(w, 404, "not_found", "this product is not in the store's price list")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	etag := `W/"` + strconv.FormatInt(it.Rev, 10) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if etagMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, r, 200, it)
}

// GET /api/v1/s/{slug}/search?q=
func (s *Server) search(w http.ResponseWriter, r *http.Request, st *store.Store) {
	deadline(w, 10*time.Second)
	items, err := st.Search(r.Context(), r.URL.Query().Get("q"), 20)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if items == nil {
		items = []*store.Item{}
	}
	w.Header().Set("Cache-Control", "no-store")
	setVersion(w, st)
	writeJSON(w, r, 200, map[string]any{"items": items})
}

// GET /api/v1/s/{slug}/sample?n=  (random products, for the printable test sheet)
func (s *Server) sample(w http.ResponseWriter, r *http.Request, st *store.Store) {
	deadline(w, 10*time.Second)
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	n = max(1, min(n, 40))
	items, total, err := st.ListProducts(r.Context(), "", 0, n)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, p := range items {
		out = append(out, map[string]any{"code": p.Display, "name": p.Name, "size": p.Size, "price": p.Price})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, 200, map[string]any{"items": out, "total": total, "decimals": st.Settings().Decimals, "currency": st.Settings().Currency, "name": st.Settings().Name})
}

// GET /api/v1/s/{slug}/catalog?since=V  -> NDJSON (gzip) for opt-in offline mode
func (s *Server) catalog(w http.ResponseWriter, r *http.Request, st *store.Store) {
	deadline(w, 5*time.Minute)
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Add("Vary", "Accept-Encoding")
	if since <= 0 && acceptsGzip(r) { // a full download: built once per version, shared by every phone
		gz, err := st.FullCatalogGzip(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		h.Set("Content-Encoding", "gzip")
		h.Set("Content-Length", strconv.Itoa(len(gz)))
		w.Write(gz)
		return
	}
	var out interface {
		Write([]byte) (int, error)
	} = w
	var zw *gzip.Writer
	if acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		zw, _ = gzip.NewWriterLevel(w, 4)
		out = zw
	}
	enc := json.NewEncoder(out)
	err := st.StreamCatalog(r.Context(), since, func(v any) error { return enc.Encode(v) })
	if zw != nil {
		zw.Close()
	}
	if err != nil {
		s.log.Warn("catalog stream aborted", "store", st.Slug, "err", err)
	}
}

// GET /api/v1/s/{slug}/events?since=V  -> text/event-stream
//
// One long-lived connection per open app. Frames are tiny: usually the changed
// product(s) themselves, so a price edit reaches a phone in one network hop.
// If a phone was away it sends its last-seen version (query or Last-Event-ID)
// and gets one folded catch-up event first.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.limit(w, s.rlPublic, ip) {
		return
	}
	st, rel, err := s.reg.Acquire(r.PathValue("slug"))
	if err != nil {
		writeErr(w, 404, "no_such_store", "this store does not exist")
		return
	}
	defer rel()

	s.sseMu.Lock()
	if s.sseIP[ip] >= s.cfg.MaxSSEPerIP {
		s.sseMu.Unlock()
		writeErr(w, 429, "too_many_streams", "too many live connections from your network")
		return
	}
	s.sseIP[ip]++
	s.sseMu.Unlock()
	defer func() {
		s.sseMu.Lock()
		if s.sseIP[ip]--; s.sseIP[ip] <= 0 {
			delete(s.sseIP, ip)
		}
		s.sseMu.Unlock()
	}()

	sub := st.Hub.Subscribe(s.cfg.MaxSSEPerStore)
	if sub == nil {
		writeErr(w, 503, "busy", "this store's live feed is at capacity; prices still work without it")
		return
	}
	defer st.Hub.Unsubscribe(sub)

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer
	w.WriteHeader(200)

	write := func(b []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(20 * time.Second))
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	// Last-Event-ID (sent by the browser on automatic reconnects) is newer than
	// the ?since= in the original URL, so it wins.
	since := int64(-1)
	if le := r.Header.Get("Last-Event-ID"); le != "" {
		if n, err := strconv.ParseInt(le, 10, 64); err == nil {
			since = n
		}
	} else if q := r.URL.Query().Get("since"); q != "" {
		if n, err := strconv.ParseInt(q, 10, 64); err == nil {
			since = n
		}
	}

	cur := st.Version()
	// Randomised retry spreads reconnects after a restart (no thundering herd).
	// "hello" deliberately has no id: the browser's Last-Event-ID must only ever
	// advance when a real change event was delivered, never past a catch-up the
	// client has not received yet.
	hello := fmt.Sprintf("retry: %d\nevent: hello\ndata: {\"v\":%d,\"now\":%d}\n\n", 2000+rand.IntN(4000), cur, time.Now().Unix())
	if !write([]byte(hello)) {
		return
	}
	sent := cur
	if since >= 0 && since != cur {
		ev, err := st.CatchUp(r.Context(), since)
		if err != nil {
			return
		}
		if ev != nil {
			if !write(ev.SSE()) {
				return
			}
			sent = max(sent, ev.V)
		}
	}

	hb := time.NewTicker(20 * time.Second)
	defer hb.Stop()
	for {
		select {
		case f, ok := <-sub.C:
			if !ok {
				return // dropped as too slow, or store closing: client reconnects and catches up
			}
			if f.V <= sent {
				continue
			}
			if !write(f.Data) {
				return
			}
			sent = f.V
		case <-hb.C:
			if !write([]byte(": hb\n\n")) {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		}
	}
}

var _ = context.Background
