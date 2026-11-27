# HTTP API

All paths are under one origin. `{slug}` is the shop's address. JSON everywhere except where noted. Errors are `{"error": "<code>", "message": "<for people>"}`.

**Money:** every `price` is an **integer in the shop's minor unit** (UGX: just shillings). The shop's `decimals` (0 for UGX) tells you where a decimal point goes. CSV files use human text ("12,500", "UGX 12500", "3.50"); JSON uses integers.

**Barcodes:** send EAN-8 / UPC-A / EAN-13 / GTIN-14 in any spelling; they are normalised to a 14-digit GTIN and the GS1 check digit must be valid (`400` `bad_code` otherwise). Other codes (`SUGAR-1KG`, `4011`) are accepted as the shop's own codes.

## Shopper (public, rate-limited per IP)

| Method & path | Returns |
|---|---|
| `GET /api/v1/s/{slug}/home` | `{store:{slug,name,currency,decimals,tz_offset,announcement,checkout_enabled?}, promos:[{id,title,text,from,to,items:[{code,name,size,price,promo_price}],more}], v}`. ETag; `304` when unchanged. Includes live **and upcoming (≤7 days)** offers; the client filters by time. |
| `GET /api/v1/s/{slug}/items/{code}` | `{code,name,size,category,price,promos:[{id,title,price,from,to}],rev}` (~200 bytes). `404 not_found` if not in the price list. `ETag: W/"<rev>"`, honours `If-None-Match` (→ `304`). Header `X-PT-V` = current shop version. |
| `GET /api/v1/s/{slug}/search?q=` | `{items:[...]}` up to 20, by name/code fragment (≥2 chars). |
| `GET /api/v1/s/{slug}/events[?since=V]` | `text/event-stream`, see below. |
| `GET /api/v1/s/{slug}/catalog[?since=V]` | gzip NDJSON for the opt-in offline mode. Header `{"v","since","full","total"}`, then one item or `{"gone":code}` per line, then `{"end":true,"n":N}` (a missing end line = truncated download). Full downloads are built once per version and cached. |
| `GET /api/v1/s/{slug}/sample?n=` | a few products, for the printable test sheet. |
| `POST /api/v1/s/{slug}/checkouts` | Creates (or reuses via optional `prev_code` when unchanged) an anonymous cart snapshot and returns `201` `{code,created_at,expires_at,total,unknown_count,items:[{code,display,name,size,qty,price,line_total}]}`. Request: `{items:[{code,qty}], prev_code?}` (≤ 200 items, qty 1–999). `200` (not `201`) when an unchanged cart re-uses its open code. `403 checkout_disabled` when `checkout_enabled` is off. Prices are always computed server-side; no shopper identity is stored. Rate-limited per IP per shop (2/s, burst 30), and each IP may open at most 2 000 new codes per shop per ~3 h (`429`; showing an existing code again always works and never counts). This in-memory limit is what stops one client from filling the shop-wide cap of 20 000 open codes. |

### The live stream

```
retry: 3412
event: hello
data: {"v":42,"now":1764000000}

id: 43
event: change
data: {"p":42,"v":43,"items":[{"code":"…","name":"Sugar","price":6000,"promos":[…],"rev":43}],"gone":["…"],"home":true}
```

* `hello` has **no `id`** on purpose, so the browser's `Last-Event-ID` only advances on real changes.
* `p` is the version the event follows. If `p` equals the client's last version it is contiguous; otherwise the client must treat everything older as unverified.
* `items` are the full new records of changed products (≤ 50 per event). `gone` lists removed codes. `home:true` = refetch `/home`.
* `reset:true` (no items) = too much changed (e.g. a nightly price file) or the cursor is unusable: revalidate what you display (or re-download the catalogue).
* Connecting with `?since=V` (or a `Last-Event-ID` header, which wins) returns one **catch-up event** `p = V` first if the client is behind.
* A heartbeat comment `: hb` every 20 s. Slow clients are dropped; reconnect with your last version.
* Limits: `MaxSSEPerStore` (20 000) and `MaxSSEPerIP` (1 000) → `503 busy` / `429 too_many_streams`; prices keep working without the stream.

