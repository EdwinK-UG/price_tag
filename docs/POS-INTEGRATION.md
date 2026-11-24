# Selling to a shop that already has a POS

You do **not** need to own, replace or even deeply understand their POS. The only thing Price Tag needs from it is **the price list**, as a file, one way (shop → Price Tag). This is also the easiest thing to sell: it is read-only, touches no transactions, and no customer data ever leaves the shop.

```
  Their till / back-office ──► price export (CSV)  ──►  connector  ──HTTPS──►  POST /ingest  ──► phones
   (what already feeds the                         (a scheduled            (outbound only:
    registers, scales, shelf labels)                job; 20 lines)          no firewall changes)
```

## Four ways to get the price list out of a POS (easiest first)

1. **Upload by hand** (no integration at all). The owner exports a CSV from the POS (almost every system can) and uploads it in **Admin → Import**, which shows a preview before anything is saved. A good first pilot: zero IT work.
2. **Scheduled export + connector** *(recommended for a real deployment)*. Most grocery POS/back-office systems already write a price/PLU file for the registers or scales (CSV, XML, fixed-width) to a folder on a schedule. A small job on the shop's PC picks it up and sends it. With the bundled connector:
   ```sh
   pricetag push -url https://prices.example.com -slug fresh-mart -token ptk_… -file /path/to/prices.csv
   ```
   Run it from cron / Task Scheduler every night **and** after the POS's own price-update job. Because ingest is idempotent, sending the full file every time is safe and cheap: only real changes reach phones. If the export is not CSV, put a ten-line script in front that converts it (XML/fixed-width → CSV or the JSON form).
3. **The POS vendor's API / webhooks** (Square, Lightspeed, Clover, Odoo, QuickBooks POS …). Write a small adapter that reads items and pushes them to `/ingest`. Price Tag's side stays the same.
4. **Read-only database view** (SQL Server / MySQL / Firebird / Access behind the POS). Needs the shop's IT or the vendor's blessing; ask for a *read-only* account and a query returning barcode, name, price. Run it from the same connector job.

Pick (1) for the pilot, (2) for the rollout. Only reach for (3)/(4) when the file route is not possible.

## What the shop's price file must contain

`barcode, name, price` are required (size/category optional). Any common header spelling works (`Barcode`, `Item Description`, `Selling Price`…). The barcode must be the one printed on the pack **or** the shop's own code; GS1 barcodes must have a valid check digit (a mistyped one is reported by row, not silently saved). See `testdata/sample-prices.csv`.

## Things to settle with each shop (and what to do)

| Question | Answer |
|---|---|
| Is the shelf price the till price? | Use the **till/ POS selling price** (tax-inclusive if that is what the shelf shows). Do not mix. |
| Different prices per branch? | One shop = one slug. A chain with 3 branches = 3 slugs, each with its own file. |
| Promotions in the POS? | Either let them flow in as normal price changes, or run them in **Admin → Offers** (shows on the home page and the product price, starts/ends by itself). Don't do both for the same product. |
| Weighed / price-embedded barcodes (start with `2`, e.g. deli, produce)? | **Not supported yet**: the price is inside the barcode. See REPORT.md, "Not done". |
| Products with no barcode? | Give them a short shop code (`BANANA`, `4011`), print it on the shelf label, and shoppers can type it. |
| What if the shop's internet is down? | Shopper phones keep working from their saved copy; the feed catches up when the shop's connector next sends. |
| Who owns the data? | The shop. **Admin → Import & export → Download everything** is a SQLite file with all of it; `pricetag store backup` makes the same file for you. |

## The pitch in three lines

*"Your customers scan any product and see today's price, with your offers on top. You change a price and it changes on their phones immediately. Your till stays exactly as it is; we only read the price list you already have."*

## Onboarding checklist (about an hour)

1. `pricetag store create -slug … -name … -owner-email …` (or the operator API) → give the owner their login.
2. Owner signs in, sets the shop name/announcement, prints the **QR poster** (Settings).
3. Load prices: Admin → Import (preview, then apply). Spot-check 10 products against the shelf.
4. (Rollout) Admin → Integrations → create a key → schedule the connector.
5. Put the QR poster at the entrance and by the tills. Test on three different phones in the aisle.
