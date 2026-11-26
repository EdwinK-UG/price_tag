# Testing

```sh
make test-all          # everything below (about 3-4 minutes)
make test-go           # Go unit + integration tests, with the race detector
make test-js           # barcode decoder + web-asset guards (Node, no browser)
make test-e2e          # real browser tests against the real binary
```

Browser tests need Node 20+, `cd tests && npm install`, Playwright's Chromium, and ImageMagick (`convert`) for the fake-camera fixtures. They start the production binary (`bin/pricetag`) on a free port with a freshly seeded demo shop, so they exercise exactly what ships.

## What each suite proves

### Go (`internal/…`, 65 tests + 4 fuzzers, run with `-race`)

| Area | Examples of what is asserted |
|---|---|
| `barcode` | check digits; UPC-A and EAN-13 spellings are the same product; misreads rejected; canonical form is idempotent (fuzzed, 1.7 M executions) |
| `store` | version bumps only on real change; identical re-import is free; events carry the right items; catch-up folds many changes into one (including promo changes bumping `products.rev`); large commits send `reset`; tombstones, compaction, forced resync; promo rules (offer must be lower; ended/scheduled offers); CSV dialects (BOM, `;`, tabs, header aliases); CSV/formula-injection guard round trip; money parsing (fuzzed); users/sessions/last-owner protection; token hashing; backups contain no live sessions; **40 concurrent writers get strictly ordered versions while readers run**; a second process on the same file cannot duplicate versions; **schema v1 → v2 migration**, checkout code alphabet & `NormalizeCheckoutCode` (fuzzed), idempotent reuse on unchanged cart, new code on change, live price & offer re-check, atomic one-time verify + audit trail, expired open checkout cleanup (preserving verified checkouts), `MaxOpenCheckouts` cap |
| `hub` | fan-out to 100; slow subscribers are dropped, fast ones are not; limits; close |
| `auth` | argon2id round trip; tampered hash parameters rejected; tokens unique |
| `server` | lookup is ≤300 bytes, 304 on matching ETag (and ETag invalidation on promo create/delete), `X-PT-V`; path-traversal slugs rejected; **a price edit becomes an SSE event with the right item**; catch-up via `?since=` and `Last-Event-ID`; stream limits per IP; CSRF (missing header, cross-origin); cookie flags; session isolation between shops; editor cannot reach owner endpoints; audit trail names the actor; ingest with CSV/JSON, dry run, idempotency, revocation; operator API (off by default); login and account throttling, and that **successful logins don't count**; catalogue cached per version; static pages, security headers on HTML only; **checkout API**: off by default (`404`), `POST /checkouts` server-side pricing & reuse, `GET /admin/checkouts/{code}` byte-identical `404` on invalid/missing/expired + `409` on verified, editor & owner verification + CSRF + audit log, per-IP and per-shop rate limiting with refund on valid lookup |
| `demo` | the seed is complete and valid; seeding twice is a no-op |

### JS unit (`tests/js`, 33 tests)

* **Decoder**: clean codes at module widths 2–5 px = 100 %; blur σ≈1 px (3 px modules) = 100 %, σ≈2 px (4 px modules) = 100 %; noise, uneven light, ±8–12° tilt, upside-down, held sideways ≥ 90–100 %; a realistic mix of all ≥ 90 %; **0 false positives on 600 noise/stripe frames; 300 deliberately scratched barcodes never produced a wrong code** (154 still read correctly, the rest returned nothing); a barcode cut off at the frame edge returns nothing.
* Client/server **barcode rules agree** (same table as the Go test).
* **Cart (`cart.test.js`, 7 tests)**: effective price `min(base, running offer)`, integer totals and overflow guard, unknown-price exclusion, live/offer-boundary/deletion/restore repricing, per-shop `localStorage` persistence and error/corruption resilience, order-independent cart `fingerprint()` and code formatting.
* **Asset guards** (these fail the build if the promise regresses): code parses as ES2017 (old phones), shopper app ≤ 16 KB gzipped (15.6 KB), lazy cart + checkout ≤ 8 KB gzipped (6.8 KB), scanner ≤ 8 KB gzipped (6.3 KB), no inline script/style/handlers, no `innerHTML`/`eval`, service worker never touches `/api/` or admin.

### Browser (`tests/e2e`, 56 tests)

