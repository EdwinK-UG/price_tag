package server

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pricetag/internal/auth"
	"pricetag/internal/barcode"
	"pricetag/internal/store"
)

type adminFn func(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User)

func cookieName(slug string) string { return "pt_" + slug }

func (s *Server) sessionCookie(r *http.Request, slug string) string {
	if c, err := r.Cookie(cookieName(slug)); err == nil {
		return c.Value
	}
	return ""
}

// sameOrigin rejects cross-site browser requests. Cookies are SameSite=Strict
// as well; this is the belt to that pair of braces.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // non-browser clients (curl) send no Origin
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// admin wraps a store-owner endpoint: rate limit, session check, CSRF guard on
// writes, role check.
func (s *Server) admin(role string, fn adminFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limit(w, s.rlAdmin, s.clientIP(r)) {
			return
		}
		slug := r.PathValue("slug")
		st, rel, err := s.reg.Acquire(slug)
		if err != nil {
			writeErr(w, 404, "no_such_store", "this store does not exist")
			return
		}
		defer rel()
		deadline(w, 60*time.Second)
		w.Header().Set("Cache-Control", "no-store")
		u, err := st.SessionUser(r.Context(), s.sessionCookie(r, slug))
		if err != nil {
			writeErr(w, 401, "unauthorized", "please sign in")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !sameOrigin(r) || r.Header.Get("X-PT-CSRF") != "1" {
				writeErr(w, 403, "csrf", "cross-site request blocked")
				return
			}
		}
		if role == "owner" && u.Role != "owner" {
			writeErr(w, 403, "forbidden", "only a store owner can do this")
			return
		}
		fn(w, r, st, u)
	}
}

func (s *Server) adminRoutes() {
	m := s.mux
	p := "/api/v1/s/{slug}/admin/"
	m.HandleFunc("POST "+p+"login", s.login)
	m.HandleFunc("POST "+p+"logout", s.logout)
	m.HandleFunc("GET "+p+"me", s.whoami)
	m.HandleFunc("POST "+p+"password", s.admin("", s.changePassword))
	m.HandleFunc("GET "+p+"stats", s.admin("", func(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
		sts, err := st.Stats(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, r, 200, sts)
	}))

	m.HandleFunc("GET "+p+"products", s.admin("", s.listProducts))
	m.HandleFunc("PUT "+p+"products/{code}", s.admin("", s.putProduct))
	m.HandleFunc("PUT "+p+"products/{code}/price", s.admin("", s.putPrice))
	m.HandleFunc("DELETE "+p+"products/{code}", s.admin("", s.deleteProduct))

	m.HandleFunc("GET "+p+"promos", s.admin("", s.listPromos))
	m.HandleFunc("GET "+p+"promos/{id}", s.admin("", s.getPromo))
	m.HandleFunc("POST "+p+"promos", s.admin("", s.createPromo))
	m.HandleFunc("PUT "+p+"promos/{id}", s.admin("", s.updatePromo))
	m.HandleFunc("DELETE "+p+"promos/{id}", s.admin("", s.deletePromo))

	m.HandleFunc("POST "+p+"import", s.admin("", func(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
		s.doImport(w, r, st, u.Email)
	}))
	m.HandleFunc("GET "+p+"export.csv", s.admin("", s.exportCSV))
	m.HandleFunc("GET "+p+"export.db", s.admin("owner", s.exportDB))
	m.HandleFunc("GET "+p+"history", s.admin("", s.history))

	m.HandleFunc("GET "+p+"settings", s.admin("", func(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
		writeJSON(w, r, 200, st.Settings())
	}))
	m.HandleFunc("PUT "+p+"settings", s.admin("owner", s.putSettings))

	m.HandleFunc("GET "+p+"users", s.admin("owner", s.listUsers))
	m.HandleFunc("POST "+p+"users", s.admin("owner", s.createUser))
	m.HandleFunc("PATCH "+p+"users/{id}", s.admin("owner", s.patchUser))

	m.HandleFunc("GET "+p+"tokens", s.admin("owner", s.listTokens))
	m.HandleFunc("POST "+p+"tokens", s.admin("owner", s.createToken))
	m.HandleFunc("DELETE "+p+"tokens/{id}", s.admin("owner", s.revokeToken))
}

