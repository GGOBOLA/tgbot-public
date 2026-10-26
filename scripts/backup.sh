#!/usr/bin/env bash
# Daily SQLite backup for tgbot. Uses the Python stdlib sqlite3 module because the
# sqlite3 CLI is not installed on this box. Keeps the last 14 snapshots.
set -euo pipefail

cd "$(dirname "$0")/.."
# Same .env as the other scripts, so a configured DB_PATH is honoured here too.
# An explicit DB_PATH from the caller still wins.
DB_OVERRIDE="${DB_PATH:-}"
set -a
# shellcheck disable=SC1091
. ./.env
set +a
DB="${DB_OVERRIDE:-${DB_PATH:-./data/bot.db}}"
DEST="${BACKUP_DIR:-./backups}"
KEEP=14

umask 077
mkdir -p "$DEST"
chmod 700 "$DEST" 2>/dev/null || true
stamp="$(date -u +%Y%m%d-%H%M%S)"
out="$DEST/bot-$stamp.db"

python3 - "$DB" "$out" <<'PY'
import os, sqlite3, sys
src, dst = sys.argv[1], sys.argv[2]
tmp = dst + ".tmp"
os.umask(0o077)

def cleanup():
    for p in (tmp, tmp + "-wal", tmp + "-shm"):
        try:
            os.unlink(p)
        except FileNotFoundError:
            pass

# Write under a temporary name and rename only after the snapshot passes.
# A corrupt file must never become the newest snapshot: the age-only deploy
# check would trust it, and the 14-file prune would count it.
try:
    src_conn = sqlite3.connect(f"file:{src}?mode=ro", uri=True)
    try:
        dst_conn = sqlite3.connect(tmp)
        try:
            with dst_conn:
                src_conn.backup(dst_conn)   # online backup API: safe while the bot holds WAL
            row = dst_conn.execute("pragma integrity_check").fetchone()
        finally:
            dst_conn.close()
    finally:
        src_conn.close()

    if not row or row[0] != "ok":
        print(f"backup: integrity_check failed on {dst}: {row}", file=sys.stderr)
        raise RuntimeError("integrity_check did not pass")

    # The destination inherits WAL mode from the source header, which leaves
    # empty -wal/-shm siblings next to the snapshot. A restore artifact should
    # be one self-contained file, so flip it back to a rollback journal and drop
    # the sidecars (the gate fails on stray sidecars for exactly this reason).
    solo = sqlite3.connect(tmp)
    try:
        solo.execute("pragma journal_mode=delete")
        solo.execute("pragma wal_checkpoint(truncate)")
    finally:
        solo.close()
    for side in (tmp + "-wal", tmp + "-shm"):
        try:
            os.remove(side)
        except FileNotFoundError:
            pass
    os.chmod(tmp, 0o600)
except Exception as e:
    cleanup()
    print(f"backup: snapshot failed: {e}", file=sys.stderr)
    sys.exit(1)

os.replace(tmp, dst)
PY

# Prune: keep the newest $KEEP snapshots, removing each deleted snapshot's journal
# sidecars too (a stray -wal/-shm from another moment can corrupt a restore).
# NUL-delimited: a path with a space must not be word-split into the wrong rm.
find "$DEST" -maxdepth 1 -name 'bot-*.db' -printf '%T@ %p\0' 2>/dev/null \
  | sort -zrn \
  | tail -z -n +$((KEEP + 1)) \
  | while IFS= read -r -d '' rec; do
      f="${rec#* }"
      rm -f "$f" "$f-wal" "$f-shm"
    done

echo "$(date -u +%FT%TZ) backed up $DB -> $out"
