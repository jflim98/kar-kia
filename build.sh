#!/usr/bin/env bash
# Build a static Linux binary for Debian and package it as a tar.gz.
#
#   ./build.sh                 # builds linux/amd64
#   GOARCH=arm64 ./build.sh    # builds linux/arm64
#
# Produces (next to this script):
#   assistant-linux            the binary (matches run.sh's expected name)
#   assistant-linux.tar.gz     the compressed binary
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

BIN="assistant-linux"
ARCH="${GOARCH:-amd64}"

# Build the static binary (CGO off => no glibc dependency). Flags match the Dockerfile.
echo "building $BIN (linux/$ARCH) ..."
GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/assistant

# Ensure the exec bit is set so the extracted binary runs on Debian. chmod handles native
# Linux/macOS builds; --mode forces 0755 into the archive even on Windows/NTFS, where the
# local filesystem has no exec bit for tar to read.
chmod +x "$BIN"

# Compress into a tar.gz.
echo "packaging $BIN.tar.gz ..."
tar --mode=0755 -czf "$BIN.tar.gz" "$BIN"

echo "done:"
echo "  $SCRIPT_DIR/$BIN"
echo "  $SCRIPT_DIR/$BIN.tar.gz"
