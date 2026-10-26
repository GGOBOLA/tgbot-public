#!/usr/bin/env bash
# send_admins TEXT — Telegram-message every ADMIN_IDS entry.
# The token is read from the environment and never appears in argv or a URL
# printed to a log.
# html_escape TEXT — escape the three characters that break Telegram's HTML
# parse_mode. The alert templates below keep their own <b> tags; only values that
# come from outside (hostnames, unit names, database paths, error strings) are
# escaped, so a hostname containing "&" or a path with "<" cannot make the whole
# message fail to parse and go undelivered.
html_escape() {
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}

send_admins() {
  TEXT="$1" python3 - <<'PY'
import os, sys, urllib.parse, urllib.request
tok = os.environ.get("TELEGRAM_BOT_TOKEN", "")
admins = [a.strip() for a in os.environ.get("ADMIN_IDS", "").split(",") if a.strip()]
text = os.environ["TEXT"]
if not tok or not admins:
    print("notify: no token or admins configured", file=sys.stderr)
    sys.exit(1)
sent = 0
for a in admins:
    data = urllib.parse.urlencode(
        {"chat_id": a, "text": text, "parse_mode": "HTML", "disable_web_page_preview": "true"}
    ).encode()
    req = urllib.request.Request(f"https://api.telegram.org/bot{tok}/sendMessage", data=data)
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            r.read()
        sent += 1
    except Exception as e:  # never print the URL: it contains the token
        print(f"notify: send failed: {e.__class__.__name__}", file=sys.stderr)
# Exit non-zero only when nobody was told. Callers use this to decide whether the
# alert actually happened; one delivered admin means it did.
sys.exit(1 if sent == 0 else 0)
PY
}
