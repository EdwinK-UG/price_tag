package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"pricetag/internal/barcode"
)

const (
	// Unambiguous 31-character alphabet (no 0/O, 1/I/L): 31^6 = 887,503,681 codes.
	checkoutAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	checkoutCodeLen  = 6
	MaxCheckoutItems = 200
	MaxCheckoutQty   = 999
	MaxOpenCheckouts = 20000 // shop-wide safety cap; per-client limits live in the server
	MaxMismatchItems = 100
)

var checkoutAlphaLUT = func() [256]bool {
	var t [256]bool
	for i := 0; i < len(checkoutAlphabet); i++ {
		t[checkoutAlphabet[i]] = true
	}
	return t
}()

// NormalizeCheckoutCode strips spaces and hyphens, uppercases ASCII, and checks
// that the result is 6 characters from the unambiguous checkout alphabet.
func NormalizeCheckoutCode(raw string) (string, bool) {
	if len(raw) > 32 {
		return "", false
	}
	var buf [checkoutCodeLen]byte
	n := 0
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '-' {
			continue
		}
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if n >= checkoutCodeLen || !checkoutAlphaLUT[c] {
			return "", false
		}
		buf[n] = c
		n++
	}
	if n != checkoutCodeLen {
		return "", false
	}
	return string(buf[:]), true
}

// generateCheckoutCode picks 6 unbiased random characters from checkoutAlphabet.
func generateCheckoutCode() (string, error) {
	const alphaLen = byte(len(checkoutAlphabet)) // 31
	const limit = 256 - (256 % int(alphaLen))    // 248: rejection-sampling ceiling
	var out [checkoutCodeLen]byte
	var rb [16]byte
	pos := 0
	for pos < checkoutCodeLen {
		if _, err := rand.Read(rb[:]); err != nil {
			return "", err
		}
		for _, b := range rb {
			if int(b) < limit {
				out[pos] = checkoutAlphabet[b%alphaLen]
				pos++
				if pos == checkoutCodeLen {
					break
				}
			}
		}
	}
	return string(out[:]), nil
}

// rollAuditRecheck makes an unbiased one-time server-side decision using
// crypto/rand whether an order is flagged for a full spot-check rescan.
func rollAuditRecheck(enabled bool, rate int) (bool, error) {
	if !enabled || rate <= 0 {
		return false, nil
	}
	if rate >= 100 {
		return true, nil
	}
	const limit = 200 // largest multiple of 100 <= 256
	var rb [8]byte
	for {
		if _, err := rand.Read(rb[:]); err != nil {
			return false, err
		}
		for _, b := range rb {
			if int(b) < limit {
				return int(b%100) < rate, nil
			}
		}
	}
}

// effectiveItemPrice returns min(base, best running offer at now) and the winning offer title.
func effectiveItemPrice(it *Item, now int64) (price int64, promoTitle string) {
	price = it.Price
	for _, p := range it.Promos {
		if p.From <= now && now < p.To && p.Price < price {
			price = p.Price
			promoTitle = p.Title
		}
	}
	return price, promoTitle
}

func safeMulAdd(total, unit int64, qty int) (lineTotal int64, newTotal int64, ok bool) {
	q := int64(qty)
	if q <= 0 || unit < 0 {
		return 0, total, false
	}
	if unit > 0 && q > math.MaxInt64/unit {
		return 0, total, false
	}
	lt := unit * q
	if lt > math.MaxInt64-total {
		return 0, total, false
	}
	return lt, total + lt, true
}

// CheckoutItemInput is one barcode+quantity submitted by a shopper's cart.
// Prices are always computed server-side from the shop's current price list.
type CheckoutItemInput struct {
	Code string `json:"code"`
	Qty  int    `json:"qty"`
}

type CheckoutSnapshotItem struct {
	Code      string `json:"code"`
	Qty       int    `json:"qty"`
	UnitPrice *int64 `json:"unit_price"` // nil if not in price list at checkout time
}

