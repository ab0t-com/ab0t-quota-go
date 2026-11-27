#!/usr/bin/env bash
# migrate-check.sh — ab0t-quota-go v0.1.12 -> v0.2.0: find what to LOOK AT after the upgrade.
#
# Read-only. It NEVER edits your code, writes no files and makes no network calls.
#
# WHAT CHANGED (your code compiles unchanged; no exported symbol changed)
#   - An unresolved tier (engine.UnknownTier, reason "tier_unresolved") no longer proceeds:
#     Result.Allowed() is false and the middleware answers 503 instead of admitting the request with
#     no limit. Opt out with "enforcement": {"allow_unknown_tier": true} in quota-config.json.
#   - With billing wired, a durable outbox and no outbox.sns_topic_arn, quota.Setup now starts and
#     sends lifecycle events to billing over HTTPS (it used to refuse to start).
#   - Billing/payment env vars: BILLING_SERVICE_URL, BILLING_SERVICE_API_KEY, PAYMENT_SERVICE_URL,
#     PAYMENT_SERVICE_API_KEY (what `authsetup connect` prints). The AB0T_QUOTA_* names still work.
#
# WHAT IT REPORTS (per finding: file:line, the rule, what to check)
#   R1 UNKNOWNTIER  a site that reads a quota decision (.Allowed()), names engine.UnknownTier, calls
#                   .Engine.Acquire(, or expects the "tier_unresolved" reason (code, alert rule, test,
#                   dashboard): an unresolved tier is now refused (503 / ErrTierUnresolved), not admitted.
#   R3 ENVNAMES     an old env-var name (AB0T_QUOTA_BILLING_URL / _TOKEN, AB0T_QUOTA_PAYMENT_URL / _TOKEN):
#                   rename it to the canonical name (same value). AB0T_QUOTA_SERVICE_TOKEN is unchanged.
#   (INFO) quota-config files without tier_provider.default_tier; GuardConfig/MiddlewareDeps FailOpen
#          (it does not admit an unresolved tier); configs with an outbox but no sns_topic_arn (HTTPS).
#
# MARKING A SITE REVIEWED: put   quotago:v0.2.0-reviewed   on the line or the line above it,
# or   quotago:v0.2.0-reviewed-file   anywhere in the file. JSON has no comments: put the marker in a
# "_comment" key on the line above, or run with --advisory once you have reviewed the config.
#
# Usage:   ./migrate-check.sh [path-to-your-repo] [--advisory]
# Exit:    0 = nothing left to look at · 1 = findings to review · 2 = usage error
# See MIGRATION.md and CLIENT_NOTICE.md (same directory).

set -u
set -o pipefail

TARGET="."
ADVISORY=0
for a in "$@"; do
  case "$a" in
    --advisory) ADVISORY=1 ;;
    -h|--help) sed -n '2,31p' "$0"; exit 0 ;;
    -*) echo "unknown option: $a" >&2; exit 2 ;;
    *) TARGET="$a" ;;
  esac
done
if [ ! -d "$TARGET" ]; then echo "not a directory: $TARGET" >&2; exit 2; fi

YELLOW='\033[1;33m'; GREEN='\033[0;32m'; RED='\033[0;31m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
MARK_LINE='quotago:v0.2.0-reviewed'
MARK_FILE='quotago:v0.2.0-reviewed-file'
FOUND=0

echo -e "${BOLD}ab0t-quota-go v0.1.12 -> v0.2.0 migration check (read-only; no source change required)${NC}"
echo "target: $TARGET"

EXCL=(--exclude-dir=vendor --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=.archive
      --exclude-dir=migrations)

# (prose — *.md — is documentation, not a decision site; it is not scanned)
TIERRE='\.Allowed\(\)|engine\.UnknownTier|tier_unresolved|\.Engine\.Acquire\('
ENVRE='AB0T_QUOTA_(BILLING|PAYMENT)_(URL|TOKEN)'
TIERFILES="$(grep -rlE "$TIERRE" --exclude='*.md' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
ENVFILES="$(grep -rlE "$ENVRE" --exclude='*.md' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
CFGFILES="$(grep -rlE '"tiers"[[:space:]]*:' --include='quota-config*.json' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
FAILOPEN="$(grep -rnE 'FailOpen:[[:space:]]*true' --include='*.go' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"

# reviewed FILE LINE → 0 if the site (or its file) carries the marker
reviewed() {
  grep -q "$MARK_FILE" "$1" 2>/dev/null && return 0
  awk -v n="$2" -v m="$MARK_LINE" 'NR==n-1 || NR==n { if (index($0, m)) f=1 } END { exit f ? 0 : 1 }' "$1"
}

# scan "FILES" ERE → prints unreviewed file:line:text
scan() {
  local files="$1" re="$2" f n rest
  [ -n "$files" ] || return 0
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    grep -nE "$re" "$f" 2>/dev/null | while IFS=: read -r n rest; do
      reviewed "$f" "$n" || echo "$f:$n:$rest"
    done
  done <<< "$files"
}

