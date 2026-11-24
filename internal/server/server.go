// Package server is the HTTP layer: public shopper API, server-sent events,
// store-owner admin API, machine ingest for POS integrations, and the
// operator API used to onboard stores.
package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"pricetag/internal/store"
	"pricetag/web"
)

type Config struct {
	DataDir        string
	TrustProxy     bool   // trust X-Forwarded-For / -Proto from a reverse proxy in front
	OperatorToken  string // enables /api/v1/operator/* when >= 24 chars
	MaxSSEPerStore int
	MaxSSEPerIP    int
	AccessLog      bool
	Logger         *slog.Logger
	// PublicRPS is the per-IP request rate for shopper endpoints. Mobile
	// carriers in East Africa put many phones behind one IP, so keep it generous.
	PublicRPS   float64
	PublicBurst int
}

type Server struct {
	cfg    Config
	log    *slog.Logger
	reg    *Registry
	static staticFiles
	mux    *http.ServeMux

	rlPublic, rlCatalog, rlLogin, rlAccount, rlAdmin, rlIngest, rlOperator *limiter

	sseMu sync.Mutex
	sseIP map[string]int

	ctx    context.Context
	cancel context.CancelFunc
	start  time.Time
}

func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.MaxSSEPerStore <= 0 {
		cfg.MaxSSEPerStore = 20000
	}
	if cfg.MaxSSEPerIP <= 0 {
		cfg.MaxSSEPerIP = 100
	}
	if cfg.PublicRPS <= 0 {
		cfg.PublicRPS = 50
	}
	if cfg.PublicBurst <= 0 {
		cfg.PublicBurst = 200
	}
	sweepStaleExports(cfg.DataDir, cfg.Logger)
	st, err := loadStatic(web.FS)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg: cfg, log: cfg.Logger, reg: NewRegistry(cfg.DataDir, cfg.Logger), static: st, mux: http.NewServeMux(),
		rlPublic:   newLimiter(cfg.PublicRPS, cfg.PublicBurst),
		rlCatalog:  newLimiter(1.0/300, 6), // ~12 full downloads/hour/IP
		rlLogin:    newLimiter(1.0/6, 10),  // per IP+store
		rlAccount:  newLimiter(1.0/30, 8),  // per account: slows guessing without hard lock-out
		rlAdmin:    newLimiter(30, 120),
		rlIngest:   newLimiter(1, 20),
		rlOperator: newLimiter(1, 20),
		sseIP:      map[string]int{},
		ctx:        ctx, cancel: cancel, start: time.Now(),
	}
	s.routes()
	go s.reg.Janitor(ctx, time.Minute, 10*time.Minute)
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, l := range []*limiter{s.rlPublic, s.rlCatalog, s.rlLogin, s.rlAccount, s.rlAdmin, s.rlIngest, s.rlOperator} {
					l.gc(15 * time.Minute)
				}
			}
		}
	}()
	return s, nil
}

func (s *Server) Registry() *Registry { return s.reg }

// Shutdown disconnects event streams and closes every store.
func (s *Server) Shutdown() {
	s.cancel()
	s.reg.CloseAll()
}

func (s *Server) Handler() http.Handler {
	return s.recoverer(s.secure(s.mux))
}

// ---- middleware ---------------------------------------------------------------

const csp = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' data: blob:; " +
	"connect-src 'self'; worker-src 'self'; manifest-src 'self'; media-src 'self' blob:; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		// The heavy document-level headers only matter for HTML pages. Leaving them
		// off JSON/asset responses saves ~500 bytes on every price lookup, which is
		// real money on a prepaid data bundle.
		p := r.URL.Path
		if !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/static/") {
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", csp)
			h.Set("Permissions-Policy", "camera=(self), geolocation=(), microphone=(), payment=()")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
		}
		if s.isTLS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if s.cfg.AccessLog || rec.status >= 500 {
			s.log.Info("req", "m", r.Method, "p", r.URL.Path, "st", rec.status, "ms", time.Since(start).Milliseconds(), "ip", s.clientIP(r))
		}
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				s.log.Error("panic", "path", r.URL.Path, "err", v)
				writeErr(w, 500, "internal", "something went wrong")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(c int)           { r.status = c; r.ResponseWriter.WriteHeader(c) }
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter } // keeps Flush/deadlines working

func (s *Server) isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",") // the last entry is the one our own proxy appended
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) limit(w http.ResponseWriter, l *limiter, key string) bool {
	ok, wait := l.allow(key)
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many requests, slow down")
	}
	return ok
}

// ---- response helpers ----------------------------------------------------------

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeErr(w, 500, "internal", "encoding failed")
		return
	}
	writeJSONBytes(w, r, status, b)
}

var gzPool = sync.Pool{New: func() any { zw, _ := gzip.NewWriterLevel(io.Discard, 5); return zw }}

func writeJSONBytes(w http.ResponseWriter, r *http.Request, status int, b []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Add("Vary", "Accept-Encoding")
	if len(b) > 1024 && acceptsGzip(r) {
		var buf bytes.Buffer
		zw := gzPool.Get().(*gzip.Writer)
		zw.Reset(&buf)
		zw.Write(b)
		zw.Close()
		gzPool.Put(zw)
		h.Set("Content-Encoding", "gzip")
		b = buf.Bytes()
	}
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(b)
	}
}

