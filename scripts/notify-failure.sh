#!/usr/bin/env bash
# notify-failure.sh UNIT — message the admins that a systemd unit failed.
# Wired as OnFailure= so a non-zero exit is not just a journal line nobody reads.
set -uo pipefail

cd "$(dirname "$0")/.."
set -a
# shellcheck disable=SC1091
. ./.env
set +a
# shellcheck disable=SC1091
. scripts/lib/telegram.sh

unit="${1:-unknown unit}"
send_admins "🔴 <b>Systemd unit failed</b>
$(html_escape "$unit") exited non-zero on $(html_escape "$(hostname)") at $(date -u +%H:%M)Z.

Check: journalctl -u $(html_escape "$unit") -n 50" || true
