package store

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"pricetag/internal/barcode"
)

const (
	MaxAuditReportDays = 366
	DefaultAuditDays   = 30
)

type AuditMismatchItem struct {
	Kind             string `json:"kind"` // "extra" | "missing"
	Barcode          string `json:"barcode,omitempty"`
	Display          string `json:"display,omitempty"`
	Name             string `json:"name,omitempty"`
	Description      string `json:"description,omitempty"`
	Qty              int    `json:"qty"`
	CurrentUnitPrice *int64 `json:"current_unit_price"`
	CurrentLineTotal *int64 `json:"current_line_total"`
}

type AuditMismatchEntry struct {
	Code                 string              `json:"code"`
	VerifiedAt           int64               `json:"verified_at"`
	VerifiedBy           string              `json:"verified_by"`
	AuditRecheck         bool                `json:"audit_recheck"`
	VerifyMode           string              `json:"verify_mode"`
	Note                 string              `json:"note,omitempty"`
	EstimatedMissedValue int64               `json:"estimated_missed_value"`
	UnpricedExtraCount   int                 `json:"unpriced_extra_count"`
	Items                []AuditMismatchItem `json:"items"`
}

type AuditReportData struct {
	From                       string               `json:"from"`
	To                         string               `json:"to"`
	Currency                   string               `json:"currency"`
	Decimals                   int                  `json:"decimals"`
	TotalCheckouts             int                  `json:"total_checkouts"`
	RecheckedCheckouts         int                  `json:"rechecked_checkouts"`
	MismatchCheckouts          int                  `json:"mismatch_checkouts"`
	RecheckedMismatchCheckouts int                  `json:"rechecked_mismatch_checkouts"`
	MismatchRatePct            float64              `json:"mismatch_rate_pct"`
	EstimatedMissedValue       int64                `json:"estimated_missed_value"`
	UnpricedExtraCount         int                  `json:"unpriced_extra_count"`
	MismatchesTruncated        bool                 `json:"mismatches_truncated,omitempty"`
	Mismatches                 []AuditMismatchEntry `json:"mismatches"`
}

// ParseAuditDateRange parses optional YYYY-MM-DD from/to dates in the shop's
// fixed timezone offset (tzOffsetMin minutes east of UTC).
// Defaults to the last 30 days inclusive ending today when omitted.
func ParseAuditDateRange(fromStr, toStr string, tzOffsetMin int, now time.Time) (fromLabel, toLabel string, fromUnix, toUnix int64, err error) {
	loc := time.FixedZone("shop", tzOffsetMin*60)
	localNow := now.In(loc)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)

	parseDay := func(raw string, def time.Time, field string) (time.Time, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return def, nil
		}
		t, perr := time.ParseInLocation("2006-01-02", raw, loc)
		if perr != nil {
			return time.Time{}, invalid(field, "%s must be a date in YYYY-MM-DD format", field)
		}
		return t, nil
	}

	toDay, err := parseDay(toStr, today, "to")
	if err != nil {
		return "", "", 0, 0, err
	}
	defFrom := toDay.AddDate(0, 0, -(DefaultAuditDays - 1))
	fromDay, err := parseDay(fromStr, defFrom, "from")
	if err != nil {
		return "", "", 0, 0, err
	}
	if fromDay.After(toDay) {
		return "", "", 0, 0, invalid("range", "'from' date must be on or before 'to' date")
	}
	days := int(toDay.Sub(fromDay)/(24*time.Hour)) + 1
	if days > MaxAuditReportDays {
		return "", "", 0, 0, invalid("range", "date range cannot exceed %d days", MaxAuditReportDays)
	}
	return fromDay.Format("2006-01-02"), toDay.Format("2006-01-02"), fromDay.Unix(), toDay.AddDate(0, 0, 1).Unix(), nil
}