// ---- auth -------------------------------------------------------------------------

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	ip := s.clientIP(r)
	ipKey := slug + "|" + ip
	if !s.limit(w, s.rlLogin, ipKey) {
		return
	}
	if !sameOrigin(r) || r.Header.Get("X-PT-CSRF") != "1" {
		writeErr(w, 403, "csrf", "cross-site request blocked")
		return
	}
	var in struct{ Email, Password string }
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	acctKey := slug + "|" + strings.ToLower(strings.TrimSpace(in.Email))
	if !s.limit(w, s.rlAccount, acctKey) {
		return
	}
	st, rel, err := s.reg.Acquire(slug)
	if err != nil {
		writeErr(w, 404, "no_such_store", "this store does not exist")
		return
	}
	defer rel()
	w.Header().Set("Cache-Control", "no-store")
	u, err := st.Authenticate(r.Context(), in.Email, in.Password)
	if err == store.ErrBadLogin {
		s.log.Info("login failed", "store", slug, "ip", ip)
		writeErr(w, 401, "bad_login", "wrong e-mail or password")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	s.rlLogin.refund(ipKey) // only failures count against the throttles
	s.rlAccount.refund(acctKey)
	tok, exp, err := st.CreateSession(r.Context(), u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName(slug), Value: tok, Path: "/", Expires: exp, HttpOnly: true, Secure: s.isTLS(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, r, 200, map[string]any{"user": u})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !sameOrigin(r) || r.Header.Get("X-PT-CSRF") != "1" {
		writeErr(w, 403, "csrf", "cross-site request blocked")
		return
	}
	if st, rel, err := s.reg.Acquire(slug); err == nil {
		defer rel()
		if tok := s.sessionCookie(r, slug); tok != "" {
			st.DeleteSession(r.Context(), tok)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName(slug), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.isTLS(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

// whoami answers 200 either way ({"user":null} when signed out) so a signed-out
// visit to the admin page is not logged as an error in the browser console.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	if !s.limit(w, s.rlAdmin, s.clientIP(r)) {
		return
	}
	slug := r.PathValue("slug")
	st, rel, err := s.reg.Acquire(slug)
	if err != nil {
		writeErr(w, 404, "no_such_store", "this store does not exist")
		return
	}
	defer rel()
	w.Header().Set("Cache-Control", "no-store")
	u, err := st.SessionUser(r.Context(), s.sessionCookie(r, slug))
	if err != nil {
		writeJSON(w, r, 200, map[string]any{"user": nil})
		return
	}
	writeJSON(w, r, 200, map[string]any{"user": u, "settings": st.Settings(), "version": st.Version()})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	if _, err := st.Authenticate(r.Context(), u.Email, in.Current); err != nil {
		writeErr(w, 403, "bad_password", "your current password is wrong")
		return
	}
	if err := st.UpdateUser(r.Context(), u.Email, u.ID, store.UserPatch{Password: &in.New}); err != nil {
		s.fail(w, r, err)
		return
	}
	// all sessions were revoked; sign this browser in again
	tok, exp, err := st.CreateSession(r.Context(), u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName(st.Slug), Value: tok, Path: "/", Expires: exp, HttpOnly: true, Secure: s.isTLS(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

// ---- products ------------------------------------------------------------------------

func (s *Server) listProducts(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	q := r.URL.Query()
	off, _ := strconv.Atoi(q.Get("offset"))
	lim, _ := strconv.Atoi(q.Get("limit"))
	if lim == 0 {
		lim = 50
	}
	items, total, err := st.ListProducts(r.Context(), q.Get("q"), off, lim)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"items": items, "total": total})
}

type productBody struct {
	Name     string `json:"name"`
	Size     string `json:"size"`
	Category string `json:"category"`
	Price    *int64 `json:"price"`
}

func (s *Server) putProduct(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	var in productBody
	if !decodeJSON(w, r, &in, 16<<10) {
		return
	}
	if in.Price == nil {
		writeErr(w, 400, "price", "price is required")
		return
	}
	code, status, err := st.UpsertProduct(r.Context(), u.Email, store.ProductInput{Code: r.PathValue("code"), Name: in.Name, Size: in.Size, Category: in.Category, Price: *in.Price})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"code": code, "display": barcode.Display(code), "status": status})
}

func (s *Server) putPrice(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	var in struct {
		Price *int64 `json:"price"`
	}
	if !decodeJSON(w, r, &in, 1<<10) {
		return
	}
	if in.Price == nil {
		writeErr(w, 400, "price", "price is required")
		return
	}
	status, err := st.SetPrice(r.Context(), u.Email, r.PathValue("code"), *in.Price)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"status": status, "version": st.Version()})
}

