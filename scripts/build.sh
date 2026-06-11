#!/usr/bin/env bash
# build.sh — cross-compile quotactl for all supported platforms.
#
# Output: release/<version>/quotactl-<os>-<arch>[.exe] + SHA256SUMS
#
# Usage:
#   scripts/build.sh                # version = git describe (or v0.1.0-dev)
#   scripts/build.sh v0.1.0          # explicit version
#   VERSION=v0.1.0 scripts/build.sh  # via env
#
# Targets (5 platforms, matches the matrix the Python lib distributes via
# PyInstaller in CI). We intentionally do NOT distribute the library
# packages — Go consumers import them with `go get`. Only the quotactl
# admin CLI ships as a binary.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

VERSION="${1:-${VERSION:-}}"
if [ -z "$VERSION" ]; then
  if VERSION="$(git describe --tags --always --dirty 2>/dev/null)"; then
    :
  else
    VERSION="v0.1.0-dev"
  fi
fi

OUT_DIR="release/${VERSION}"
mkdir -p "$OUT_DIR"

# (os, arch) pairs. arm64 covers Apple Silicon + AWS Graviton + most modern arm.
TARGETS=(
  "darwin amd64"
  "darwin arm64"
  "linux amd64"
  "linux arm64"
  "windows amd64"
)

COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

LDFLAGS="-s -w \
  -X main.version=${VERSION} \
  -X main.commit=${COMMIT} \
  -X main.buildTime=${BUILD_TIME}"

echo "building quotactl ${VERSION} (commit=${COMMIT})"
echo "output: ${OUT_DIR}"

for target in "${TARGETS[@]}"; do
  read -r OS ARCH <<<"$target"
  EXT=""
  [ "$OS" = "windows" ] && EXT=".exe"
  BIN="quotactl-${OS}-${ARCH}${EXT}"
  OUT="${OUT_DIR}/${BIN}"
  echo "  -> ${BIN}"
  GOOS="$OS" GOARCH="$ARCH" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$OUT" ./cmd/quotactl
done

# Checksums — sha256sum on Linux, shasum -a 256 on macOS.
echo "computing checksums"
( cd "$OUT_DIR" && \
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum quotactl-* > SHA256SUMS
  else
    shasum -a 256 quotactl-* > SHA256SUMS
  fi )

echo
echo "done. artifacts:"
ls -la "$OUT_DIR"
echo
echo "to release: scripts/release.sh ${VERSION}"
