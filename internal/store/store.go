// Package store is one store's (tenant's) data: a single SQLite file holding
// products, promotions, staff, API tokens and history. Everything a store owns
// lives in that one file, so "give me my data" is just handing it over.
//
// Every write that changes shopper-visible data bumps a per-store version `v`
// inside the same transaction and publishes a change event to connected
// phones after commit. Rows carry the version they last changed at (`rev`),
// which is what makes delta sync and catch-up after offline cheap and exact.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"pricetag/internal/auth"
	"pricetag/internal/hub"
)

const (
	schemaVersion = 3
	// MaxInline is the most changed items sent inline in one event; bigger
	// commits (e.g. a nightly price file) send a "reset" hint instead.
	MaxInline = 50
	MaxPrice  = int64(1) << 50

	DefaultCheckoutTTLMin = 180 // 3 hours
	CheckoutModeTick      = "tick"
	CheckoutModeRescan    = "rescan"
)

var (
	ErrNotFound         = errors.New("not found")
	ErrExists           = errors.New("already exists")
	ErrAlreadyVerified  = errors.New("already verified")
	ErrCheckoutDisabled = errors.New("checkout disabled")
	ErrTooManyCheckouts = errors.New("too many open checkouts")
)

// ValidationError is a problem with caller input; Code is machine-readable.
type ValidationError struct {
	Code string
	Msg  string
}

func (e *ValidationError) Error() string { return e.Msg }

