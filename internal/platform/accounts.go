package platform

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// ---- contacts --------------------------------------------------------------------------

// Contact is a normalised e-mail address or phone number (E.164, "+256772123456").
type Contact struct {
	Kind  string `json:"kind"` // "email" | "phone"
	Value string `json:"value"`
}

// NormalizeContact accepts what people actually type: "0772 123 456",
// "+256 772-123456", "256772123456", or an e-mail address.
func (p *Platform) NormalizeContact(raw string) (Contact, error) {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "@") {
		e := strings.ToLower(s)
		a, err := mail.ParseAddress(e)
		if err != nil || a.Address != e || len(e) > 254 || !strings.Contains(e[strings.LastIndex(e, "@"):], ".") {
			return Contact{}, bad("contact", "enter a valid e-mail address")
		}
		return Contact{Kind: "email", Value: e}, nil
	}
	var d strings.Builder
	plus := strings.HasPrefix(s, "+")
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			d.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' || r == '+':
		default:
			return Contact{}, bad("contact", "enter a phone number or an e-mail address")
		}
	}
	n := d.String()
	cc := p.cfg.DefaultCountryCode
	switch {
	case plus:
	case strings.HasPrefix(n, "00"):
		n = n[2:]
	case strings.HasPrefix(n, "0"):
		n = cc + n[1:]
	case strings.HasPrefix(n, cc) && len(n) > len(cc)+6:
	default:
		n = cc + n
	}
	if len(n) < 8 || len(n) > 15 || n[0] == '0' {
		return Contact{}, bad("contact", "enter a full phone number, for example 0772 123456")
	}
	return Contact{Kind: "phone", Value: "+" + n}, nil
}

// Mask hides most of a contact for display ("+25677•••456", "j•••@gmail.com").
func Mask(c string) string {
	if i := strings.Index(c, "@"); i > 0 {
		return c[:1] + "•••" + c[i:]
	}
	if len(c) > 7 {
		return c[:6] + "•••" + c[len(c)-3:]
	}
	return "•••"
}

// ---- one-time sign-in codes --------------------------------------------------------------

const (
	otpTTL         = 10 * time.Minute
	otpMaxAttempts = 5
	otpPerWindow   = 3
	otpWindow      = 15 * time.Minute
	sessionTTL     = 90 * 24 * time.Hour
)

func (p *Platform) mac(parts ...string) []byte {
	m := hmac.New(sha256.New, p.secret)
	for _, s := range parts {
		m.Write([]byte(s))
		m.Write([]byte{0})
	}
	return m.Sum(nil)
}

func randomCode() (string, error) {
	var b [4]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		v := binary.BigEndian.Uint32(b[:])
		if v < 4294000000 { // unbiased modulo 1,000,000
			return fmt.Sprintf("%06d", v%1000000), nil
		}
	}
}

// StartSignIn sends a 6-digit code to the contact by the chosen channel
// (e-mail for addresses; "sms" or "whatsapp" for phone numbers). The shop
// whose page the shopper is on pays for paid channels. The answer is the same
// whether or not an account exists, so it cannot be used to discover accounts.
func (p *Platform) StartSignIn(ctx context.Context, slug, shopName string, c Contact, via string) error {
	switch {
	case c.Kind == "email":
		via = "email"
	case via != "sms" && via != "whatsapp":
		return bad("via", "choose SMS or WhatsApp for the code")
	}
	code, err := randomCode()
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	tx, err := p.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var recent int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM otps WHERE target=? AND created_at>?`, c.Value, now.Add(-otpWindow).Unix()).Scan(&recent); err != nil {
		return err
	}
	if recent >= otpPerWindow {
		return ErrTooMany
	}
	if _, err := tx.ExecContext(ctx, `UPDATE otps SET used=1 WHERE target=? AND used=0`, c.Value); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO otps(target,code_hash,created_at,expires_at) VALUES(?,?,?,?)`,
		c.Value, p.mac("otp", c.Value, code), now.Unix(), now.Add(otpTTL).Unix()); err != nil {
		return err
	}
	text := fmt.Sprintf("Your Price Tag code is %s. It expires in 10 minutes. Never share it with anyone, even shop staff.", code)
	if shopName != "" {
		text = fmt.Sprintf("Your Price Tag code for %s is %s. It expires in 10 minutes. Never share it with anyone, even shop staff.", shopName, code)
	}
	subject := "Your Price Tag sign-in code"
	if err := p.queueTx(ctx, tx, slug, "otp", via, c.Value, subject, text, nil); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.Kick()
	return nil
}