// CheckoutCreated is returned to the shopper. It deliberately NEVER includes
// audit_recheck so shoppers cannot know in advance whether they were flagged.
type CheckoutCreated struct {
	Code         string                 `json:"code"`
	CreatedAt    int64                  `json:"created_at"`
	ExpiresAt    int64                  `json:"expires_at"`
	Reused       bool                   `json:"reused"`
	Items        []CheckoutSnapshotItem `json:"items"`
	Total        int64                  `json:"total"`
	UnknownCount int                    `json:"unknown_count"`
}

type CashierCheckoutItem struct {
	Code              string `json:"code"`
	Display           string `json:"display"`
	Name              string `json:"name"`
	Size              string `json:"size,omitempty"`
	Qty               int    `json:"qty"`
	SnapshotPrice     *int64 `json:"snapshot_price"`
	CurrentPrice      *int64 `json:"current_price"`
	CurrentBasePrice  *int64 `json:"current_base_price,omitempty"`
	PromoTitle        string `json:"promo_title,omitempty"`
	PriceChanged      bool   `json:"price_changed"`
	SnapshotLineTotal *int64 `json:"snapshot_line_total"`
	CurrentLineTotal  *int64 `json:"current_line_total"`
}

type CashierCheckout struct {
	Code              string                `json:"code"`
	CreatedAt         int64                 `json:"created_at"`
	ExpiresAt         int64                 `json:"expires_at"`
	Status            string                `json:"status"`
	DefaultMode       string                `json:"default_mode"`
	AuditRecheck      bool                  `json:"audit_recheck"`
	Items             []CashierCheckoutItem `json:"items"`
	SnapshotTotal     int64                 `json:"snapshot_total"`
	CurrentTotal      int64                 `json:"current_total"`
	UnknownCount      int                   `json:"unknown_count"`
	PriceChangedCount int                   `json:"price_changed_count"`
}

type MismatchItemInput struct {
	Barcode     string `json:"barcode,omitempty"`
	Description string `json:"description,omitempty"`
	Qty         int    `json:"qty"`
}

type CheckoutMismatchInput struct {
	ExtraItems   []MismatchItemInput `json:"extra_items,omitempty"`
	MissingItems []MismatchItemInput `json:"missing_items,omitempty"`
	Note         string              `json:"note,omitempty"`
}

type VerifyCheckoutInput struct {
	Mode            string                 `json:"mode,omitempty"`
	RescanConfirmed bool                   `json:"rescan_confirmed,omitempty"`
	Mismatch        *CheckoutMismatchInput `json:"mismatch,omitempty"`
}

type CheckoutVerified struct {
	OK             bool   `json:"ok"`
	Code           string `json:"code"`
	VerifiedAt     int64  `json:"verified_at"`
	VerifiedBy     string `json:"verified_by"`
	VerifyMode     string `json:"verify_mode"`
	AuditRecheck   bool   `json:"audit_recheck"`
	MismatchLogged bool   `json:"mismatch_logged"`
}

func sameSnapshot(a, b []CheckoutSnapshotItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Code != b[i].Code || a[i].Qty != b[i].Qty {
			return false
		}
		if (a[i].UnitPrice == nil) != (b[i].UnitPrice == nil) {
			return false
		}
		if a[i].UnitPrice != nil && *a[i].UnitPrice != *b[i].UnitPrice {
			return false
		}
	}
	return true
}

