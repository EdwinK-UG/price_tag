# Deploying

> **Not tested in the build environment:** the Dockerfile, `docker-compose.yml`, Caddyfile, systemd unit and CI workflow were written carefully but there was no Docker daemon or systemd available to run them. The binary and everything it serves *were* tested. Treat the first deployment as a dry run.

Requirements: one small Linux machine (1 vCPU / 1 GB RAM is plenty to start; see sizing), a domain name, ports 80/443. **HTTPS is mandatory**: phones refuse camera access on plain HTTP.

## Option A: Docker + Caddy (automatic HTTPS)

```sh
git clone … && cd price_tag
printf 'DOMAIN=prices.example.com\nOPERATOR_TOKEN=%s\n' "$(openssl rand -hex 24)" > deploy/.env
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml exec pricetag pricetag store create -data /data -slug fresh-mart -name "Fresh Mart" -owner-email boss@fresh.example
```

Caddy gets and renews the certificate and passes `flush_interval -1` so live updates are never buffered. The app runs with `PRICETAG_TRUST_PROXY=1`, so cookies get `Secure` and rate limits see real client IPs. If another proxy/CDN sits in front of Caddy, the *last* `X-Forwarded-For` entry must still be the real client (otherwise per-IP limits see the CDN).

## Option B: bare binary + systemd

```sh
make build && sudo install -m 0755 bin/pricetag /usr/local/bin/pricetag
sudo useradd --system --home /var/lib/pricetag pricetag
sudo install -m 0644 deploy/pricetag.service /etc/systemd/system/ && sudo systemctl enable --now pricetag
# put Caddy or nginx in front for HTTPS; for nginx add:  proxy_buffering off;  proxy_read_timeout 1h;
```

## Configuration

| Flag / env | Default | Notes |
|---|---|---|
| `-addr` / `PRICETAG_ADDR` | `:8080` | |
| `-data` / `PRICETAG_DATA` | `./data` | one `.db` per shop. **Back this up.** |
| `-trust-proxy` / `PRICETAG_TRUST_PROXY=1` | off | only behind a proxy you control |
| `PRICETAG_OPERATOR_TOKEN` | unset (API off) | ≥ 24 chars; enables `/api/v1/operator/*` for your sign-up flow |
| `-public-rps` / `-public-burst` | 100 / 400 per IP | a shop's Wi-Fi is one IP: don't set low |
| `-max-sse-per-ip` / `-max-sse-per-store` | 1000 / 20000 | live connections |
| `-dev-tls`, `-tls-cert/-tls-key` | off | for LAN tests / no reverse proxy |

## Messages: e-mail, SMS, WhatsApp

Without these settings every channel runs in **development mode**: messages (including sign-in codes) are recorded and shown in the operator console, not delivered. Configure each channel you want to really send:

| Channel | Environment variables | Notes |
|---|---|---|
| E-mail | `PRICETAG_SMTP_ADDR=smtp.example.com:587` `PRICETAG_SMTP_USER` `PRICETAG_SMTP_PASS` `PRICETAG_SMTP_FROM="Price Tag <no-reply@example.com>"` | any SMTP provider; set SPF/DKIM for your domain or mail lands in spam |
| SMS | `PRICETAG_AT_USERNAME` `PRICETAG_AT_APIKEY` `PRICETAG_AT_FROM` (sender ID, optional) `PRICETAG_AT_SANDBOX=1` (testing) | Africa's Talking. Sender IDs must be registered per country |
| WhatsApp | `PRICETAG_WA_TOKEN` `PRICETAG_WA_PHONE_ID` `PRICETAG_WA_TEMPLATE` `PRICETAG_WA_LANG=en` | WhatsApp Cloud API (Meta). Business-initiated messages need an **approved template** with one body parameter; requires a verified Meta business |
| Links | `PRICETAG_PUBLIC_URL=https://prices.example.com` | used in set-up / reset / unsubscribe links |
| Country | `PRICETAG_COUNTRY_CODE=256` | for local phone numbers (Uganda by default) |
| Alerts | `PRICETAG_OPERATOR_EMAIL` | e-mailed when a shop registers |

**These three integrations were written from the providers' documentation and tested against fake servers only. Test each with a real account (Africa's Talking has a free sandbox) before relying on it.** Each shop's SMS and WhatsApp usage is metered per month in the operator console for invoicing; the allowance stops a shop from running up costs.

Back up the shopper-account database too (`_platform.db`; `deploy/backup.sh` does it with `pricetag store backup-accounts`).

## Sizing (measured on 4 shared vCPUs, so conservative)

* idle: ~40 MB. Each connected (live) phone: ~26 KB. **5 000 live phones = 171 MB**, and a price change reached all 5 000 in 26 ms.
* 16 000 lookups/second on one machine with the load generator on the same CPUs (p50 6.8 ms at 128 concurrent).
* A 50 000-product price file imports in ~2 s; re-sending it unchanged ~0.75 s and pushes nothing to phones.
* Set `LimitNOFILE`/`ulimit -n` ≥ 65536 if you expect thousands of live phones (one file handle each).
* The cap `-max-sse-per-store 20000` ⇒ roughly 520 MB RAM at the ceiling. Phones beyond it still get prices, just not push.

## Backups (do this before the first real customer)

```sh
sudo install -m 0755 deploy/backup.sh /opt/pricetag-backup.sh
# crontab:  15 2 * * *  /opt/pricetag-backup.sh        then copy /var/backups/pricetag off the machine
```

`pricetag store backup` uses SQLite's online backup, safe while serving, and the result is a normal shop file. **Restore** = stop the service, put the file in the data directory as `<slug>.db`, start. (Phones that were ahead of the restored data notice the version went backwards, clear their cache and resync automatically.) SQLite runs with `synchronous=FULL`, so an acknowledged edit survives a power cut; backups protect against disk loss and mistakes.

## Operating notes

* `GET /healthz` for monitoring. Logs are structured (`-json-log`). There is no metrics endpoint yet.
* Upgrade = replace the binary and restart (phones reconnect within seconds). The shop file format is versioned (`schema_version`); there are no migrations yet because there is only version 1.
* Lost owner password: `pricetag store passwd -slug … -email …` (also re-enables a disabled account and signs the user out everywhere).
* Don't run mutating CLI commands (other than `store create`/`passwd`/`backup`) directly against a data directory while the server is running: use the API (`pricetag push`) so phones are notified.
