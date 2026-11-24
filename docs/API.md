# HTTP API

All paths are under one origin. `{slug}` is the shop's address. JSON everywhere except where noted. Errors are `{"error": "<code>", "message": "<for people>"}`.

**Money:** every `price` is an **integer in the shop's minor unit** (UGX: just shillings). The shop's `decimals` (0 for UGX) tells you where a decimal point goes. CSV files use human text ("12,500", "UGX 12500", "3.50"); JSON uses integers.

**Barcodes:** send EAN-8 / UPC-A / EAN-13 / GTIN-14 in any spelling; they are normalised to a 14-digit GTIN and the GS1 check digit must be valid (`400` `bad_code` otherwise). Other codes (`SUGAR-1KG`, `4011`) are accepted as the shop's own codes.

## Shopper (public, read-only, rate-limited per IP)

| Method & path | Returns |
|---|---|
| `GET /api/v1/s/{slug}/home` | `{store:{slug,name,currency,decimals,tz_offset,announcement}, promos:[{id,title,text,from,to,items:[{code,name,size,price,promo_price}],more}], v}`. ETag; `304` when unchanged. Includes live **and upcoming (≤7 days)** offers; the client filters by time. |
| `GET /api/v1/s/{slug}/items/{code}` | `{code,name,size,category,price,promos:[{id,title,price,from,to}],rev}` (~200 bytes). `404 not_found` if not in the price list. `ETag: W/"<rev>"`, honours `If-None-Match` (→ `304`). Header `X-PT-V` = current shop version. |
| `GET /api/v1/s/{slug}/search?q=` | `{items:[...]}` up to 20, by name/code fragment (≥2 chars). |
| `GET /api/v1/s/{slug}/events[?since=V]` | `text/event-stream`, see below. |
| `GET /api/v1/s/{slug}/catalog[?since=V]` | gzip NDJSON for the opt-in offline mode. Header `{"v","since","full","total"}`, then one item or `{"gone":code}` per line, then `{"end":true,"n":N}` (a missing end line = truncated download). Full downloads are built once per version and cached. |
| `GET /api/v1/s/{slug}/sample?n=` | a few products, for the printable test sheet. |

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

Editors and owners: `GET …/admin/stats`, `GET/PUT/DELETE …/admin/products[/{code}]`, `PUT …/admin/products/{code}/price {price}`, `GET/POST …/admin/promos`, `GET/PUT/DELETE …/admin/promos/{id}`, `POST …/admin/import[?dry_run=1]`, `GET …/admin/export.csv`, `GET …/admin/history?kind=&ref=&before=&limit=`, `GET …/admin/settings`, `POST …/admin/password {current,new}`.
Owners only: `PUT …/admin/settings`, `GET/POST/PATCH …/admin/users`, `GET/POST/DELETE …/admin/tokens`, `GET …/admin/export.db` (the whole shop as a SQLite file).

Offer body: `{title,text,starts_at,ends_at,enabled,items:[{code,price}]}` (times in Unix seconds). Rules: ends after starts; ≤ 1 year; ≤ 500 products; each product must exist and the offer price must be **lower** than its normal price.

## Operator (onboarding; disabled unless `PRICETAG_OPERATOR_TOKEN` is set, ≥ 24 characters)

`POST /api/v1/operator/stores {slug,name,owner_email,owner_name?,owner_password?,currency?,decimals?,tz_offset?}` → `201` with the shop's URLs and (if generated) a one-time owner password. `GET /api/v1/operator/stores` → slugs. Both need `Authorization: Bearer <operator token>`. Wire this to your own sign-up form to onboard shops without shell access.

## Health

`GET /healthz` → `{"ok":true,"stores_open":N,"uptime_s":S}`.