## Machine ingest (POS / ERP connectors)

`POST /api/v1/s/{slug}/ingest[?dry_run=1]` with `Authorization: Bearer ptk_…` (create the key under **Integrations**).

* `Content-Type: text/csv` (default) or `application/json` `{"products":[{"code","name","size","category","price"}]}`.
* CSV: first row = headers. Recognised (case/space-insensitive): code ← `barcode, code, gtin, ean, ean13, upc, plu, sku, itemcode`; name ← `name, description, item, product, itemname`; price ← `price, sellingprice, retailprice, unitprice, shelfprice`; optional `size`/`pack`/`unit`, `category`/`department`/`group`. Comma, semicolon or tab separated; a UTF-8 BOM is fine; prices like `12,500` or `UGX 12500` are fine.
* **Idempotent**: unchanged rows cost nothing and bump nothing, so send the **whole** file as often as you like. Products missing from the file are *not* deleted.
* Whole file is one transaction → one version → one push event (or one `reset` hint if > 50 changed).
* Response: `{"total","created","updated","restored","unchanged","errors":[{"row","code","message"}],"version","dry_run"}`. Invalid rows are skipped and reported; valid rows are applied. `?dry_run=1` reports the same counts without saving.
* Limits: 32 MB body, 200 000 rows.

```sh
curl -X POST -H "Authorization: Bearer ptk_…" -H "Content-Type: text/csv" \
     --data-binary @prices.csv https://prices.example.com/api/v1/s/fresh-mart/ingest
```

## Store admin (cookie session; every write needs header `X-PT-CSRF: 1` and a same-origin `Origin`)

`POST …/admin/login {email,password}` → sets `pt_<slug>` cookie. `POST …/admin/logout`. `GET …/admin/me` → `{user:null}` when signed out (200). Rate limits count failed attempts only.

Editors and owners: `GET …/admin/stats`, `GET/PUT/DELETE …/admin/products[/{code}]`, `PUT …/admin/products/{code}/price {price}`, `GET/POST …/admin/promos`, `GET/PUT/DELETE …/admin/promos/{id}`, `GET …/admin/checkouts/{code}` (re-checks live prices against the snapshot and returns `audit_recheck` if the order was randomly flagged at creation; returns the **same** `404` for invalid, missing, expired **and already-used** codes; failed lookups are throttled per IP and per shop, successful lookups are refunded), `POST …/admin/checkouts/{code}/verify {mode?,rescan_confirmed?,mismatch?}` (marks the code `verified` once, prevents reuse (a used code answers with the same `404` as an unknown one), requires `rescan_confirmed:true` and forces `rescan` on flagged orders, optionally records extra/missing items + note, and logs actor/time/mismatch in `history`), `POST …/admin/import[?dry_run=1]`, `GET …/admin/export.csv`, `GET …/admin/history?kind=&ref=&before=&limit=`, `GET …/admin/settings`, `POST …/admin/password {current,new}`.
Owners only: `PUT …/admin/settings` (also configures `checkout_enabled` default `false`, `checkout_ttl_min` 5–1440 default `180`, `checkout_default_mode` `"tick"` | `"rescan"`, `audit_enabled` default `false`, `audit_rate` 0–100 default `0`), `GET …/admin/audit-report?from=YYYY-MM-DD&to=YYYY-MM-DD` (summary counts, mismatch rate among rechecked orders, estimated missed value at current prices, and logged mismatches), `GET …/admin/audit-report.csv?from=&to=` (formula-injection guarded CSV export), `GET/POST/PATCH …/admin/users`, `GET/POST/DELETE …/admin/tokens`, `GET …/admin/export.db` (the whole shop as a SQLite file).

Offer body: `{title,text,starts_at,ends_at,enabled,items:[{code,price}]}` (times in Unix seconds). Rules: ends after starts; ≤ 1 year; ≤ 500 products; each product must exist and the offer price must be **lower** than its normal price.

## Operator (onboarding; disabled unless `PRICETAG_OPERATOR_TOKEN` is set, ≥ 24 characters)

