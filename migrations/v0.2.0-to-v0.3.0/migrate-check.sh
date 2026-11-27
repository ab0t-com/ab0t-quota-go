#!/usr/bin/env bash
# migrate-check.sh — ab0t-quota-go v0.2.0 -> v0.3.0: find what to LOOK AT after the upgrade.
#
# Read-only. It NEVER edits your code, writes no files and makes no network calls.
#
# WHAT CHANGED (your code compiles unchanged; no exported symbol changed)
#   - ONE DEFAULT CHANGED: a config file that omits enforcement.enabled now runs with quotas ENFORCED
#     (it was false — every request allowed), and a credit_grant that omits reset_on_downgrade now has
#     it true (it was false). Python parity. Write the keys to keep the old behaviour.
#   - quota.Setup sends your declared prices (pricing.products) to billing once per start
#     (PUT /billing/price-list/{service_name}), in a goroutine; opt out with
#     "billing_integration": {"price_list": false}.
#   - The default sign-up gift lands on the person's own workspace when the auth event names it
#     (data.workspace_org_id), else on the event's org as before.
#   - Opt-in: "auth_events": {"grant_on_member_joined": true} gives the sign-up gift to a person who
#     joins with an account they already have (org.member.added), once per person per company.
#
# WHAT IT REPORTS (per finding: file:line, the rule, what to check)
#   R0 DEFAULTS   a quota config (a *.json with "tiers") that omits enforcement.enabled, or a tier whose
#                 credit_grant omits reset_on_downgrade. NOT silenced by a marker: write the key
#                 (true = the new default, false = the old behaviour). Uses jq when installed, else a
#                 coarser text check.
#   R1 PRICELIST  a JSON config declaring "pricing" with "products": its prices will be published to
#                 billing at startup (or set billing_integration.price_list to false).
#   R2 WORKSPACE  Go code that implements or reads authevents.CreditGrantRequest (a custom granter or
#                 hook): CreditGrantRequest.OrgID can now be the person's workspace.
#
# MARKING A SITE REVIEWED: put   quotago:v0.3.0-reviewed   on the line or the line above it,
# or   quotago:v0.3.0-reviewed-file   anywhere in the file. JSON has no comments: put the marker in a
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
    -h|--help) sed -n '2,34p' "$0"; exit 0 ;;
    -*) echo "unknown option: $a" >&2; exit 2 ;;
    *) TARGET="$a" ;;
  esac
done
if [ ! -d "$TARGET" ]; then echo "not a directory: $TARGET" >&2; exit 2; fi

YELLOW='\033[1;33m'; GREEN='\033[0;32m'; RED='\033[0;31m'; BOLD='\033[1m'; NC='\033[0m'
MARK_LINE='quotago:v0.3.0-reviewed'
MARK_FILE='quotago:v0.3.0-reviewed-file'
FOUND=0

echo -e "${BOLD}ab0t-quota-go v0.2.0 -> v0.3.0 migration check (read-only)${NC}"
echo "target: $TARGET"

EXCL=(--exclude-dir=vendor --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=.archive
      --exclude-dir=migrations)

PRICERE='"products"[[:space:]]*:'
GRANTRE='CreditGrantRequest'
PRICEFILES="$(grep -rlE '"pricing"[[:space:]]*:' --include='*.json' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
GRANTFILES="$(grep -rlE "$GRANTRE" --include='*.go' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"

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

