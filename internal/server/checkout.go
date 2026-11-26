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
// Marks a checkout verified/paid so the code cannot be reused, and logs who did
// it and when in the store's audit trail.
func (s *Server) verifyCheckout(w http.ResponseWriter, r *http.Request, st *store.Store, u *store.User) {
	var in struct {
		Mode string `json:"mode"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
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
	res, err := st.VerifyCheckout(r.Context(), u.Email, r.PathValue("code"), in.Mode)
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