| File | Proves |
|---|---|
| `shopper` | home page, offers, typing a barcode, offer price vs normal price, misread barcode caught **without a network call**, unknown product, name search, Back button closes the sheet, recents persist |
| `cart` | **add to cart, qty controls, remove, reload persistence, clear-cart confirmation**; live SSE price change reprices cart with `"Price changed"` marker; offer start/end reprices cart automatically; offline cart labels each price with its saved age and marks totals outdated, then catches up online; unknown & live-deleted products show `"Price unknown"` and are excluded with a warning; cross-tab badge sync & 320 px layout |
| `checkout` | **checkout is off by default** (button hidden, QR lib not loaded); enabling in Settings updates shopper live; **Checkout creates 6-char unambiguous code + SVG QR, reuses code on unchanged cart, creates new code on change**; cashier (`editor` role) sees live price changes (both old and current price), ticks items off, verifies order, blocks code reuse, and records verification in Activity; per-shop `"rescan"` default mode works and expired/unknown codes show the exact same error |
| `live` | **a price edit reaches an open phone: p50 12–19 ms, p95 21–37 ms over 25 edits**; flash + "Price just changed"; 5 phones all update; offers appear/disappear live; an offer **ends by itself** and a **scheduled** one starts by itself with no network message; removed/added products; a base price below a running offer shows the lower price; a 160-row bulk import refreshes the open item through one reset hint; re-sending the same file pushes nothing |
| `offline` | seen prices and the app itself open with no connection; changes made while offline are applied on reconnect (17–35 ms after the network returns); the whole price list can be saved and then unseen products + name search work offline; **server restart** is invisible to phones (new price shown < 0.7 s after the server is back); **restoring an older backup**: phones that were ahead discard their stale cache and show the restored data |
| `scan` | **a fake webcam showing a barcode → price on screen** (fallback decoder, normal & 6× slower CPU); unknown barcode; native `BarcodeDetector` path (stubbed) incl. UPC-E expansion; a native detector that throws falls back; camera denied / missing / busy / insecure are explained and typing still works |
| `slowphone` | CPU 6× slower **and** Slow-3G / Fast-3G: see numbers in REPORT.md; budgets enforced (cold visit < 30 KB, lookup < 650 B, repeat lookup = 0 requests, repeat visit < 6 KB) |
| `admin` | sign-in/out, HttpOnly+SameSite cookie, inline price edit reaches a phone, bad barcode/price messages, offer creation + end-now reflected on phones, offer validation, CSV import preview-then-apply with row errors, team (editor can't see Team/Integrations; API returns 403), integration key shown once / works / revoked, settings rename reaches phones, QR poster drawn, activity & history, database + CSV export |
| `pages` | every page loads styled with **zero console errors/CSP violations**; **the printable test sheet is scannable: every barcode it draws decodes back to its product with the app's own decoder**; 320 px screens don't scroll sideways; dark mode screenshots |

Artifacts (latency, perf numbers, screenshots) are written to `tests/e2e/.artifacts/`.

## Load test (not part of `make test-all`)

```sh
./bin/pricetag store create -data /tmp/lt -slug bench -name Bench -owner-email o@bench.example -password bench-password-1
./bin/pricetag serve -addr 127.0.0.1:8090 -data /tmp/lt -public-rps 1000000 -public-burst 1000000 -max-sse-per-ip 100000 &
ulimit -n 65536
go run ./tools/loadtest -url http://127.0.0.1:8090 -slug bench -email o@bench.example -password bench-password-1 \
        -products 50000 -conc 128 -duration 8s -sse 5000 -server-pid $!
```

## Testing on a real phone

1. `make demo-phone`, open the printed `https://<ip>:8443/s/demo/` on the phone (accept the certificate warning).
2. Open `/s/demo/test-sheet` on your computer and scan the codes from the screen; then try **printed** codes and **real packaged products** (see "Field test" in REPORT.md: this is the main thing not yet tested).
3. Watch `./bin/pricetag demo-tick` change prices while the phone shows an item.
4. Switch the phone to airplane mode: saved prices should still open and say "No connection".

## Fuzzing

```sh
go test ./internal/barcode -run xxx -fuzz FuzzCanonical -fuzztime 30s
go test ./internal/store   -run xxx -fuzz FuzzParseMoney -fuzztime 30s
go test ./internal/store   -run xxx -fuzz FuzzParseCSV   -fuzztime 30s
go test ./internal/store   -run xxx -fuzz FuzzNormalizeCheckoutCode -fuzztime 10s
```
