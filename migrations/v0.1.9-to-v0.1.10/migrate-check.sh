#!/usr/bin/env bash
# migrate-check.sh — ab0t-quota-go v0.1.9 -> v0.1.10: find what to LOOK AT after the upgrade.
#
# Read-only. It NEVER edits your code and writes no files.
#
# WHAT CHANGED (bug fixes + additive API — your code compiles unchanged)
#   - The auth-event receiver answers at its full documented path (was 404 for every event).
#   - Only credit_grant.trigger "signup" grants on auth events (v0.1.9 granted any trigger).
#   - billing.Client.GrantCredit uses billing's real promotional-credit route (was always 404);
#     new quota.BillingCreditGranter(nil), GrantPromotionalCredit, ApplyCreditGrant.
#   - quotactl subscribe-events / replay / backfill exit non-zero on failure; events / delete-user refuse;
#     replay/backfill read the secret from AB0T_AUTH_WEBHOOK_SECRET (no longer the flag default).
#   - Settlement-fallback money fixes; per-org alert cooldown.
#
# WHAT IT REPORTS (per finding: file:line, the rule, what to check)
#   R1 GRANTER      a hand-written granter calling promotional-credit — a built-in exists; read
#                   MIGRATION.md §1 (idempotency-key shape) BEFORE switching.
#   R2 TRIGGER      a quota-config credit_grant whose trigger is not "signup": it no longer grants on
#                   org.created / user.org_assigned.
#   R3 NOTRIGGER    a credit_grant with no trigger: still granted (with a warning); add "trigger": "signup".
#   R4 CLI          scripts/CI running quotactl subscribe-events|replay|backfill|events|delete-user:
#                   exit codes changed (non-zero on failure; events/delete-user refuse).
#   R5 SECRET-ARGV  quotactl replay/backfill given --secret on the command line (visible in ps).
#   R6 GRANTCREDIT  a .GrantCredit( call on the billing client: it now really grants.
#   R7 RECORDUSAGE  (INFO, unchanged behaviour) .RecordUsage( calls — set Cost/PlatformFee "0" on metering rows.
#   R8 CONFIGPATH   AB0T_QUOTA_CONFIG_PATH anywhere: never read — rename to QUOTA_CONFIG_PATH.
#   R9 STRIPPREFIX  (INFO) http.StripPrefix around WebhookHandler: still works; optional simplify.
#
# MARKING A SITE REVIEWED: put   quotago:v0.1.10-reviewed   on the line or the line above it,
# or   quotago:v0.1.10-reviewed-file   anywhere in the file.
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

YELLOW='\033[1;33m'; GREEN='\033[0;32m'; RED='\033[0;31m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
MARK_LINE='quotago:v0.1.10-reviewed'
MARK_FILE='quotago:v0.1.10-reviewed-file'
FOUND=0

echo -e "${BOLD}ab0t-quota-go v0.1.9 -> v0.1.10 migration check (read-only; no source change required)${NC}"
echo "target: $TARGET"

EXCL=(--exclude-dir=vendor --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=.archive
      --exclude-dir=migrations)

# Go files importing the library (non-test and test).
GOFILES="$(grep -rlE '"github\.com/ab0t-com/ab0t-quota-go(/[a-z]+)*"' --include='*.go' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
CFGFILES="$(grep -rlE '"tiers"[[:space:]]*:' --include='quota-config*.json' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
SCRIPTFILES="$(grep -rlE 'quotactl' --include='*.sh' --include='Makefile' --include='*.mk' --include='*.yml' \
  --include='*.yaml' --include='Dockerfile' --include='*.bash' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"

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

NONTEST="$(printf '%s\n' "$GOFILES" | grep -v '_test\.go$' || true)"

emit "R1 GRANTER — a hand-written promotional-credit granter" \
  "a built-in exists: quota.BillingCreditGranter(nil). Its billing idempotency key is user:{u}:initial_credit:{tier}. If yours used another shape, switching is safe only with a DURABLE handler ledger (Redis/DDB) — MIGRATION.md §1. Keep or switch, then mark '$MARK_LINE'." \
  "$(scan "$NONTEST" 'promotional-credit')"

