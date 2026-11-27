package server

import (
	"encoding/csv"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pricetag/internal/auth"
	"pricetag/internal/platform"
	"pricetag/internal/store"
)

const shopperCookie = "pt_shopper"

func (s *Server) accountRoutes() {
	m := s.mux
	m.HandleFunc("GET /signup", s.page("shared/signup.html"))
	m.HandleFunc("GET /operator/{$}", s.page("operator/index.html"))
	m.HandleFunc("GET /u/{token}", s.unsubscribe)
	m.HandleFunc("POST /api/v1/signup", s.signup)

	p := "/api/v1/s/{slug}/account"
	m.HandleFunc("GET "+p, s.shopperRoute(false, s.accountState))
	m.HandleFunc("POST "+p+"/start", s.shopperRoute(false, s.accountStart))
	m.HandleFunc("POST "+p+"/verify", s.shopperRoute(false, s.accountVerify))
	m.HandleFunc("POST "+p+"/profile", s.shopperRoute(true, s.accountProfile))
	m.HandleFunc("POST "+p+"/join", s.shopperRoute(true, s.accountJoin))
	m.HandleFunc("POST "+p+"/marketing", s.shopperRoute(true, s.accountMarketing))
	m.HandleFunc("POST "+p+"/leave", s.shopperRoute(true, s.accountLeave))
	m.HandleFunc("GET "+p+"/shops", s.shopperRoute(true, s.accountShops))
	m.HandleFunc("POST "+p+"/logout", s.shopperRoute(false, s.accountLogout))
	m.HandleFunc("POST "+p+"/delete", s.shopperRoute(true, s.accountDelete))

	a := "/api/v1/s/{slug}/admin/"
	m.HandleFunc("GET "+a+"customers", s.admin("owner", s.customers))
	m.HandleFunc("GET "+a+"customers.csv", s.admin("owner", s.customersCSV))
	m.HandleFunc("GET "+a+"messages", s.admin("owner", s.messages))
	m.HandleFunc("POST "+a+"messages", s.admin("owner", s.sendMessage))
	m.HandleFunc("POST "+a+"forgot", s.forgotPassword)
	m.HandleFunc("POST "+a+"setup", s.setupPassword)

	o := "/api/v1/operator/"
	m.HandleFunc("GET "+o+"status", s.operator(s.opStatus))
	m.HandleFunc("GET "+o+"applications", s.operator(s.opApplications))
	m.HandleFunc("POST "+o+"applications/{id}/approve", s.operator(s.opApprove))
	m.HandleFunc("POST "+o+"applications/{id}/reject", s.operator(s.opReject))
	m.HandleFunc("GET "+o+"shops", s.operator(s.opShops))
	m.HandleFunc("PUT "+o+"shops/{slug}/limits", s.operator(s.opLimits))
	m.HandleFunc("GET "+o+"outbox", s.operator(s.opOutbox))
}

// baseURL is the public address used in links inside messages.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if s.isTLS(r) {
		scheme = "https"
	}
	host := r.Host
	if s.cfg.TrustProxy {
		if fh := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); fh != "" {
			host = fh
		}
	}
	return scheme + "://" + host
}

func (s *Server) platformFail(w http.ResponseWriter, r *http.Request, err error) {
	var ie *platform.InputError
	switch {
	case errors.As(err, &ie):
		writeErr(w, 400, ie.Code, ie.Msg)
	case errors.Is(err, platform.ErrBadCode):
		writeErr(w, 400, "bad_code", err.Error())
	case errors.Is(err, platform.ErrTooMany):
		writeErr(w, 429, "too_many", err.Error())
	case errors.Is(err, platform.ErrContactInUse):
		writeErr(w, 409, "contact_in_use", err.Error())
	case errors.Is(err, platform.ErrChannelUnavailable):
		writeErr(w, 409, "channel_unavailable", err.Error())
	case errors.Is(err, platform.ErrNotMember):
		writeErr(w, 409, "not_member", err.Error())
	case errors.Is(err, platform.ErrNotFound):
		writeErr(w, 404, "not_found", "not found")
	default:
		s.fail(w, r, err)
	}
}

