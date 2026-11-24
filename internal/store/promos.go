package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"pricetag/internal/barcode"
)

type PromoItemInput struct {
	Code  string `json:"code"`
	Price int64  `json:"price"` // promotional price, minor units
}

type PromoInput struct {
	Title    string           `json:"title"`
	Text     string           `json:"text"`
	StartsAt int64            `json:"starts_at"`
	EndsAt   int64            `json:"ends_at"`
	Enabled  bool             `json:"enabled"`
	Items    []PromoItemInput `json:"items"`
}

type PromoItemOut struct {
	Code      string `json:"code"`
	Display   string `json:"display"`
	Name      string `json:"name"`
	Size      string `json:"size,omitempty"`
	BasePrice int64  `json:"base_price"`
	Price     int64  `json:"price"`
}

type Promo struct {
	ID        int64          `json:"id"`
	Title     string         `json:"title"`
	Text      string         `json:"text"`
	StartsAt  int64          `json:"starts_at"`
	EndsAt    int64          `json:"ends_at"`
	Enabled   bool           `json:"enabled"`
	ItemCount int            `json:"item_count"`
	Items     []PromoItemOut `json:"items,omitempty"`
	Status    string         `json:"status"` // live | scheduled | ended | disabled
}

const (
	maxPromoItems = 500
	maxPromoSpan  = 366 * 24 * 3600
)

func promoStatus(enabled bool, from, to, now int64) string {
	switch {
	case !enabled:
		return "disabled"
	case to <= now:
		return "ended"
	case from > now:
		return "scheduled"
	}
	return "live"
}

func (in *PromoInput) validate() ([]PromoItemInput, error) {
	in.Title = cleanText(in.Title, 80)
	in.Text = cleanText(in.Text, 300)
	switch {
	case in.Title == "" || utf8.RuneCountInString(in.Title) > 80:
		return nil, invalid("title", "offer title must be 1-80 characters")
	case utf8.RuneCountInString(in.Text) > 300:
		return nil, invalid("text", "offer description is limited to 300 characters")
	case in.StartsAt <= 0 || in.EndsAt <= 0:
		return nil, invalid("dates", "start and end time are required")
	case in.EndsAt <= in.StartsAt:
		return nil, invalid("dates", "the offer must end after it starts")
	case in.EndsAt-in.StartsAt > maxPromoSpan:
		return nil, invalid("dates", "an offer can run for at most one year")
	case len(in.Items) > maxPromoItems:
		return nil, invalid("items", "at most %d products per offer", maxPromoItems)
	}
	seen := map[string]bool{}
	out := make([]PromoItemInput, 0, len(in.Items))
	for _, it := range in.Items {
		code, err := barcode.Canonical(it.Code)
		if err != nil {
			return nil, invalid("items", "product %q: %v", it.Code, err)
		}
		if seen[code] {
			return nil, invalid("items", "product %s is listed twice", barcode.Display(code))
		}
		seen[code] = true
		if it.Price < 0 || it.Price > MaxPrice {
			return nil, invalid("items", "offer price for %s is out of range", barcode.Display(code))
		}
		out = append(out, PromoItemInput{Code: code, Price: it.Price})
	}
	return out, nil
}

func (c *txc) setPromoItems(id int64, items []PromoItemInput) error {
	for _, it := range items {
		var base int64
		var del int
		err := c.tx.QueryRowContext(c.ctx, `SELECT price,deleted FROM products WHERE code=?`, it.Code).Scan(&base, &del)
		if err == sql.ErrNoRows || del == 1 {
			return invalid("items", "product %s is not in the catalogue", barcode.Display(it.Code))
		} else if err != nil {
			return err
		}
		if it.Price >= base {
			return invalid("promo_price_not_lower", "offer price for %s must be lower than its normal price", barcode.Display(it.Code))
		}
		if _, err := c.tx.ExecContext(c.ctx, `INSERT INTO promo_items(promo_id,code,price) VALUES(?,?,?)`, id, it.Code, it.Price); err != nil {
			return err
		}
		c.touch(it.Code)
	}
	return nil
}

func (c *txc) promoCodes(id int64) ([]string, error) {
	rows, err := c.tx.QueryContext(c.ctx, `SELECT code FROM promo_items WHERE promo_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		out = append(out, code)
	}
	return out, rows.Err()
}

// CreatePromo creates an offer. Items may be empty (a pure announcement).
func (s *Store) CreatePromo(ctx context.Context, actor string, in PromoInput) (int64, error) {
	items, err := in.validate()
	if err != nil {
		return 0, err
	}
	var id int64
	_, err = s.write(ctx, actor, false, func(c *txc) error {
		res, e := c.tx.ExecContext(ctx, `INSERT INTO promotions(title,text,starts_at,ends_at,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
			in.Title, in.Text, in.StartsAt, in.EndsAt, b2i(in.Enabled), c.now, c.now)
		if e != nil {
			return e
		}
		id, _ = res.LastInsertId()
		if e = c.setPromoItems(id, items); e != nil {
			return e
		}
		c.home = true
		return c.history("promo", fmt.Sprint(id), fmt.Sprintf("Offer created: %s (%d products)", in.Title, len(items)), nil, nil)
	})
	return id, err
}

