#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DIST_DIR="${ROOT_DIR}/dist"

mkdir -p "${DIST_DIR}"

cd "${ROOT_DIR}"

# Intel Mac: use /usr/local (Homebrew on Intel)
export PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-/usr/local/lib/pkgconfig}"
export CGO_ENABLED=1
export GOOS=darwin
export GOARCH=amd64

echo "Building DJOneHub for macOS x86_64 (Intel)..."
echo "  PKG_CONFIG_PATH=${PKG_CONFIG_PATH}"

go build \
  -p 2 \
  -trimpath -ldflags="-s -w" \
  -o "${DIST_DIR}/djonehub-macos-x86_64" ./cmd/djonehub-macos

cp "${DIST_DIR}/djonehub-macos-x86_64" "${DIST_DIR}/djonehub-macos"

echo ""
echo "Build complete: ${DIST_DIR}/djonehub-macos-x86_64"
echo "Run with: ./dist/djonehub-macos"
