#!/usr/bin/env bash
# scan.sh — run gitleaks against this project's files.
#
# Default mode: scan the working tree (no git history). This is the
# right behavior when ab0t-quota-go lives inside a larger mono-repo
# whose unrelated history we don't want to scan. When ab0t-quota-go
# ships as its own GitHub repo, history-mode (--history) also works
# correctly.
#
# Usage:
#   scripts/scan.sh                  # default: working tree only
#   scripts/scan.sh --history        # also scan git history (own-repo mode)
#   scripts/scan.sh --staged         # only staged changes (what pre-commit does)
#   scripts/scan.sh --since v0.1.0   # only commits after the tag
#   scripts/scan.sh --report out.json   # write JSON report

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

if ! command -v gitleaks >/dev/null 2>&1; then
  echo "error: gitleaks not installed." >&2
  echo "  brew install gitleaks" >&2
  echo "  OR https://github.com/gitleaks/gitleaks/releases" >&2
  exit 1
fi

MODE=tree
REPORT=""
SINCE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --staged)  MODE=staged; shift ;;
    --history) MODE=history; shift ;;
    --since)   SINCE="$2"; shift 2 ;;
    --report)  REPORT="$2"; shift 2 ;;
    -h|--help) sed -n '1,17p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

ARGS=(--config .gitleaks.toml --redact --verbose --no-banner)
if [ -n "$REPORT" ]; then
  ARGS+=(--report-path "$REPORT" --report-format json)
fi
if [ -n "$SINCE" ]; then
  ARGS+=(--log-opts "$SINCE..HEAD")
fi

case "$MODE" in
  staged)
    echo "scanning staged changes only"
    gitleaks protect --staged "${ARGS[@]}"
    ;;
  tree)
    echo "scanning working tree (no git history)"
    gitleaks detect --no-git --source=. "${ARGS[@]}"
    ;;
  history)
    echo "scanning full git history (own-repo mode)"
    gitleaks detect "${ARGS[@]}"
    ;;
esac