type Shopper struct {
	ID        int64  `json:"-"`
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Channel   string `json:"channel"` // how they want messages: sms | whatsapp | email
	CreatedAt int64  `json:"created_at"`
}

const shopperCols = `id,name,COALESCE(email,''),COALESCE(phone,''),channel,created_at`

func scanShopper(row interface{ Scan(...any) error }) (*Shopper, error) {
	var s Shopper
	if err := row.Scan(&s.ID, &s.Name, &s.Email, &s.Phone, &s.Channel, &s.CreatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

func col(c Contact) string {
	if c.Kind == "email" {
		return "email"
	}
	return "phone"
}

// VerifySignIn checks the code. With current == 0 it signs in (creating the
// account on first use). With current > 0 (already signed in) it adds the
// contact to that account instead (e.g. adding a phone to an e-mail account).
func (p *Platform) VerifySignIn(ctx context.Context, c Contact, code string, current int64) (sh *Shopper, created bool, err error) {
	code = strings.TrimSpace(code)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now().Unix()
	tx, err := p.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var id, exp int64
	var attempts, used int
	var hash []byte
	err = tx.QueryRowContext(ctx, `SELECT id,code_hash,expires_at,attempts,used FROM otps WHERE target=? ORDER BY id DESC LIMIT 1`, c.Value).Scan(&id, &hash, &exp, &attempts, &used)
	if err == sql.ErrNoRows {
		return nil, false, ErrBadCode
	} else if err != nil {
		return nil, false, err
	}
	if used == 1 || exp <= now || attempts >= otpMaxAttempts {
		return nil, false, ErrBadCode
	}
	if !hmac.Equal(hash, p.mac("otp", c.Value, code)) {
		tx.ExecContext(ctx, `UPDATE otps SET attempts=attempts+1 WHERE id=?`, id)
		tx.Commit()
		return nil, false, ErrBadCode
	}
	if _, err := tx.ExecContext(ctx, `UPDATE otps SET used=1 WHERE id=?`, id); err != nil {
		return nil, false, err
	}
	owner, err := scanShopper(tx.QueryRowContext(ctx, `SELECT `+shopperCols+` FROM shoppers WHERE `+col(c)+`=?`, c.Value))
	if err != nil && err != sql.ErrNoRows {
		return nil, false, err
	}
	switch {
	case current > 0:
		if owner != nil && owner.ID != current {
			return nil, false, ErrContactInUse
		}
		if _, err := tx.ExecContext(ctx, `UPDATE shoppers SET `+col(c)+`=? WHERE id=?`, c.Value, current); err != nil {
			return nil, false, err
		}
		owner, err = scanShopper(tx.QueryRowContext(ctx, `SELECT `+shopperCols+` FROM shoppers WHERE id=?`, current))
		if err != nil {
			return nil, false, err
		}
	case owner == nil:
		ch := "email"
		if c.Kind == "phone" {
			ch = "sms"
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO shoppers(`+col(c)+`,channel,created_at) VALUES(?,?,?)`, c.Value, ch, now)
		if err != nil {
			return nil, false, err
		}
		nid, _ := res.LastInsertId()
		owner = &Shopper{ID: nid, Channel: ch, CreatedAt: now}
		if c.Kind == "email" {
			owner.Email = c.Value
		} else {
			owner.Phone = c.Value
		}
		created = true
	}
	return owner, created, tx.Commit()
}

// ---- sessions ------------------------------------------------------------------------------

func newToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	t := base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(t))
	return t, h[:], nil
}

func (p *Platform) CreateSession(ctx context.Context, shopperID int64) (string, time.Time, error) {
	tok, h, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := p.now().Add(sessionTTL)
	_, err = p.w.ExecContext(ctx, `INSERT INTO shopper_sessions(token_hash,shopper_id,expires_at,last_seen) VALUES(?,?,?,?)`, h, shopperID, exp.Unix(), p.now().Unix())
	return tok, exp, err
}

// SessionShopper returns the signed-in shopper, or nil (not an error) when none.
func (p *Platform) SessionShopper(ctx context.Context, token string) (*Shopper, error) {
	if token == "" || len(token) > 100 {
		return nil, nil
	}
	h := sha256.Sum256([]byte(token))
	var exp, seen int64
	var s Shopper
	err := p.r.QueryRowContext(ctx, `SELECT s.id,s.name,COALESCE(s.email,''),COALESCE(s.phone,''),s.channel,s.created_at,x.expires_at,x.last_seen
		FROM shopper_sessions x JOIN shoppers s ON s.id=x.shopper_id WHERE x.token_hash=?`, h[:]).Scan(&s.ID, &s.Name, &s.Email, &s.Phone, &s.Channel, &s.CreatedAt, &exp, &seen)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	now := p.now().Unix()
	if exp <= now {
		return nil, nil
	}
	if now-seen > 3600 {
		p.w.ExecContext(ctx, `UPDATE shopper_sessions SET last_seen=?, expires_at=? WHERE token_hash=?`, now, now+int64(sessionTTL/time.Second), h[:])
	}
	return &s, nil
}

func (p *Platform) DeleteSession(ctx context.Context, token string) {
	h := sha256.Sum256([]byte(token))
	p.w.ExecContext(ctx, `DELETE FROM shopper_sessions WHERE token_hash=?`, h[:])
}

// ---- profile, consent, deletion ---------------------------------------------------------------

func (p *Platform) UpdateProfile(ctx context.Context, id int64, name, channel string) (*Shopper, error) {
	name = strings.Join(strings.Fields(name), " ")
	if len([]rune(name)) > 60 {
		return nil, bad("name", "name is limited to 60 characters")
	}
	sh, err := scanShopper(p.r.QueryRowContext(ctx, `SELECT `+shopperCols+` FROM shoppers WHERE id=?`, id))
	if err != nil {
		return nil, ErrNotFound
	}
	switch channel {
	case "email":
		if sh.Email == "" {
			return nil, bad("channel", "add and confirm an e-mail address first")
		}
	case "sms", "whatsapp":
		if sh.Phone == "" {
			return nil, bad("channel", "add and confirm a phone number first")
		}
	default:
		return nil, bad("channel", "choose SMS, WhatsApp or e-mail")
	}
	if _, err := p.w.ExecContext(ctx, `UPDATE shoppers SET name=?, channel=? WHERE id=?`, name, channel, id); err != nil {
		return nil, err
	}
	sh.Name, sh.Channel = name, channel
	return sh, nil
}

type Membership struct {
	Slug      string `json:"slug"`
	JoinedAt  int64  `json:"joined_at"`
	Marketing bool   `json:"marketing"`
}

func (p *Platform) Membership(ctx context.Context, id int64, slug string) (*Membership, error) {
	m := Membership{Slug: slug}
	var mk int
	err := p.r.QueryRowContext(ctx, `SELECT joined_at,marketing FROM memberships WHERE shopper_id=? AND slug=?`, id, slug).Scan(&m.JoinedAt, &mk)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	m.Marketing = mk == 1
	return &m, nil
}

// Join records the shopper's consent to share their details with this shop.
func (p *Platform) Join(ctx context.Context, id int64, slug string, marketing bool) error {
	_, err := p.w.ExecContext(ctx, `INSERT INTO memberships(shopper_id,slug,joined_at,marketing) VALUES(?,?,?,?)
		ON CONFLICT(shopper_id,slug) DO UPDATE SET marketing=excluded.marketing`, id, slug, p.now().Unix(), b2i(marketing))
	return err
}

func (p *Platform) SetMarketing(ctx context.Context, id int64, slug string, on bool) error {
	res, err := p.w.ExecContext(ctx, `UPDATE memberships SET marketing=? WHERE shopper_id=? AND slug=?`, b2i(on), id, slug)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotMember
	}
	return nil
}

// Leave withdraws consent: the shop can no longer see or message this shopper.
func (p *Platform) Leave(ctx context.Context, id int64, slug string) error {
	_, err := p.w.ExecContext(ctx, `DELETE FROM memberships WHERE shopper_id=? AND slug=?`, id, slug)
	return err
}

func (p *Platform) Memberships(ctx context.Context, id int64) ([]Membership, error) {
	rows, err := p.r.QueryContext(ctx, `SELECT slug,joined_at,marketing FROM memberships WHERE shopper_id=? ORDER BY joined_at DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		var mk int
		if err := rows.Scan(&m.Slug, &m.JoinedAt, &mk); err != nil {
			return nil, err
		}
		m.Marketing = mk == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteShopper erases the account, its sessions and every shop's access.
func (p *Platform) DeleteShopper(ctx context.Context, id int64) error {
	_, err := p.w.ExecContext(ctx, `DELETE FROM shoppers WHERE id=?`, id)
	return err
}

// Member is what a shop sees about one of its members: only what the shopper
// agreed to share by joining, and only the contact for their chosen channel.
type Member struct {
	Name      string `json:"name"`
	Channel   string `json:"channel"`
	Contact   string `json:"contact"`
	Marketing bool   `json:"marketing"`
	JoinedAt  int64  `json:"joined_at"`
}

func contactFor(s *Shopper) string {
	if s.Channel == "email" {
		return s.Email
	}
	if s.Phone != "" {
		return s.Phone
	}
	return s.Email
}

func (p *Platform) Members(ctx context.Context, slug string) ([]Member, error) {
	rows, err := p.r.QueryContext(ctx, `SELECT s.id,s.name,COALESCE(s.email,''),COALESCE(s.phone,''),s.channel,s.created_at,m.marketing,m.joined_at
		FROM memberships m JOIN shoppers s ON s.id=m.shopper_id WHERE m.slug=? ORDER BY m.joined_at DESC`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var s Shopper
		var mk int
		var m Member
		if err := rows.Scan(&s.ID, &s.Name, &s.Email, &s.Phone, &s.Channel, &s.CreatedAt, &mk, &m.JoinedAt); err != nil {
			return nil, err
		}
		m.Name, m.Channel, m.Contact, m.Marketing = s.Name, s.Channel, contactFor(&s), mk == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- unsubscribe links -------------------------------------------------------------------------

// UnsubscribeToken is a short signed token for the link in every message.
func (p *Platform) UnsubscribeToken(shopperID int64, slug string) string {
	m := p.mac("unsub", fmt.Sprint(shopperID), slug)
	return fmt.Sprintf("%d.%s.%s", shopperID, slug, base64.RawURLEncoding.EncodeToString(m[:9]))
}

// Unsubscribe turns off offers for the shop named in a valid token and
// returns the shop's slug.
func (p *Platform) Unsubscribe(ctx context.Context, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ErrNotFound
	}
	var id int64
	if _, err := fmt.Sscan(parts[0], &id); err != nil || id <= 0 {
		return "", ErrNotFound
	}
	if !hmac.Equal([]byte(p.UnsubscribeToken(id, parts[1])), []byte(token)) {
		return "", ErrNotFound
	}
	p.w.ExecContext(ctx, `UPDATE memberships SET marketing=0 WHERE shopper_id=? AND slug=?`, id, parts[1])
	return parts[1], nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
