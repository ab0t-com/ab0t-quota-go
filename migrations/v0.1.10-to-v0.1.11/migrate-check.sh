#!/usr/bin/env bash
# migrate-check.sh — ab0t-quota-go v0.1.10 -> v0.1.11: find what to LOOK AT after the upgrade.
#
# Read-only. It NEVER edits your code, writes no files and makes no network calls.
#
# WHAT CHANGED (bug fixes — your code compiles unchanged; no exported symbol changed)
#   - The billing/payment HTTP client follows a redirect only on the same host, keeping the same
#     ORIGIN (scheme + host + port) or upgrading http -> https. An https -> http redirect on the
#     same host used to be followed and sent your X-API-Key in cleartext; it is now refused.
#   - The capability report no longer says "receiver will reject all events" when
#     AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS still accepts events.
#   - A blank, "." or ".." org / subscription / session id is refused before any request.
#   - config.Decimal reads a JSON NUMBER exactly (v0.1.10 went through float64).
#
# WHAT IT REPORTS (per finding: file:line, the rule, what to check)
#   R1 REDIRECT     AB0T_QUOTA_BILLING_URL / AB0T_QUOTA_PAYMENT_URL set to an http:// address: the key
#                   travels in cleartext on every first request — set the https:// URL (an http -> https
#                   upgrade is still followed; a redirect to another host or port is refused).
#   R2 MONEYNUMBER  a quota-config money value (amount_per_period / initial_credit) written as a long
#                   JSON number: v0.1.11 reads it exactly as written; v0.1.10 rounded it through float64.
#   R3 WHYOFFTEXT   something matching the capability text "reject all events" (an alert rule, a
#                   test, a log query): with only the previous secret set, the text is now different.
#   (INFO) https:// billing/payment URLs — a redirect to http:// is now refused (check with curl -sI).
#
# MARKING A SITE REVIEWED: put   quotago:v0.1.11-reviewed   on the line or the line above it,
# or   quotago:v0.1.11-reviewed-file   anywhere in the file. JSON has no comments: put the marker in a
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
MARK_LINE='quotago:v0.1.11-reviewed'
MARK_FILE='quotago:v0.1.11-reviewed-file'
FOUND=0

echo -e "${BOLD}ab0t-quota-go v0.1.10 -> v0.1.11 migration check (read-only; no source change required)${NC}"
echo "target: $TARGET"

EXCL=(--exclude-dir=vendor --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=.archive
      --exclude-dir=migrations)

URLFILES="$(grep -rlE 'AB0T_QUOTA_(BILLING|PAYMENT)_URL' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
CFGFILES="$(grep -rlE '"tiers"[[:space:]]*:' --include='quota-config*.json' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"
# (prose — *.md — is not an alert rule or a test; it is not scanned for R3)
TEXTFILES="$(grep -rlF 'reject all events' --exclude='*.md' "${EXCL[@]}" "$TARGET" 2>/dev/null || true)"

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

URLRE='AB0T_QUOTA_(BILLING|PAYMENT)_URL"?[[:space:]]*[:=,][[:space:]]*"?'
emit "R1 REDIRECT — a billing/payment base URL on http://" \
  "your mesh key travels in cleartext on every first request to an http:// address. v0.1.11 still follows an http -> https upgrade on the same host, but refuses a redirect to another host or port. Set the https:// URL here (check: curl -sI <url>/health); once it is https, or you have reviewed it, mark '$MARK_LINE'." \
  "$(scan "$URLFILES" "${URLRE}http://")"

MONEY=""
if [ -n "$CFGFILES" ]; then
  MONEY="$(scan "$CFGFILES" '"(amount_per_period|initial_credit)"[[:space:]]*:[[:space:]]*-?[0-9][0-9.eE+-]{15,}')"
fi
emit "R2 MONEYNUMBER — a money value written as a long JSON number" \
  "v0.1.11 reads it exactly as written; v0.1.10 rounded it through float64 (e.g. 9007199254740993 -> 9007199254740992, 0.123456789012345678901 -> 0.12345678901234568). Confirm the written value is the one you mean; prefer a JSON string (\"5.00\") for money." \
  "$MONEY"

emit "R3 WHYOFFTEXT — matching the capability text 'reject all events'" \
  "with AB0T_AUTH_WEBHOOK_SECRET unset but AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS set, WhyOff[\"auth_events_signed\"] now reads 'receiver accepts only events signed with the previous secret(s) in AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS ...' (it was false before). With neither set the text is unchanged. Match on the WhyOff key 'auth_events_signed', not its text." \
  "$(scan "$TEXTFILES" 'reject all events')"

HTTPS="$(scan "$URLFILES" "${URLRE}https://")"
if [ -n "$HTTPS" ]; then
  echo -e "\n${CYAN}? INFO — https billing/payment URLs: a redirect from them to http:// is now refused (it sent your key in cleartext). Nothing to do unless calls start failing with 'another origin'.${NC}"
  printf '%s\n' "$HTTPS" | sed 's/^/    /'
fi
if [ -n "$URLFILES$CFGFILES" ]; then
  echo -e "\n${CYAN}? INFO — a blank, \".\" or \"..\" org / subscription / session id is now refused before any request (it addressed another route). RecordUsage still returns nil (logged).${NC}"
fi
[ -n "$URLFILES$CFGFILES$TEXTFILES" ] || echo -e "\n${GREEN}No file sets AB0T_QUOTA_BILLING_URL / AB0T_QUOTA_PAYMENT_URL, holds a quota-config.json or matches the capability text.${NC}"

echo
echo "======================================================"
if [ "$FOUND" -eq 0 ]; then
  echo -e "${GREEN}Nothing left to look at for v0.1.11.${NC} Then: go build ./... && go vet ./... && go test ./..."
  exit 0
fi
echo -e "${RED}${FOUND} site(s) to review for v0.1.11.${NC} Your code compiles unchanged — resolve each (fix or mark"
echo "'$MARK_LINE'), re-run this checker (expect 0), then: go build ./... && go vet ./... && go test ./..."
[ "$ADVISORY" -eq 1 ] && { echo "(--advisory: exiting 0)"; exit 0; }
exit 1