func invalid(code, format string, a ...any) error {
	return &ValidationError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

var reservedSlugs = map[string]bool{
	"admin": true, "api": true, "static": true, "s": true, "operator": true, "assets": true,
	"healthz": true, "metrics": true, "www": true, "app": true, "sw": true, "tmp": true,
}

func ValidSlug(s string) bool { return slugRe.MatchString(s) && !reservedSlugs[s] }

type Settings struct {
	Name                string `json:"name"`
	Currency            string `json:"currency"`
	Decimals            int    `json:"decimals"`
	TZOffset            int    `json:"tz_offset"` // minutes east of UTC; Uganda (EAT) = 180
	Announcement        string `json:"announcement"`
	CheckoutEnabled     bool   `json:"checkout_enabled"`
	CheckoutTTLMin      int    `json:"checkout_ttl_min"`
	CheckoutDefaultMode string `json:"checkout_default_mode"` // "tick" | "rescan"
	AuditEnabled        bool   `json:"audit_enabled"`
	AuditRate           int    `json:"audit_rate"` // 0..100 percentage of checkouts flagged for full rescan
}

func DefaultSettings(name string) Settings {
	return Settings{
		Name: name, Currency: "UGX", Decimals: 0, TZOffset: 180,
		CheckoutEnabled: false, CheckoutTTLMin: DefaultCheckoutTTLMin, CheckoutDefaultMode: CheckoutModeTick,
		AuditEnabled: false, AuditRate: 0,
	}
}

func (s *Settings) applyDefaults() {
	if s.CheckoutTTLMin == 0 {
		s.CheckoutTTLMin = DefaultCheckoutTTLMin
	}
	if s.CheckoutDefaultMode == "" {
		s.CheckoutDefaultMode = CheckoutModeTick
	}
}

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

func (s *Settings) normalize() error {
	s.Name = strings.TrimSpace(s.Name)
	s.Announcement = strings.TrimSpace(s.Announcement)
	s.Currency = strings.ToUpper(strings.TrimSpace(s.Currency))
	s.CheckoutDefaultMode = strings.ToLower(strings.TrimSpace(s.CheckoutDefaultMode))
	s.applyDefaults()
	switch {
	case s.Name == "" || len([]rune(s.Name)) > 80:
		return invalid("name", "store name must be 1-80 characters")
	case !currencyRe.MatchString(s.Currency):
		return invalid("currency", "currency must be a 3-letter code such as UGX")
	case s.Decimals < 0 || s.Decimals > 3:
		return invalid("decimals", "decimals must be 0-3")
	case s.TZOffset < -720 || s.TZOffset > 840:
		return invalid("tz_offset", "time zone offset out of range")
	case len([]rune(s.Announcement)) > 200:
		return invalid("announcement", "announcement is limited to 200 characters")
	case s.CheckoutTTLMin < 5 || s.CheckoutTTLMin > 1440:
		return invalid("checkout_ttl_min", "checkout expiry must be between 5 and 1440 minutes")
	case s.CheckoutDefaultMode != CheckoutModeTick && s.CheckoutDefaultMode != CheckoutModeRescan:
		return invalid("checkout_default_mode", "checkout default mode must be tick or rescan")
	case s.AuditRate < 0 || s.AuditRate > 100:
		return invalid("audit_rate", "audit rate must be between 0 and 100")
	}
	return nil
}

type Store struct {
	Slug string
	Hub  *hub.Hub

	path string
	w    *sql.DB // single writer connection
	r    *sql.DB // read pool

	mu       sync.Mutex // serialises writes + publishing so events are ordered
	v        atomic.Int64
	homeRev  atomic.Int64
	purged   atomic.Int64
	settings atomic.Pointer[Settings]

	homeMu    sync.Mutex
	homeCache *HomeSnapshot

	tokenSeen sync.Map // api token id -> last_used write time (throttle)

	catMu sync.Mutex // cached gzip of the full catalogue at version catV
	catV  int64
	catGz []byte
}

const schemaSQL = `
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE products(
  code TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  size TEXT NOT NULL DEFAULT '',
  category TEXT NOT NULL DEFAULT '',
  price INTEGER NOT NULL CHECK(price >= 0),
  deleted INTEGER NOT NULL DEFAULT 0,
  rev INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX products_rev ON products(rev);
CREATE TABLE promotions(
  id INTEGER PRIMARY KEY,
  title TEXT NOT NULL,
  text TEXT NOT NULL DEFAULT '',
  starts_at INTEGER NOT NULL,
  ends_at INTEGER NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX promotions_ends ON promotions(ends_at);
CREATE TABLE promo_items(
  promo_id INTEGER NOT NULL REFERENCES promotions(id) ON DELETE CASCADE,
  code TEXT NOT NULL,
  price INTEGER NOT NULL CHECK(price >= 0),
  PRIMARY KEY(promo_id, code)
) WITHOUT ROWID;
CREATE INDEX promo_items_code ON promo_items(code);
CREATE TABLE users(
  id INTEGER PRIMARY KEY,
  email TEXT NOT NULL UNIQUE COLLATE NOCASE,
  name TEXT NOT NULL,
  role TEXT NOT NULL CHECK(role IN ('owner','editor')),
  pass_hash TEXT NOT NULL,
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE sessions(
  token_hash BLOB PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  last_seen INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE api_tokens(
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  last_used INTEGER,
  revoked INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE history(
  id INTEGER PRIMARY KEY,
  at INTEGER NOT NULL,
  actor TEXT NOT NULL,
  kind TEXT NOT NULL,
  ref TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL,
  old_price INTEGER,
  new_price INTEGER
);
CREATE INDEX history_ref ON history(kind, ref, id);
`

const schemaV2SQL = `
CREATE TABLE checkouts(
  code TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('open','verified')),
  verified_at INTEGER,
  verified_by TEXT NOT NULL DEFAULT '',
  verify_mode TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX checkouts_expires ON checkouts(expires_at);
CREATE INDEX checkouts_created ON checkouts(created_at);
CREATE TABLE checkout_items(
  code TEXT NOT NULL REFERENCES checkouts(code) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  barcode TEXT NOT NULL,
  qty INTEGER NOT NULL CHECK(qty >= 1),
  unit_price INTEGER,
  PRIMARY KEY(code, seq)
) WITHOUT ROWID;
`

const schemaV3SQL = `
ALTER TABLE checkouts ADD COLUMN audit_recheck INTEGER NOT NULL DEFAULT 0;
CREATE INDEX checkouts_verified ON checkouts(status, verified_at);
CREATE TABLE checkout_mismatches(
  id INTEGER PRIMARY KEY,
  code TEXT NOT NULL UNIQUE REFERENCES checkouts(code) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  actor TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT ''
);
CREATE INDEX checkout_mismatches_created ON checkout_mismatches(created_at);
CREATE TABLE checkout_mismatch_items(
  id INTEGER PRIMARY KEY,
  mismatch_id INTEGER NOT NULL REFERENCES checkout_mismatches(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK(kind IN ('extra','missing')),
  barcode TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  qty INTEGER NOT NULL CHECK(qty >= 1)
);
CREATE INDEX checkout_mismatch_items_mid ON checkout_mismatch_items(mismatch_id);
`

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	// FULL: an acknowledged price edit survives a power cut (shops often have no UPS).
	// Writes are rare and small, so the extra fsync is a few milliseconds.
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: uriPath(path), RawQuery: q.Encode()}
	return u.String()
}