// ---- shopper accounts -------------------------------------------------------------------

type shopperFn func(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper)

// shopperRoute: rate limit, shop lookup, CSRF guard on writes, optional sign-in.
func (s *Server) shopperRoute(needSignIn bool, fn shopperFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limit(w, s.rlAdmin, s.clientIP(r)) {
			return
		}
		st, rel, err := s.reg.Acquire(r.PathValue("slug"))
		if err != nil {
			writeErr(w, 404, "no_such_store", "this store does not exist")
			return
		}
		defer rel()
		deadline(w, 20*time.Second)
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && (!s.sameOrigin(r) || r.Header.Get("X-PT-CSRF") != "1") {
			writeErr(w, 403, "csrf", "cross-site request blocked")
			return
		}
		var sh *platform.Shopper
		if c, err := r.Cookie(shopperCookie); err == nil {
			if sh, err = s.plat.SessionShopper(r.Context(), c.Value); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		if needSignIn && sh == nil {
			writeErr(w, 401, "signed_out", "please sign in")
			return
		}
		fn(w, r, st, sh)
	}
}

func (s *Server) accountState(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper) {
	ch, err := s.plat.Channels(r.Context(), st.Slug)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"shopper": nil, "member": nil, "channels": ch, "shop": st.Settings().Name}
	if sh != nil {
		out["shopper"] = sh
		m, err := s.plat.Membership(r.Context(), sh.ID, st.Slug)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if m != nil {
			out["member"] = m
		}
	}
	writeJSON(w, r, 200, out)
}

func (s *Server) accountStart(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper) {
	if !s.limit(w, s.rlCodeSend, s.clientIP(r)) {
		return
	}
	var in struct{ Contact, Via string }
	if !decodeJSON(w, r, &in, 2<<10) {
		return
	}
	c, err := s.plat.NormalizeContact(in.Contact)
	if err != nil {
		s.platformFail(w, r, err)
		return
	}
	if err := s.plat.StartSignIn(r.Context(), st.Slug, st.Settings().Name, c, in.Via); err != nil {
		s.platformFail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"sent": true, "to": platform.Mask(c.Value), "contact": c})
}

func (s *Server) accountVerify(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper) {
	if !s.limit(w, s.rlCodeVerify, s.clientIP(r)) {
		return
	}
	var in struct{ Contact, Code string }
	if !decodeJSON(w, r, &in, 2<<10) {
		return
	}
	c, err := s.plat.NormalizeContact(in.Contact)
	if err != nil {
		s.platformFail(w, r, err)
		return
	}
	var cur int64
	if sh != nil {
		cur = sh.ID
	}
	got, created, err := s.plat.VerifySignIn(r.Context(), c, in.Code, cur)
	if err != nil {
		s.platformFail(w, r, err)
		return
	}
	if sh == nil {
		tok, exp, err := s.plat.CreateSession(r.Context(), got.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: shopperCookie, Value: tok, Path: "/", Expires: exp, HttpOnly: true, Secure: s.isTLS(r), SameSite: http.SameSiteStrictMode})
	}
	m, _ := s.plat.Membership(r.Context(), got.ID, st.Slug)
	writeJSON(w, r, 200, map[string]any{"shopper": got, "created": created, "member": m})
}

func (s *Server) accountProfile(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper) {
	var in struct{ Name, Channel string }
	if !decodeJSON(w, r, &in, 2<<10) {
		return
	}
	got, err := s.plat.UpdateProfile(r.Context(), sh.ID, in.Name, in.Channel)
	if err != nil {
		s.platformFail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"shopper": got})
}

func (s *Server) accountJoin(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper) {
	var in struct {
		Marketing bool `json:"marketing"`
	}
	if !decodeJSON(w, r, &in, 1<<10) {
		return
	}
	if err := s.plat.Join(r.Context(), sh.ID, st.Slug, in.Marketing); err != nil {
		s.fail(w, r, err)
		return
	}
	m, _ := s.plat.Membership(r.Context(), sh.ID, st.Slug)
	writeJSON(w, r, 200, map[string]any{"member": m})
}

