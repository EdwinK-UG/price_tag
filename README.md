# Price Tag

A shopper walks into a shop, scans a product's barcode with their phone, and sees the shop's **current price** (and any offer on it). The shop owner changes a price or starts an offer and **every open phone updates within a fraction of a second**. Built to stay fast on cheap Android phones and poor mobile networks.

* **One small server binary** (11 MB, no dependencies to install) serving a shopper web app, a store-owner admin and an API.
* **Each shop owns its data**: one SQLite file per shop. Download it any time, or run it yourself.
* **Works with the shop's existing till/POS**: it only needs a read-only price file (CSV/JSON) pushed to one URL. No access to their system.
* **No app store, no install**: shoppers scan a QR code at the entrance; the page installs itself like an app and works offline.

**New here? Follow [docs/TRY-IT.md](docs/TRY-IT.md)** to run a supermarket and a phone end to end. See **[docs/REPORT.md](docs/REPORT.md)** for what was built, how well it works (measured), what is *not* done, and a review guide.

## Try it in two commands

Needs Go 1.24+ (nothing else).

```sh
make demo            # builds, seeds a demo supermarket, serves http://localhost:8080
```

Then open:

| | |
|---|---|
| `http://localhost:8080/s/demo/` | the shopper page (use a phone-sized window) |
| `http://localhost:8080/s/demo/test-sheet` | **printable demo barcodes**: point your webcam/phone at these |
| `http://localhost:8080/s/demo/admin/` | store admin: `owner@demo.example` / `demo-password-1` |

**See a live price change:** open the shopper page and tap a product. In another window sign in to the admin, tap the product's price, type a new one, Enter. The shopper's screen changes by itself.

**Or watch prices move on their own:** `./bin/pricetag demo-tick` changes a random price every 3 seconds.

### Testing the camera on a real phone

Phones only allow camera access on **HTTPS** pages (or `localhost`). On your own Wi-Fi:

```sh
make demo-phone      # serves https://<your-computer-ip>:8443 with a throw-away certificate
```

Open the address it prints on the phone, accept the certificate warning once, then scan the barcodes on the test sheet shown on your computer screen. For a real shop, use a domain and the Caddy setup in [docs/DEPLOY.md](docs/DEPLOY.md) (automatic HTTPS).

## How a shop gets onboarded

```sh
./bin/pricetag store create -slug fresh-mart -name "Fresh Mart" -owner-email boss@fresh.example
#  prints the shop's URLs and a one-time owner password
```

The owner signs in at `/s/fresh-mart/admin/`, prints the QR poster from **Settings**, and either types prices in, uploads a CSV, or (for a shop with a POS) creates an **integration key** so their system can push the price file automatically; see [docs/POS-INTEGRATION.md](docs/POS-INTEGRATION.md).

## Layout

```
cmd/pricetag/      the program: serve, store create/list/backup/passwd, demo, demo-tick, push
internal/store/    one shop's data (SQLite): products, offers, staff, history, change events
internal/server/   HTTP: shopper API, live updates (SSE), admin API, ingest, operator API
internal/barcode/  barcode normalisation (shared rules with the browser)
internal/auth/     password hashing (argon2id), tokens
internal/hub/      non-blocking fan-out for live updates
internal/demo/     the sample supermarket
web/shopper/       the shopper app (plain JS, ~14 KB gzipped) + barcode decoder
web/admin/         the store-owner admin
web/shared/        service worker, test-barcode sheet, icons
tools/loadtest/    load + fan-out benchmark
tests/js/          decoder / barcode / asset-budget tests (Node, no browser)
tests/e2e/         real-browser tests (Playwright + Chromium)
deploy/            Dockerfile-based compose + Caddy, systemd unit, backup script
docs/              REPORT, ARCHITECTURE, API, POS-INTEGRATION, TESTING, DEPLOY
```

## Tests

```sh
make test-all        # go vet + Go tests (race detector) + JS tests + browser tests
```

Details and what each suite proves: [docs/TESTING.md](docs/TESTING.md).

## Licence / third-party

Application code: yours to decide. Vendored: `web/vendor/qrcode-generator` (MIT), see `web/vendor/NOTICE.txt`. Go dependencies: `modernc.org/sqlite` (BSD-3), `golang.org/x/crypto` (BSD-3).
