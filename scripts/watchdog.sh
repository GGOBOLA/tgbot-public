#!/usr/bin/env bash
# Health watchdog: messages the admins when the bot or its gateway is down and
# again when it recovers. Transition-only, so a persistent outage cannot spam.
#
# Catches: the service stopped or crash-looping, the gateway not answering
# /healthz, and a nearly full disk. It cannot catch the VM itself being down —
# nothing on the box can report that; use an external check for it.
set -uo pipefail

# The override must come from the real environment, not .env: a leftover test
# line in .env would pin the state to "fail" and silence every real outage.
FORCE_FAIL="${WATCHDOG_FORCE_FAIL:-}"
cd "$(dirname "$0")/.."
set -a
# shellcheck disable=SC1091
. ./.env
set +a
# shellcheck disable=SC1091
. scripts/lib/telegram.sh

STATE_DIR="${WATCHDOG_STATE_DIR:-./data}"
STATE="$STATE_DIR/watchdog.state"
mkdir -p "$STATE_DIR"
LAST=$(cat "$STATE" 2>/dev/null || echo ok)
now="$(date -u +%s)"

REASON=""
if ! systemctl is-active --quiet tgbot; then
  REASON="service tgbot is not active"
elif [ -n "${GATEWAY_ADDR:-}" ] && ! curl -fsS -m 3 "http://${GATEWAY_ADDR}/healthz" | grep -qx ok; then
  # Only probe the gateway when one is configured: a bot-only deployment has no
  # listener, and falling back to 127.0.0.1:8787 made this watchdog report a
  # permanent outage that then masked the next real one.
  REASON="gateway /healthz did not answer ok"
else
  # The disk that matters is the one holding the database, wherever it is.
  DISK_PATH="$(dirname "${DB_PATH:-/}")"
  AVAIL_KB=$(df -Pk "$DISK_PATH" 2>/dev/null | awk 'NR==2{print $4}')
  case "$AVAIL_KB" in
    '' | *[!0-9]*) REASON="disk check could not run for $DISK_PATH" ;;
    *) [ "$AVAIL_KB" -lt 524288 ] && REASON="low disk on $DISK_PATH: ${AVAIL_KB} KB free" ;;
  esac

  # Nothing else watches the snapshots: the backup unit has no OnFailure and the
  # only age check (verify-timers.sh) is not scheduled anywhere. A daily backup
  # that stops arriving must reach a human through this path.
  if [ -z "$REASON" ]; then
    BACKUP_DIR="${BACKUP_DIR:-./backups}"
    newest="$(find "$BACKUP_DIR" -maxdepth 1 -name 'bot-*.db' -printf '%T@ %p\n' 2>/dev/null | sort -nr | head -1 | cut -d' ' -f2-)"
    if [ -z "$newest" ]; then
      REASON="no backup snapshot in $BACKUP_DIR"
    else
      age_h=$(( (now - $(stat -c %Y "$newest")) / 3600 ))
      [ "$age_h" -gt 48 ] && REASON="newest backup $(basename "$newest") is ${age_h}h old: the daily snapshot is not running"
    fi
  fi
fi

# Used by the install test to prove delivery without stopping the bot.
if [ -n "$FORCE_FAIL" ]; then
  REASON="$FORCE_FAIL"
fi


if [ -n "$REASON" ]; then
  if [ "$LAST" != "fail" ]; then
    # Only record the transition once somebody was actually told, otherwise the
    # alert is consumed and never retried.
    if send_admins "🔴 <b>tgbot is down</b>
$(html_escape "$REASON")
host: $(html_escape "$(hostname)") · $(date -u +%H:%M)Z"; then
      echo fail >"$STATE"
      exit 0
    fi
    echo "watchdog: alert delivery failed; state left as $LAST" >&2
    exit 1
  fi
  exit 0
fi

if [ "$LAST" = "fail" ]; then
  if ! send_admins "🟢 <b>tgbot recovered</b>
healthz ok · $(html_escape "$(hostname)") · $(date -u +%H:%M)Z"; then
    echo "watchdog: recovery message failed; state left as fail" >&2
    exit 1
  fi
  echo ok >"$STATE"
fi
exit 0
