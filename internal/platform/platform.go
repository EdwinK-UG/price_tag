// Package platform holds what is shared ACROSS shops: shopper accounts (one
// account for every Price Tag shop), the consent each shopper gives each shop,
// shop sign-up applications, outgoing messages (SMS, WhatsApp, e-mail) and
// per-shop message usage and caps. It lives in its own SQLite file,
// "_platform.db", next to the per-shop files (that name can never be a shop).
//
// Privacy model: an account is not visible to any shop until the shopper taps
// "Join" in that shop. Joining shares the shopper's name and chosen contact
// with that shop only. Receiving offers is a separate opt-in (off by default),
// every message carries an unsubscribe link, and leaving a shop or deleting
// the account removes the shop's access immediately.
package platform

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const FileName = "_platform.db"

const schemaVersion = 1

const schemaSQL = `
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE applications(
  id INTEGER PRIMARY KEY,
  created_at INTEGER NOT NULL,
  shop_name TEXT NOT NULL,
  slug TEXT NOT NULL,
  owner_name TEXT NOT NULL,
  owner_email TEXT NOT NULL,
  owner_phone TEXT NOT NULL DEFAULT '',
  town TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','rejected')),
  decided_at INTEGER,
  decision_note TEXT NOT NULL DEFAULT '',
  store_slug TEXT NOT NULL DEFAULT ''
);
CREATE INDEX applications_status ON applications(status, id);
CREATE TABLE shoppers(
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  email TEXT UNIQUE,
  phone TEXT UNIQUE,
  channel TEXT NOT NULL DEFAULT '' CHECK(channel IN ('','sms','whatsapp','email')),
  created_at INTEGER NOT NULL
);
CREATE TABLE shopper_sessions(
  token_hash BLOB PRIMARY KEY,
  shopper_id INTEGER NOT NULL REFERENCES shoppers(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL,
  last_seen INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE otps(
  id INTEGER PRIMARY KEY,
  target TEXT NOT NULL,
  code_hash BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  used INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX otps_target ON otps(target, created_at);
CREATE TABLE memberships(
  shopper_id INTEGER NOT NULL REFERENCES shoppers(id) ON DELETE CASCADE,
  slug TEXT NOT NULL,
  joined_at INTEGER NOT NULL,
  marketing INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(shopper_id, slug)
) WITHOUT ROWID;
CREATE INDEX memberships_slug ON memberships(slug, joined_at);
CREATE TABLE shop_limits(
  slug TEXT PRIMARY KEY,
  sms_month_cap INTEGER NOT NULL DEFAULT 0,
  wa_month_cap INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE TABLE usage(
  slug TEXT NOT NULL,
  month TEXT NOT NULL,
  channel TEXT NOT NULL,
  units INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(slug, month, channel)
) WITHOUT ROWID;
CREATE TABLE broadcasts(
  id INTEGER PRIMARY KEY,
  slug TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL,
  body TEXT NOT NULL,
  audience INTEGER NOT NULL,
  queued INTEGER NOT NULL,
  skipped INTEGER NOT NULL
);
CREATE INDEX broadcasts_slug ON broadcasts(slug, created_at);
CREATE TABLE outbox(
  id INTEGER PRIMARY KEY,
  slug TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  channel TEXT NOT NULL CHECK(channel IN ('sms','whatsapp','email')),
  to_addr TEXT NOT NULL,
  subject TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  units INTEGER NOT NULL DEFAULT 1,
  broadcast_id INTEGER,
  status TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','sent','logged','failed')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_try_at INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  sent_at INTEGER
);
CREATE INDEX outbox_due ON outbox(status, next_try_at);
CREATE INDEX outbox_slug ON outbox(slug, channel, status);
`

var (
	ErrNotFound           = errors.New("not found")
	ErrTooMany            = errors.New("too many attempts; please wait a few minutes")
	ErrBadCode            = errors.New("that code is wrong or has expired")
	ErrContactInUse       = errors.New("that phone number or e-mail already belongs to another account")
	ErrChannelUnavailable = errors.New("this way of sending messages is not available for this shop")
	ErrNotMember          = errors.New("join this shop first")
)

// InputError is a problem with what a person typed; Code is machine-readable.
type InputError struct{ Code, Msg string }

func (e *InputError) Error() string { return e.Msg }

func bad(code, format string, a ...any) error {
	return &InputError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

type Config struct {
	DefaultCountryCode string // digits, e.g. "256" for Uganda; used for local numbers like 0772...
	Senders            Senders
	Logger             *slog.Logger
}

type Platform struct {
	cfg    Config
	w, r   *sql.DB
	secret []byte
	mu     sync.Mutex // serialises writes that read-then-write (caps, OTP limits)
	kick   chan struct{}
	log    *slog.Logger
	now    func() time.Time

	devMu  sync.Mutex
	devLog []DevMessage // last messages handled by the development (log) sender
}

func uri(path string, readOnly bool) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("_txlock", "immediate")
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}).String()
}

// Open opens (creating if needed) the platform database in dir.
func Open(dir string, cfg Config) (*Platform, error) {
	if cfg.DefaultCountryCode == "" {
		cfg.DefaultCountryCode = "256"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	path := filepath.Join(dir, FileName)
	w, err := sql.Open("sqlite", uri(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", uri(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	p := &Platform{cfg: cfg, w: w, r: r, kick: make(chan struct{}, 1), log: cfg.Logger, now: time.Now}
	if err := p.init(); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func (p *Platform) init() error {
	tx, err := p.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := tx.Exec(schemaSQL); err != nil {
			return err
		}
		sec := make([]byte, 32)
		if _, err := rand.Read(sec); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('schema_version',?),('secret',?)`, strconv.Itoa(schemaVersion), hex.EncodeToString(sec)); err != nil {
			return err
		}
	}
	var ver, sec string
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&ver); err != nil {
		return err
	}
	if v, _ := strconv.Atoi(ver); v > schemaVersion {
		return fmt.Errorf("platform file is schema v%s, this build understands up to v%d", ver, schemaVersion)
	}
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key='secret'`).Scan(&sec); err != nil {
		return err
	}
	if p.secret, err = hex.DecodeString(sec); err != nil || len(p.secret) < 16 {
		return errors.New("platform secret is damaged")
	}
	return tx.Commit()
}

func (p *Platform) Close() error {
	e1 := p.w.Close()
	e2 := p.r.Close()
	if e1 != nil {
		return e1
	}
	return e2
}

// Backup writes a consistent copy (without live sessions or sign-in codes).
func (p *Platform) Backup(ctx context.Context, dst string) error {
	if _, err := p.w.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return err
	}
	cp, err := sql.Open("sqlite", uri(dst, false))
	if err != nil {
		return err
	}
	defer cp.Close()
	_, err = cp.ExecContext(ctx, `DELETE FROM shopper_sessions; DELETE FROM otps;`)
	return err
}

// Cleanup removes expired sessions/codes and old delivered messages.
func (p *Platform) Cleanup(ctx context.Context) {
	now := p.now().Unix()
	p.w.ExecContext(ctx, `DELETE FROM shopper_sessions WHERE expires_at<?`, now)
	p.w.ExecContext(ctx, `DELETE FROM otps WHERE expires_at<?`, now-86400)
	p.w.ExecContext(ctx, `DELETE FROM outbox WHERE status IN ('sent','logged','failed') AND created_at<?`, now-90*86400)
}
