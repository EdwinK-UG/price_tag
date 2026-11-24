# Price Tag: project report

*Written for the owner and for an independent reviewer. It says what was built, how it was checked, what the numbers are, and, just as importantly, what is **not** done or **not verified**. Where something is a judgement call rather than a fact, it says so.*

---

## 1. What you asked for, and where it is

| You asked for | Status | Evidence |
|---|---|---|
| Scan a barcode, get the price and details | **Built** | `web/shopper`; browser tests `shopper`, `scan` |
| Very light, very fast | **Built, measured** | shopper app **14.5 KB gzipped**; first visit on Slow 3G shows content in **1.9 s** having downloaded **16.9 KB**; a price lookup is **406 bytes**; repeat lookups **0 bytes**; server 11.7 MB binary, ~40 MB RAM |
| A price changed by the owner shows up very quickly | **Built, measured** | edit → phone screen **p50 12 ms, p95 21 ms** (25 edits, local); reaches **5 000 connected phones in 26 ms** |
| Promotions appear on the shop's home page | **Built** | Admin → Offers; live on the home page and on each product's price; start/end by themselves, even offline |
| Each store owns its data / is customisable per store | **Built** | one SQLite file per shop; owner can download everything; per-shop name, announcement, currency, time zone, staff, keys; same binary can be self-hosted |
| Stores sign up | **Built (operator-assisted)** | CLI `store create` and an operator API for your own sign-up form. No public self-service sign-up page (see §6) |
| Ugandan market, but high quality | **Built** | UGX integer pricing, East Africa Time offsets, in-store (prefix-2) demo barcodes, low-data design, works on weak phones/networks; English only (see §6) |
| Works with a POS you did not build | **Built (file-based)** | `POST /ingest` + `pricetag push` + CSV import with preview; see `docs/POS-INTEGRATION.md` |
| Slow Android phones, every shortage considered | **Largely** | CPU 6× + Slow-3G tested in a browser; **not** tested on real handsets (see §6, risk #1) |
| A way for us to test | **Built** | `make demo`, printable test barcodes, `demo-tick`, `make demo-phone`, 132 automated tests (56 Go + 3 fuzzers + 26 JS + 47 browser) |
| A thorough report for review | **Done** | this document |

> **Cloud environment ("price tag"):** you also asked me to propose network, secrets and variables for the cloud environment and *ask before saving*. I **saved nothing**. Everything needed was already in the container (Go 1.24, Node 22, Chromium, ImageMagick) and the network is unrestricted, so there was no concrete need for a change. See §9.

## 2. How it works, in brief

One Go binary serves three things from embedded files: the **shopper web app**, the **store-owner admin**, and a **JSON/SSE API**. Each shop is **one SQLite file**. Every change that shoppers can see increments a per-shop **version**; changed rows are stamped with it. After a commit, a tiny event with the changed items goes to every open phone over one **server-sent-events** stream. A phone that was away sends its last version and receives **one** folded catch-up message. Full detail: `docs/ARCHITECTURE.md`.

## 3. Decisions that differ from what I told you earlier in the chat (please read)

In the first answer I suggested FastAPI + Postgres + Redis + React Native and syncing the **whole catalogue to every phone**. After you described Uganda (slow phones, expensive data, patchy networks) I changed course:

1. **Go + SQLite (one file per shop) instead of Postgres/Redis.** Far lighter to run (no database server to operate or pay for), and it makes "each shop owns its data" literally true. Trade-off: SQLite means one writer per shop and one machine (no clustering). For shops that is ample (a price edit is one tiny transaction) but it is not a multi-region architecture.
2. **A web app (installable, offline-capable) instead of a native app.** Nothing to download from a store; a QR code at the entrance is enough; one codebase. Trade-off: the camera needs HTTPS, and scanning quality depends on the phone's browser (mitigated by three scanning tiers).
3. **Lookup per scan + local cache by default; downloading the whole catalogue is opt-in.** A 1 MB download is a real cost on prepaid data and a slow-phone burden, and most shoppers scan a handful of items. The full download (and offline mode) exists for shops with bad signal.
4. **Live pushes use SSE, and only changed items.** Polling would cost battery and data all day.

## 4. Measured results

All measurements: this repository's own tests on a 4-vCPU container, Chromium 1243 (Playwright), local loopback. They show the software's own cost, **not** a real mobile network or a real handset. Numbers are from the final clean run.

### 4.1 Shopper experience (CPU 6× slower **and** emulated mobile network)

| | Slow 3G (400 kbit/s, 400 ms RTT) | Fast 3G (1.6 Mbit/s, 150 ms RTT) |
|---|---|---|
| First visit ever: first paint | 1.06 s | 0.47 s |
| First visit: offers visible | **1.94 s** | **1.14 s** |
| First visit: data downloaded (page, CSS, JS, offers) | **16.9 KB** (5 requests) | same |
| First price lookup (type code → price on screen) | **0.98 s** | **0.40 s** |
| …bytes for that lookup (incl. HTTP headers) | **406 B** | same |
| Same product again while live | **0.12 s, 0 requests** | 0.17 s, 0 requests |
| Next visit (service worker + saved page) | **0.22 s, 161 B** | 0.21 s, 161 B |

Lookup time on Slow 3G is almost entirely the 400 ms round trip (nothing left to remove); the app work is small.

### 4.2 Live updates

| Scenario | Result |
|---|---|
| Admin edit → price changed on an open phone (25 edits, including the HTTP edit itself) | min 4 · **p50 12** · **p95 21** · max 33 ms |
| Phone returns online after missing 3 edits | correct price on screen **17 ms** after the network is back (one catch-up message) |
| Server restarts; edit made right after | phone reconnects and shows the new price **0.7 s** after the server is back |
| Offer ends / starts while a phone is open | switches at the exact second, no network message |
| Identical price file re-sent | 0 changes, version unchanged, **0 events** |
| 160-row bulk import | one `reset` hint; open item and recents revalidated |

### 4.3 Scanning

| | Result |
|---|---|
| Fake webcam showing a barcode (fallback decoder) → price on screen | **274 ms** (includes camera start and lazy-loading the scanner) |
| Same with CPU 6× slower | **652 ms** |
| Decoder, synthetic 640×480 frames | clean (module 2–5 px) 100 %; blur σ≈1 px 100 %; σ≈2 px (4 px modules) 100 %; upside-down ≥ 97 %; sideways ≥ 95 %; tilt ≤12° ≥ 90 %; noise/uneven light ≥ 90–95 %; realistic mix ≥ 90 % |
| **Wrong results** | **0** over 600 noise/stripe frames and 300 scratched barcodes (154 of those still read correctly; the rest correctly returned nothing) |
| Decode cost | 0.04 ms (hit) / ~1 ms (worst miss) per frame on a desktop CPU |
| The printable test sheet | every barcode it draws decodes back to the right product |

### 4.4 Server (one machine, load generator on the same 4 vCPUs, so conservative)

| | |
|---|---|
| Lookups, 128 concurrent clients, 50 000-product shop | **16 238 req/s**, p50 6.8 ms, p95 17.6 ms, p99 25.6 ms, 0 errors |
| Import 50 000 products (2.9 MB CSV) | 1.96 s (one version, one event) |
| Re-send the same file | 0.75 s, nothing pushed |
| 5 000 live phones held open | **171 MB** total server memory (~26 KB per phone) |
| One price change → all 5 000 phones | p50 15 ms, p95 25 ms, last 26 ms |

### 4.5 Quality gates

* **Go**: 56 tests + 3 fuzzers (barcode canonical form 1.7 M executions, money 200 k, CSV 275 k, no failures), all under `-race`, `go vet` clean.
* **JS unit**: 26 tests.
* **Browser**: 47 tests, 0 console errors or CSP violations on any page.
* Asset budgets enforced in CI-able tests: shopper app ≤ 16 KB gzipped (14.5), scanner ≤ 8 KB (6.5), code parses as **ES2017** (Chrome 58+).

## 5. Security review (my own; please re-check independently)

**Built in:** argon2id passwords; random 256-bit sessions stored only as hashes (`HttpOnly`, `SameSite=Strict`, `Secure` behind TLS); CSRF header + same-origin check; scoped, hashed, revocable integration keys; roles with last-owner protection; per-IP and per-account throttles that count only failed logins; strict CSP (no inline script/style/eval); all server text inserted via `textContent` (a test fails the build on `innerHTML`/`eval`); CSV export guards against spreadsheet formula injection (round-trips on import); slugs validated against path traversal; request body and row limits; bounded in-memory limiter maps; database backups strip live sessions; `synchronous=FULL` so an acknowledged edit survives a power cut; an audit trail of who changed which price.

**Weaknesses I know about (not fixed):**

1. **Prices are public.** Anyone who knows a shop's URL can read its prices (and download its catalogue). That is true of shelf prices too, but a competitor could scrape a whole shop. Per-IP rate limits only slow it. A per-shop "private mode" (in-store code / IP allow-list) would fix it; it is not built.
2. **Per-IP limits are a blunt tool.** Shops and mobile carriers put many people behind one IP, so I sized the defaults for a whole shop (100 req/s, 1 000 live connections per IP). A determined attacker with many IPs is not stopped; use Caddy/Cloudflare for that.
3. **No 2FA, no e-mail password reset** (reset is an operator CLI command). Staff passwords are 8+ characters with no breach check.
4. **No metrics/alerting.** Only `/healthz` and logs.
5. **Single process, single machine.** No high availability. The files are portable, so recovery = restore a backup elsewhere, but there will be downtime.
6. **The operator token** (if enabled) is a single shared secret that can create shops. Keep it out of browsers; put the operator API behind your own sign-up server.
7. The SQLite driver (`modernc.org/sqlite`) is a pure-Go translation of SQLite; mature, but a different code path from the C original. `golang.org/x/crypto` and the driver should be updated routinely (`go list -m -u all`).
8. Data protection: shoppers have no accounts and the app stores no personal data about them (IP addresses appear in access logs only if `-access-log` is on, and in memory for rate limiting). Shop staff e-mails/names are stored. **Check Uganda's Data Protection and Privacy Act obligations (registration, privacy notice) before launch; I am not a lawyer.**

## 6. Not done / not verified (be sceptical of anything built on these)

**Verification gaps: the biggest risks**

1. **No real phone has ever run this.** Everything was tested in headless Chromium with CPU and network throttling and a *fake webcam* playing synthetic barcode images. Real cameras add glare, curved/shiny packaging, motion blur, autofocus hunting, low light and a hundred device quirks. The decoder has not seen a photograph of a real product. **Do a field test**: 30–50 real products, 3–4 phones (include the cheapest Android you expect, and an iPhone), in an actual aisle's lighting. If the built-in decoder under-performs, the planned stage 2 is lazy-loading ZXing-WASM (MIT wrapper around Apache-2.0 zxing-cpp, ~400 KB gz) *only* after ~8 s of failed scanning. (Chrome on most Android phones uses the native `BarcodeDetector`, which is much better; that path is tested only with a stub.)
2. **Not tested on:** iOS Safari, Firefox, Samsung Internet, UC/Opera Mini (Opera Mini's "extreme" data-saving mode most likely cannot run this at all), real Wi-Fi/NAT behaviour, real carrier proxies (some mobile networks buffer or cut idle streams; the 20 s heartbeat and automatic reconnect are designed for it but untested).
3. **Docker image, compose file, Caddyfile, systemd unit, CI workflow, backup script: written, never run** (no Docker daemon or systemd in the build environment).
4. TLS-only features (secure cookies, HSTS, camera) were exercised only through `localhost` and the `-dev-tls` option's code path, not through a real certificate.
5. No accessibility audit with a screen reader (TalkBack); I used labels, roles, live regions and large touch targets, nothing more.
6. No long-run soak test (days), no test of a disk-full or corrupted-file scenario.

**Features not built**

* **Weighed / price-embedded barcodes** (prefix `2`, e.g. deli, produce). The price is inside the barcode; today such codes are looked up as normal codes and will usually be "not found". Needs a per-shop rule (which prefix, where the price digits are). Important for supermarkets with a deli counter.
* **Public self-service shop sign-up** and billing. (CLI + operator API exist.)
* **Languages:** English only. Luganda/Swahili strings need native-speaker work; the UI text is already collected in the JS files for translation.
* Unit prices (price per kg), stock levels, aisle/location, product photos, categories browsing, VAT breakdown, multi-branch price books, scanning barcodes in the admin to add products, recurring offers.
* **Time zones are a fixed offset** (East Africa Time = UTC+3, no daylight saving); fine for Uganda, wrong for DST regions.
* Admin has no undo; the activity log is the recovery aid.
* No schema migrations (only version 1 exists); the version check is in place.
* The offline **"load prices from a file / over USB"** idea and dedicated cheap hardware you asked about are **not built** (see §8).

## 7. Known limitations of the design (not bugs)

* The phone only knows an item's current price if it has the live stream or asks; offline it shows the saved copy, **labelled with its age**, never as current.
* After a very large change (>50 items in one commit) phones revalidate only what is on screen, not everything they ever cached; cached items older than the change are re-checked the next time they are opened.
* A promotion's effective price is `min(normal, running offer)`; it is shown even if the shop mis-sets an offer above the normal price (the admin refuses that at creation, but a later price drop can cross it).
* If a shop's own price file and an Admin offer both describe the same discount, the owner will see both (documented in POS-INTEGRATION.md).

## 8. Your side questions (answered in the chat; summarised here)

* **Offline in small towns:** the app can save the whole price list on the phone (≈1 MB per 20 000 products; later syncs only fetch changes) and then needs no signal. A cheap second-hand Android phone running it is the lowest-cost "price checker"; sync once in the morning over the owner's mobile hotspot (a few shillings of data). Bluetooth is a worse sync path than a hotspot or USB.
* **A calculator-like device:** possible (ESP32 + $5–8 barcode module + 2-line LCD/e-ink + battery, ≈ $15–25 parts; 20 000 prices ≈ 1 MB in flash; sync by USB/Wi-Fi). It needs custom firmware and a compact export from the server; only worth it at volume. Not built.

## 9. Cloud environment

Name "price tag", personal, **unrestricted network**, no variables, no secrets, no setup script, no repositories attached. The container already has Go 1.24, Node 22, Chromium (Playwright) and ImageMagick, and the Go modules/npm packages download over the open network. **I saved no changes.** If you want faster session starts, a setup script could do `cd tests && npm install --no-package-lock acorn` (the only package not pre-installed). No secrets or variables are needed for development. For a real deployment the only secret is `OPERATOR_TOKEN` (and the TLS certificate that Caddy fetches itself).

## 10. Recommended next steps (in priority order)

1. **Field test with real phones and real products** (§6.1). This decides whether stage-2 scanning is needed.
2. **Deploy once for real** (a small VPS + Caddy), then run the load test against it, test through a real certificate and over a real mobile network, and **restore from a backup once** to prove it.
3. **Weighed/price-embedded barcodes** for supermarkets with delis/produce.
4. **Private mode** for a shop's prices (in-store code or IP allow-list) and a metrics endpoint.
5. **Self-service sign-up + billing**, e-mail/SMS password reset.
6. **Luganda/Swahili**, then an accessibility pass.
7. The **offline "load from file" mode** and kiosk mode for shop-owned phones; consider the ESP32 device only if volume justifies it.

## 11. Guide for the reviewing agent

**Run:** `make test-all` (needs Go 1.24, Node 22, `cd tests && npm install`, Playwright Chromium, ImageMagick), or `make demo` and use it.

**Read in this order (≈ 7 000 lines of Go incl. tests, ≈ 1 900 lines of browser code):**
1. `docs/ARCHITECTURE.md`, then `internal/store/store.go` (`write()`: versioning, ordering) and `internal/store/feed.go` (`buildEvent`, `CatchUp`, `StreamCatalog`, `Home`).
2. `internal/server/public.go` (`events`: the subscribe → hello → catch-up → live ordering) and `internal/server/admin.go`.
3. `web/shopper/app.js` (`onChange`, `lookup`, `fetchItem`, `connect`, `revalidateVisible`) and `web/shopper/decode.js`.
4. `tests/e2e/*.e2e.js` for what is actually proven.

**Invariants worth attacking:**
* *Event ordering / no lost update:* subscribe-before-snapshot in `events`, `write()` publishing under the mutex, the client's `p == lastV` gap rule, `rev` comparison on cache writes. Try to find an interleaving where a phone keeps a stale price while showing "Live".
* *"Fresh" logic:* `chk >= fresh` and `S.live`. Try: server restart, restore from older backup, version going backwards, `reset` during a lookup, two tabs.
* *Reconnect:* the app deliberately does **not** use EventSource's automatic retry (its URL keeps a stale `?since=`); a bug found and fixed during testing. Check nothing regresses it.
* *Decoder false positives:* a wrong price is worse than a slow scan. Look for ways a non-barcode (or a different symbology) could decode to a valid-looking EAN.
* *Auth:* CSRF on every unsafe route, session scoping per shop (cookie `pt_<slug>`), role checks, throttles, `export.db` contents, operator API.
* *Money:* integer minor units everywhere; `ParseMoney` edge cases; the CSV round-trip.
* *Resource exhaustion:* SSE caps, limiter map caps, import size/row caps, `LIKE` search cost on large catalogues, catalogue cache memory (one gzip per open shop).

**Bugs found and fixed during development (so you can check they stayed fixed):** CSV row numbers after blank lines; `ParseMoney` accepted `1e5` as 15; backup on a read-only handle; decoder bar-width bias from hysteresis (replaced by sub-pixel edges) and false positives on noise (layout checks + two-line agreement); service worker never cached the page shell (offline reload gave 503); EventSource reconnect used a stale cursor (missed changes after a server restart); an open item sheet/recents stayed stale after a bulk import; login throttle counted successful logins; CSP blocked inline `<style>` on two pages; restored-from-older-backup servers were ignored as "duplicate"; JSON `errors:null` vs `[]`; per-IP defaults too low for a shop's shared Wi-Fi.

**Repository facts:** about 85 files; Go ≈ 5 300 lines + ≈ 2 000 lines of tests; browser code ≈ 1 850 lines (+ vendored QR library); tests ≈ 1 400 lines JS. Dependencies: `modernc.org/sqlite`, `golang.org/x/crypto` (Go); none at runtime in the browser except the vendored MIT QR generator (admin only); `playwright` + `acorn` for tests.
