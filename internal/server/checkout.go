package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"pricetag/internal/store"
)

// POST /api/v1/s/{slug}/checkouts
// Public endpoint (when enabled in shop settings): turns an anonymous cart into
// a short random 6-character code. Stores no shopper identity.
func (s *Server) createCheckout(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !s.limit(w, s.rlCheckoutCreate, slug+"|"+s.clientIP(r)) {
		return
	}
	st, rel, err := s.reg.Acquire(slug)
	if err != nil {
		writeErr(w, 404, "no_such_store", "this store does not exist")
		return
	}
	defer rel()
	deadline(w, 15*time.Second)
	w.Header().Set("Cache-Control", "no-store")
	if !st.Settings().CheckoutEnabled {
		writeErr(w, 403, "checkout_disabled", "checkout codes are not enabled for this store")
		return
	}
	var in struct {
		Items    []store.CheckoutItemInput `json:"items"`
		PrevCode string                    `json:"prev_code"`
	}
	if !decodeJSON(w, r, &in, 64<<10) {
		return
	}
	cc, err := st.CreateCheckout(r.Context(), in.Items, in.PrevCode)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrCheckoutDisabled):
			writeErr(w, 403, "checkout_disabled", "checkout codes are not enabled for this store")
		case errors.Is(err, store.ErrTooManyCheckouts):
			writeErr(w, 503, "busy", "too many open checkout codes right now; try again shortly")
		default:
			s.fail(w, r, err)
		}
		return
	}
	status := http.StatusCreated
	if cc.Reused {
		status = http.StatusOK
	}
	writeJSON(w, r, status, cc)
}

// GET /api/v1/s/{slug}/admin/checkouts/{code}
// Cashier lookup: throttled per IP and per shop on failed/invalid lookups so
// codes cannot be guessed. Invalid, nonexistent, and expired codes return the
// exact same 404 response.
func (s *Server) getCheckout(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	ipKey := st.Slug + "|" + s.clientIP(r)
	if !s.limit(w, s.rlCheckoutIP, ipKey) {
		return
	}
	if !s.limit(w, s.rlCheckoutStore, st.Slug) {
		s.rlCheckoutIP.refund(ipKey)
		return
	}
	co, err := st.GetCheckoutForCashier(r.Context(), r.PathValue("code"))
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, 404, "not_found", "checkout code not found or expired")
		case errors.Is(err, store.ErrAlreadyVerified):
			writeErr(w, 409, "already_verified", "this checkout code has already been verified")
		default:
			s.fail(w, r, err)
		}
		return
	}
	// Only failed/invalid lookups consume guessing budget; busy tills scanning valid codes stay fast.
	s.rlCheckoutIP.refund(ipKey)
	s.rlCheckoutStore.refund(st.Slug)
	writeJSON(w, r, 200, co)
}

// POST /api/v1/s/{slug}/admin/checkouts/{code}/verify
// Marks a checkout verified/paid so the code cannot be reused, enforces full
// rescan confirmation on spot-checked orders, optionally logs a mismatch, and
// records the action in the store's audit trail.
func (s *Server) verifyCheckout(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	var in store.VerifyCheckoutInput
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, 413, "too_large", "request is too large")
		} else {
			writeErr(w, 400, "bad_json", "the request body is not valid for this endpoint")
		}
		return
	}
	res, err := st.VerifyCheckout(r.Context(), u.Email, r.PathValue("code"), in)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, 404, "not_found", "checkout code not found or expired")
		case errors.Is(err, store.ErrAlreadyVerified):
			writeErr(w, 409, "already_verified", "this checkout code has already been verified")
		default:
			s.fail(w, r, err)
		}
		return
	}
	writeJSON(w, r, 200, res)
}

// GET /api/v1/s/{slug}/admin/audit-report?from=YYYY-MM-DD&to=YYYY-MM-DD
// Owner-only summary of verified checkouts, spot-checks, mismatch rate among
// rechecked orders, and estimated missed value at current prices.
func (s *Server) auditReport(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	q := r.URL.Query()
	fromLabel, toLabel, fromUnix, toUnix, err := store.ParseAuditDateRange(q.Get("from"), q.Get("to"), st.Settings().TZOffset, time.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := st.AuditReport(r.Context(), fromLabel, toLabel, fromUnix, toUnix, 500)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, 200, rep)
}

// GET /api/v1/s/{slug}/admin/audit-report.csv?from=YYYY-MM-DD&to=YYYY-MM-DD
// Owner-only CSV export of logged mismatches (formula-injection guarded).
func (s *Server) auditReportCSV(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	q := r.URL.Query()
	fromLabel, toLabel, fromUnix, toUnix, err := store.ParseAuditDateRange(q.Get("from"), q.Get("to"), st.Settings().TZOffset, time.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+st.Slug+`-audit-report-`+fromLabel+`-to-`+toLabel+`.csv"`)
	if err := st.ExportAuditReportCSV(r.Context(), fromLabel, toLabel, fromUnix, toUnix, w); err != nil {
		s.log.Error("audit csv export", "err", err)
	}
}
