package store

import (
	"context"
	"database/sql"
	"errors"
	"net/mail"
	"strings"
	"time"

	"pricetag/internal/auth"
)

var (
	ErrBadLogin  = errors.New("wrong e-mail or password")
	ErrLastOwner = errors.New("a store must keep at least one active owner")
	ErrNoSession = errors.New("no valid session")
	ErrBadToken  = errors.New("invalid API token")
)

type User struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"` // owner | editor
	Disabled  bool   `json:"disabled"`
	CreatedAt int64  `json:"created_at"`
}

func normEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	if len(e) > 254 {
		return "", invalid("email", "e-mail address is too long")
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || !strings.Contains(e[strings.LastIndex(e, "@"):], ".") {
		return "", invalid("email", "enter a valid e-mail address")
	}
	return e, nil
}

func (s *Store) audit(ctx context.Context, actor, kind, ref, summary string) {
	_, _ = s.w.ExecContext(ctx, `INSERT INTO history(at,actor,kind,ref,summary) VALUES(?,?,?,?,?)`, time.Now().Unix(), actor, kind, ref, summary)
}

func (s *Store) CreateUser(email, name, role, password string) (*User, error) {
	email, err := normEmail(email)
	if err != nil {
		return nil, err
	}
	name = cleanText(name, 80)
	if name == "" || len([]rune(name)) > 80 {
		return nil, invalid("name", "name must be 1-80 characters")
	}
	if role != "owner" && role != "editor" {
		return nil, invalid("role", "role must be owner or editor")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, invalid("password", "%v", err)
	}
	now := time.Now().Unix()
	res, err := s.w.Exec(`INSERT INTO users(email,name,role,pass_hash,created_at) VALUES(?,?,?,?,?)`, email, name, role, hash, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrExists
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Email: email, Name: name, Role: role, CreatedAt: now}, nil
}

func scanUser(row interface{ Scan(...any) error }, hash *string) (*User, error) {
	var u User
	var dis int
	var dummy string
	if hash == nil {
		hash = &dummy
	}
	if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &dis, &u.CreatedAt, hash); err != nil {
		return nil, err
	}
	u.Disabled = dis == 1
	return &u, nil
}

const userCols = `id,email,name,role,disabled,created_at,pass_hash`

// Authenticate checks credentials; it spends the same time whether or not the
// account exists, and disabled accounts look like wrong passwords.
func (s *Store) Authenticate(ctx context.Context, email, password string) (*User, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	var hash string
	u, err := scanUser(s.r.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email=?`, e), &hash)
	if err == sql.ErrNoRows {
		auth.BurnVerify(password)
		return nil, ErrBadLogin
	} else if err != nil {
		return nil, err
	}
	if !auth.VerifyPassword(hash, password) || u.Disabled {
		return nil, ErrBadLogin
	}
	return u, nil
}

const sessionTTL = 14 * 24 * time.Hour

func (s *Store) CreateSession(ctx context.Context, userID int64) (token string, expires time.Time, err error) {
	tok, hash, err := auth.NewToken("")
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now()
	expires = now.Add(sessionTTL)
	_, err = s.w.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,created_at,expires_at,last_seen) VALUES(?,?,?,?,?)`,
		hash, userID, now.Unix(), expires.Unix(), now.Unix())
	return tok, expires, err
}

func (s *Store) SessionUser(ctx context.Context, token string) (*User, error) {
	if token == "" || len(token) > 100 {
		return nil, ErrNoSession
	}
	hash := auth.HashToken(token)
	now := time.Now().Unix()
	var exp, seen int64
	var u User
	var dis int
	err := s.r.QueryRowContext(ctx, `SELECT u.id,u.email,u.name,u.role,u.disabled,u.created_at,s.expires_at,s.last_seen
		FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=?`, hash).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &dis, &u.CreatedAt, &exp, &seen)
	if err == sql.ErrNoRows {
		return nil, ErrNoSession
	} else if err != nil {
		return nil, err
	}
	u.Disabled = dis == 1
	if exp <= now || u.Disabled {
		return nil, ErrNoSession
	}
	if now-seen > 600 { // sliding expiry without a write per request
		_, _ = s.w.ExecContext(ctx, `UPDATE sessions SET last_seen=?, expires_at=? WHERE token_hash=?`, now, now+int64(sessionTTL/time.Second), hash)
	}
	return &u, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) {
	_, _ = s.w.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, auth.HashToken(token))
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

type UserPatch struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
	Password *string `json:"password"`
}

