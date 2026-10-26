#!/usr/bin/env bash
# Dev runner: load .env, sync modules, start the bot.
set -euo pipefail
cd "$(dirname "$0")"

if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
else
  echo "no .env (copy .env.example and fill it in)" >&2
  exit 1
fi

# Read-only module sync: `go mod tidy` here would mutate go.mod/go.sum of a
# binary that moves money, seconds before it starts.
go mod download
exec go run ./cmd/bot