func (s *Server) deleteProduct(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	if err := st.DeleteProduct(r.Context(), u.Email, r.PathValue("code")); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

// ---- promotions -----------------------------------------------------------------------

func promoID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 404, "not_found", "not found")
		return 0, false
	}
	return id, true
}

func (s *Server) listPromos(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	ps, err := st.ListPromos(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"promos": ps})
}

func (s *Server) getPromo(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	id, ok := promoID(w, r)
	if !ok {
		return
	}
	p, err := st.GetPromo(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, p)
}

func (s *Server) createPromo(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	var in store.PromoInput
	if !decodeJSON(w, r, &in, 256<<10) {
		return
	}
	id, err := st.CreatePromo(r.Context(), u.Email, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 201, map[string]any{"id": id})
}

func (s *Server) updatePromo(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	id, ok := promoID(w, r)
	if !ok {
		return
	}
	var in store.PromoInput
	if !decodeJSON(w, r, &in, 256<<10) {
		return
	}
	if err := st.UpdatePromo(r.Context(), u.Email, id, in); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

func (s *Server) deletePromo(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	id, ok := promoID(w, r)
	if !ok {
		return
	}
	if err := st.DeletePromo(r.Context(), u.Email, id); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

// ---- import / export ---------------------------------------------------------------------

const maxImportBytes = 32 << 20

// doImport accepts a CSV (default) or {"products":[...]} JSON body. With
// ?dry_run=1 it reports what WOULD change without saving.
func (s *Server) doImport(w http.ResponseWriter, r *http.Request, st *store.Store, actor string) {
	deadline(w, 3*time.Minute)
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	dry := r.URL.Query().Get("dry_run") == "1"
	var rows []store.ImportRow
	var perrs []store.RowError
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "application/json") {
		var in struct {
			Products []store.ProductInput `json:"products"`
		}
		if !decodeJSON(w, r, &in, maxImportBytes) {
			return
		}
		for i, p := range in.Products {
			rows = append(rows, store.ImportRow{Row: i + 1, In: p})
		}
	} else {
		var err error
		rows, perrs, err = store.ParseCSV(r.Body, st.Settings().Decimals)
		if err != nil {
			if strings.Contains(err.Error(), "too large") {
				writeErr(w, 413, "too_large", "the file is too large")
				return
			}
			s.fail(w, r, err)
			return
		}
	}
	res, err := st.ImportProducts(r.Context(), actor, rows, dry)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res.Errors = append(append([]store.RowError{}, perrs...), res.Errors...)
	if len(res.Errors) > 200 { // keep responses small; counts stay exact
		res.Errors = res.Errors[:200]
	}
	res.Total += len(perrs)
	writeJSON(w, r, 200, res)
}

func (s *Server) exportCSV(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	deadline(w, 2*time.Minute)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+st.Slug+`-prices-`+time.Now().Format("2006-01-02")+`.csv"`)
	if err := st.ExportCSV(r.Context(), w); err != nil {
		s.log.Warn("export failed", "store", st.Slug, "err", err)
	}
}