func (s *Server) accountMarketing(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper) {
	var in struct {
		On bool `json:"on"`
	}
	if !decodeJSON(w, r, &in, 1<<10) {
		return
	}
	if err := s.plat.SetMarketing(r.Context(), sh.ID, st.Slug, in.On); err != nil {
		s.platformFail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

func (s *Server) accountLeave(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper) {
	if err := s.plat.Leave(r.Context(), sh.ID, st.Slug); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

// accountShops lists every shop this shopper has joined (with names), so they
// can see and withdraw what they share, from any shop's page.
func (s *Server) accountShops(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper) {
	ms, err := s.plat.Memberships(r.Context(), sh.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type shop struct {
		platform.Membership
		Name string `json:"name"`
	}
	out := []shop{}
	for _, m := range ms {
		name := m.Slug
		if other, rel, err := s.reg.Acquire(m.Slug); err == nil {
			name = other.Settings().Name
			rel()
		}
		out = append(out, shop{m, name})
	}
	writeJSON(w, r, 200, map[string]any{"shops": out})
}

func (s *Server) accountLogout(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper) {
	if c, err := r.Cookie(shopperCookie); err == nil {
		s.plat.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: shopperCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.isTLS(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

func (s *Server) accountDelete(w http.ResponseWriter, r *http.Request, st *store.Store, sh *platform.Shopper) {
	if err := s.plat.DeleteShopper(r.Context(), sh.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: shopperCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.isTLS(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

// GET /u/{token}: one-tap unsubscribe from a shop's messages (link in every message).
func (s *Server) unsubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.limit(w, s.rlPublic, s.clientIP(r)) {
		return
	}
	slug, err := s.plat.Unsubscribe(r.Context(), r.PathValue("token"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	msg := "This link is not valid."
	if err == nil {
		name := slug
		if st, rel, err := s.reg.Acquire(slug); err == nil {
			name = st.Settings().Name
			rel()
		}
		msg = "Done. " + name + " will not send you offers any more. You can turn them back on from the shop's page in Price Tag."
	} else {
		w.WriteHeader(404)
	}
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Price Tag</title><link rel="stylesheet" href="/static/shared/landing.css"></head><body><main><h1>Price Tag</h1><p>%s</p></main></body></html>`, html.EscapeString(msg))
}

// ---- shop sign-up -----------------------------------------------------------------------

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	if !s.limit(w, s.rlSignup, s.clientIP(r)) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !s.sameOrigin(r) || r.Header.Get("X-PT-CSRF") != "1" {
		writeErr(w, 403, "csrf", "cross-site request blocked")
		return
	}
	var in struct {
		platform.Application
		Website string `json:"website"` // honeypot: people never fill it in
	}
	if !decodeJSON(w, r, &in, 8<<10) {
		return
	}
	if in.Website != "" {
		writeJSON(w, r, 201, map[string]any{"ok": true})
		return
	}
	in.Application.Slug = strings.ToLower(strings.TrimSpace(in.Application.Slug))
	if s.reg.Exists(in.Application.Slug) {
		writeErr(w, 409, "slug_taken", "that shop address is already taken; choose another")
		return
	}
	id, err := s.plat.Submit(r.Context(), in.Application, store.ValidSlug)
	if err != nil {
		s.platformFail(w, r, err)
		return
	}
	if s.cfg.OperatorEmail != "" {
		s.plat.QueueEmail(r.Context(), "", "application", s.cfg.OperatorEmail, "New shop application: "+in.ShopName,
			fmt.Sprintf("%s (%s) asked to join Price Tag as %q.\nReview it: %s/operator/", in.OwnerName, in.OwnerEmail, in.ShopName, s.baseURL(r)))
	}
	writeJSON(w, r, 201, map[string]any{"ok": true, "id": id})
}

// ---- shop owner: customers and messages ----------------------------------------------

func (s *Server) customers(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	ms, err := s.plat.Members(r.Context(), st.Slug)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	counts := map[string]int{"total": len(ms), "offers": 0, "sms": 0, "whatsapp": 0, "email": 0}
	for _, m := range ms {
		if m.Marketing {
			counts["offers"]++
			counts[m.Channel]++
		}
	}
	writeJSON(w, r, 200, map[string]any{"customers": ms, "counts": counts})
}

func (s *Server) customersCSV(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	ms, err := s.plat.Members(r.Context(), st.Slug)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+st.Slug+`-customers.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"name", "contact", "channel", "wants_offers", "joined"})
	guard := func(v string) string {
		if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
			return "'" + v
		}
		return v
	}
	for _, m := range ms {
		cw.Write([]string{guard(m.Name), guard(m.Contact), m.Channel, strconv.FormatBool(m.Marketing), time.Unix(m.JoinedAt, 0).UTC().Format("2006-01-02")})
	}
	cw.Flush()
	s.audit(st, r, u.Email, "customers", "Customer list downloaded")
}

func (s *Server) audit(st *store.Store, r *http.Request, actor, ref, summary string) {
	st.Audit(r.Context(), actor, "customers", ref, summary)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	list, err := s.plat.Broadcasts(r.Context(), st.Slug)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	us, err := s.plat.Usage(r.Context(), st.Slug)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"messages": list, "usage": us, "delivering": s.plat.Delivering(), "max_len": platform.MaxBroadcastLen, "per_day": platform.MaxBroadcastsPerDay})
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	var in struct {
		Text   string `json:"text"`
		DryRun bool   `json:"dry_run"`
	}
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	plan, err := s.plat.Broadcast(r.Context(), st.Slug, st.Settings().Name, u.Email, in.Text, s.baseURL(r), in.DryRun)
	if err != nil {
		var ie *platform.InputError
		if errors.As(err, &ie) && plan != nil {
			w.Header().Set("Content-Type", "application/json")
			writeJSON(w, r, 400, map[string]any{"error": ie.Code, "message": ie.Msg, "plan": plan})
			return
		}
		s.platformFail(w, r, err)
		return
	}
	if !in.DryRun {
		st.Audit(r.Context(), u.Email, "customers", fmt.Sprint(plan.ID), fmt.Sprintf("Message sent to %d customers", plan.Queued))
	}
	writeJSON(w, r, 200, plan)
}

// ---- owner password links -------------------------------------------------------------

func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !s.limit(w, s.rlLogin, slug+"|"+s.clientIP(r)) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !s.sameOrigin(r) || r.Header.Get("X-PT-CSRF") != "1" {
		writeErr(w, 403, "csrf", "cross-site request blocked")
		return
	}
	var in struct{ Email string }
	if !decodeJSON(w, r, &in, 1<<10) {
		return
	}
	st, rel, err := s.reg.Acquire(slug)
	if err != nil {
		writeErr(w, 404, "no_such_store", "this store does not exist")
		return
	}
	defer rel()
	// Same answer whether or not the address exists.
	if u, err := st.UserByEmail(r.Context(), in.Email); err == nil {
		if tok, err := st.CreatePasswordToken(r.Context(), u.ID, "reset", 2*time.Hour); err == nil {
			link := s.baseURL(r) + "/s/" + slug + "/admin/#setup=" + tok
			s.plat.QueueEmail(r.Context(), slug, "password", u.Email, "Reset your Price Tag password",
				"Someone asked to reset the password for "+u.Email+" at "+st.Settings().Name+".\n\nChoose a new password (link valid for 2 hours):\n"+link+"\n\nIf it was not you, ignore this e-mail.")
		}
	}
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

func (s *Server) setupPassword(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !s.limit(w, s.rlLogin, slug+"|"+s.clientIP(r)) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !s.sameOrigin(r) || r.Header.Get("X-PT-CSRF") != "1" {
		writeErr(w, 403, "csrf", "cross-site request blocked")
		return
	}
	var in struct{ Token, Password string }
	if !decodeJSON(w, r, &in, 2<<10) {
		return
	}
	st, rel, err := s.reg.Acquire(slug)
	if err != nil {
		writeErr(w, 404, "no_such_store", "this store does not exist")
		return
	}
	defer rel()
	u, err := st.UsePasswordToken(r.Context(), in.Token, in.Password)
	if errors.Is(err, store.ErrBadToken) {
		writeErr(w, 400, "bad_link", "this link has expired or was already used; ask for a new one")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	tok, exp, err := st.CreateSession(r.Context(), u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName(slug), Value: tok, Path: "/", Expires: exp, HttpOnly: true, Secure: s.isTLS(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, r, 200, map[string]any{"user": u})
}

// ---- operator console ------------------------------------------------------------------

func (s *Server) opStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, 200, map[string]any{"delivering": s.plat.Delivering(), "dev_messages": s.plat.DevMessages()})
}

func (s *Server) opApplications(w http.ResponseWriter, r *http.Request) {
	l, err := s.plat.Applications(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"applications": l})
}

func appID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// opApprove creates the shop from an application and sends the owner a
// one-time link to choose their password (also returned, to pass on by hand).
func (s *Server) opApprove(w http.ResponseWriter, r *http.Request) {
	a, err := s.plat.Application(r.Context(), appID(r))
	if err != nil {
		s.platformFail(w, r, err)
		return
	}
	if a.Status != "pending" {
		writeErr(w, 409, "decided", "this application was already "+a.Status)
		return
	}
	var in struct {
		Slug string `json:"slug"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &in, 1<<10) {
		return
	}
	slug := a.Slug
	if in.Slug != "" {
		slug = strings.ToLower(strings.TrimSpace(in.Slug))
	}
	st, err := store.Create(s.cfg.DataDir, slug, store.DefaultSettings(a.ShopName), a.OwnerEmail, a.OwnerName, auth.RandomPassword(24))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.reg.Adopt(st); err != nil {
		st.Close()
	}
	st2, rel, err := s.reg.Acquire(slug)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer rel()
	owner, err := st2.UserByEmail(r.Context(), a.OwnerEmail)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tok, err := st2.CreatePasswordToken(r.Context(), owner.ID, "setup", store.PasswordTokenTTL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	base := s.baseURL(r)
	setup := base + "/s/" + slug + "/admin/#setup=" + tok
	if err := s.plat.Decide(r.Context(), a.ID, true, slug, ""); err != nil {
		s.platformFail(w, r, err)
		return
	}
	s.plat.QueueEmail(r.Context(), slug, "welcome", a.OwnerEmail, "Welcome to Price Tag: "+a.ShopName,
		"Hello "+a.OwnerName+",\n\n"+a.ShopName+" is now on Price Tag.\n\n1. Choose your password (link valid for 7 days):\n"+setup+
			"\n\n2. Load your prices: Admin > Import (a CSV from your till system or a spreadsheet).\n3. Print your QR poster: Admin > Settings.\n\nYour shop page: "+base+"/s/"+slug+"/\n")
	writeJSON(w, r, 201, map[string]any{"slug": slug, "url": base + "/s/" + slug + "/", "admin_url": base + "/s/" + slug + "/admin/", "setup_url": setup})
}

func (s *Server) opReject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Note string `json:"note"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &in, 2<<10) {
		return
	}
	if err := s.plat.Decide(r.Context(), appID(r), false, "", in.Note); err != nil {
		s.platformFail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

func (s *Server) opShops(w http.ResponseWriter, r *http.Request) {
	slugs, err := s.reg.List()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type row struct {
		Slug  string         `json:"slug"`
		Name  string         `json:"name"`
		Usage platform.Usage `json:"usage"`
	}
	out := []row{}
	for _, slug := range slugs {
		name := slug
		if st, rel, err := s.reg.Acquire(slug); err == nil {
			name = st.Settings().Name
			rel()
		}
		u, err := s.plat.Usage(r.Context(), slug)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, row{slug, name, u})
	}
	writeJSON(w, r, 200, map[string]any{"shops": out})
}

func (s *Server) opLimits(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !s.reg.Exists(slug) {
		writeErr(w, 404, "no_such_store", "this store does not exist")
		return
	}
	var in platform.Limits
	if !decodeJSON(w, r, &in, 1<<10) {
		return
	}
	if err := s.plat.SetLimits(r.Context(), slug, in); err != nil {
		s.platformFail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]bool{"ok": true})
}

func (s *Server) opOutbox(w http.ResponseWriter, r *http.Request) {
	l, err := s.plat.Outbox(r.Context(), 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, map[string]any{"outbox": l})
}