# defaults_scan → file:line:what for every quota config (a *.json with "tiers") that omits
# enforcement.enabled, or a tier whose credit_grant omits reset_on_downgrade. Not marker-suppressible.
CFGFILES="$(grep -rlE '"tiers"[[:space:]]*:' --include='*.json' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
defaults_scan() {
  local f t line
  [ -n "$CFGFILES" ] || return 0
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    if command -v jq >/dev/null 2>&1 && jq -e . "$f" >/dev/null 2>&1; then
      if [ "$(jq -r 'if (.enforcement | type) == "object" and (.enforcement | has("enabled")) then "set" else "unset" end' "$f")" = unset ]; then
        line="$(grep -nE '"enforcement"[[:space:]]*:' "$f" | head -1 | cut -d: -f1)"
        echo "$f:${line:-1}: enforcement.enabled omitted — quotas are now ENFORCED (it was false: every request allowed)"
      fi
      jq -r '.tiers[]? | select((.credit_grant | type) == "object" and (.credit_grant | has("reset_on_downgrade") | not)) | .tier_id // "?"' "$f" 2>/dev/null \
        | while IFS= read -r t; do
            line="$(grep -nE "\"tier_id\"[[:space:]]*:[[:space:]]*\"$t\"" "$f" | head -1 | cut -d: -f1)"
            echo "$f:${line:-1}: tier \"$t\": credit_grant.reset_on_downgrade omitted — now true (it was false)"
          done
    else   # no jq (or not plain JSON): coarser text check
      grep -qE '"enforcement"[[:space:]]*:' "$f" && grep -qE '"enabled"[[:space:]]*:' "$f" \
        || echo "$f:1: enforcement.enabled appears to be omitted — quotas are now ENFORCED (install jq for an exact check)"
      [ "$(grep -cE '"credit_grant"[[:space:]]*:' "$f")" -le "$(grep -cE '"reset_on_downgrade"[[:space:]]*:' "$f")" ] \
        || echo "$f:1: a credit_grant appears to omit reset_on_downgrade — now true (install jq for an exact check)"
    fi
  done <<< "$CFGFILES"
}

emit "R0 DEFAULTS — a config that omits a key whose default changed" \
  "v0.3.0 loads an omitted enforcement.enabled as TRUE (quotas ENFORCED; it was false — every request allowed) and an omitted credit_grant.reset_on_downgrade as TRUE (it was false) — the Python library's defaults. Write the key in the file: true to accept the new default (and silence the startup warning), or \"enforcement\": {\"enabled\": false} / \"reset_on_downgrade\": false to keep the v0.2.0 behaviour. A marker does not silence this rule." \
  "$(defaults_scan)"

emit "R1 PRICELIST — declared prices will be published to billing" \
  "quota.Setup now sends pricing.products to billing once per start (PUT /billing/price-list/{service_name}, your billing key), from a goroutine — Setup never waits; q.Capabilities().PriceList shows the outcome. Check the prices are the ones you charge (they are what billing compares each usage event with). To keep them out of billing set \"billing_integration\": {\"price_list\": false}. Then mark '$MARK_LINE'." \
  "$(scan "$PRICEFILES" "$PRICERE")"

emit "R2 WORKSPACE — code that reads a sign-up grant request" \
  "the default sign-up handler now gives the gift to the person's own workspace when the auth event names it (data.workspace_org_id): CreditGrantRequest.OrgID may be that workspace, not the org the person joined. Check a custom CreditGranter / OnGranted hook does not assume otherwise. Then mark '$MARK_LINE'." \
  "$(scan "$GRANTFILES" "$GRANTRE")"

[ -n "$CFGFILES$PRICEFILES$GRANTFILES" ] || echo -e "\n${GREEN}No quota config found, no config declares pricing and no code reads a sign-up grant request.${NC}"

echo
echo "======================================================"
if [ "$FOUND" -eq 0 ]; then
  echo -e "${GREEN}Nothing left to look at for v0.3.0.${NC} Then: go build ./... && go vet ./... && go test ./..."
  exit 0
fi
echo -e "${RED}${FOUND} site(s) to review for v0.3.0.${NC} Your code compiles unchanged — resolve each (check or mark"
echo "'$MARK_LINE'), re-run this checker (expect 0), then: go build ./... && go vet ./... && go test ./..."
[ "$ADVISORY" -eq 1 ] && { echo "(--advisory: exiting 0)"; exit 0; }
exit 1
