# Try the whole journey yourself (about 20 minutes)

This walks through what a supermarket and a shopper actually do, on your own computer and phone. You do **not** need to host anything for testing: your Mac runs the server, and a free temporary tunnel gives your phone a real HTTPS link (phones only allow the camera on HTTPS pages).

What exists today, and who uses it:

| Who | Where | What they do |
|---|---|---|
| **You (the operator)** | the `pricetag` command | create a shop account for each supermarket you sign up (there is no public sign-up page yet) |
| **Shop owner / staff** | `…/s/<shop>/admin/` in any browser | sign in; **Import** their price file (CSV) with a preview; edit prices; create **Offers**; add **Team** members; print the **QR poster** (Settings); create an **Integration** key so their till system can send prices automatically; use the **Checkout** tab at the till |
| **Shopper** | `…/s/<shop>/` (opened by scanning the QR poster) | scan or type a barcode → price; cart; checkout code |

## 0. One-time setup on the Mac

```sh
brew install go cloudflared          # cloudflared = the free temporary tunnel
cd price_tag && git pull && make build
```

## 1. Start the server and give it a public HTTPS address

Terminal 1 (leave it running):

```sh
./bin/pricetag serve -data ./data -trust-proxy
```

(`-trust-proxy` tells the server it sits behind the tunnel, so sign-in and secure cookies work through it. Only use it when something like the tunnel or Caddy is in front.)

Terminal 2 (leave it running):

```sh
cloudflared tunnel --url http://localhost:8080
```

It prints a line like `https://random-words.trycloudflare.com`. **That is your test address.** It works from any phone, on Wi-Fi or mobile data, while both terminals are running. Anyone who has the link can open it, and it changes every time you restart the tunnel.

## 2. Sign up a supermarket (as the operator)

Terminal 3:

```sh
./bin/pricetag store create -slug fresh-mart -name "Fresh Mart" -owner-email owner@freshmart.example
```

It prints a one-time password. (Later this step becomes a public sign-up page; see the plan in the chat.)

## 3. The shop owner loads their prices

1. Make a realistic price file, like a till system would export (300 products, **made-up prices**):
   ```sh
   go run ./tools/samplecsv -out fresh-mart-prices.csv
   ```
   A ready-made one is also in the repo: `testdata/supermarket-300.csv`.
   *Want real products from your kitchen to scan?* `go run ./tools/samplecsv -source openfoodfacts -country uganda -out real.csv` uses real barcodes of goods sold in Uganda from Open Food Facts (prices still invented). Their service is often busy; retry if it says so.
2. Open **`https://<your-tunnel>/s/fresh-mart/admin/`** (use the tunnel address, not localhost: the QR poster is made from the address you are using). Sign in with the e-mail and password from step 2.
3. **Import** tab → choose the CSV → you see a **preview** (new / changed / problems). Nothing is saved yet. → **Apply these changes**.
4. **Prices** tab: search, tap a price to change it.
5. **Offers** tab: create an offer (title, dates, products, offer prices).
6. **Settings** tab: shop name, announcement, time zone; tick **Checkout** if you want scan-and-go; **Print poster** gives the QR code for the shop entrance.

## 4. The shopper side, on your phone

1. Point the phone's normal camera app at the QR poster (on paper or on your Mac's screen). It opens the shop page.
2. Open **`https://<your-tunnel>/s/fresh-mart/test-sheet`** on the Mac: it shows real, scannable barcodes for products in this shop. Tap **Scan a barcode** on the phone and scan them off the screen.
3. Keep a product open on the phone, change its price in the admin on the Mac: the phone updates by itself.
4. Add a few items to the cart, tap **Checkout** → a 6-letter code. In the admin, **Checkout** tab → type the code → tick the items → **Mark verified / paid**.
5. Turn on airplane mode: products you already looked at still show their price, labelled as saved.

## 5. Simulate the shop's till system sending prices

In the admin, **Integrations** → create a key, then:

```sh
./bin/pricetag push -url https://<your-tunnel> -slug fresh-mart -token ptk_... -file fresh-mart-prices.csv
```

Edit a few prices in the CSV and push again: only the changed products reach phones. Or watch random price changes: `./bin/pricetag demo-tick -url http://localhost:8080 -slug fresh-mart -token ptk_...`.

## Other ways to reach a phone

* **If sign-in says "cross-site request blocked"** through the tunnel, make sure the server was started with `-trust-proxy`.
* **Same Wi-Fi, no tunnel:** `make demo-phone` serves `https://<mac-ip>:8443` with a self-signed certificate. The phone shows a security warning you must accept, and some phones still refuse the camera on such pages. The tunnel is more reliable.
* **A real pilot with a real shop:** host it on a small server with a domain name; see `docs/DEPLOY.md`.

## The built-in demo shop

`make demo` creates a ready-made shop called **Price Tag** at `/s/demo/` (owner `owner@demo.example` / `demo-password-1`) with 57 products and three offers, if you just want to click around.