TRIG=""; NOTRIG=""
if [ -n "$CFGFILES" ]; then
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    grep -q "$MARK_FILE" "$f" && continue
    # one "credit_grant": {...} object per hit; inspect the next 12 lines for its trigger
    while IFS=: read -r n _; do
      blk="$(sed -n "${n},$((n + 12))p" "$f" | sed '/}/q')"
      reviewed "$f" "$n" && continue
      t="$(printf '%s' "$blk" | grep -oE '"trigger"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed -E 's/.*:[[:space:]]*"([^"]*)"/\1/')"
      if [ -z "$t" ]; then NOTRIG+="$f:$n: credit_grant without \"trigger\""$'\n'
      elif [ "$t" != "signup" ]; then TRIG+="$f:$n: credit_grant trigger \"$t\""$'\n'; fi
    done < <(grep -nE '"credit_grant"[[:space:]]*:[[:space:]]*\{' "$f")
  done <<< "$CFGFILES"
fi
emit "R2 TRIGGER — a credit_grant that is not a sign-up grant" \
  "v0.1.10 does NOT grant it on org.created / user.org_assigned (v0.1.9 handed it out as a gift on every sign-up/join). If you wanted a sign-up gift on this tier, see MIGRATION.md §2; then mark '$MARK_LINE' on the credit_grant line." \
  "$(printf '%s' "$TRIG" | sed '/^$/d')"
emit "R3 NOTRIGGER — a credit_grant with no trigger" \
  "still granted on sign-up in Go (with a boot warning); the Python runtime grants nothing without it. Add \"trigger\": \"signup\"." \
  "$(printf '%s' "$NOTRIG" | sed '/^$/d')"

emit "R4 CLI — quotactl verbs whose exit code changed" \
  "subscribe-events / replay / backfill now exit NON-ZERO on failure; events / delete-user refuse (non-zero) instead of reading an empty stub. Remove '|| true' that hid failures; replace events/delete-user with a call into your service (q.LedgerStore)." \
  "$(scan "$SCRIPTFILES" 'quotactl[[:space:]]+(subscribe-events|replay|backfill|events|delete-user)')"

emit "R5 SECRET-ARGV — the webhook secret passed on the command line" \
  "replay/backfill read AB0T_AUTH_WEBHOOK_SECRET themselves; drop --secret (argv is visible in ps)." \
  "$(scan "$SCRIPTFILES" 'quotactl[[:space:]].*--secret')"

emit "R6 GRANTCREDIT — billing.Client.GrantCredit calls" \
  "v0.1.9 always failed (404, nothing granted). v0.1.10 grants for real into credit_balance via promotional-credit, keyed by IdempotencyKey or user:{u}:initial_credit:{tier}. OrgID is required. Confirm the call should move money." \
  "$(scan "$NONTEST" '\.GrantCredit\(ctx|\.Billing\.GrantCredit\(|[^A-Za-z]GrantCredit\(context')"

RU="$(scan "$NONTEST" '\.RecordUsage\(')"
if [ -n "$RU" ]; then
  echo -e "\n${CYAN}? INFO R7 — RecordUsage (unchanged): an EMPTY Cost/PlatformFee is sent as absent and billing prices the row with its fallback + minimum charge. Set both to \"0\" on metering rows.${NC}"
  printf '%s\n' "$RU" | sed 's/^/    /'
fi

ALLFILES="$(printf '%s\n%s\n%s\n' "$GOFILES" "$CFGFILES" "$SCRIPTFILES" | sed '/^$/d' | sort -u)"
ENVFILES="$(grep -rlE 'AB0T_QUOTA_CONFIG_PATH' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
emit "R8 CONFIGPATH — AB0T_QUOTA_CONFIG_PATH is not a variable the library reads" \
  "rename it to QUOTA_CONFIG_PATH (the library loaded ./quota-config.json or /etc/ab0t/quota-config.json instead, silently, before v0.1.10)." \
  "$(scan "$ENVFILES" 'AB0T_QUOTA_CONFIG_PATH')"

SP="$(scan "$NONTEST" 'StripPrefix\(.*WebhookHandler')"
if [ -n "$SP" ]; then
  echo -e "\n${CYAN}? INFO R9 — http.StripPrefix around WebhookHandler still works; v0.1.10 also answers the full-path mount${NC}"
  printf '%s\n' "$SP" | sed 's/^/    /'
fi
if [ -n "$GOFILES" ]; then
  echo -e "\n${CYAN}? INFO — after deploying, read the boot log: 'ab0t-quota capabilities' and any 'quota config:' warnings (Capabilities.ConfigWarnings).${NC}"
fi
[ -n "$ALLFILES" ] || echo -e "\n${GREEN}No file references ab0t-quota-go, quota-config.json or quotactl.${NC}"

echo
echo "======================================================"
if [ "$FOUND" -eq 0 ]; then
  echo -e "${GREEN}Nothing left to look at for v0.1.10.${NC} Then: go build ./... && go vet ./... && go test ./..."
  exit 0
fi
echo -e "${RED}${FOUND} site(s) to review for v0.1.10.${NC} Your code compiles unchanged — resolve each (fix or mark"
echo "'$MARK_LINE'), re-run this checker (expect 0), then: go build ./... && go vet ./... && go test ./..."
[ "$ADVISORY" -eq 1 ] && { echo "(--advisory: exiting 0)"; exit 0; }
exit 1
