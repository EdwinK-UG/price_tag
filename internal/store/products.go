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

type ProductInput struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Size     string `json:"size"`
	Category string `json:"category"`
	Price    int64  `json:"price"` // minor units
}

type UpsertStatus string

const (
	StatusCreated   UpsertStatus = "created"
	StatusUpdated   UpsertStatus = "updated"
	StatusRestored  UpsertStatus = "restored"
	StatusUnchanged UpsertStatus = "unchanged"
)

func cleanText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return s
}

func (in *ProductInput) validate() (code string, err error) {
	code, err = barcode.Canonical(in.Code)
	if err != nil {
		return "", invalid("code", "%v", err)
	}
	in.Name = cleanText(in.Name, 200)
	in.Size = cleanText(in.Size, 40)
	in.Category = cleanText(in.Category, 60)
	switch {
	case in.Name == "":
		return "", invalid("name", "name is required")
	case !utf8.ValidString(in.Name) || utf8.RuneCountInString(in.Name) > 200:
		return "", invalid("name", "name must be at most 200 characters")
	case utf8.RuneCountInString(in.Size) > 40:
		return "", invalid("size", "size must be at most 40 characters")
	case utf8.RuneCountInString(in.Category) > 60:
		return "", invalid("category", "category must be at most 60 characters")
	case in.Price < 0 || in.Price > MaxPrice:
		return "", invalid("price", "price is out of range")
	}
	return code, nil
}

// upsertTx applies one product inside an open write transaction.
func (c *txc) upsertProduct(in ProductInput) (UpsertStatus, error) {
	code, err := in.validate()
	if err != nil {
		return "", err
	}
	sel, err := c.stmt(`SELECT name,size,category,price,deleted FROM products WHERE code=?`)
	if err != nil {
		return "", err
	}
	var name, size, cat string
	var price int64
	var del int
	err = sel.QueryRowContext(c.ctx, code).Scan(&name, &size, &cat, &price, &del)
	switch {
	case err == sql.ErrNoRows:
		ins, err := c.stmt(`INSERT INTO products(code,name,size,category,price,deleted,rev,updated_at) VALUES(?,?,?,?,?,0,?,?)`)
		if err != nil {
			return "", err
		}
		if _, err := ins.ExecContext(c.ctx, code, in.Name, in.Size, in.Category, in.Price, c.v, c.now); err != nil {
			return "", err
		}
		c.touch(code)
		p := in.Price
		return StatusCreated, c.history("product", code, "Added "+in.Name, nil, &p)
	case err != nil:
		return "", err
	}
	if del == 0 && name == in.Name && size == in.Size && cat == in.Category && price == in.Price {
		return StatusUnchanged, nil
	}
	upd, err := c.stmt(`UPDATE products SET name=?,size=?,category=?,price=?,deleted=0,rev=?,updated_at=? WHERE code=?`)
	if err != nil {
		return "", err
	}
	if _, err := upd.ExecContext(c.ctx, in.Name, in.Size, in.Category, in.Price, c.v, c.now, code); err != nil {
		return "", err
	}
	c.touch(code)
	switch {
	case del == 1:
		p := in.Price
		return StatusRestored, c.history("product", code, "Restored "+in.Name, nil, &p)
	case price != in.Price:
		o, n := price, in.Price
		return StatusUpdated, c.history("product", code, "Price changed: "+in.Name, &o, &n)
	default:
		return StatusUpdated, c.history("product", code, "Details edited: "+in.Name, nil, nil)
	}
}

// UpsertProduct creates or updates one product and returns its canonical code.
func (s *Store) UpsertProduct(ctx context.Context, actor string, in ProductInput) (string, UpsertStatus, error) {
	code, err := in.validate()
	if err != nil {
		return "", "", err
	}
	var st UpsertStatus
	_, err = s.write(ctx, actor, false, func(c *txc) error {
		var e error
		st, e = c.upsertProduct(in)
		return e
	})
	return code, st, err
}

// SetPrice changes just the price (the store owner's most common action).
func (s *Store) SetPrice(ctx context.Context, actor, code string, price int64) (UpsertStatus, error) {
	canon, err := barcode.Canonical(code)
	if err != nil {
		return "", invalid("code", "%v", err)
	}
	var st UpsertStatus
	_, err = s.write(ctx, actor, false, func(c *txc) error {
		var in ProductInput
		var del int
		e := c.tx.QueryRowContext(ctx, `SELECT name,size,category,deleted FROM products WHERE code=?`, canon).Scan(&in.Name, &in.Size, &in.Category, &del)
		if e == sql.ErrNoRows || del == 1 {
			return ErrNotFound
		} else if e != nil {
			return e
		}
		in.Code, in.Price = canon, price
		st, e = c.upsertProduct(in)
		return e
	})
	return st, err
}

