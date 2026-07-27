#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DIST_DIR="${ROOT_DIR}/dist"

mkdir -p "${DIST_DIR}"

cd "${ROOT_DIR}"

ARCH=$(uname -m)
GOARCH="$ARCH"

# pkg-config path: /opt/homebrew for ARM Mac, /usr/local for Intel Mac
if [ -d "/opt/homebrew/lib/pkgconfig" ]; then
  export PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig}"
elif [ -d "/usr/local/lib/pkgconfig" ]; then
  export PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-/usr/local/lib/pkgconfig}"
fi

export CGO_ENABLED=1
export GOOS=darwin

echo "Building DJOneHub for macOS ${ARCH}..."
echo "  PKG_CONFIG_PATH=${PKG_CONFIG_PATH}"

go build \
  -p 2 \
  -trimpath -ldflags="-s -w" \
  -o "${DIST_DIR}/djonehub-macos-${ARCH}" ./cmd/djonehub-macos

cp "${DIST_DIR}/djonehub-macos-${ARCH}" "${DIST_DIR}/djonehub-macos"

echo ""
echo "Build complete: ${DIST_DIR}/djonehub-macos-${ARCH}"
echo "Run with: ./dist/djonehub-macos"
