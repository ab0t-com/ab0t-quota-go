#!/usr/bin/env bash
# migrate-check.sh — ab0t-quota-go v0.3.1 -> v0.3.2: find what to LOOK AT after the upgrade.
#
# Read-only. It NEVER edits your code, writes no files and makes no network calls.
#
# WHAT CHANGED (your code compiles unchanged; no exported symbol was removed or re-typed)
#   - The default sign-up gift now listens on auth.user.registered — the event auth DELIVERS for a new
#     account. Before, it listened only on org.created / user.org_assigned, which auth never delivers to
#     a webhook: the gift never arrived. `quotactl subscribe-events` and AutoSubscribeAuthEvents now
#     subscribe to auth.user.registered (+ org.member.added when auth_events.grant_on_member_joined).
#     Once a subscription on that event exists, gifts START arriving.
#   - Capabilities().CreditGrant is false (WhyOff["credit_grant"]) when no gift can arrive.
#   - tier_provider "mesh" now reads billing's tier for each org; "jwt" reads "org_tier" by default
#     (the old "tier" claim is still read); a price-list 403 is retried every 6 h (28 times at most);
#     the Python library's billing_model names are accepted (aliases warn).
#
# WHAT IT REPORTS (per finding: file:line, the rule, what to check)
#   R1 OLDEVENTS   a script, config or Go file that names user.org_assigned (not an auth event type at
#                  all), or that subscribes / registers on org.created without auth.user.registered:
#                  a subscription on only those names delivers NO sign-up gift.
#   R2 SUBSCRIBE   a `quotactl subscribe-events` call without --events: it now subscribes to
#                  auth.user.registered, and fails (naming --rotate-secret) when your existing
#                  subscription listens to the old names — re-run it once with --rotate-secret.
#   R3 GIFTSTART   Go code with its own CreditGranter / sign-up handling: once the subscription is
#                  fixed the library grants the gift itself — do not grant it a second way.
#   R4 MODELS      a quota config (a *.json with "tiers") whose billing_model is an alias (free_tier,
#                  enterprise, metered_billing), a Python-only experimental name (metered, seat_based,
#                  subscription_with_overage: refused from the next minor), or a credit_grant with
#                  trigger scheduled_period_start (accepted, never fired).
#   R5 TIERSOURCE  a quota config with tier_provider "jwt" and no jwt_claim_key (the default claim is
#                  now org_tier; tokens from the ab0t auth service carry neither), or "mesh" (it now
#                  calls billing: needs BILLING_SERVICE_URL / BILLING_SERVICE_API_KEY).
#
# MARKING A SITE REVIEWED: put   quotago:v0.3.2-reviewed   on the line or the line above it,
# or   quotago:v0.3.2-reviewed-file   anywhere in the file. JSON has no comments: put the marker in a
# "_comment" key on the line above, or run with --advisory once you have reviewed the config.
#
# Usage:   ./migrate-check.sh [path-to-your-repo] [--advisory]
# Exit:    0 = nothing left to look at · 1 = findings to review · 2 = usage error
# See CLIENT_NOTICE.md (same directory).

set -u
set -o pipefail

TARGET="."
ADVISORY=0
for a in "$@"; do
  case "$a" in
    --advisory) ADVISORY=1 ;;
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    -*) echo "unknown option: $a" >&2; exit 2 ;;
    *) TARGET="$a" ;;
  esac
done
if [ ! -d "$TARGET" ]; then echo "not a directory: $TARGET" >&2; exit 2; fi

YELLOW='\033[1;33m'; GREEN='\033[0;32m'; RED='\033[0;31m'; BOLD='\033[1m'; NC='\033[0m'
MARK_LINE='quotago:v0.3.2-reviewed'
MARK_FILE='quotago:v0.3.2-reviewed-file'
FOUND=0

echo -e "${BOLD}ab0t-quota-go v0.3.1 -> v0.3.2 migration check (read-only)${NC}"
echo "target: $TARGET"

EXCL=(--exclude-dir=vendor --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=.archive
      --exclude-dir=migrations)