// DeleteProduct soft-deletes (keeps a tombstone so phones can drop it).
func (s *Store) DeleteProduct(ctx context.Context, actor, code string) error {
	canon, err := barcode.Canonical(code)
	if err != nil {
		return invalid("code", "%v", err)
	}
	_, err = s.write(ctx, actor, false, func(c *txc) error {
		var name string
		var price int64
		e := c.tx.QueryRowContext(ctx, `SELECT name,price FROM products WHERE code=? AND deleted=0`, canon).Scan(&name, &price)
		if e == sql.ErrNoRows {
			return ErrNotFound
		} else if e != nil {
			return e
		}
		if _, e = c.tx.ExecContext(ctx, `UPDATE products SET deleted=1,rev=?,updated_at=? WHERE code=?`, c.v, c.now, canon); e != nil {
			return e
		}
		res, e := c.tx.ExecContext(ctx, `DELETE FROM promo_items WHERE code=?`, canon)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n > 0 {
			c.home = true
		}
		c.touch(canon)
		o := price
		return c.history("product", canon, "Removed "+name, &o, nil)
	})
	return err
}

type AdminProduct struct {
	Code       string `json:"code"`
	Display    string `json:"display"`
	Name       string `json:"name"`
	Size       string `json:"size"`
	Category   string `json:"category"`
	Price      int64  `json:"price"`
	PromoPrice *int64 `json:"promo_price,omitempty"`
	UpdatedAt  int64  `json:"updated_at"`
}

// ListProducts pages through the catalogue for the admin screen.
func (s *Store) ListProducts(ctx context.Context, q string, offset, limit int) (items []AdminProduct, total int, err error) {
	limit = max(1, min(limit, 200))
	offset = max(0, offset)
	where := `deleted=0`
	var args []any
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + escapeLike(q) + "%"
		where += ` AND (name LIKE ? ESCAPE '\' OR code LIKE ? ESCAPE '\' OR category LIKE ? ESCAPE '\')`
		args = append(args, like, like, like)
	}
	if err = s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM products WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	now := time.Now().Unix()
	qa := append([]any{now, now}, args...)
	qa = append(qa, limit, offset)
	rows, err := s.r.QueryContext(ctx, `SELECT code,name,size,category,price,updated_at,
		(SELECT MIN(pi.price) FROM promo_items pi JOIN promotions p ON p.id=pi.promo_id
		  WHERE pi.code=products.code AND p.enabled=1 AND p.starts_at<=? AND p.ends_at>?)
		FROM products WHERE `+where+` ORDER BY name COLLATE NOCASE, code LIMIT ? OFFSET ?`, qa...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items = []AdminProduct{}
	for rows.Next() {
		var p AdminProduct
		var pp sql.NullInt64
		if err := rows.Scan(&p.Code, &p.Name, &p.Size, &p.Category, &p.Price, &p.UpdatedAt, &pp); err != nil {
			return nil, 0, err
		}
		p.Display = barcode.Display(p.Code)
		if pp.Valid {
			v := pp.Int64
			p.PromoPrice = &v
		}
		items = append(items, p)
	}
	return items, total, rows.Err()
}

// ---- bulk import -----------------------------------------------------------

const MaxImportRows = 200_000

type RowError struct {
	Row     int    `json:"row"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type ImportResult struct {
	Total     int        `json:"total"`
	Created   int        `json:"created"`
	Updated   int        `json:"updated"`
	Restored  int        `json:"restored"`
	Unchanged int        `json:"unchanged"`
	Errors    []RowError `json:"errors"`
	Version   int64      `json:"version"`
	DryRun    bool       `json:"dry_run"`
}

// ImportRow pairs a product with its source row number for error reporting.
type ImportRow struct {
	Row int
	In  ProductInput
}

// ImportProducts upserts many products in ONE transaction (one version, one
// event). Rows that fail validation are reported and skipped; unchanged rows
// cost nothing, so a nightly full price file only touches what really changed.
// With dry=true nothing is saved but the counts are real.
func (s *Store) ImportProducts(ctx context.Context, actor string, rows []ImportRow, dry bool) (*ImportResult, error) {
	if len(rows) > MaxImportRows {
		return nil, invalid("too_many_rows", "at most %d rows per import", MaxImportRows)
	}
	res := &ImportResult{Total: len(rows), Errors: []RowError{}, DryRun: dry}
	// Last occurrence of a repeated code wins; earlier ones are reported.
	last := make(map[string]int, len(rows))
	for i, r := range rows {
		if c, err := barcode.Canonical(r.In.Code); err == nil {
			last[c] = i
		}
	}
	_, err := s.write(ctx, actor, dry, func(c *txc) error {
		for i, r := range rows {
			canon, cerr := barcode.Canonical(r.In.Code)
			if cerr == nil && last[canon] != i {
				res.Errors = append(res.Errors, RowError{Row: r.Row, Code: r.In.Code, Message: "duplicate code in file; a later row is used instead"})
				continue
			}
			st, e := c.upsertProduct(r.In)
			if e != nil {
				if ve, ok := e.(*ValidationError); ok {
					res.Errors = append(res.Errors, RowError{Row: r.Row, Code: r.In.Code, Message: ve.Msg})
					continue
				}
				return e
			}
			switch st {
			case StatusCreated:
				res.Created++
			case StatusUpdated:
				res.Updated++
			case StatusRestored:
				res.Restored++
			default:
				res.Unchanged++
			}
		}
		if res.Created+res.Updated+res.Restored > 0 {
			sum := fmt.Sprintf("Import: %d new, %d changed", res.Created+res.Restored, res.Updated)
			return c.history("import", "", sum, nil, nil)
		}
		return nil
	})
	if err != nil && err != errDryRun {
		return nil, err
	}
	res.Version = s.Version()
	return res, nil
}
