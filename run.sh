#!/bin/bash
# run.sh — start leecher.
#
# Prerequisites:
#   sudo bash install.sh   (first time only)
#
# Usage:
#   bash run.sh             (default: uses config.json)

set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$DIR/leecher"

if [[ ! -x "$BINARY" ]]; then
    echo "  ERROR: leecher binary not found at $BINARY" >&2
    exit 1
fi

# Create config from example if missing
if [[ ! -f "$DIR/config.json" && -f "$DIR/config.json.example" ]]; then
    cp "$DIR/config.json.example" "$DIR/config.json"
    echo "  Created config.json from example."
fi

# Ensure downloads directory exists
mkdir -p "$DIR/downloads"

cleanup() {
    echo ""
    echo "  Stopping leecher..."
    kill "$APP_PID" 2>/dev/null || true
    wait 2>/dev/null || true
    echo "  Stopped."
    exit 0
}
trap cleanup INT TERM

# ---------------------------------------------------------------------------
# Start leecher
# ---------------------------------------------------------------------------
# If running as root and leecher user exists, drop privileges.
if [[ $EUID -eq 0 ]] && id -u leecher &>/dev/null; then
    runuser -u leecher -- "$BINARY" &
else
    "$BINARY" &
fi
APP_PID=$!

echo "  leecher started (PID $APP_PID)"
wait $APP_PID || true