INCL=(--include='*.go' --include='*.sh' --include='*.bash' --include='*.yml' --include='*.yaml'
      --include='*.json' --include='*.toml' --include='*.env' --include='Makefile' --include='Dockerfile*')

reviewed() {
  grep -q "$MARK_FILE" "$1" 2>/dev/null && return 0
  awk -v n="$2" -v m="$MARK_LINE" 'NR==n-1 || NR==n { if (index($0, m)) f=1 } END { exit f ? 0 : 1 }' "$1"
}

# scan FILES ERE [EXCLUDE_ERE] → prints unreviewed file:line:text
scan() {
  local files="$1" re="$2" not="${3:-}" f n rest
  [ -n "$files" ] || return 0
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    grep -nE "$re" "$f" 2>/dev/null | while IFS=: read -r n rest; do
      if [ -n "$not" ] && printf '%s' "$rest" | grep -qE -e "$not"; then continue; fi
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

ALLFILES="$(grep -rlE 'org\.created|user\.org_assigned|subscribe-events' "${INCL[@]}" "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
GRANTFILES="$(grep -rlE 'CreditGranter|BuildDefaultCreditGrantHandler|OnAuthEvent\(' --include='*.go' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
CFGFILES="$(grep -rlE '"tiers"[[:space:]]*:' --include='*.json' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"

# R1: user.org_assigned anywhere; org.created on a line that subscribes/registers without auth.user.registered.
r1() {
  scan "$ALLFILES" 'user\.org_assigned'
  scan "$ALLFILES" 'org\.created.*' 'auth\.user\.registered|user\.org_assigned' \
    | grep -E -- '--events|EventTypes|event_types|OnAuthEvent|RegisterHandler|subscribe' || true
}
# R2: subscribe-events without --events on the same line.
r2() { scan "$ALLFILES" 'subscribe-events' '[-][-]events'; }

models_scan() {
  local f t line
  [ -n "$CFGFILES" ] || return 0
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    grep -q "$MARK_FILE" "$f" 2>/dev/null && continue
    if ! { command -v jq >/dev/null 2>&1 && jq -e . "$f" >/dev/null 2>&1; }; then   # coarser text check
      grep -nE '"billing_model"[[:space:]]*:[[:space:]]*"(free_tier|enterprise|metered_billing|metered|seat_based|subscription_with_overage)"|"scheduled_period_start"' "$f" \
        | sed "s#^#$f:#; s#\$# (install jq for an exact check)#"
      continue
    fi
    {
      jq -r '.tiers[]? | select(.billing_model == "free_tier" or .billing_model == "enterprise" or .billing_model == "metered_billing") | "\(.tier_id // "?")\t\(.billing_model)\talias"' "$f"
      jq -r '.tiers[]? | select(.billing_model == "metered" or .billing_model == "seat_based" or .billing_model == "subscription_with_overage") | "\(.tier_id // "?")\t\(.billing_model)\texperimental"' "$f"
      jq -r '.tiers[]? | select((.credit_grant | type) == "object" and .credit_grant.trigger == "scheduled_period_start") | "\(.tier_id // "?")\tscheduled_period_start\tnever_fires"' "$f"
    } | while IFS=$'\t' read -r t what kind; do
      line="$(grep -nE "\"tier_id\"[[:space:]]*:[[:space:]]*\"$t\"" "$f" | head -1 | cut -d: -f1)"
      case "$kind" in
        alias) echo "$f:${line:-1}: tier \"$t\": billing_model \"$what\" is an alias — write the canonical name (free_tier/enterprise → capacity_only, metered_billing → metered)" ;;
        experimental) echo "$f:${line:-1}: tier \"$t\": billing_model \"$what\" is experimental with no runtime in either library — this library will refuse it from its next minor (unless AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS=true); the Python library already does" ;;
        never_fires) echo "$f:${line:-1}: tier \"$t\": credit_grant.trigger scheduled_period_start is never fired by either library — this grant is never given" ;;
      esac
    done
  done <<< "$CFGFILES"
}

