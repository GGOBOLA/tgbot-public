#!/usr/bin/env bash
# Daily reconciliation. Two invariants:
#   1. Within the window, what the gateway recorded as usage cost equals what it
#      took out of balances (ledger rows with reason='usage').
#   2. Every customer's balance equals the sum of their ledger deltas — the
#      property that makes the ledger an audit trail rather than a log.
# Rows marked estimated (usage.estimated = 1) are excluded from both sides of
# invariant 1. Such a row is a hold charge for a call whose usage the upstream
# never reported, so its cost is a guess — and the guess has no exact ledger
# link to compare against. Counting one would let an estimate pass as a price;
# excluding it means a known gap stays out of the drift number instead of
# hiding or inventing drift.
# Silent when both hold; messages every admin with the numbers when they do not.
set -uo pipefail

cd "$(dirname "$0")/.."
# An explicit DB_PATH from the caller wins over the one in .env, so the check can
# be exercised against a copy without touching the live books.
DB_OVERRIDE="${DB_PATH:-}"
set -a
# shellcheck disable=SC1091
. ./.env
set +a
# shellcheck disable=SC1091
. scripts/lib/telegram.sh

# The only value this script produces is an alert about the books. Without a
# token the comparison still runs, finds drift, and tells nobody — the failure
# this check exists to prevent. Refuse to pretend.
if [ -z "${TELEGRAM_BOT_TOKEN:-}" ] || [ -z "${ADMIN_IDS:-}" ]; then
  echo "reconcile: .env has no TELEGRAM_BOT_TOKEN/ADMIN_IDS (running as $(id -un)); a drift alert could not be delivered" >&2
  exit 2
fi

DB="${DB_OVERRIDE:-${DB_PATH:-./data/bot.db}}"
WINDOW_HOURS="${RECONCILE_WINDOW_HOURS:-24}"

if ! OUT=$(python3 - "$DB" "$WINDOW_HOURS" <<'PY'
import sqlite3, sys, time
db, hours = sys.argv[1], int(sys.argv[2])
c = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
# The schema stores unix seconds, not datetime text, and money is integer µsen
# in `cost`/`delta`/`balance`.
since = int(time.time()) - hours * 3600
# usage.estimated arrives with schema v2, which the running binary writes on
# its first start. Until then the file is still v1 — and a v1 file has no
# estimates by definition, so the sums below are already real. Tolerate that
# state instead of failing the check the script exists to run.
marked = "estimated" in {row[1] for row in c.execute("pragma table_info(usage)")}
usage = c.execute(
    "select coalesce(sum(cost), 0) from usage where created_at >= ?"
    + (" and estimated = 0" if marked else ""),
    (since,),
).fetchone()[0]
# An estimated charge wrote a usage row and its ledger row in the same
# transaction, but nothing links the two rows. The only way to drop the ledger
# side of an estimate is to match the charge it produced: same customer, same
# second, same amount. A mismatch can only over-report drift (both rows of a
# real charge would have to collide with an estimate), never hide it.
ledger_sql = (
    "select coalesce(sum(-l.delta), 0) from ledger l "
    "where l.reason = 'usage' and l.created_at >= ?"
)
if marked:
    ledger_sql += (
        " and not exists (select 1 from usage u"
        " where u.estimated = 1"
        " and u.telegram_id = l.telegram_id"
        " and u.cost = -l.delta"
        " and u.created_at = l.created_at)"
    )
ledger = c.execute(ledger_sql, (since,)).fetchone()[0]
bad = c.execute(
    """select count(*) from (
         select cu.telegram_id,
                cu.balance,
                coalesce((select sum(l.delta) from ledger l
                          where l.telegram_id = cu.telegram_id), 0) as led
         from customers cu
       ) where balance <> led"""
).fetchone()[0]
neg = c.execute(
    "select count(*), coalesce(sum(balance), 0) from customers where balance < 0"
).fetchone()
print(usage, ledger, bad, neg[0], neg[1])
PY
); then
  # A check that cannot read the database has not passed: without this, empty
  # fields coerce to 0 in $(( )) and the script reported drift=0 and exit 0.
  echo "$(date -u +%FT%TZ) reconcile FAILED to run (db=$DB window=${WINDOW_HOURS}h)" >&2
  send_admins "🔴 <b>Reconciliation could not run</b>
db: $(html_escape "$DB") · window: ${WINDOW_HOURS}h · $(html_escape "$(hostname)")

The query failed (missing, locked or corrupt DB). The books were NOT checked." || true
  exit 2
fi

read -r usage_usen ledger_usen bad_customers debt_count debt_usen <<<"$OUT"
for field in "$usage_usen" "$ledger_usen" "$bad_customers" "$debt_count" "$debt_usen"; do
  case "$field" in
    '' | *[!0-9-]*)
      echo "$(date -u +%FT%TZ) reconcile FAILED: non-numeric result '${OUT}'" >&2
      exit 2
      ;;
  esac
done

drift=$((usage_usen - ledger_usen))
if [ "$drift" -lt 0 ]; then drift=$((-drift)); fi

echo "$(date -u +%FT%TZ) reconcile window=${WINDOW_HOURS}h usage=${usage_usen}usen ledger=${ledger_usen}usen drift=${drift}usen bad_customers=${bad_customers} debtors=${debt_count} debt=${debt_usen}usen"

problems=""
if [ "$drift" -ne 0 ]; then
  problems="usage cost ${usage_usen} µsen vs ledger charges ${ledger_usen} µsen (difference ${drift} µsen)"
fi
if [ "${bad_customers:-0}" -ne 0 ]; then
  problems="${problems:+$problems
}${bad_customers} customer(s) whose balance does not equal their ledger sum"
fi

if [ -n "$problems" ]; then
  send_admins "⚠️ <b>Reconciliation failed</b>
${problems}
window: ${WINDOW_HOURS}h · $(html_escape "$(hostname)") · $(date -u +%H:%M)Z

Check the journal (tgbot) and the usage/ledger tables."
  exit 1
fi

# A debt is not an error: the gateway records one call's overshoot as a negative
# balance on purpose. Mention it only when it exists, so it is not a surprise.
if [ "${debt_count:-0}" -gt 0 ] && [ -n "${RECONCILE_REPORT_DEBT:-}" ]; then
  send_admins "ℹ️ <b>Debt on the books</b>
${debt_count} customer(s) owe ${debt_usen} µsen from calls that outran their balance."
fi
exit 0
