package store

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"pricetag/internal/hub"
)

// ItemPromo is one offer on one product. Clients decide whether it is active
// by comparing From/To with (server-corrected) time, so an offer ending needs
// no push at all.
type ItemPromo struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Price int64  `json:"price"`
	From  int64  `json:"from"`
	To    int64  `json:"to"`
}

// Item is what a shopper sees for one scanned product. Prices are integers in
// the store's minor unit (UGX has none, so it is simply shillings).
type Item struct {
	Code     string      `json:"code"`
	Name     string      `json:"name"`
	Size     string      `json:"size,omitempty"`
	Category string      `json:"category,omitempty"`
	Price    int64       `json:"price"`
	Promos   []ItemPromo `json:"promos,omitempty"`
	Rev      int64       `json:"rev"`
}

// Event is one version step (or, for catch-up, several folded together).
type Event struct {
	Prev  int64    `json:"p"`
	V     int64    `json:"v"`
	Items []*Item  `json:"items,omitempty"`
	Gone  []string `json:"gone,omitempty"`
	Home  bool     `json:"home,omitempty"`
	Reset bool     `json:"reset,omitempty"`
}

// SSE renders the event as a server-sent-event frame.
func (e *Event) SSE() []byte {
	b, _ := json.Marshal(e)
	var sb strings.Builder
	sb.WriteString("id: ")
	sb.WriteString(strconv.FormatInt(e.V, 10))
	sb.WriteString("\nevent: change\ndata: ")
	sb.Write(b)
	sb.WriteString("\n\n")
	return []byte(sb.String())
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func placeholders(n int) string {
	if n == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// itemsFor loads current records for codes. Codes that are missing or deleted
// are returned in gone. Offers that already ended are left out.
func itemsFor(ctx context.Context, q querier, codes []string, now int64) (items []*Item, gone []string, err error) {
	byCode := make(map[string]*Item, len(codes))
	const chunk = 400
	for i := 0; i < len(codes); i += chunk {
		part := codes[i:min(i+chunk, len(codes))]
		rows, err := q.QueryContext(ctx, `SELECT code,name,size,category,price,rev FROM products WHERE deleted=0 AND code IN (`+placeholders(len(part))+`)`, toAny(part)...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			it := &Item{}
			if err := rows.Scan(&it.Code, &it.Name, &it.Size, &it.Category, &it.Price, &it.Rev); err != nil {
				rows.Close()
				return nil, nil, err
			}
			byCode[it.Code] = it
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
		if len(byCode) == 0 {
			continue
		}
		args := append(toAny(part), now)
		prow, err := q.QueryContext(ctx, `SELECT pi.code,p.id,p.title,pi.price,p.starts_at,p.ends_at
			FROM promo_items pi JOIN promotions p ON p.id=pi.promo_id
			WHERE pi.code IN (`+placeholders(len(part))+`) AND p.enabled=1 AND p.ends_at>?
			ORDER BY p.starts_at, p.id`, args...)
		if err != nil {
			return nil, nil, err
		}
		for prow.Next() {
			var code string
			var ip ItemPromo
			if err := prow.Scan(&code, &ip.ID, &ip.Title, &ip.Price, &ip.From, &ip.To); err != nil {
				prow.Close()
				return nil, nil, err
			}
			if it := byCode[code]; it != nil {
				it.Promos = append(it.Promos, ip)
			}
		}
		if err := prow.Err(); err != nil {
			prow.Close()
			return nil, nil, err
		}
		prow.Close()
	}
	for _, code := range codes {
		if it, ok := byCode[code]; ok {
			items = append(items, it)
		} else {
			gone = append(gone, code)
		}
	}
	return items, gone, nil
}

func buildEvent(ctx context.Context, tx *sql.Tx, c *txc) (*Event, error) {
	ev := &Event{Prev: c.v - 1, V: c.v, Home: c.home}
	if len(c.touched) > MaxInline {
		ev.Reset = true
		ev.Home = true
		return ev, nil
	}
	codes := make([]string, 0, len(c.touched))
	for code := range c.touched {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	var err error
	ev.Items, ev.Gone, err = itemsFor(ctx, tx, codes, c.now)
	if err != nil {
		return nil, err
	}
	if !ev.Home && len(codes) > 0 {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM promo_items WHERE code IN (`+placeholders(len(codes))+`) LIMIT 1`, toAny(codes)...).Scan(&one)
		if err == nil {
			ev.Home = true
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}
	return ev, nil
}

func (s *Store) publish(ev *Event) {
	s.homeMu.Lock()
	s.homeCache = nil
	s.homeMu.Unlock()
	s.Hub.Publish(hub.Frame{V: ev.V, Data: ev.SSE()})
}

// Lookup returns the current record for a canonical code, or ErrNotFound.
func (s *Store) Lookup(ctx context.Context, code string) (*Item, error) {
	items, _, err := itemsFor(ctx, s.r, []string{code}, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return items[0], nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Search finds products by name or code fragment (used when a barcode is
// damaged or unreadable). Limit is capped at 30.
func (s *Store) Search(ctx context.Context, q string, limit int) ([]*Item, error) {
	q = strings.TrimSpace(q)
	if len([]rune(q)) < 2 {
		return nil, nil
	}
	if len(q) > 60 {
		q = q[:60]
	}
	limit = max(1, min(limit, 30))
	like := "%" + escapeLike(q) + "%"
	rows, err := s.r.QueryContext(ctx, `SELECT code FROM products WHERE deleted=0 AND (name LIKE ? ESCAPE '\' OR code LIKE ? ESCAPE '\')
		ORDER BY (name LIKE ? ESCAPE '\') DESC, name COLLATE NOCASE LIMIT ?`, like, like, escapeLike(q)+"%", limit)
	if err != nil {
		return nil, err
	}
	var codes []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return nil, err
		}
		codes = append(codes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items, _, err := itemsFor(ctx, s.r, codes, time.Now().Unix())
	return items, err
}

// CatchUp folds everything since `since` into one event, so a phone that was
// offline (or whose connection dropped) is brought up to date in one message.
// It returns nil when the client is already current.
func (s *Store) CatchUp(ctx context.Context, since int64) (*Event, error) {
	cur := s.v.Load()
	if since == cur {
		return nil, nil
	}
	ev := &Event{Prev: since, V: cur}
	if since > cur || since < s.purged.Load() || since < 0 {
		ev.Reset, ev.Home = true, true
		return ev, nil
	}
	ev.Home = s.homeRev.Load() > since
	rows, err := s.r.QueryContext(ctx, `SELECT code FROM products WHERE rev>? LIMIT ?`, since, MaxInline+1)
	if err != nil {
		return nil, err
	}
	var codes []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return nil, err
		}
		codes = append(codes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(codes) > MaxInline {
		ev.Reset, ev.Home = true, true
		return ev, nil
	}
	ev.Items, ev.Gone, err = itemsFor(ctx, s.r, codes, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	if len(codes) > 0 {
		ev.Home = true // cheap and always correct: promo membership may have changed
	}
	return ev, nil
}

// ---- catalogue download (opt-in offline mode) ----------------------------

type CatalogHeader struct {
	V     int64 `json:"v"`
	Since int64 `json:"since"`
	Full  bool  `json:"full"`
	Total int   `json:"total,omitempty"`
}

type CatalogGone struct {
	Gone string `json:"gone"`
}

type CatalogEnd struct {
	End bool `json:"end"`
	N   int  `json:"n"`
}

// StreamCatalog emits a header, then one JSON object per changed product
// (since==0 or an unusable cursor means "everything"), then an end marker so a
// truncated download is detectable. Everything is read from a single snapshot.
func (s *Store) StreamCatalog(ctx context.Context, since int64, emit func(any) error) error {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='v'`).Scan(&raw); err != nil {
		return err
	}
	v, _ := strconv.ParseInt(raw, 10, 64)
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='purged_before'`).Scan(&raw); err != nil {
		return err
	}
	purged, _ := strconv.ParseInt(raw, 10, 64)
	full := since <= 0 || since > v || since < purged
	if full {
		since = 0
	}
	hdr := CatalogHeader{V: v, Since: since, Full: full}
	if full {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM products WHERE deleted=0`).Scan(&hdr.Total); err != nil {
			return err
		}
	}
	if err := emit(hdr); err != nil {
		return err
	}
	now := time.Now().Unix()
	promos := map[string][]ItemPromo{}
	prow, err := tx.QueryContext(ctx, `SELECT pi.code,p.id,p.title,pi.price,p.starts_at,p.ends_at
		FROM promo_items pi JOIN promotions p ON p.id=pi.promo_id WHERE p.enabled=1 AND p.ends_at>? ORDER BY p.starts_at,p.id`, now)
	if err != nil {
		return err
	}
	for prow.Next() {
		var code string
		var ip ItemPromo
		if err := prow.Scan(&code, &ip.ID, &ip.Title, &ip.Price, &ip.From, &ip.To); err != nil {
			prow.Close()
			return err
		}
		promos[code] = append(promos[code], ip)
	}
	prow.Close()
	if err := prow.Err(); err != nil {
		return err
	}
	q := `SELECT code,name,size,category,price,rev,deleted FROM products WHERE rev>?`
	if full {
		q += ` AND deleted=0`
	}
	rows, err := tx.QueryContext(ctx, q, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var it Item
		var del int
		if err := rows.Scan(&it.Code, &it.Name, &it.Size, &it.Category, &it.Price, &it.Rev, &del); err != nil {
			return err
		}
		if del == 1 {
			err = emit(CatalogGone{Gone: it.Code})
		} else {
			it.Promos = promos[it.Code]
			err = emit(&it)
		}
		if err != nil {
			return err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return emit(CatalogEnd{End: true, N: n})
}

// ---- home page ------------------------------------------------------------

type HomeStore struct {
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Currency     string `json:"currency"`
	Decimals     int    `json:"decimals"`
	TZOffset     int    `json:"tz_offset"`
	Announcement string `json:"announcement,omitempty"`
}

type HomePromoItem struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	Size       string `json:"size,omitempty"`
	Price      int64  `json:"price"`
	PromoPrice int64  `json:"promo_price"`
}

type HomePromo struct {
	ID    int64           `json:"id"`
	Title string          `json:"title"`
	Text  string          `json:"text,omitempty"`
	From  int64           `json:"from"`
	To    int64           `json:"to"`
	Items []HomePromoItem `json:"items,omitempty"`
	More  int             `json:"more,omitempty"`
}

type HomeData struct {
	Store  HomeStore   `json:"store"`
	Promos []HomePromo `json:"promos"`
	V      int64       `json:"v"`
}

type HomeSnapshot struct {
	JSON []byte
	ETag string
	V    int64
	At   int64
}

const (
	homeMaxPromos = 20
	homeMaxItems  = 12
	homeLookahead = 7 * 24 * 3600
	homeTTL       = 5 // seconds; offers are time-windowed, clients also filter
)

// Home returns the store's home-page payload (store info + live and upcoming
// offers), cached for a few seconds and dropped on any change.
func (s *Store) Home(ctx context.Context) (*HomeSnapshot, error) {
	now := time.Now().Unix()
	s.homeMu.Lock()
	if h := s.homeCache; h != nil && now-h.At < homeTTL {
		s.homeMu.Unlock()
		return h, nil
	}
	s.homeMu.Unlock()
	v := s.v.Load()
	set := s.Settings()
	data := HomeData{V: v, Promos: []HomePromo{}, Store: HomeStore{Slug: s.Slug, Name: set.Name, Currency: set.Currency,
		Decimals: set.Decimals, TZOffset: set.TZOffset, Announcement: set.Announcement}}
	rows, err := s.r.QueryContext(ctx, `SELECT id,title,text,starts_at,ends_at FROM promotions
		WHERE enabled=1 AND ends_at>? AND starts_at<? ORDER BY (starts_at<=?) DESC, ends_at ASC, id LIMIT ?`,
		now, now+homeLookahead, now, homeMaxPromos)
	if err != nil {
		return nil, err
	}
	idx := map[int64]int{}
	var ids []any
	for rows.Next() {
		var p HomePromo
		if err := rows.Scan(&p.ID, &p.Title, &p.Text, &p.From, &p.To); err != nil {
			rows.Close()
			return nil, err
		}
		idx[p.ID] = len(data.Promos)
		ids = append(ids, p.ID)
		data.Promos = append(data.Promos, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		irows, err := s.r.QueryContext(ctx, `SELECT pi.promo_id,p.code,p.name,p.size,p.price,pi.price
			FROM promo_items pi JOIN products p ON p.code=pi.code
			WHERE p.deleted=0 AND pi.promo_id IN (`+placeholders(len(ids))+`)
			ORDER BY pi.promo_id, (p.price-pi.price)*1000/MAX(p.price,1) DESC, p.name COLLATE NOCASE`, ids...)
		if err != nil {
			return nil, err
		}
		for irows.Next() {
			var pid int64
			var it HomePromoItem
			if err := irows.Scan(&pid, &it.Code, &it.Name, &it.Size, &it.Price, &it.PromoPrice); err != nil {
				irows.Close()
				return nil, err
			}
			p := &data.Promos[idx[pid]]
			if len(p.Items) < homeMaxItems {
				p.Items = append(p.Items, it)
			} else {
				p.More++
			}
		}
		irows.Close()
		if err := irows.Err(); err != nil {
			return nil, err
		}
	}
	js, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum(js)
	snap := &HomeSnapshot{JSON: js, ETag: `"` + hex.EncodeToString(sum[:8]) + `"`, V: v, At: now}
	s.homeMu.Lock()
	if s.v.Load() == v { // don't cache something already superseded
		s.homeCache = snap
	}
	s.homeMu.Unlock()
	return snap, nil
}