// UpdatePromo replaces an offer's details and product list.
func (s *Store) UpdatePromo(ctx context.Context, actor string, id int64, in PromoInput) error {
	items, err := in.validate()
	if err != nil {
		return err
	}
	_, err = s.write(ctx, actor, false, func(c *txc) error {
		var one int
		if e := c.tx.QueryRowContext(ctx, `SELECT 1 FROM promotions WHERE id=?`, id).Scan(&one); e == sql.ErrNoRows {
			return ErrNotFound
		} else if e != nil {
			return e
		}
		old, e := c.promoCodes(id)
		if e != nil {
			return e
		}
		for _, code := range old {
			c.touch(code)
		}
		if _, e = c.tx.ExecContext(ctx, `UPDATE promotions SET title=?,text=?,starts_at=?,ends_at=?,enabled=?,updated_at=? WHERE id=?`,
			in.Title, in.Text, in.StartsAt, in.EndsAt, b2i(in.Enabled), c.now, id); e != nil {
			return e
		}
		if _, e = c.tx.ExecContext(ctx, `DELETE FROM promo_items WHERE promo_id=?`, id); e != nil {
			return e
		}
		if e = c.setPromoItems(id, items); e != nil {
			return e
		}
		c.home = true
		return c.history("promo", fmt.Sprint(id), "Offer edited: "+in.Title, nil, nil)
	})
	return err
}

func (s *Store) DeletePromo(ctx context.Context, actor string, id int64) error {
	_, err := s.write(ctx, actor, false, func(c *txc) error {
		var title string
		if e := c.tx.QueryRowContext(ctx, `SELECT title FROM promotions WHERE id=?`, id).Scan(&title); e == sql.ErrNoRows {
			return ErrNotFound
		} else if e != nil {
			return e
		}
		old, e := c.promoCodes(id)
		if e != nil {
			return e
		}
		for _, code := range old {
			c.touch(code)
		}
		if _, e = c.tx.ExecContext(ctx, `DELETE FROM promotions WHERE id=?`, id); e != nil {
			return e
		}
		c.home = true
		return c.history("promo", fmt.Sprint(id), "Offer deleted: "+title, nil, nil)
	})
	return err
}

// ListPromos returns offers that are live, scheduled, disabled or ended in the
// last 30 days, newest first.
func (s *Store) ListPromos(ctx context.Context) ([]Promo, error) {
	now := time.Now().Unix()
	rows, err := s.r.QueryContext(ctx, `SELECT p.id,p.title,p.text,p.starts_at,p.ends_at,p.enabled,
		(SELECT COUNT(*) FROM promo_items WHERE promo_id=p.id)
		FROM promotions p WHERE p.ends_at>? ORDER BY p.starts_at DESC, p.id DESC LIMIT 200`, now-30*24*3600)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Promo{}
	for rows.Next() {
		var p Promo
		var en int
		if err := rows.Scan(&p.ID, &p.Title, &p.Text, &p.StartsAt, &p.EndsAt, &en, &p.ItemCount); err != nil {
			return nil, err
		}
		p.Enabled = en == 1
		p.Status = promoStatus(p.Enabled, p.StartsAt, p.EndsAt, now)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPromo(ctx context.Context, id int64) (*Promo, error) {
	var p Promo
	var en int
	err := s.r.QueryRowContext(ctx, `SELECT id,title,text,starts_at,ends_at,enabled FROM promotions WHERE id=?`, id).Scan(&p.ID, &p.Title, &p.Text, &p.StartsAt, &p.EndsAt, &en)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	p.Enabled = en == 1
	p.Status = promoStatus(p.Enabled, p.StartsAt, p.EndsAt, time.Now().Unix())
	rows, err := s.r.QueryContext(ctx, `SELECT pr.code,pr.name,pr.size,pr.price,pi.price FROM promo_items pi
		JOIN products pr ON pr.code=pi.code WHERE pi.promo_id=? ORDER BY pr.name COLLATE NOCASE`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p.Items = []PromoItemOut{}
	for rows.Next() {
		var it PromoItemOut
		if err := rows.Scan(&it.Code, &it.Name, &it.Size, &it.BasePrice, &it.Price); err != nil {
			return nil, err
		}
		it.Display = barcode.Display(it.Code)
		p.Items = append(p.Items, it)
	}
	p.ItemCount = len(p.Items)
	return &p, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

var _ = strings.TrimSpace