tiersource_scan() {
  local f kind key
  [ -n "$CFGFILES" ] || return 0
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    grep -q "$MARK_FILE" "$f" 2>/dev/null && continue
    if command -v jq >/dev/null 2>&1 && jq -e . "$f" >/dev/null 2>&1; then
      kind="$(jq -r '.tier_provider.type // ""' "$f")"
      key="$(jq -r '.tier_provider.jwt_claim_key // ""' "$f")"
    else
      kind="$(grep -oE '"type"[[:space:]]*:[[:space:]]*"(jwt|mesh|static)"' "$f" | head -1 | grep -oE '(jwt|mesh|static)')"
      key="$(grep -oE '"jwt_claim_key"' "$f" | head -1)"
    fi
    local line; line="$(grep -nE '"tier_provider"' "$f" | head -1 | cut -d: -f1)"
    if [ "$kind" = jwt ] && [ -z "$key" ]; then
      echo "$f:${line:-1}: tier_provider jwt without jwt_claim_key — the default claim is now \"org_tier\" (\"tier\" is still read); tokens issued by the ab0t auth service carry neither: use \"mesh\""
    elif [ "$kind" = mesh ]; then
      echo "$f:${line:-1}: tier_provider mesh now reads billing's tier for each org (GET /billing/{org_id}/tier) — set BILLING_SERVICE_URL=https://billing.service.ab0t.com and BILLING_SERVICE_API_KEY; a 403 means your key cannot read other customers' tiers (everyone gets default_tier)"
    fi
  done <<< "$CFGFILES"
}

emit "R1 OLDEVENTS — subscribes or listens only to events auth never delivers" \
  "auth delivers auth.user.registered, auth.user.login, org.member.added, org.member.removed and auth.agent.suspended to webhooks — never org.created or user.org_assigned. Use auth.user.registered (+ org.member.added with auth_events.grant_on_member_joined), then re-create your subscription: quotactl subscribe-events --rotate-secret. Then mark '$MARK_LINE'." \
  "$(r1)"

emit "R2 SUBSCRIBE — subscribe-events with the default events" \
  "the default is now auth.user.registered (+ org.member.added when the config opts in; pass --config). If your existing subscription listens to the old names the command now FAILS and names --rotate-secret: run it once with --rotate-secret (backfill sign-ups from the gap). Then mark '$MARK_LINE'." \
  "$(r2)"

emit "R3 GIFTSTART — your own grant code" \
  "once a subscription on auth.user.registered exists, the library grants the sign-up gift itself (once per person per company). If you granted it another way because it never arrived, stop doing that, or your customers get it twice. Then mark '$MARK_LINE'." \
  "$(scan "$GRANTFILES" 'CreditGranter|BuildDefaultCreditGrantHandler|OnAuthEvent\(')"

emit "R4 MODELS — billing_model names and a grant that never fires" \
  "both libraries now share one name set (capacity_only, consumption_only, subscription_with_credits, subscription_unlock_only; experimental metered, seat_based, subscription_with_overage, one_time_purchase). Write the canonical name; drop an experimental model or set AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS=true knowingly; replace scheduled_period_start (nothing fires it). A marker in a \"_comment\" key, or --advisory, records the review." \
  "$(models_scan)"

emit "R5 TIERSOURCE — where a paying customer's tier comes from" \
  "check q.Capabilities().ConfigWarnings / WhyOff[\"tier_provider\"] at startup, and run 'quotactl doctor --money --live --org <an org that pays> --expect-tier <its tier>'." \
  "$(tiersource_scan)"

[ -n "$ALLFILES$GRANTFILES$CFGFILES" ] || echo -e "\n${GREEN}No auth-event names, subscribe-events calls, grant code or quota config found.${NC}"

echo
echo "======================================================"
if [ "$FOUND" -eq 0 ]; then
  echo -e "${GREEN}Nothing left to look at for v0.3.2.${NC} Then: go build ./... && go vet ./... && go test ./... && quotactl doctor --money"
  exit 0
fi
echo -e "${RED}${FOUND} site(s) to review for v0.3.2.${NC} Your code compiles unchanged — resolve each (fix or mark"
echo "'$MARK_LINE'), re-run this checker (expect 0), then: go build ./... && go vet ./... && go test ./..."
[ "$ADVISORY" -eq 1 ] && { echo "(--advisory: exiting 0)"; exit 0; }
exit 1
