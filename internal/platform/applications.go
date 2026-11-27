package platform

import (
	"context"
	"database/sql"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// Application is a shop asking to join Price Tag. The operator approves it.
type Application struct {
	ID           int64  `json:"id"`
	CreatedAt    int64  `json:"created_at"`
	ShopName     string `json:"shop_name"`
	Slug         string `json:"slug"`
	OwnerName    string `json:"owner_name"`
	OwnerEmail   string `json:"owner_email"`
	OwnerPhone   string `json:"owner_phone"`
	Town         string `json:"town"`
	Note         string `json:"note"`
	Status       string `json:"status"`
	DecidedAt    *int64 `json:"decided_at,omitempty"`
	DecisionNote string `json:"decision_note,omitempty"`
	StoreSlug    string `json:"store_slug,omitempty"`
}

func clean(s string, n int) (string, bool) {
	s = strings.Join(strings.Fields(s), " ")
	return s, utf8.RuneCountInString(s) <= n
}

// Submit records a sign-up request. validSlug checks the wanted shop address.
func (p *Platform) Submit(ctx context.Context, a Application, validSlug func(string) bool) (int64, error) {
	var ok bool
	if a.ShopName, ok = clean(a.ShopName, 80); !ok || a.ShopName == "" {
		return 0, bad("shop_name", "shop name must be 1-80 characters")
	}
	if a.OwnerName, ok = clean(a.OwnerName, 80); !ok || a.OwnerName == "" {
		return 0, bad("owner_name", "your name must be 1-80 characters")
	}
	a.OwnerEmail = strings.ToLower(strings.TrimSpace(a.OwnerEmail))
	if m, err := mail.ParseAddress(a.OwnerEmail); err != nil || m.Address != a.OwnerEmail {
		return 0, bad("owner_email", "enter a valid e-mail address")
	}
	if strings.TrimSpace(a.OwnerPhone) != "" {
		c, err := p.NormalizeContact(a.OwnerPhone)
		if err != nil || c.Kind != "phone" {
			return 0, bad("owner_phone", "enter a valid phone number")
		}
		a.OwnerPhone = c.Value
	}
	a.Slug = strings.ToLower(strings.TrimSpace(a.Slug))
	if !validSlug(a.Slug) {
		return 0, bad("slug", "the shop address must be 3-40 lowercase letters, digits or hyphens")
	}
	if a.Town, ok = clean(a.Town, 80); !ok {
		return 0, bad("town", "town is limited to 80 characters")
	}
	if a.Note, ok = clean(a.Note, 500); !ok {
		return 0, bad("note", "the note is limited to 500 characters")
	}
	var pending int
	if err := p.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM applications WHERE status='pending'`).Scan(&pending); err != nil {
		return 0, err
	}
	if pending >= 1000 {
		return 0, ErrTooMany
	}
	res, err := p.w.ExecContext(ctx, `INSERT INTO applications(created_at,shop_name,slug,owner_name,owner_email,owner_phone,town,note) VALUES(?,?,?,?,?,?,?,?)`,
		p.now().Unix(), a.ShopName, a.Slug, a.OwnerName, a.OwnerEmail, a.OwnerPhone, a.Town, a.Note)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanApp(row interface{ Scan(...any) error }) (*Application, error) {
	var a Application
	var d sql.NullInt64
	if err := row.Scan(&a.ID, &a.CreatedAt, &a.ShopName, &a.Slug, &a.OwnerName, &a.OwnerEmail, &a.OwnerPhone, &a.Town, &a.Note, &a.Status, &d, &a.DecisionNote, &a.StoreSlug); err != nil {
		return nil, err
	}
	if d.Valid {
		v := d.Int64
		a.DecidedAt = &v
	}
	return &a, nil
}

const appCols = `id,created_at,shop_name,slug,owner_name,owner_email,owner_phone,town,note,status,decided_at,decision_note,store_slug`

func (p *Platform) Applications(ctx context.Context) ([]Application, error) {
	rows, err := p.r.QueryContext(ctx, `SELECT `+appCols+` FROM applications ORDER BY (status='pending') DESC, id DESC LIMIT 300`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Application{}
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (p *Platform) Application(ctx context.Context, id int64) (*Application, error) {
	a, err := scanApp(p.r.QueryRowContext(ctx, `SELECT `+appCols+` FROM applications WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return a, err
}

// Decide marks a pending application approved (with the created shop's slug) or rejected.
func (p *Platform) Decide(ctx context.Context, id int64, approve bool, storeSlug, note string) error {
	status := "rejected"
	if approve {
		status = "approved"
	}
	note, _ = clean(note, 300)
	res, err := p.w.ExecContext(ctx, `UPDATE applications SET status=?, decided_at=?, decision_note=?, store_slug=? WHERE id=? AND status='pending'`,
		status, p.now().Unix(), note, storeSlug, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
