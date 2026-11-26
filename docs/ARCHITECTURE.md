# Architecture

## The idea in one paragraph

A barcode is just a key. The price lives in the shop's price list. So the product is a **very fast key-to-price lookup that stays in sync with the shop's data, and tells every open phone the instant something changes**. Everything below serves that.

```
 shop's till / POS ──(price file, CSV/JSON, one URL)──┐
 owner's admin page ──(type a price / upload CSV)─────┤
                                                      ▼
                         ┌──────────────────── pricetag server (one Go binary) ───────────────────┐
                         │  one SQLite file per shop:  products · offers · staff · history        │
                         │  every change = one new "version" v, stamped on the rows it touched    │
                         │  after commit: tiny event  {p:v-1, v, items:[changed items]}           │
                         └──────────┬───────────────────────────────┬──────────────────────────────┘
                     GET /items/{code} (≈400 B)          GET /events  (one open stream per phone)
                                    ▼                               ▼
                         shopper phone (web app) ── IndexedDB cache of everything it has seen
```

## Key decisions (and why)

**Go + SQLite, one file per shop.**
Light and fast: 11 MB static binary, ~40 MB RAM idle, no database server, no cache server, no runtime to install. A lookup is one indexed read (microseconds). One file per shop means *"you own your data"* is literally true (`Settings → Download everything`), backups are file copies, a shop can be moved or self-hosted by copying one file, and one shop can never read another's data. SQLite in WAL mode with one writer and a reader pool; `synchronous=FULL` so an acknowledged price survives a power cut (shops often have no UPS).

**Server-sent events (SSE), not WebSockets or polling.**
One-way, plain HTTP, auto-recovering, trivially proxied, ~26 KB of server memory per connected phone. Polling would cost data and battery on every phone all day. The phone closes the stream after 30 s in the background.

**A version number makes sync exact and cheap.**
Every write transaction that changes something shoppers can see increments the shop's version `v` (an identical re-import changes nothing and bumps nothing). Each product row records the `rev` it last changed at. That gives:
* **live events** `{p, v, items}`: a phone checks `p == its last v` (no gap) and applies the items;
* **catch-up** after being offline/away: the phone sends its last `v`; the server folds *everything since* into **one** event (`rev > v`), or answers `reset` if too much changed (>50 items) or the cursor is too old;
* **delta download** for the offline catalogue (`rev > v`), and **ETag = rev** so a revalidation is a 304 with no body.
Tombstones (deleted products) are kept 90 days so offline phones learn about deletions.

**The phone trusts its cache only while the live stream vouches for it.**
Each cached item has `chk` (the version it was verified at). It is shown **without any network request** if the stream is live and `chk >= fresh`, where `fresh` moves forward whenever the phone had a gap or got a `reset`. Otherwise it is shown immediately but revalidated with a conditional request. Offline, it is shown and labelled "Saved 3 h ago. No connection." The result: repeat looks cost 0 bytes; first looks cost ~400 bytes.

**Offers are time-windowed on the phone.**
An offer carries `from`/`to`. The phone compares with the *server's* clock (corrected from the `Date` header, so a wrong phone clock does not matter) and schedules one timer for the next start/end. So an offer starting at 18:00 or ending at midnight needs **no** network message, and works offline. The effective price is `min(base, best running offer)`: a "deal" that costs more than the normal price is never shown.

**Small, boring client.**
Plain ES2017 (runs on Chrome 58+), no framework, no build step, DOM built with `textContent` (never `innerHTML`). The shell paints from HTML + the last saved home page before any network call; the service worker serves it offline. The scanner (6.5 KB gz) loads only when "Scan" is tapped, and the shopping cart (`web/shopper/cart.js` + `cart.css`, 5.4 KB gz) loads only when the shopper adds an item or opens the cart (or reloads with a non-empty cart). Cart state lives per shop in `localStorage` (`pt:<slug>:cart`), uses integer minor units and the same effective-price / offer-boundary logic, reprices live on SSE changes and `reset` revalidation, labels offline prices with their saved age and an outdated-total warning, and excludes unknown/unlisted items with a warning.

**Barcode scanning has three tiers.**
1. the browser's native `BarcodeDetector` (Chrome on most Android phones: fast, zero download);
2. otherwise a small built-in EAN-13/UPC-A/EAN-8 decoder (`web/shopper/decode.js`, 4 KB gz): sub-pixel edge finding (two methods), ZXing-style digit matching, GS1 check digit, layout sanity checks, and **two separate scan lines must agree**, so noise never produces a wrong code;
3. typing the number, or searching by name.
The scan loop paces itself to the phone's speed.

**Multi-tenant without a central database.**
Shops are files in the data directory; a slug is the filename (validated `^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`). The registry opens files lazily and closes idle ones. The same binary is the hosted multi-shop service *and* a single-shop self-hosted server.

## Data model (per shop)

`products(code PK, name, size, category, price INTEGER, deleted, rev, updated_at)` · `promotions` + `promo_items(promo_id, code, price)` · `users` · `sessions` (token hash) · `api_tokens` (token hash) · `history` (who changed what, old → new price) · `meta` (version `v`, `home_rev`, `purged_before`, settings JSON).
Prices are **integers in the currency's minor unit** (UGX has none, so it is just shillings; `decimals` is stored per shop and locked once products exist).
Codes are normalised (`internal/barcode`): EAN-8/UPC-A/EAN-13/GTIN-14 → 14-digit GTIN with a **verified check digit**, so a UPC-A scan and its EAN-13 spelling hit the same row, and a camera misread is rejected on the phone. Anything else (a shop's own short codes, produce PLUs) is an "internal" code.

## Concurrency model

* One writer per shop: `write()` takes a mutex, opens an `IMMEDIATE` transaction, **re-reads `v` from the database** (so even a second process on the same file cannot produce a duplicate version), applies the change, builds the event inside the transaction, commits, **then publishes while still holding the mutex** so events are strictly ordered.
* Readers use a separate pool and never block the writer (WAL).
* The hub never blocks a publisher: a phone whose 32-frame buffer fills is dropped and reconnects (it catches up from the database).
* Server shutdown cancels all streams so a restart takes ~1 s; phones reconnect with jittered back-off and catch up (measured below).

## Security model (summary; details in REPORT.md)

* Passwords: argon2id (OWASP minimum parameters), hashing concurrency-limited. Sessions: 256-bit random tokens, only the SHA-256 stored, `HttpOnly`, `SameSite=Strict`, `Secure` behind TLS, 14-day sliding expiry; disabling a user or changing a password signs them out everywhere.
* CSRF: custom header **and** same-origin check on every state-changing admin request.
* Integration keys: scoped to *push products only*, stored hashed, shown once, revocable, rate-limited.
* Roles: `owner` (everything) and `editor` (prices, products, offers). The last active owner cannot be removed.
* Public API is read-only and rate-limited per IP **sized for a shop's shared Wi-Fi IP**; login throttles count only *failed* attempts.
* CSP `default-src 'self'`, no inline script/style, no `eval`; all server text is inserted as text.
* Every price/offer change is recorded in `history` with the actor.
* Database exports/backups drop live session rows.

## What is deliberately *not* in the design

* No accounts or personal data for shoppers.
* No dependency on the shop's POS beyond a price file (see POS-INTEGRATION.md).
* No central database, queue, cache or message broker.
