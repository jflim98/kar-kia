#!/usr/bin/env bash
# Run the assistant.
#
#   ./run.sh                 # uses ./data locally, or ~/asst-data on a server
#   ./run.sh /path/to/data   # explicit data dir
#   DATA_DIR=/path ./run.sh  # same, via env
#
# Auto-scaffolds the data dir on first run. Secrets/config live in the data dir
# (secrets.yaml / config.yaml); edit those before or after the first run.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Data dir: arg > $DATA_DIR > ./data if it exists > ~/asst-data.
if [[ -n "${1:-}" ]]; then
  DATA_DIR="$1"
elif [[ -n "${DATA_DIR:-}" ]]; then
  DATA_DIR="$DATA_DIR"
elif [[ -d "$SCRIPT_DIR/data" ]]; then
  DATA_DIR="$SCRIPT_DIR/data"
else
  DATA_DIR="$HOME/asst-data"
fi

# Find the binary: a build next to this script, then on PATH.
if [[ -x "$SCRIPT_DIR/assistant" ]]; then
  BIN="$SCRIPT_DIR/assistant"
elif [[ -x "$SCRIPT_DIR/assistant-linux" ]]; then
  BIN="$SCRIPT_DIR/assistant-linux"
elif command -v assistant >/dev/null 2>&1; then
  BIN="$(command -v assistant)"
else
  echo "error: assistant binary not found." >&2
  echo "  build it:   go build -o assistant ./cmd/assistant" >&2
  echo "  or place 'assistant' / 'assistant-linux' next to run.sh, or on PATH." >&2
  exit 1
fi

echo "assistant: $BIN"
echo "data dir:  $DATA_DIR"

# Scaffold on first run.
if [[ ! -f "$DATA_DIR/config.yaml" ]]; then
  echo "first run — scaffolding $DATA_DIR ..."
  "$BIN" init --data-dir "$DATA_DIR"
  echo "edit $DATA_DIR/secrets.yaml and $DATA_DIR/config.yaml, then run again."
  exit 0
fi

exec "$BIN" run --data-dir "$DATA_DIR"
