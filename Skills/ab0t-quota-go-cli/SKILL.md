---
name: ab0t-quota-go-cli
description: Operate a deployed ab0t-quota-go service via the `quotactl` admin CLI. Use when emitting conforming infrastructure artifacts (`provision --emit compose|terraform|acl|iam`), starting a verified local dev Redis (`provision --local`), grading production posture before go-live (`doctor`), registering a webhook subscription with auth, replaying missed events, backfilling credit grants for legacy users, querying the handler ledger, deleting a user's records for GDPR, printing the live Capabilities snapshot, installing the binary via `go install` or GitHub releases, debugging "no events firing" / "credits not granted" / "handler running twice", or scripting any admin task that the library exposes.
---

# quotactl — ab0t-quota-go Admin CLI

Eight subcommands. Each takes `--help` for full flags.

## Install

```bash
# Option 1 — from source (needs Go toolchain)
go install github.com/ab0t-com/ab0t-quota-go/cmd/quotactl@v0.2.0

# Option 2 — download prebuilt
VERSION=v0.1.10
OS=$(uname | tr A-Z a-z)
ARCH=amd64  # or arm64
curl -L -o quotactl \
  https://github.com/ab0t-com/ab0t-quota-go/releases/download/${VERSION}/quotactl-${OS}-${ARCH}
chmod +x quotactl && sudo mv quotactl /usr/local/bin/
quotactl --version
```

Verify the SHA256SUMS from the same release if you care about supply chain.

## Subcommand reference

### subscribe-events — register webhook with auth (idempotent)

```bash
quotactl subscribe-events \
  --events org.created,user.org_assigned \
  --prefix /api \
  --name my-service-credit-grant
```

Required env: `AB0T_AUTH_AUTH_URL`, `AB0T_AUTH_ADMIN_TOKEN`,
`AB0T_AUTH_WEBHOOK_PUBLIC_URL`, `AB0T_AUTH_WEBHOOK_SECRET`.

GETs existing subscriptions first (every page); only POSTs create on
no-match. Safe to run repeatedly. Use `--org-slug <slug>` to filter to a
single org by login slug, or `--org-id <id>` if you already know it.

v0.1.10: any failure exits non-zero with the HTTP status (v0.1.9 exited 0
with an empty `subscription_id:` — read the id there). An existing
subscription with other events, or (where auth discloses it) another
secret, is an error naming `--rotate-secret`, which deletes and re-creates
it with the current secret and `--events` — the only way to rotate a
secret, since auth cannot edit one.

### events — query the ledger

```bash
quotactl events --user u-alice                    # all attempts for this user
quotactl events --status failed_permanent          # all permanent failures
quotactl events --status success --limit 200
```

Must pass at least one of `--user` or `--status`. Output is one JSON row
per line — pipe to `jq` for filtering.

**Note:** the standalone CLI cannot reach your service's ledger. v0.1.9
printed an empty result (exit 0); v0.1.10 refuses with a non-zero exit.
Query `q.LedgerStore.QueryByUser` / `QueryByStatus` inside your service
(e.g. an admin route). `delete-user` behaves the same way — use
`q.LedgerStore.DeleteUser` for a deletion request.

### replay — re-send saved events to a receiver

```bash
quotactl replay \
  --file events.jsonl \
  --target https://your-svc.example.com/api/quotas/_webhooks/auth
# the HMAC secret is read from AB0T_AUTH_WEBHOOK_SECRET (avoid --secret: argv is visible in ps)
```

Accepts JSONL (one event per line) or a JSON array. Signs each with HMAC
and POSTs. The receiver's delivery dedup makes replay safe — already-
processed events are no-ops at the ledger layer.

Use `--bare` if the receiver expects bare-hex (no `sha256=` prefix).

### backfill — synthesize signup events for legacy users

```bash
quotactl backfill \
  --input users.csv \
  --target https://svc.example.com/api/quotas/_webhooks/auth   # secret from AB0T_AUTH_WEBHOOK_SECRET
```

v0.1.10: `replay` and `backfill` exit non-zero if any event is not
accepted (v0.1.9 exited 0 and only printed the status).

`users.csv` format (one per line, `#` comments OK):

```
u-alice,o-acme
u-bob,o-acme
u-charlie
```

The receiver's **business dedup** ensures already-granted users are
no-ops. Use `--dry-run` to preview without sending.

### delete-user — GDPR forget

```bash
quotactl delete-user --user-id u-alice   # v0.1.10+: refuses (non-zero exit) — see below
```

The standalone CLI cannot reach your service's ledger (v0.1.9 printed
"deleted 0 rows" and exited 0 while the real ledger kept the data). Run
`q.LedgerStore.DeleteUser(ctx, userID)` inside the service — it removes all
handler-ledger rows for the user_id. The counters (per-org metering) are
untouched — they don't carry user_id by default.

### capabilities — print the Setup snapshot

```bash
quotactl capabilities --config quota-config.json
```

Loads the config the same way `quota.Setup` does, runs the wiring, and
prints the resulting Capabilities JSON. Use this as a deploy smoke test:

```bash
quotactl capabilities | jq '.Billing, .CreditGrant, .AlertsWebhook'
```

If any expected-on capability is `false`, check `.WhyOff` for the reason
string.