// AuditReport summarizes verified checkouts, spot-checks, mismatch rate among
// rechecked checkouts, and estimated missed UGX value at current prices.
func (s *Store) AuditReport(ctx context.Context, fromLabel, toLabel string, fromUnix, toUnix int64, maxRows int) (*AuditReportData, error) {
	set := s.Settings()
	rep := &AuditReportData{
		From:       fromLabel,
		To:         toLabel,
		Currency:   set.Currency,
		Decimals:   set.Decimals,
		Mismatches: []AuditMismatchEntry{},
	}

	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN c.audit_recheck = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN m.id IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN c.audit_recheck = 1 AND m.id IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM checkouts c
		LEFT JOIN checkout_mismatches m ON m.code = c.code
		WHERE c.status = 'verified' AND c.verified_at >= ? AND c.verified_at < ?`,
		fromUnix, toUnix,
	).Scan(&rep.TotalCheckouts, &rep.RecheckedCheckouts, &rep.MismatchCheckouts, &rep.RecheckedMismatchCheckouts)
	if err != nil {
		return nil, err
	}
	if rep.RecheckedCheckouts > 0 {
		rep.MismatchRatePct = math.Round(float64(rep.RecheckedMismatchCheckouts)*1000/float64(rep.RecheckedCheckouts)) / 10
	}

	type rawMismatch struct {
		id    int64
		entry AuditMismatchEntry
	}
	mrows, err := tx.QueryContext(ctx, `
		SELECT m.id, c.code, c.verified_at, c.verified_by, c.audit_recheck, c.verify_mode, m.note
		FROM checkout_mismatches m
		JOIN checkouts c ON c.code = m.code
		WHERE c.status = 'verified' AND c.verified_at >= ? AND c.verified_at < ?
		ORDER BY c.verified_at DESC, m.id DESC`,
		fromUnix, toUnix,
	)
	if err != nil {
		return nil, err
	}
	var list []rawMismatch
	byID := map[int64]int{}
	for mrows.Next() {
		var id int64
		var e AuditMismatchEntry
		var recheck int
		if err := mrows.Scan(&id, &e.Code, &e.VerifiedAt, &e.VerifiedBy, &recheck, &e.VerifyMode, &e.Note); err != nil {
			mrows.Close()
			return nil, err
		}
		e.AuditRecheck = recheck != 0
		e.Items = []AuditMismatchItem{}
		byID[id] = len(list)
		list = append(list, rawMismatch{id: id, entry: e})
	}
	if err := mrows.Close(); err != nil {
		return nil, err
	}
	if err := mrows.Err(); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return rep, nil
	}

	// Load all mismatch items in the range in one query and collect barcodes to price at current prices.
	irows, err := tx.QueryContext(ctx, `
		SELECT i.mismatch_id, i.kind, i.barcode, i.description, i.qty
		FROM checkout_mismatch_items i
		JOIN checkout_mismatches m ON m.id = i.mismatch_id
		JOIN checkouts c ON c.code = m.code
		WHERE c.status = 'verified' AND c.verified_at >= ? AND c.verified_at < ?
		ORDER BY i.id ASC`,
		fromUnix, toUnix,
	)
	if err != nil {
		return nil, err
	}
	codeSet := map[string]struct{}{}
	for irows.Next() {
		var mid int64
		var mi AuditMismatchItem
		if err := irows.Scan(&mid, &mi.Kind, &mi.Barcode, &mi.Description, &mi.Qty); err != nil {
			irows.Close()
			return nil, err
		}
		if mi.Barcode != "" {
			mi.Display = barcode.Display(mi.Barcode)
			codeSet[mi.Barcode] = struct{}{}
		}
		if idx, ok := byID[mid]; ok {
			list[idx].entry.Items = append(list[idx].entry.Items, mi)
		}
	}
	if err := irows.Close(); err != nil {
		return nil, err
	}
	if err := irows.Err(); err != nil {
		return nil, err
	}

	now := time.Now().Unix()
	byCode := map[string]*Item{}
	if len(codeSet) > 0 {
		codes := make([]string, 0, len(codeSet))
		for c := range codeSet {
			codes = append(codes, c)
		}
		liveItems, _, err := itemsFor(ctx, tx, codes, now)
		if err != nil {
			return nil, err
		}
		for _, it := range liveItems {
			byCode[it.Code] = it
		}
	}

	for idx := range list {
		e := &list[idx].entry
		for j := range e.Items {
			mi := &e.Items[j]
			if mi.Barcode != "" {
				if live := byCode[mi.Barcode]; live != nil {
					mi.Name = live.Name
					effP, _ := effectiveItemPrice(live, now)
					mi.CurrentUnitPrice = &effP
					if lt, _, ok := safeMulAdd(0, effP, mi.Qty); ok {
						mi.CurrentLineTotal = &lt
					}
				}
			}
			if mi.Kind == "extra" {
				if mi.CurrentUnitPrice != nil {
					if _, nt, ok := safeMulAdd(e.EstimatedMissedValue, *mi.CurrentUnitPrice, mi.Qty); ok {
						e.EstimatedMissedValue = nt
					}
					if _, nt, ok := safeMulAdd(rep.EstimatedMissedValue, *mi.CurrentUnitPrice, mi.Qty); ok {
						rep.EstimatedMissedValue = nt
					}
				} else {
					e.UnpricedExtraCount += mi.Qty
					rep.UnpricedExtraCount += mi.Qty
				}
			}
		}
		if maxRows <= 0 || len(rep.Mismatches) < maxRows {
			rep.Mismatches = append(rep.Mismatches, *e)
		} else {
			rep.MismatchesTruncated = true
		}
	}
	return rep, nil
}

// ExportAuditReportCSV writes all logged mismatches in the date range as CSV
// with spreadsheet formula-injection protection on all text columns.
func (s *Store) ExportAuditReportCSV(ctx context.Context, fromLabel, toLabel string, fromUnix, toUnix int64, w io.Writer) error {
	rep, err := s.AuditReport(ctx, fromLabel, toLabel, fromUnix, toUnix, 0)
	if err != nil {
		return err
	}
	set := s.Settings()
	loc := time.FixedZone("shop", set.TZOffset*60)

	cw := csv.NewWriter(w)
	header := []string{
		"code", "verified_at", "verified_by", "spot_check", "verify_mode",
		"item_kind", "barcode", "item_name", "qty", "current_unit_price", "estimated_line_value", "note",
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, m := range rep.Mismatches {
		ts := time.Unix(m.VerifiedAt, 0).In(loc).Format("2006-01-02 15:04:05")
		spot := "no"
		if m.AuditRecheck {
			spot = "yes"
		}
		if len(m.Items) == 0 {
			row := []string{
				m.Code, ts, guardFormula(m.VerifiedBy), spot, m.VerifyMode,
				"note", "", "", "0", "", "", guardFormula(m.Note),
			}
			if err := cw.Write(row); err != nil {
				return err
			}
			continue
		}
		for _, it := range m.Items {
			label := it.Name
			if label == "" {
				label = it.Description
			} else if it.Description != "" && it.Description != it.Name {
				label = it.Name + " (" + it.Description + ")"
			}
			unitStr, lineStr := "", ""
			if it.CurrentUnitPrice != nil {
				unitStr = FormatMoney(*it.CurrentUnitPrice, set.Decimals)
			}
			if it.CurrentLineTotal != nil {
				lineStr = FormatMoney(*it.CurrentLineTotal, set.Decimals)
			}
			row := []string{
				m.Code, ts, guardFormula(m.VerifiedBy), spot, m.VerifyMode,
				it.Kind, guardFormula(it.Display), guardFormula(label), strconv.Itoa(it.Qty),
				unitStr, lineStr, guardFormula(m.Note),
			}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("write audit csv: %w", err)
	}
	return nil
}