// fail maps domain errors onto HTTP responses; unknown errors are logged and
// reported generically (never leak SQL or paths).
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *store.ValidationError
	switch {
	case errors.As(err, &ve):
		writeErr(w, 400, ve.Code, ve.Msg)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, "not_found", "not found")
	case errors.Is(err, store.ErrExists):
		writeErr(w, 409, "exists", "that already exists")
	case errors.Is(err, store.ErrLastOwner):
		writeErr(w, 409, "last_owner", err.Error())
	case errors.Is(err, context.Canceled), r.Context().Err() != nil:
		// client went away
	default:
		s.log.Error("request failed", "path", r.URL.Path, "err", err)
		writeErr(w, 500, "internal", "something went wrong")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, max int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, 413, "too_large", "request is too large")
		} else {
			writeErr(w, 400, "bad_json", "the request body is not valid for this endpoint")
		}
		return false
	}
	return true
}

// ---- routing ---------------------------------------------------------------------

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.healthz)
	m.HandleFunc("GET /{$}", s.page("shared/index.html"))
	m.HandleFunc("GET /sw.js", s.serviceWorker)
	m.HandleFunc("GET /static/{path...}", s.staticFile)

	m.HandleFunc("GET /s/{slug}/{$}", s.storePage("shopper/index.html"))
	m.HandleFunc("GET /s/{slug}/admin/{$}", s.storePage("admin/index.html"))
	m.HandleFunc("GET /s/{slug}/test-sheet", s.storePage("shared/test-sheet.html"))
	m.HandleFunc("GET /s/{slug}/manifest.webmanifest", s.manifest)

	m.HandleFunc("GET /api/v1/s/{slug}/home", s.public(s.rlPublic, s.home))
	m.HandleFunc("GET /api/v1/s/{slug}/items/{code}", s.public(s.rlPublic, s.item))
	m.HandleFunc("GET /api/v1/s/{slug}/search", s.public(s.rlPublic, s.search))
	m.HandleFunc("GET /api/v1/s/{slug}/sample", s.public(s.rlPublic, s.sample))
	m.HandleFunc("GET /api/v1/s/{slug}/catalog", s.public(s.rlCatalog, s.catalog))
	m.HandleFunc("GET /api/v1/s/{slug}/events", s.events)

	m.HandleFunc("POST /api/v1/s/{slug}/ingest", s.ingest)

	s.adminRoutes()
	s.operatorRoutes()

	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, 404, "not_found", "no such endpoint")
			return
		}
		http.NotFound(w, r)
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, 200, map[string]any{"ok": true, "stores_open": s.reg.OpenCount(), "uptime_s": int(time.Since(s.start).Seconds())})
}

func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a := s.static[name]; a != nil {
			a.serve(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func (s *Server) staticFile(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if strings.Contains(p, "..") {
		http.NotFound(w, r)
		return
	}
	if a := s.static[p]; a != nil {
		a.serve(w, r)
		return
	}
	http.NotFound(w, r)
}

// The service worker must be served from the origin root to control /s/*.
func (s *Server) serviceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Service-Worker-Allowed", "/")
	if a := s.static["shared/sw.js"]; a != nil {
		a.serve(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) storePage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		if !s.reg.Exists(slug) {
			http.Error(w, "This store does not exist.", http.StatusNotFound)
			return
		}
		s.page(name)(w, r)
	}
}

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	st, rel, err := s.reg.Acquire(slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rel()
	name := st.Settings().Name
	short := name
	if len([]rune(short)) > 12 {
		short = string([]rune(short)[:12])
	}
	m := map[string]any{
		"name": name + " prices", "short_name": short, "id": "/s/" + slug + "/", "start_url": "/s/" + slug + "/",
		"scope": "/s/" + slug + "/", "display": "standalone", "orientation": "portrait",
		"background_color": "#ffffff", "theme_color": "#0b6b4f",
		"icons": []map[string]string{
			{"src": "/static/shared/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any"},
			{"src": "/static/shared/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any"},
			{"src": "/static/shared/icon-maskable-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"},
		},
	}
	b, _ := json.Marshal(m)
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// public wraps a shopper-facing handler: per-IP rate limit, store lookup,
// short write deadline.
func (s *Server) public(rl *limiter, fn func(http.ResponseWriter, *http.Request, *store.Store)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limit(w, rl, s.clientIP(r)) {
			return
		}
		st, rel, err := s.reg.Acquire(r.PathValue("slug"))
		if err != nil {
			writeErr(w, 404, "no_such_store", "this store does not exist")
			return
		}
		defer rel()
		fn(w, r, st)
	}
}

// sweepStaleExports removes temp dirs left by an export that was interrupted
// (crash, kill) so they cannot accumulate on the data volume.
func sweepStaleExports(dir string, log *slog.Logger) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".export-") {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
				os.RemoveAll(filepath.Join(dir, e.Name()))
				log.Info("removed stale export dir", "dir", e.Name())
			}
		}
	}
}