`POST /api/v1/operator/stores {slug,name,owner_email,owner_name?,owner_password?,currency?,decimals?,tz_offset?}` → `201` with the shop's URLs and (if generated) a one-time owner password. `GET /api/v1/operator/stores` → slugs. Both need `Authorization: Bearer <operator token>`. Wire this to your own sign-up form to onboard shops without shell access.

## Shop registration (public)

`POST /api/v1/signup {shop_name, slug, owner_name, owner_email, owner_phone?, town?, note?}` (needs `X-PT-CSRF: 1`; 1 per minute per IP, burst 5; a hidden `website` field must stay empty) → `201`. `409 slug_taken` if the address exists. The form is at `/signup`. The operator approves it (below).

## Shopper accounts (one account for every shop; cookie `pt_shopper`, HttpOnly, SameSite=Strict, 90 days)

All under `/api/v1/s/{slug}/account` (writes need `X-PT-CSRF: 1`). The shop in the path matters: it pays for SMS/WhatsApp codes sent from its page, and it is the shop being joined.

| Method & path | |
|---|---|
| `GET …/account` | `{shopper|null, member|null, channels:{sms,whatsapp,email}, shop}`: who is signed in, whether they joined THIS shop, which code channels this shop can use now |
| `POST …/start {contact, via}` | sends a 6-digit code. `contact` = phone (any local or international form; local numbers get +256) or e-mail; `via` = `sms` or `whatsapp` for phones. Same answer whether or not an account exists. Limits: 3 codes per contact per 15 min, per-IP rate. `409 channel_unavailable` when the shop has no allowance for that channel |
| `POST …/verify {contact, code}` | signs in (creating the account on first use). If already signed in, adds the contact to the account instead (`409 contact_in_use` if another account has it). A code works once, for 10 minutes, with 5 tries |
| `POST …/profile {name, channel}` | name (optional) and how shops may message you: `sms`, `whatsapp` or `email` (needs that contact) |
| `POST …/join {marketing}` | **consent**: this shop may now see your name and chosen contact; `marketing` = also send me offers (default off) |
| `POST …/marketing {on}` · `POST …/leave` | turn offers on/off · withdraw consent (the shop loses access immediately) |
| `GET …/shops` | every shop that can see your details |
| `POST …/logout` · `POST …/delete` | sign out · delete the account (every shop loses access) |
| `GET /u/{token}` | one-tap unsubscribe link included in every message |

## Shop owner: customers, messages, page sections, passwords

Owner role only: `GET …/admin/customers` (members who joined, with what they agreed to share), `GET …/admin/customers.csv`, `GET …/admin/messages` (history, this month's usage and allowance), `POST …/admin/messages {text, dry_run?}` (to every member who opted in, each on their chosen channel, with an unsubscribe link; ≤ 459 characters, ≤ 3 per day; refused as a whole if SMS/WhatsApp would exceed the shop's monthly allowance). Page sections are `home_blocks` in `PUT …/admin/settings`: up to 6 of `offers`, `announcement`, `signup`, `text` (`{type,title,text}`); the scan button and search box are always above them.

No session needed: `POST …/admin/forgot {email}` (always the same answer; e-mails a 2-hour link if the address exists), `POST …/admin/setup {token, password}` (one-time link from the welcome or reset e-mail: `/s/{slug}/admin/#setup=…`; signs in).

## Operator console (`/operator/`, same operator token)

`GET /api/v1/operator/applications`, `POST …/applications/{id}/approve {slug?}` (creates the shop, e-mails the owner a 7-day set-up link and returns it as `setup_url`), `POST …/applications/{id}/reject {note?}`, `GET …/shops` (with this month's usage), `PUT …/shops/{slug}/limits {sms_month_cap, wa_month_cap}` (the shop pays; 0 = channel off for that shop, the default), `GET …/outbox` (delivery status; real sign-in codes hidden), `GET …/status` (which channels really deliver; messages recorded in development mode).

## Health

`GET /healthz` → `{"ok":true,"stores_open":N,"uptime_s":S}`.
