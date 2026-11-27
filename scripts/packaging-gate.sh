#!/usr/bin/env bash
# packaging-gate.sh — prove the module, as a consumer gets it, declares and builds everything it needs.
#
# The release-blocking class this prevents: code that imports something the published metadata does
# not provide (a dependency missing from go.mod / go.sum, a go.mod that needs an update). It builds
# and passes in this checkout's warm module cache, and fails for the first consumer.
#
# Checks (fail-fast; exit 1 on the first failure):
#   1. go.mod / go.sum are tidy          (`go mod tidy -diff`; on Go < 1.23 a copy-and-compare)
#   2. builds with a read-only go.mod    (GOFLAGS=-mod=readonly go build ./...)
#   3. module cache matches go.sum       (go mod verify)
#   4. consumer smoke                    a FRESH consumer module (outside this repo) requires the
#                                        library via `replace`, resolves its own go.sum from scratch,
#                                        builds and runs scripts/packaginggate/main.go: meter.New +
#                                        Reserve against the in-process fake, billing.New, mesh.Resolve,
#                                        exact-money rounding. No network is used by the program; the
#                                        dependency resolution in step 4 may download modules (use your
#                                        GOPROXY / GOFLAGS as for any build).
#
# Usage:
#   scripts/packaging-gate.sh            # run all checks (work dir: ${TMPDIR:-/tmp}/ab0t-quota-go-packaging-gate.*,
#                                        # left in place for inspection; the OS temp cleanup removes it)
#
# Called by scripts/release.sh. Exit: 0 = all passed · 1 = a check failed.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

MODULE="$(go list -m)"
GATE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/ab0t-quota-go-packaging-gate.XXXXXX")"

fail() { echo "x PACKAGING GATE FAILED — $*" >&2; exit 1; }

echo "==> packaging gate for ${MODULE}"

# 1. tidy
echo "==> 1/4 go.mod / go.sum are tidy"
if go help mod tidy 2>/dev/null | grep -q -- '-diff'; then
  if ! go mod tidy -diff >"${GATE_DIR}/tidy.diff" 2>&1; then
    cat "${GATE_DIR}/tidy.diff" >&2
    fail "go.mod / go.sum are not tidy — run 'go mod tidy' and commit the result"
  fi
else
  mkdir -p "${GATE_DIR}/tidy"
  cp go.mod go.sum "${GATE_DIR}/tidy/"
  restore() { cp "${GATE_DIR}/tidy/go.mod" "${GATE_DIR}/tidy/go.sum" "$REPO_ROOT/"; }
  trap restore EXIT
  go mod tidy
  if ! cmp -s go.mod "${GATE_DIR}/tidy/go.mod" || ! cmp -s go.sum "${GATE_DIR}/tidy/go.sum"; then
    diff -u "${GATE_DIR}/tidy/go.mod" go.mod >&2 || true
    fail "go.mod / go.sum are not tidy — run 'go mod tidy' and commit the result"
  fi
  restore
  trap - EXIT
fi

# 2. read-only build
echo "==> 2/4 build with a read-only go.mod"
GOFLAGS=-mod=readonly go build ./... || fail "the module does not build with -mod=readonly (go.mod needs an update)"

# 3. verify
echo "==> 3/4 go mod verify"
go mod verify || fail "go mod verify: the module cache does not match go.sum"

# 4. consumer smoke
echo "==> 4/4 consumer smoke (fresh module: ${GATE_DIR}/consumer)"
mkdir -p "${GATE_DIR}/consumer"
GOVERSION_LINE="$(grep -E '^go [0-9]' go.mod | head -1)"
{
  echo "module packaging-gate.invalid/consumer"
  echo
  echo "${GOVERSION_LINE}"
  echo
  echo "require ${MODULE} v0.0.0"
  echo
  echo "replace ${MODULE} => ${REPO_ROOT}"
} >"${GATE_DIR}/consumer/go.mod"
# The program is tracked under a build tag in this repo; strip the tag line in the consumer copy.
grep -v '^//go:build packaging_gate' scripts/packaginggate/main.go >"${GATE_DIR}/consumer/main.go"
(
  cd "${GATE_DIR}/consumer"
  GOFLAGS=-mod=mod go mod tidy || fail "a consumer cannot resolve the module's dependencies (go mod tidy)"
  go build -o "${GATE_DIR}/consumer/smoke" . || fail "a consumer cannot build against the module"
  "${GATE_DIR}/consumer/smoke" || fail "the consumer smoke program failed (see FAIL lines above)"
)

echo "==> packaging gate passed (work dir: ${GATE_DIR})"