func loadSnapshotItems(ctx context.Context, q querier, code string) ([]CheckoutSnapshotItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT barcode, qty, unit_price FROM checkout_items WHERE code=? ORDER BY seq`, code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckoutSnapshotItem
	for rows.Next() {
		var it CheckoutSnapshotItem
		var up sql.NullInt64
		if err := rows.Scan(&it.Code, &it.Qty, &up); err != nil {
			return nil, err
		}
		if up.Valid {
			v := up.Int64
			it.UnitPrice = &v
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CreateCheckout stores an anonymous cart snapshot in SQLite and returns a
// random 6-character code. If prevCode refers to an open, unexpired checkout
// with the exact same items/quantities/prices, it returns that same code.
func (s *Store) CreateCheckout(ctx context.Context, rawItems []CheckoutItemInput, prevCode string) (*CheckoutCreated, error) {
	return s.createCheckout(ctx, rawItems, prevCode, true)
}

// ReuseCheckout returns prevCode's checkout when the cart is unchanged and the
// code is still open, and ErrNotFound when a new code would be needed. It never
// creates a code, so it is safe to serve to a client that is out of allowance.
func (s *Store) ReuseCheckout(ctx context.Context, rawItems []CheckoutItemInput, prevCode string) (*CheckoutCreated, error) {
	return s.createCheckout(ctx, rawItems, prevCode, false)
}

func (s *Store) createCheckout(ctx context.Context, rawItems []CheckoutItemInput, prevCode string, allowNew bool) (*CheckoutCreated, error) {
	set := s.Settings()
	if !set.CheckoutEnabled {
		return nil, ErrCheckoutDisabled
	}
	if len(rawItems) == 0 {
		return nil, invalid("empty_cart", "cart is empty")
	}
	if len(rawItems) > MaxCheckoutItems {
		return nil, invalid("too_many_items", "at most %d products per checkout", MaxCheckoutItems)
	}
	merged := make(map[string]int, len(rawItems))
	for _, in := range rawItems {
		canon, err := barcode.Canonical(in.Code)
		if err != nil {
			return nil, invalid("code", "invalid barcode %q: %v", in.Code, err)
		}
		if in.Qty < 1 || in.Qty > MaxCheckoutQty {
			return nil, invalid("qty", "quantity must be 1-%d", MaxCheckoutQty)
		}
		next := merged[canon] + in.Qty
		if next > MaxCheckoutQty {
			return nil, invalid("qty", "quantity must be 1-%d", MaxCheckoutQty)
		}
		merged[canon] = next
	}
	codes := make([]string, 0, len(merged))
	for c := range merged {
		codes = append(codes, c)
	}
	sort.Strings(codes)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkouts WHERE status='open' AND expires_at<=?`, now); err != nil {
		return nil, err
	}

	liveItems, _, err := itemsFor(ctx, tx, codes, now)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]*Item, len(liveItems))
	for _, it := range liveItems {
		byCode[it.Code] = it
	}

	snap := make([]CheckoutSnapshotItem, 0, len(codes))
	var total int64
	unknown := 0
	for _, c := range codes {
		qty := merged[c]
		var up *int64
		if it := byCode[c]; it != nil {
			p, _ := effectiveItemPrice(it, now)
			up = &p
			_, nt, ok := safeMulAdd(total, p, qty)
			if !ok {
				return nil, invalid("overflow", "cart total is too large")
			}
			total = nt
		} else {
			unknown++
		}
		snap = append(snap, CheckoutSnapshotItem{Code: c, Qty: qty, UnitPrice: up})
	}

	// Idempotent re-tap: if prevCode is still open, unexpired, and identical, return it.
	if prev, ok := NormalizeCheckoutCode(prevCode); ok {
		var createdAt, expiresAt int64
		var status string
		err := tx.QueryRowContext(ctx, `SELECT created_at, expires_at, status FROM checkouts WHERE code=?`, prev).Scan(&createdAt, &expiresAt, &status)
		if err == nil && status == "open" && expiresAt > now {
			existing, err := loadSnapshotItems(ctx, tx, prev)
			if err == nil && sameSnapshot(existing, snap) {
				if err := tx.Commit(); err != nil {
					return nil, err
				}
				return &CheckoutCreated{
					Code: prev, CreatedAt: createdAt, ExpiresAt: expiresAt, Reused: true,
					Items: snap, Total: total, UnknownCount: unknown,
				}, nil
			}
		}
	}

	if !allowNew {
		return nil, ErrNotFound
	}
	var openCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkouts WHERE status='open'`).Scan(&openCount); err != nil {
		return nil, err
	}
	if openCount >= MaxOpenCheckouts {
		return nil, ErrTooManyCheckouts
	}

	ttlSec := int64(set.CheckoutTTLMin) * 60
	if ttlSec <= 0 {
		ttlSec = int64(DefaultCheckoutTTLMin) * 60
	}
	expiresAt := now + ttlSec

	recheck, err := rollAuditRecheck(set.AuditEnabled, set.AuditRate)
	if err != nil {
		return nil, err
	}
	recheckInt := 0
	if recheck {
		recheckInt = 1
	}

	var newCode string
	for attempt := 0; attempt < 8; attempt++ {
		cand, err := generateCheckoutCode()
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO checkouts(code, created_at, expires_at, status, audit_recheck) VALUES(?,?,?,'open',?)`, cand, now, expiresAt, recheckInt)
		if err == nil {
			newCode = cand
			break
		}
		if !strings.Contains(err.Error(), "UNIQUE") {
			return nil, err
		}
	}
	if newCode == "" {
		return nil, fmt.Errorf("could not allocate unique checkout code")
	}

	st, err := tx.PrepareContext(ctx, `INSERT INTO checkout_items(code, seq, barcode, qty, unit_price) VALUES(?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	for i, it := range snap {
		var up any
		if it.UnitPrice != nil {
			up = *it.UnitPrice
		}
		if _, err := st.ExecContext(ctx, newCode, i, it.Code, it.Qty, up); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &CheckoutCreated{
		Code: newCode, CreatedAt: now, ExpiresAt: expiresAt, Reused: false,
		Items: snap, Total: total, UnknownCount: unknown,
	}, nil
}

// GetCheckoutForCashier fetches an open, unexpired checkout and re-checks every
// item against the shop's current price list and running offers.
// Nonexistent, invalid, and expired codes all return ErrNotFound.
func (s *Store) GetCheckoutForCashier(ctx context.Context, rawCode string) (*CashierCheckout, error) {
	code, ok := NormalizeCheckoutCode(rawCode)
	if !ok {
		return nil, ErrNotFound
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var createdAt, expiresAt int64
	var status string
	var auditRecheck int
	err = tx.QueryRowContext(ctx, `SELECT created_at, expires_at, status, audit_recheck FROM checkouts WHERE code=?`, code).Scan(&createdAt, &expiresAt, &status, &auditRecheck)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if status == "verified" {
		return nil, ErrAlreadyVerified
	}
	now := time.Now().Unix()
	if expiresAt <= now {
		return nil, ErrNotFound
	}

	snap, err := loadSnapshotItems(ctx, tx, code)
	if err != nil {
		return nil, err
	}
	barcodes := make([]string, len(snap))
	for i, it := range snap {
		barcodes[i] = it.Code
	}
	liveItems, _, err := itemsFor(ctx, tx, barcodes, now)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]*Item, len(liveItems))
	for _, it := range liveItems {
		byCode[it.Code] = it
	}

	out := &CashierCheckout{
		Code: code, CreatedAt: createdAt, ExpiresAt: expiresAt, Status: status,
		DefaultMode:  s.Settings().CheckoutDefaultMode,
		AuditRecheck: auditRecheck != 0,
		Items:        make([]CashierCheckoutItem, 0, len(snap)),
	}
	for _, si := range snap {
		ci := CashierCheckoutItem{
			Code:          si.Code,
			Display:       barcode.Display(si.Code),
			Qty:           si.Qty,
			SnapshotPrice: si.UnitPrice,
		}
		if si.UnitPrice != nil {
			if lt, nt, ok := safeMulAdd(out.SnapshotTotal, *si.UnitPrice, si.Qty); ok {
				ci.SnapshotLineTotal = &lt
				out.SnapshotTotal = nt
			}
		}
		if live := byCode[si.Code]; live != nil {
			ci.Name = live.Name
			ci.Size = live.Size
			effP, promoTitle := effectiveItemPrice(live, now)
			ci.CurrentPrice = &effP
			if promoTitle != "" {
				bp := live.Price
				ci.CurrentBasePrice = &bp
				ci.PromoTitle = promoTitle
			}
			if lt, nt, ok := safeMulAdd(out.CurrentTotal, effP, si.Qty); ok {
				ci.CurrentLineTotal = &lt
				out.CurrentTotal = nt
			}
		} else {
			out.UnknownCount++
		}
		if (ci.SnapshotPrice == nil) != (ci.CurrentPrice == nil) ||
			(ci.SnapshotPrice != nil && ci.CurrentPrice != nil && *ci.SnapshotPrice != *ci.CurrentPrice) {
			ci.PriceChanged = true
			out.PriceChangedCount++
		}
		out.Items = append(out.Items, ci)
	}
	return out, nil
}

type normalizedMismatchItem struct {
	kind        string
	barcode     string
	description string
	qty         int
}

func normalizeMismatch(in *CheckoutMismatchInput) (items []normalizedMismatchItem, note string, err error) {
	if in == nil {
		return nil, "", nil
	}
	if len(in.ExtraItems) > MaxMismatchItems || len(in.MissingItems) > MaxMismatchItems {
		return nil, "", invalid("mismatch", "at most %d extra and %d missing items per mismatch", MaxMismatchItems, MaxMismatchItems)
	}
	note = strings.TrimSpace(in.Note)
	if len([]rune(note)) > 300 {
		return nil, "", invalid("note", "mismatch note is limited to 300 characters")
	}
	addList := func(kind string, raw []MismatchItemInput) error {
		for _, it := range raw {
			if it.Qty < 1 || it.Qty > MaxCheckoutQty {
				return invalid("qty", "mismatch quantity must be 1-%d", MaxCheckoutQty)
			}
			bc := strings.TrimSpace(it.Barcode)
			desc := strings.TrimSpace(it.Description)
			if len([]rune(desc)) > 120 {
				return invalid("description", "mismatch item description is limited to 120 characters")
			}
			if bc == "" && desc == "" {
				return invalid("mismatch", "each %s item needs a barcode or a description", kind)
			}
			if bc != "" {
				canon, cerr := barcode.Canonical(bc)
				if cerr != nil {
					return invalid("barcode", "invalid barcode %q: %v", bc, cerr)
				}
				bc = canon
			}
			items = append(items, normalizedMismatchItem{kind: kind, barcode: bc, description: desc, qty: it.Qty})
		}
		return nil
	}
	if err := addList("extra", in.ExtraItems); err != nil {
		return nil, "", err
	}
	if err := addList("missing", in.MissingItems); err != nil {
		return nil, "", err
	}
	if len(items) == 0 && note == "" {
		return nil, "", invalid("mismatch", "log at least one extra or missing item, or a note")
	}
	return items, note, nil
}

// VerifyCheckout marks an open checkout as verified/paid so the code cannot be
// reused, enforces full-rescan confirmation on spot-checked orders, optionally
// stores a mismatch record, and records the action in the audit trail (history).
func (s *Store) VerifyCheckout(ctx context.Context, actor, rawCode string, in VerifyCheckoutInput) (*CheckoutVerified, error) {
	code, ok := NormalizeCheckoutCode(rawCode)
	if !ok {
		return nil, ErrNotFound
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = s.Settings().CheckoutDefaultMode
	}
	if mode != CheckoutModeTick && mode != CheckoutModeRescan {
		return nil, invalid("mode", "verify mode must be tick or rescan")
	}
	mmItems, mmNote, err := normalizeMismatch(in.Mismatch)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var status string
	var expiresAt int64
	var auditRecheck int
	err = tx.QueryRowContext(ctx, `SELECT status, expires_at, audit_recheck FROM checkouts WHERE code=?`, code).Scan(&status, &expiresAt, &auditRecheck)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if status == "verified" {
		return nil, ErrAlreadyVerified
	}
	now := time.Now().Unix()
	if expiresAt <= now {
		return nil, ErrNotFound
	}

	flagged := auditRecheck != 0
	if flagged {
		if !in.RescanConfirmed {
			return nil, invalid("rescan_required", "this order is flagged for full rescan; confirm every item was rescanned")
		}
		mode = CheckoutModeRescan
	}

	snap, err := loadSnapshotItems(ctx, tx, code)
	if err != nil {
		return nil, err
	}
	barcodes := make([]string, len(snap))
	itemCount := 0
	for i, si := range snap {
		barcodes[i] = si.Code
		itemCount += si.Qty
	}
	liveItems, _, err := itemsFor(ctx, tx, barcodes, now)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]*Item, len(liveItems))
	for _, it := range liveItems {
		byCode[it.Code] = it
	}
	var currentTotal int64
	for _, si := range snap {
		if live := byCode[si.Code]; live != nil {
			p, _ := effectiveItemPrice(live, now)
			if _, nt, ok := safeMulAdd(currentTotal, p, si.Qty); ok {
				currentTotal = nt
			}
		}
	}

	res, err := tx.ExecContext(ctx, `UPDATE checkouts SET status='verified', verified_at=?, verified_by=?, verify_mode=? WHERE code=? AND status='open'`,
		now, actor, mode, code)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrAlreadyVerified
	}

	extraUnits, missingUnits := 0, 0
	mismatchLogged := in.Mismatch != nil
	if mismatchLogged {
		mres, err := tx.ExecContext(ctx, `INSERT INTO checkout_mismatches(code, created_at, actor, note) VALUES(?,?,?,?)`,
			code, now, actor, mmNote)
		if err != nil {
			return nil, err
		}
		mid, err := mres.LastInsertId()
		if err != nil {
			return nil, err
		}
		ist, err := tx.PrepareContext(ctx, `INSERT INTO checkout_mismatch_items(mismatch_id, kind, barcode, description, qty) VALUES(?,?,?,?,?)`)
		if err != nil {
			return nil, err
		}
		defer ist.Close()
		for _, mi := range mmItems {
			if _, err := ist.ExecContext(ctx, mid, mi.kind, mi.barcode, mi.description, mi.qty); err != nil {
				return nil, err
			}
			if mi.kind == "extra" {
				extraUnits += mi.qty
			} else {
				missingUnits += mi.qty
			}
		}
	}

	set := s.Settings()
	details := mode
	if flagged {
		details += ", spot-check"
	}
	sum := fmt.Sprintf("Checkout %s verified (%s, %d items, %s %s)", code, details, itemCount, set.Currency, FormatMoney(currentTotal, set.Decimals))
	if mismatchLogged {
		sum += fmt.Sprintf(" — MISMATCH: %d extra, %d missing", extraUnits, missingUnits)
		if mmNote != "" {
			sum += " (" + mmNote + ")"
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO history(at,actor,kind,ref,summary) VALUES(?,?,?,?,?)`, now, actor, "checkout", code, sum); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &CheckoutVerified{
		OK: true, Code: code, VerifiedAt: now, VerifiedBy: actor, VerifyMode: mode,
		AuditRecheck: flagged, MismatchLogged: mismatchLogged,
	}, nil
}

// CleanupExpiredCheckouts deletes expired open checkouts (verified orders are
// retained for audit reporting) and returns the number of rows removed.
func (s *Store) CleanupExpiredCheckouts(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.w.ExecContext(ctx, `DELETE FROM checkouts WHERE status='open' AND expires_at<=?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