// uriPath makes a path safe for a SQLite "file:" URI. A relative path such as
// "data/demo.db" would become "file://data/demo.db", where SQLite reads "data"
// as a host name and refuses to open it (reported, misleadingly, as "out of
// memory"). So: absolute, forward slashes, leading slash (also right for
// Windows drive paths: "/C:/...").
func uriPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// DBPath is where a slug's file lives under the data directory.
func DBPath(dir, slug string) string { return filepath.Join(dir, slug+".db") }

// Exists reports whether a store file exists.
func Exists(dir, slug string) bool {
	if !ValidSlug(slug) {
		return false
	}
	st, err := os.Stat(DBPath(dir, slug))
	return err == nil && st.Mode().IsRegular()
}

func openDB(path string) (w, r *sql.DB, err error) {
	w, err = sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, nil, err
	}
	w.SetMaxOpenConns(1)
	if err = w.Ping(); err != nil {
		w.Close()
		return nil, nil, err
	}
	r, err = sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, nil, err
	}
	r.SetMaxOpenConns(4)
	r.SetMaxIdleConns(2)
	r.SetConnMaxIdleTime(2 * time.Minute)
	return w, r, nil
}

// Create makes a brand-new store file with its first owner. It fails with
// ErrExists rather than touching an existing store.
func Create(dir, slug string, set Settings, ownerEmail, ownerName, ownerPassword string) (*Store, error) {
	if !ValidSlug(slug) {
		return nil, invalid("slug", "store address must be 3-40 characters: lowercase letters, digits and hyphens")
	}
	if err := set.normalize(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	path := DBPath(dir, slug)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		if os.IsExist(err) {
			return nil, ErrExists
		}
		return nil, err
	}
	f.Close()
	fail := func(err error) (*Store, error) {
		os.Remove(path)
		os.Remove(path + "-wal")
		os.Remove(path + "-shm")
		return nil, err
	}
	w, r, err := openDB(path)
	if err != nil {
		return fail(err)
	}
	setJSON, _ := json.Marshal(set)
	tx, err := w.Begin()
	if err != nil {
		w.Close()
		r.Close()
		return fail(err)
	}
	_, err = tx.Exec(schemaSQL + schemaV2SQL + schemaV3SQL)
	if err == nil {
		_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES('schema_version',?),('v','0'),('home_rev','0'),('purged_before','0'),('settings',?),('created_at',?)`,
			strconv.Itoa(schemaVersion), string(setJSON), strconv.FormatInt(time.Now().Unix(), 10))
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		w.Close()
		r.Close()
		return fail(err)
	}
	s := &Store{Slug: slug, path: path, w: w, r: r, Hub: hub.New()}
	if err := s.load(); err != nil {
		s.Close()
		return fail(err)
	}
	if _, err := s.CreateUser(ownerEmail, ownerName, "owner", ownerPassword); err != nil {
		s.Close()
		return fail(err)
	}
	return s, nil
}

// Open opens an existing store.
func Open(dir, slug string) (*Store, error) {
	if !ValidSlug(slug) {
		return nil, ErrNotFound
	}
	path := DBPath(dir, slug)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	w, r, err := openDB(path)
	if err != nil {
		return nil, err
	}
	s := &Store{Slug: slug, path: path, w: w, r: r, Hub: hub.New()}
	if err := s.load(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(from int) error {
	for v := from; v < schemaVersion; v++ {
		tx, err := s.w.Begin()
		if err != nil {
			return err
		}
		// Re-read the version under the write lock: another process (a second
		// server, or a CLI command) may have migrated this file meanwhile.
		var raw string
		if err := tx.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&raw); err != nil {
			tx.Rollback()
			return err
		}
		if cur, _ := strconv.Atoi(raw); cur > v {
			tx.Rollback()
			continue
		}
		var stepErr error
		switch v {
		case 1:
			_, stepErr = tx.Exec(schemaV2SQL)
		case 2:
			_, stepErr = tx.Exec(schemaV3SQL)
		default:
			stepErr = fmt.Errorf("no migration from schema v%d", v)
		}
		if stepErr == nil {
			_, stepErr = tx.Exec(`UPDATE meta SET value=? WHERE key='schema_version'`, strconv.Itoa(v+1))
		}
		if stepErr == nil {
			stepErr = tx.Commit()
		} else {
			tx.Rollback()
			return fmt.Errorf("migrate schema v%d->v%d: %w", v, v+1, stepErr)
		}
	}
	return nil
}

func (s *Store) load() error {
	var ver int
	var raw string
	if err := s.r.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&raw); err != nil {
		return fmt.Errorf("not a price-tag store file: %w", err)
	}
	ver, _ = strconv.Atoi(raw)
	if ver < 1 {
		return fmt.Errorf("not a price-tag store file: invalid schema version %q", raw)
	}
	if ver > schemaVersion {
		return fmt.Errorf("store file is schema v%d, this build understands up to v%d", ver, schemaVersion)
	}
	if ver < schemaVersion {
		if err := s.migrate(ver); err != nil {
			return err
		}
	}
	for key, dst := range map[string]*atomic.Int64{"v": &s.v, "home_rev": &s.homeRev, "purged_before": &s.purged} {
		var val string
		if err := s.r.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&val); err != nil {
			return err
		}
		n, _ := strconv.ParseInt(val, 10, 64)
		dst.Store(n)
	}
	var js string
	if err := s.r.QueryRow(`SELECT value FROM meta WHERE key='settings'`).Scan(&js); err != nil {
		return err
	}
	var set Settings
	if err := json.Unmarshal([]byte(js), &set); err != nil {
		return err
	}
	set.applyDefaults()
	s.settings.Store(&set)
	return nil
}

func (s *Store) Close() error {
	s.Hub.Close()
	e1 := s.w.Close()
	e2 := s.r.Close()
	if e1 != nil {
		return e1
	}
	return e2
}

// Version is the current data version (increments on every shopper-visible change).
func (s *Store) Version() int64 { return s.v.Load() }

func (s *Store) Settings() Settings { return *s.settings.Load() }

// ---- write transactions -------------------------------------------------

type txc struct {
	ctx      context.Context
	tx       *sql.Tx
	v        int64 // the version this transaction will become
	now      int64
	actor    string
	touched  map[string]struct{}
	home     bool
	stmts    map[string]*sql.Stmt
	onCommit func()
}

func (c *txc) touch(code string) { c.touched[code] = struct{}{} }

func (c *txc) stmt(q string) (*sql.Stmt, error) {
	if st, ok := c.stmts[q]; ok {
		return st, nil
	}
	st, err := c.tx.PrepareContext(c.ctx, q)
	if err != nil {
		return nil, err
	}
	c.stmts[q] = st
	return st, nil
}

func (c *txc) history(kind, ref, summary string, oldP, newP *int64) error {
	st, err := c.stmt(`INSERT INTO history(at,actor,kind,ref,summary,old_price,new_price) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	var o, n any
	if oldP != nil {
		o = *oldP
	}
	if newP != nil {
		n = *newP
	}
	_, err = st.ExecContext(c.ctx, c.now, c.actor, kind, ref, summary, o, n)
	return err
}

var errDryRun = errors.New("dry run")

// write runs fn in one immediate transaction. If fn changes nothing visible to
// shoppers the version is not bumped (so re-importing an identical price file
// is free). On commit it publishes the change event while still holding the
// write lock, which keeps events strictly ordered.
func (s *Store) write(ctx context.Context, actor string, dry bool, fn func(*txc) error) (*Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	c := &txc{ctx: ctx, tx: tx, now: time.Now().Unix(), actor: actor, touched: map[string]struct{}{}, stmts: map[string]*sql.Stmt{}}
	done := false
	defer func() {
		for _, st := range c.stmts {
			st.Close()
		}
		if !done {
			tx.Rollback()
		}
	}()
	// Authoritative version comes from the DB so a second process writing the
	// same file can never cause a duplicate version number.
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='v'`).Scan(&raw); err != nil {
		return nil, err
	}
	cur, _ := strconv.ParseInt(raw, 10, 64)
	c.v = cur + 1
	if err := fn(c); err != nil {
		return nil, err
	}
	if dry {
		return nil, errDryRun
	}
	if len(c.touched) == 0 && !c.home {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key='v'`, strconv.FormatInt(c.v, 10)); err != nil {
		return nil, err
	}
	if c.home {
		if _, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key='home_rev'`, strconv.FormatInt(c.v, 10)); err != nil {
			return nil, err
		}
	}
	ev, err := buildEvent(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	done = true
	if c.onCommit != nil {
		c.onCommit()
	}
	if c.home {
		s.homeRev.Store(c.v)
	}
	s.v.Store(c.v)
	s.publish(ev)
	return ev, nil
}

func (s *Store) setMeta(ctx context.Context, tx *sql.Tx, key, val string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, val)
	return err
}

// UpdateSettings changes name/announcement/etc. Currency and decimals are
// locked once products exist because prices are stored in minor units.
func (s *Store) UpdateSettings(ctx context.Context, actor string, in Settings) error {
	if err := in.normalize(); err != nil {
		return err
	}
	_, err := s.write(ctx, actor, false, func(c *txc) error {
		old := s.Settings()
		if in.Decimals != old.Decimals || in.Currency != old.Currency {
			var n int
			if err := c.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM products WHERE deleted=0`).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return invalid("currency_locked", "currency and decimals cannot change once products exist")
			}
		}
		if in == old {
			return nil
		}
		js, _ := json.Marshal(in)
		if err := s.setMeta(ctx, c.tx, "settings", string(js)); err != nil {
			return err
		}
		c.home = true
		cp := in
		c.onCommit = func() { s.settings.Store(&cp) }
		return c.history("settings", "", "Store settings changed", nil, nil)
	})
	return err
}