### provision — conforming infrastructure, emitted not transcribed

```bash
quotactl provision --emit compose      # dev-stack fragment
quotactl provision --emit terraform    # the DDB tables + IAM policy
quotactl provision --emit acl          # least-privilege Redis ACL
quotactl provision --emit iam --include-create   # only with auto-create opted in
quotactl provision --local [--port N] [--name NAME] [--dry-run]
```

Artifacts are generated from the same registry the boot gates enforce, so
artifact and gate cannot drift. `provision` NEVER creates cloud resources —
emit-and-let-them-apply. `--local` starts one local Docker Redis and verifies
it with the boot evaluator (its only side effect). Same verb names, exit
taxonomy and JSON schema as the Python CLI (conformance `ST-CLI-1`); the
verb is `provision`, not `setup` — `quota.Setup` is the library's own entry
point.

### doctor — production-posture grading (humans + auditors)

```bash
quotactl doctor --config quota-config.json [--json] [--fail-on-risk] [--timeout SEC]
```

Grades what "bootable" lets through: persistence behind a durability
assertion, PITR asserted-not-observed, already-evicted keys, encryption in
transit. Reports what it could not check as `not_checked` with the reason —
in the Go runtime that includes ACL/IAM breadth (compare the server's ACL
out-of-band with `quotactl provision --emit acl`). Exit mirrors the shared taxonomy (0 ok / 1 gate / 2 config /
3 unreachable / 4 internal); `--fail-on-risk` turns RISK findings into
exit 1; `--json` extends the preflight report schema with a posture section.
**Honest side-effect statement (also in the command's own output): Go's
`doctor` runs full `quota.Setup` — it may create the library's declared
tables and loads the counter script. It never claims read-only** (the Python
doctor's only server-visible write is the disclosed `SCRIPT LOAD`).

## Common workflows

### Initial onboarding

```bash
# 1. Confirm config + env produce the expected wiring
quotactl capabilities --config quota-config.json

# 2. Before go-live: grade production posture (NOT read-only — see doctor above)
quotactl doctor --config quota-config.json --fail-on-risk

# 3. Register the webhook subscription
quotactl subscribe-events
```

### Rotating the webhook secret

Auth cannot edit a subscription's secret. Deploy the service with the NEW
secret in `AB0T_AUTH_WEBHOOK_SECRET` and the old one in
`AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS` (the receiver accepts both), then:

```bash
quotactl subscribe-events --rotate-secret   # deletes + re-creates; admin token needs events.delete too
```

Events between the delete and the create are not delivered — run `backfill`
for sign-ups in that window. Remove `AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS` once
deliveries verify. (v0.1.9: delete the subscription in auth, then run
`subscribe-events` again.)

### Disaster recovery — missed events

```bash
# 1. Pull missed events from auth's outbox (out of scope; uses your tooling)
auth events list --since 2026-06-10 > events.jsonl

# 2. Replay through your receiver
quotactl replay --file events.jsonl \
  --target https://svc/api/quotas/_webhooks/auth   # secret from AB0T_AUTH_WEBHOOK_SECRET
```

Replay re-posts the SAME event ids: an event whose ledger row is already
`failed_permanent` (or `success`) is a no-op. To re-try a failed grant,
use `backfill` (new event ids; business dedup keeps granted users no-ops).

### Legacy user backfill

```bash
# 1. Dump pre-existing users → CSV (your DB)
psql -tc "select id, org_id from users where created_at < '2026-01-01'" \
  | tr -d ' ' > users.csv

# 2. Dry-run to confirm shape (secret from AB0T_AUTH_WEBHOOK_SECRET)
quotactl backfill --input users.csv --dry-run \
  --target https://svc.example.com/api/quotas/_webhooks/auth

# 3. Real run — receiver dedups so this is idempotent
quotactl backfill --input users.csv \
  --target https://svc.example.com/api/quotas/_webhooks/auth
```

### GDPR delete

`quotactl delete-user` cannot reach your service's ledger and refuses
(non-zero exit) — run the deletion inside the service:

```go
// e.g. from an admin route; keep the result as your deletion record
n, err := q.LedgerStore.DeleteUser(ctx, userID)
```

Then also flush the user from your billing/auth systems separately.

## Signature format note

The receiver accepts both:

- `X-Event-Signature: sha256=<hex>` (canonical; what `replay` / `backfill` send by default)
- `X-Event-Signature: <hex>` (bare; send it with `--bare`)

Use `--bare` only if your target rejects the prefixed form.

## Build it yourself

```bash
# Clone, build, install
git clone https://github.com/ab0t-com/ab0t-quota-go
cd ab0t-quota-go
make install   # → $GOBIN/quotactl

# Cross-compile all platforms
make dist VERSION=v0.1.10
ls release/v0.1.10/   # quotactl-<os>-<arch>[.exe] + SHA256SUMS
```

## Exit codes

- `0` — success
- `2` — any error a subcommand returns (usage error, or a runtime failure such
  as a failed `subscribe-events` call, a `replay` / `backfill` event that was
  not accepted, or the ledger verbs' refusal); stderr has `error: …`
- `doctor`: shared taxonomy 0 ok / 1 gate / 2 config / 3 unreachable / 4 internal
- `provision`: `--local` exits 3 if docker fails; 4 = internal error emitting an artifact
