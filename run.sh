#!/usr/bin/env bash
# Build (if needed) and run the collectif server.
# Usage: ./run.sh [flags passed to the binary]
#   ./run.sh
#   ./run.sh -port 8080
#   ./run.sh -bind 127.0.0.1 -port 7317
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="$ROOT/src"
BIN="$ROOT/collectif"

if ! command -v go >/dev/null 2>&1; then
  if [[ -x "$HOME/go-sdk/go/bin/go" ]]; then
    export PATH="$HOME/go-sdk/go/bin:$PATH"
  else
    echo "error: 'go' not found on PATH and no ~/go-sdk/go/bin/go fallback" >&2
    exit 1
  fi
fi

if ! command -v claude >/dev/null 2>&1; then
  echo "warning: 'claude' not found on PATH — agent spawns will fail" >&2
fi

needs_build=0
if [[ ! -x "$BIN" ]]; then
  needs_build=1
else
  while IFS= read -r -d '' f; do
    if [[ "$f" -nt "$BIN" ]]; then needs_build=1; break; fi
  done < <(find "$ROOT/go.mod" "$ROOT/go.sum" "$SRC" -type f \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' -o -path '*/static/*' \) -print0)
fi

if [[ "$needs_build" -eq 1 ]]; then
  echo "building collectif..."
  (cd "$ROOT" && go build -o "$BIN" ./src)
fi

echo "starting collectif (Ctrl+C to stop)"
exec "$BIN" "$@"