// Compact drops tombstones older than 90 days and records that clients whose
// cursor predates this must do a full resync. Safe to call any time.
func (s *Store) Compact(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-90 * 24 * time.Hour).Unix()
	res, err := s.w.ExecContext(ctx, `DELETE FROM products WHERE deleted=1 AND updated_at<?`, cutoff)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		cur := s.v.Load()
		if _, err := s.w.ExecContext(ctx, `UPDATE meta SET value=? WHERE key='purged_before'`, strconv.FormatInt(cur, 10)); err != nil {
			return err
		}
		s.purged.Store(cur)
	}
	now := time.Now().Unix()
	_, _ = s.w.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<?`, now)
	_, _ = s.w.ExecContext(ctx, `DELETE FROM checkouts WHERE status='open' AND expires_at<=?`, now)
	_, _ = s.w.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	_, _ = s.w.ExecContext(ctx, `PRAGMA optimize`)
	return nil
}

// Backup writes a consistent, standalone copy of the store to dst using
// VACUUM INTO (safe while the store is serving traffic).
func (s *Store) Backup(ctx context.Context, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	// A separate connection: VACUUM INTO needs a writable handle but only reads a
	// snapshot, so it never blocks the live writer or readers.
	db, err := sql.Open("sqlite", dsn(s.path, false))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return err
	}
	// Never ship live login sessions inside a copy that gets e-mailed around.
	cp, err := sql.Open("sqlite", dsn(dst, false))
	if err != nil {
		return err
	}
	defer cp.Close()
	_, err = cp.ExecContext(ctx, `DELETE FROM sessions`)
	return err
}

// Stats for the admin dashboard.
type Stats struct {
	Products     int   `json:"products"`
	ActivePromos int   `json:"active_promos"`
	Version      int64 `json:"version"`
	Live         int   `json:"live_viewers"`
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Version: s.Version(), Live: s.Hub.Count()}
	now := time.Now().Unix()
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM products WHERE deleted=0`).Scan(&st.Products); err != nil {
		return st, err
	}
	err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM promotions WHERE enabled=1 AND starts_at<=? AND ends_at>?`, now, now).Scan(&st.ActivePromos)
	return st, err
}

var _ = auth.MinPasswordLen
