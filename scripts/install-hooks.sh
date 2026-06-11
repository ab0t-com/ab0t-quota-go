#!/usr/bin/env bash
# install-hooks.sh — point this repo's git at .githooks/ for hooks.
#
# Run once after cloning. Updates `git config core.hooksPath` for THIS
# repo only (not global). Idempotent.
#
# Usage:
#   scripts/install-hooks.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

if [ ! -d .git ]; then
  echo "error: not in a git repo (.git not found at $REPO_ROOT)" >&2
  exit 1
fi

git config core.hooksPath .githooks
chmod +x .githooks/*

echo "ab0t-quota-go: git hooks installed (core.hooksPath -> .githooks)"
echo
echo "Installed hooks:"
ls -1 .githooks/ | sed 's/^/  /'
echo

if ! command -v gitleaks >/dev/null 2>&1; then
  printf '\033[33mwarning:\033[0m gitleaks not installed locally.\n'
  printf 'Install one of:\n'
  printf '  brew install gitleaks\n'
  printf '  go install github.com/zricethezav/gitleaks/v8@latest\n'
  printf '  https://github.com/gitleaks/gitleaks/releases\n'
fi