// exportDB hands the owner their entire data as a SQLite file (opens with any
// SQLite tool). This is the "you own your data" promise made concrete.
func (s *Server) exportDB(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	deadline(w, 5*time.Minute)
	dir, err := os.MkdirTemp(s.cfg.DataDir, ".export-")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer os.RemoveAll(dir)
	dst := filepath.Join(dir, st.Slug+".db")
	if err := st.Backup(r.Context(), dst); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="`+st.Slug+`-`+time.Now().Format("2006-01-02")+`.db"`)
	http.ServeFile(w, r, dst)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	lim, _ := strconv.Atoi(q.Get("limit"))
	ref := q.Get("ref")
	if q.Get("kind") == "product" && ref != "" {
		if c, err := barcode.Canonical(ref); err == nil {
			ref = c
		}
	}
	h, err := st.ListHistory(r.Context(), q.Get("kind"), ref, before, lim)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"entries": h})
}

// ---- settings, team, integrations ----------------------------------------------------------

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	var in store.Settings
	if !decodeJSON(w, r, &in, 8<<10) {
		return
	}
	if err := st.UpdateSettings(r.Context(), u.Email, in); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, st.Settings())
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	us, err := st.ListUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"users": us})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	var in struct{ Email, Name, Role, Password string }
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	generated := ""
	if in.Password == "" {
		generated = auth.RandomPassword(12)
		in.Password = generated
	}
	nu, err := st.CreateUser(in.Email, in.Name, in.Role, in.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp := map[string]any{"user": nu}
	if generated != "" {
		resp["password"] = generated // shown once
	}
	writeJSON(w, r, 201, resp)
}

func (s *Server) patchUser(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 404, "not_found", "not found")
		return
	}
	var p store.UserPatch
	if !decodeJSON(w, r, &p, 4<<10) {
		return
	}
	if err := st.UpdateUser(r.Context(), u.Email, id, p); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	ts, err := st.ListTokens(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"tokens": ts})
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	var in struct{ Name string }
	if !decodeJSON(w, r, &in, 1<<10) {
		return
	}
	tok, id, err := st.CreateToken(r.Context(), u.Email, in.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 201, map[string]any{"id": id, "token": tok}) // shown once
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 404, "not_found", "not found")
		return
	}
	if err := st.RevokeToken(r.Context(), u.Email, id); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

// ---- machine ingest (POS connectors) ---------------------------------------------------------

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// POST /api/v1/s/{slug}/ingest  (Authorization: Bearer ptk_...)
// The one door a POS/ERP connector needs. Send the whole price file as often as
// you like: unchanged rows cost nothing and only real changes reach phones.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	if !s.limit(w, s.rlIngest, s.clientIP(r)) {
		return
	}
	st, rel, err := s.reg.Acquire(r.PathValue("slug"))
	if err != nil {
		writeErr(w, 404, "no_such_store", "this store does not exist")
		return
	}
	defer rel()
	w.Header().Set("Cache-Control", "no-store")
	_, name, err := st.VerifyToken(r.Context(), bearer(r))
	if err != nil {
		writeErr(w, 401, "unauthorized", "missing or invalid integration key")
		return
	}
	s.doImport(w, r, st, "api:"+name)
}

// ---- operator --------------------------------------------------------------------------------

func (s *Server) operatorRoutes() {
	s.mux.HandleFunc("POST /api/v1/operator/stores", s.operator(s.opCreateStore))
	s.mux.HandleFunc("GET /api/v1/operator/stores", s.operator(s.opListStores))
}

func (s *Server) operator(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if len(s.cfg.OperatorToken) < 24 {
			writeErr(w, 404, "not_found", "no such endpoint")
			return
		}
		if !s.limit(w, s.rlOperator, s.clientIP(r)) {
			return
		}
		if subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.cfg.OperatorToken)) != 1 {
			writeErr(w, 401, "unauthorized", "invalid operator token")
			return
		}
		fn(w, r)
	}
}

func (s *Server) opListStores(w http.ResponseWriter, r *http.Request) {
	l, err := s.reg.List()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"stores": l})
}

func (s *Server) opCreateStore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug          string `json:"slug"`
		Name          string `json:"name"`
		Currency      string `json:"currency"`
		Decimals      *int   `json:"decimals"`
		TZOffset      *int   `json:"tz_offset"`
		OwnerEmail    string `json:"owner_email"`
		OwnerName     string `json:"owner_name"`
		OwnerPassword string `json:"owner_password"`
	}
	if !decodeJSON(w, r, &in, 8<<10) {
		return
	}
	set := store.DefaultSettings(in.Name)
	if in.Currency != "" {
		set.Currency = in.Currency
	}
	if in.Decimals != nil {
		set.Decimals = *in.Decimals
	}
	if in.TZOffset != nil {
		set.TZOffset = *in.TZOffset
	}
	pw, generated := in.OwnerPassword, false
	if pw == "" {
		pw, generated = auth.RandomPassword(14), true
	}
	if in.OwnerName == "" {
		in.OwnerName = "Owner"
	}
	st, err := store.Create(s.cfg.DataDir, in.Slug, set, in.OwnerEmail, in.OwnerName, pw)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.reg.Adopt(st); err != nil {
		st.Close()
	}
	resp := map[string]any{"slug": in.Slug, "url": "/s/" + in.Slug + "/", "admin_url": "/s/" + in.Slug + "/admin/", "owner_email": in.OwnerEmail}
	if generated {
		resp["owner_password"] = pw // shown once
	}
	writeJSON(w, r, 201, resp)
}
