---
name: ab0t-quota-go-cli
description: Operate a deployed ab0t-quota-go service via the `quotactl` admin CLI. Use when emitting conforming infrastructure artifacts (`provision --emit compose|terraform|acl|iam`), starting a verified local dev Redis (`provision --local`), grading production posture before go-live (`doctor`), registering a webhook subscription with auth, replaying missed events, backfilling credit grants for legacy users, querying the handler ledger, deleting a user's records for GDPR, printing the live Capabilities snapshot, installing the binary via `go install` or GitHub releases, debugging "no events firing" / "credits not granted" / "handler running twice", or scripting any admin task that the library exposes.
---

# quotactl — ab0t-quota-go Admin CLI

Eight subcommands. Each takes `--help` for full flags.

## Install

```bash
# Option 1 — from source (needs Go toolchain)
go install github.com/ab0t-com/ab0t-quota-go/cmd/quotactl@v0.1.0

# Option 2 — download prebuilt
VERSION=v0.1.0
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

GETs existing subscriptions first; only POSTs create on no-match. Safe
to run repeatedly. Use during deploy or once by hand. Use
`--org-slug <slug>` to filter to a single org by login slug, or
`--org-id <id>` if you already know it.

### events — query the ledger

```bash
quotactl events --user u-alice                    # all attempts for this user
quotactl events --status failed_permanent          # all permanent failures
quotactl events --status success --limit 200
```

Must pass at least one of `--user` or `--status`. Output is one JSON row
per line — pipe to `jq` for filtering.

**Note (v0.1.0):** ledger is in-memory only. Run this from the service's
process address space (e.g. via an admin RPC) — running the CLI alone
sees an empty store. v0.2 wires Redis + DDB backends and CLI reads them
directly.

### replay — re-send saved events to a receiver

```bash
quotactl replay \
  --file events.jsonl \
  --target https://your-svc.example.com/api/quotas/_webhooks/auth \
  --secret "$AB0T_AUTH_WEBHOOK_SECRET"
```

Accepts JSONL (one event per line) or a JSON array. Signs each with HMAC
and POSTs. The receiver's delivery dedup makes replay safe — already-
processed events are no-ops at the ledger layer.

Use `--bare` if the receiver expects bare-hex (no `sha256=` prefix).

### backfill — synthesize signup events for legacy users

```bash
quotactl backfill \
  --input users.csv \
  --target https://svc.example.com/api/quotas/_webhooks/auth \
  --secret "$AB0T_AUTH_WEBHOOK_SECRET"
```

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
quotactl delete-user --user-id u-alice
```

Removes all handler-ledger rows for the user_id. The counters
(per-org metering) are untouched — they don't carry user_id by default.

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
assertion, PITR asserted-not-observed, already-evicted keys, ACL breadth,
encryption. Reports what it could not check as `not_checked` with the
reason. Exit mirrors the shared taxonomy (0 ok / 1 gate / 2 config /
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

# 2. Register the webhook subscription
quotactl subscribe-events
```

### Disaster recovery — missed events

```bash
# 1. Pull missed events from auth's outbox (out of scope; uses your tooling)
auth events list --since 2026-06-10 > events.jsonl

# 2. Replay through your receiver
quotactl replay --file events.jsonl \
  --target https://svc/api/quotas/_webhooks/auth \
  --secret "$AB0T_AUTH_WEBHOOK_SECRET"
```

### Legacy user backfill

```bash
# 1. Dump pre-existing users → CSV (your DB)
psql -tc "select id, org_id from users where created_at < '2026-01-01'" \
  | tr -d ' ' > users.csv

# 2. Dry-run to confirm shape
quotactl backfill --input users.csv --dry-run \
  --target ... --secret ...

# 3. Real run — receiver dedups so this is idempotent
quotactl backfill --input users.csv \
  --target ... --secret ...
```

### GDPR delete

```bash
quotactl delete-user --user-id u-alice
```

Then also flush the user from your billing/auth systems separately.

## Signature format note

The receiver accepts both:

- `X-Event-Signature: sha256=<hex>` (canonical, default)
- `X-Event-Signature: <hex>` (bare; CLI default for replay/backfill)

`--bare` toggles the CLI form. Python ab0t-quota's CLI uses bare; keep
parity if you run a mixed Python/Go fleet.

## Build it yourself

```bash
# Clone, build, install
git clone https://github.com/ab0t-com/ab0t-quota-go
cd ab0t-quota-go
make install   # → $GOBIN/quotactl

# Cross-compile all platforms
make dist VERSION=v0.1.0
ls release/v0.1.0/
```

## Exit codes

- `0` — success
- `2` — usage error (missing flag, bad arg)
- non-zero (other) — runtime error; stderr has the detail