emit() { # LABEL ADVICE HITS
  local hits="$3" n
  [ -n "$hits" ] || return 0
  n="$(printf '%s\n' "$hits" | grep -c .)"
  FOUND=$((FOUND + n))
  echo -e "\n${YELLOW}● $1${NC}  ($n)"
  echo -e "  ${BOLD}check:${NC} $2"
  printf '%s\n' "$hits" | sed 's/^/    /'
}

emit "R1 UNKNOWNTIER — a quota decision / the unresolved-tier reason" \
  "an unresolved tier (engine.UnknownTier, reason tier_unresolved) is now REFUSED: Result.Allowed() is false and the middleware answers 503 {\"error\":\"quota_service_unavailable\"} without calling your handler; Engine.Acquire returns an error wrapping engine.ErrTierUnresolved (answer it 503) (v0.1.12 admitted both with no limit). If admitted work costs money, keep this and make sure the tier resolves (tier_provider.default_tier, or the cached mesh provider). Only if it must proceed, set \"enforcement\": {\"allow_unknown_tier\": true}. An alert/test that expected tier_unresolved to pass (or a 429) now sees a 503. Then mark '$MARK_LINE'." \
  "$(scan "$TIERFILES" "$TIERRE")"

emit "R3 ENVNAMES — an old billing/payment env-var name" \
  "rename to the names 'authsetup connect billing|payment' prints (same value): AB0T_QUOTA_BILLING_URL -> BILLING_SERVICE_URL, AB0T_QUOTA_BILLING_TOKEN -> BILLING_SERVICE_API_KEY, AB0T_QUOTA_PAYMENT_URL -> PAYMENT_SERVICE_URL, AB0T_QUOTA_PAYMENT_TOKEN -> PAYMENT_SERVICE_API_KEY (AB0T_QUOTA_SERVICE_TOKEN, the shared fallback, is unchanged). The old names still work (each reported once per process in a deprecation warning); if both are set the new name wins. Keep the old name until you will not roll back to v0.1.12, then mark '$MARK_LINE' or rename." \
  "$(scan "$ENVFILES" "$ENVRE")"

if [ -n "$CFGFILES" ]; then
  NODEFAULT=""
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    if grep -qE '"tier_provider"' "$f" && ! grep -qE '"default_tier"[[:space:]]*:[[:space:]]*"[^"]+"' "$f"; then
      NODEFAULT="${NODEFAULT}${f}"$'\n'
    fi
  done <<< "$CFGFILES"
  if [ -n "$NODEFAULT" ]; then
    echo -e "\n${CYAN}? INFO — quota-config without tier_provider.default_tier: a caller the provider cannot map is now refused with 503 (v0.1.12 admitted it with no limit). Add a default_tier if such callers should get a tier.${NC}"
    printf '%s' "$NODEFAULT" | sed 's/^/    /'
  fi
  NOSNS=""
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    if grep -qE '"outbox"' "$f" && ! grep -qE '"sns_topic_arn"[[:space:]]*:[[:space:]]*"[^"]+"' "$f"; then
      NOSNS="${NOSNS}${f}"$'\n'
    fi
  done <<< "$CFGFILES"
  if [ -n "$NOSNS" ]; then
    echo -e "\n${CYAN}? INFO — an outbox without outbox.sns_topic_arn: with billing wired, v0.2.0 sends lifecycle events to billing over HTTPS (v0.1.12 refused to start in paid mode). Check Capabilities().Resolved[\"lifecycle_transport\"].${NC}"
    printf '%s' "$NOSNS" | sed 's/^/    /'
  fi
fi
if [ -n "$FAILOPEN" ]; then
  echo -e "\n${CYAN}? INFO — FailOpen: true covers engine ERRORS only; it does not admit an unresolved tier (that is enforcement.allow_unknown_tier).${NC}"
  printf '%s\n' "$FAILOPEN" | sed 's/^/    /'
fi
[ -n "$TIERFILES$ENVFILES$CFGFILES$FAILOPEN" ] || echo -e "\n${GREEN}No file reads a quota decision, names the unresolved-tier reason, uses an old env-var name or holds a quota-config.json.${NC}"

echo
echo "======================================================"
if [ "$FOUND" -eq 0 ]; then
  echo -e "${GREEN}Nothing left to look at for v0.2.0.${NC} Then: go build ./... && go vet ./... && go test ./..."
  exit 0
fi
echo -e "${RED}${FOUND} site(s) to review for v0.2.0.${NC} Your code compiles unchanged — resolve each (fix or mark"
echo "'$MARK_LINE'), re-run this checker (expect 0), then: go build ./... && go vet ./... && go test ./..."
[ "$ADVISORY" -eq 1 ] && { echo "(--advisory: exiting 0)"; exit 0; }
exit 1
