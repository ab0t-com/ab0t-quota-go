#!/usr/bin/env bash
# release.sh — orchestrate a pre-release pass.
#
# This script PREPARES a release:
#   1. clean working tree check
#   2. go vet
#   3. go test -race
#   4. cross-compile binaries (build.sh)
#   5. write release notes scaffold
#   6. print the exact tag + push commands for the operator to run
#
# It does NOT run `git tag`, `git push`, or `gh release create`. The
# operator runs those by hand per project convention.
#
# Usage:
#   scripts/release.sh v0.1.0
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "usage: scripts/release.sh <version> (e.g. v0.1.0)"
  exit 1
fi

if ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9.]+)?$ ]]; then
  echo "version must look like v0.1.0 or v0.1.0-rc.1; got: $VERSION"
  exit 1
fi

echo "==> pre-release checks for ${VERSION}"

# 1. Clean working tree.
if [ -n "$(git status --porcelain)" ]; then
  echo "ERROR: working tree dirty. Commit or stash first."
  git status --short
  exit 1
fi

# 2. Vet.
echo "==> go vet"
go vet ./...

# 3. Race-detector tests.
echo "==> go test -race -count=1 ./..."
go test -race -count=1 ./...

# 4. Secret scan — block release on any finding.
echo "==> gitleaks scan"
if command -v gitleaks >/dev/null 2>&1; then
  scripts/scan.sh
else
  echo "WARNING: gitleaks not installed; skipping local secret scan." >&2
  echo "CI still scans, but you'll only discover findings after pushing." >&2
fi

# 4. Cross-build.
echo "==> cross-compile"
VERSION="$VERSION" scripts/build.sh "$VERSION"

# 5. Release notes scaffold (only if absent).
NOTES_FILE="release/${VERSION}/RELEASE_NOTES.md"
if [ ! -f "$NOTES_FILE" ]; then
  cat > "$NOTES_FILE" <<EOF
# ab0t-quota-go ${VERSION}

## Highlights
- (fill in)

## Bug fixes
- (fill in)

## Wire-level parity notes
- Matches Python ab0t-quota v0.5.2

## Install

\`\`\`bash
go get github.com/ab0t-com/ab0t-quota-go@${VERSION}
\`\`\`

Or download the quotactl CLI binary:

- macOS Intel:   quotactl-darwin-amd64
- macOS Silicon: quotactl-darwin-arm64
- Linux x86_64:  quotactl-linux-amd64
- Linux arm64:   quotactl-linux-arm64
- Windows:       quotactl-windows-amd64.exe

SHA256SUMS included.
EOF
  echo "==> wrote release notes template to ${NOTES_FILE}"
else
  echo "==> release notes already exist at ${NOTES_FILE}"
fi

echo
echo "============================================================"
echo "  Pre-release complete. To publish (run by hand):"
echo "============================================================"
echo
echo "  # 1. Edit the release notes:"
echo "  \$EDITOR ${NOTES_FILE}"
echo
echo "  # 2. Tag the commit:"
echo "  git tag -a ${VERSION} -m 'ab0t-quota-go ${VERSION}'"
echo
echo "  # 3. Push the tag (this is what triggers the Go-module proxy"
echo "  #    and \`go get\` for consumers; no extra publish step needed):"
echo "  git push origin ${VERSION}"
echo
echo "  # 4. Create the GitHub release with the binaries:"
echo "  gh release create ${VERSION} \\"
echo "    --title 'ab0t-quota-go ${VERSION}' \\"
echo "    --notes-file ${NOTES_FILE} \\"
echo "    release/${VERSION}/quotactl-* \\"
echo "    release/${VERSION}/SHA256SUMS"
echo
echo "  # 5. Confirm consumers can fetch it:"
echo "  go install github.com/ab0t-com/ab0t-quota-go/cmd/quotactl@${VERSION}"
echo
