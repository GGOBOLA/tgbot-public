# tgbot

A Telegram customer dashboard and OpenAI-compatible gateway in one Go binary.
Customers top up, mint API keys, and are billed for each call.

## Status

v1.0.0, running as `tgbot.service`, CI green.

## What it does

- **Customers** — view balances and usage, manage keys, view prices, and request top-ups.
- **Admins** — manage customers, review claims, credit balances, and set prices. The Set prices hub has paginated Priced and Unpriced lists; enter both halves of a model price together.
- **Gateway** — `POST /v1/chat/completions`, `POST /v1/messages`, `GET /v1/models` (priced models only), and `GET /healthz`.

## Quick start

```bash
cp .env.example .env && chmod 640 .env      # fill in; on the host: sudo chown root:tgbot .env
go build -o bin/tgbot ./cmd/bot && go build -o bin/seedprices ./cmd/seedprices
./bin/seedprices -db data/bot.db -prices config/prices.json
./bin/tgbot
```

`seedprices` is required. An unpriced model is refused with `400 model not offered`.
Re-running keeps existing prices; `-force` overwrites them from the file.
Payments need `PAYMENT_QR_PATH` set to the DuitNow image path.

## Configuration

Go 1.27.3. SQLite is embedded via `modernc.org/sqlite`; no database server or CGO is needed.
Config fails fast at boot.

| Variable | Required | Meaning |
| --- | --- | --- |
| `TELEGRAM_BOT_TOKEN` | yes | Telegram bot token |
| `ADMIN_IDS` | yes | Admin Telegram IDs |
| `CALLBACK_SECRET` | yes | Button signing secret, at least 16 chars |
| `DB_PATH` | yes | SQLite file path |
| `NINEROUTER_URL` | with a key | Upstream base URL; no implicit default |
| `NINEROUTER_KEY` | with a gateway | Upstream key; never logged |
| `GATEWAY_ADDR` | no | Gateway address; empty disables it |
| `PAYMENT_QR_PATH` | for top-ups | DuitNow image path |
| `WELCOME_IMAGE_PATH` | no | Dashboard image path |
| `RATE_LIMIT_PER_SEC`, `RATE_LIMIT_BURST` | no | Customer rate limit (5/s, burst 20) |
| `SPEND_ALERT_MYR`, `AUTH_ALERT_BURST` | no | Hourly RM charged (100); 401/403 per 10 min (50) |

## Deploy (single host, systemd)

```bash
sudo install -m 644 scripts/tgbot.service scripts/tgbot-backup.service scripts/tgbot-backup.timer \
  scripts/tgbot-reconcile.service scripts/tgbot-reconcile.timer scripts/tgbot-watchdog.service \
  scripts/tgbot-watchdog.timer 'scripts/tgbot-failed@.service' /etc/systemd/system/
id -u tgbot >/dev/null 2>&1 || sudo useradd --system -U --home-dir /srv/tgbot --shell /usr/sbin/nologin tgbot
sudo install -d -o tgbot -g tgbot -m 700 /srv/tgbot/data /srv/tgbot/backups
# Prices are what makes a model sellable; the gate cannot see them.
sudo -u tgbot ./bin/seedprices -db /srv/tgbot/data/bot.db -prices /srv/tgbot/config/prices.json
sudo systemctl daemon-reload
sudo systemctl enable --now tgbot.service tgbot-watchdog.timer tgbot-reconcile.timer tgbot-backup.timer
```

Create the `tgbot` user and both directories before enabling the units. The service runs unprivileged with `ProtectSystem=strict`; only `/srv/tgbot/data` is writable.

| Timer | Does |
| --- | --- |
| `tgbot-watchdog.timer` | Every 5 min: health, disk space, and snapshot age |
| `tgbot-reconcile.timer` | Daily 03:15 UTC: compare usage cost with the ledger |
| `tgbot-backup.timer` | Daily 07:00: integrity-checked snapshot; keeps 14 |

## Money and billing

Money uses integer µsen: 1 sen = 1,000,000 µsen. Each call checks failed-auth throttling, key authentication, customer rate limit, model pricing, balance for a pre-flight hold, then bills.
Billed token counts are authoritative; provider-reported tokens are informational. Billing completes even if the client disconnects.

## Guarantees

| Guarantee | How |
| --- | --- |
| A double tap cannot pay twice | Unique ledger reference and transaction guards |
| No forged or edited button | Signed button values are verified |
| No invented top-up amount | Signed amount checked against fixed list |
| No credit from a corrupt token count | Database checks and saturating cost arithmetic |
| Customers cannot reach admin actions | Central authorization check |
| A failed response is never silently free | Throttled admin alert |

## Operating

- **Restore**: stop the service, copy a backup over `DB_PATH`, then start it.
- **Ownership**: `data/` and `backups/` must be owned by `tgbot:tgbot`.
- **Prices**: change them in Set prices or edit `config/prices.json` and re-run `seedprices -force`.
- **Reconcile**: run `bash scripts/reconcile.sh` to compare usage and ledger totals.
- **Logs**: `journalctl -u tgbot -f`; bot tokens and `sk-` keys are redacted.
- **Balance**: one warning at RM 2.00, then HTTP 402 at zero.
- **Exposure**: the gateway binds loopback and terminates no TLS; put a TLS proxy in front to publish it.

## Docs

- [User guide](docs/USER-GUIDE.md)

MIT, see [LICENSE](LICENSE).