// UpdateUser edits a team member; the last active owner cannot be demoted or
// disabled. Changing a password or disabling signs the person out everywhere.
func (s *Store) UpdateUser(ctx context.Context, actor string, id int64, p UserPatch) error {
	var hash string
	if p.Password != nil {
		h, err := auth.HashPassword(*p.Password)
		if err != nil {
			return invalid("password", "%v", err)
		}
		hash = h
	}
	if p.Name != nil {
		n := cleanText(*p.Name, 80)
		if n == "" || len([]rune(n)) > 80 {
			return invalid("name", "name must be 1-80 characters")
		}
		p.Name = &n
	}
	if p.Role != nil && *p.Role != "owner" && *p.Role != "editor" {
		return invalid("role", "role must be owner or editor")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var email string
	if err := tx.QueryRowContext(ctx, `SELECT email FROM users WHERE id=?`, id).Scan(&email); err == sql.ErrNoRows {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if p.Name != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET name=? WHERE id=?`, *p.Name, id); err != nil {
			return err
		}
	}
	if p.Role != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET role=? WHERE id=?`, *p.Role, id); err != nil {
			return err
		}
	}
	if p.Disabled != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled=? WHERE id=?`, b2i(*p.Disabled), id); err != nil {
			return err
		}
	}
	if hash != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET pass_hash=? WHERE id=?`, hash, id); err != nil {
			return err
		}
	}
	var owners int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='owner' AND disabled=0`).Scan(&owners); err != nil {
		return err
	}
	if owners == 0 {
		return ErrLastOwner
	}
	if hash != "" || (p.Disabled != nil && *p.Disabled) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.audit(ctx, actor, "user", email, "Team member changed: "+email)
	return nil
}

// SetPasswordByEmail is the operator's "forgot password" path (CLI only).
func (s *Store) SetPasswordByEmail(ctx context.Context, email, password string) error {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return invalid("password", "%v", err)
	}
	e := strings.ToLower(strings.TrimSpace(email))
	res, err := s.w.ExecContext(ctx, `UPDATE users SET pass_hash=?, disabled=0 WHERE email=?`, hash, e)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_, _ = s.w.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE email=?)`, e)
	s.audit(ctx, "operator", "user", e, "Password reset by operator")
	return nil
}

// ---- integration (API) tokens ----------------------------------------------

type APIToken struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  *int64 `json:"last_used,omitempty"`
	Revoked   bool   `json:"revoked"`
}

const tokenPrefix = "ptk_"

// CreateToken makes a machine credential that may only push products
// (/ingest). The plain token is returned once and never stored.
func (s *Store) CreateToken(ctx context.Context, actor, name string) (string, int64, error) {
	name = cleanText(name, 60)
	if name == "" || len([]rune(name)) > 60 {
		return "", 0, invalid("name", "give the integration a name (up to 60 characters)")
	}
	tok, hash, err := auth.NewToken(tokenPrefix)
	if err != nil {
		return "", 0, err
	}
	res, err := s.w.ExecContext(ctx, `INSERT INTO api_tokens(name,token_hash,created_at) VALUES(?,?,?)`, name, hash, time.Now().Unix())
	if err != nil {
		return "", 0, err
	}
	id, _ := res.LastInsertId()
	s.audit(ctx, actor, "token", name, "Integration key created: "+name)
	return tok, id, nil
}

func (s *Store) ListTokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id,name,created_at,last_used,revoked FROM api_tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		var lu sql.NullInt64
		var rv int
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &lu, &rv); err != nil {
			return nil, err
		}
		if lu.Valid {
			v := lu.Int64
			t.LastUsed = &v
		}
		t.Revoked = rv == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RevokeToken(ctx context.Context, actor string, id int64) error {
	var name string
	if err := s.r.QueryRowContext(ctx, `SELECT name FROM api_tokens WHERE id=?`, id).Scan(&name); err == sql.ErrNoRows {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if _, err := s.w.ExecContext(ctx, `UPDATE api_tokens SET revoked=1 WHERE id=?`, id); err != nil {
		return err
	}
	s.audit(ctx, actor, "token", name, "Integration key revoked: "+name)
	return nil
}

// VerifyToken returns the token's id and name when valid and not revoked.
func (s *Store) VerifyToken(ctx context.Context, token string) (int64, string, error) {
	if !strings.HasPrefix(token, tokenPrefix) || len(token) > 100 {
		return 0, "", ErrBadToken
	}
	var id int64
	var name string
	var rv int
	err := s.r.QueryRowContext(ctx, `SELECT id,name,revoked FROM api_tokens WHERE token_hash=?`, auth.HashToken(token)).Scan(&id, &name, &rv)
	if err == sql.ErrNoRows || rv == 1 {
		return 0, "", ErrBadToken
	} else if err != nil {
		return 0, "", err
	}
	now := time.Now().Unix()
	if last, ok := s.tokenSeen.Load(id); !ok || now-last.(int64) >= 60 {
		s.tokenSeen.Store(id, now)
		_, _ = s.w.ExecContext(ctx, `UPDATE api_tokens SET last_used=? WHERE id=?`, now, id)
	}
	return id, name, nil
}

// ---- history -----------------------------------------------------------------

type HistoryEntry struct {
	ID       int64  `json:"id"`
	At       int64  `json:"at"`
	Actor    string `json:"actor"`
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	Summary  string `json:"summary"`
	OldPrice *int64 `json:"old_price,omitempty"`
	NewPrice *int64 `json:"new_price,omitempty"`
}

// ListHistory returns recent changes (newest first). ref filters to one
// product code or offer id; before is a cursor (entry id) for paging.
func (s *Store) ListHistory(ctx context.Context, kind, ref string, before int64, limit int) ([]HistoryEntry, error) {
	limit = max(1, min(limit, 200))
	q := `SELECT id,at,actor,kind,ref,summary,old_price,new_price FROM history WHERE 1=1`
	var args []any
	if kind != "" {
		q += ` AND kind=?`
		args = append(args, kind)
	}
	if ref != "" {
		q += ` AND ref=?`
		args = append(args, ref)
	}
	if before > 0 {
		q += ` AND id<?`
		args = append(args, before)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryEntry{}
	for rows.Next() {
		var h HistoryEntry
		var o, n sql.NullInt64
		if err := rows.Scan(&h.ID, &h.At, &h.Actor, &h.Kind, &h.Ref, &h.Summary, &o, &n); err != nil {
			return nil, err
		}
		if o.Valid {
			v := o.Int64
			h.OldPrice = &v
		}
		if n.Valid {
			v := n.Int64
			h.NewPrice = &v
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
