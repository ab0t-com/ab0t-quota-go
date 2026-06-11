# ab0t-quota-go

Go port of [ab0t-quota](https://github.com/ab0t-com/ab0t-quota) (Python).
A drop-in client library for ab0t mesh consumers that gives any Go service
quota enforcement, tier management, billing/payment integration, and an
auth-event handler framework — by talking to ab0t backend services on your
behalf.

## What this is

ab0t-quota-go is the **Go SDK for the ab0t mesh**. Your Go service uses it
to:

1. **Enforce quotas** — concurrent resources, request rates, monthly spend
   caps. Tier-aware (free / starter / pro / enterprise — or whatever you
   call them). Sub-5 ms p99 in `byo_redis` mode.
2. **Read tier + balance** — without rolling your own clients for
   `billing.service.ab0t.com` and `payment.service.ab0t.com`.
3. **React to auth events** — `auth.user.registered`,
   `auth.user.login`, `org.created`, `auth.permission.granted`, etc.
   The library mounts the webhook receiver, verifies HMAC, dispatches
   to your handlers, and auto-subscribes with auth at startup.
4. **Grant credits idempotently** — built-in delivery dedup,
   business-key dedup, exponential-backoff retry, ledger persistence,
   and CLI replay. Three storage backends (DynamoDB, Redis, in-memory)
   pick themselves based on what's available.

It is positioned as a **drop-in** — the goal is "two env vars, one JSON
config, one library call." Your service should not need to learn about
ab0t-mesh internals to use this.

## Why someone might use it

You are a Go-based SaaS service and you want any of:

- A free-tier with $X of compute credit at signup, anti-farming included
- A pricing page wired to real plans + a checkout flow that actually
  takes money via Stripe
- Tier-based rate limits with human-readable 429 messages
- Per-user spend caps inside an org
- Idempotent event handlers for things like "send a welcome email on
  signup" or "provision an account in our system on org.created"

…and you do **not** want to write Stripe webhook code, manage your
own webhook subscription with another auth provider, design retry
machinery for "the third-party webhook fired twice," or roll your own
DynamoDB counter table.

ab0t-quota-go gives you all of that with one `quota.Setup(...)` call.

## How to use it (minimal example)

```go
package main

import (
    "context"
    "log"
    "net/http"
    "os"

    "github.com/ab0t-com/ab0t-quota-go/quota"
    "github.com/ab0t-com/ab0t-quota-go/authevents"
)

// orgFromRequest extracts the org_id from your auth middleware's context.
// (The lib doesn't know about your auth — supply your own.)
func orgFromRequest(r *http.Request) string  { return r.Header.Get("X-Org-ID") }
func userFromRequest(r *http.Request) string { return r.Header.Get("X-User-ID") }

func main() {
    ctx := context.Background()

    // 1. One setup call wires the engine, /api/quotas/* endpoints,
    //    rate-limit middleware, billing/payment proxy routes, lifespan
    //    workers, and the auth-event webhook receiver.
    qctx, err := quota.Setup(ctx, quota.Config{
        ConfigPath:    "quota-config.json",                 // typed JSON config
        MeshAPIKey:    os.Getenv("AB0T_MESH_API_KEY"),      // your mesh credential
        ConsumerOrg:   os.Getenv("AB0T_CONSUMER_ORG_ID"),   // your service's mesh org UUID
        WebhookSecret: os.Getenv("AB0T_AUTH_WEBHOOK_SECRET"), // optional; mounts the receiver
    })
    if err != nil {
        log.Fatal(err)
    }
    defer qctx.Close()

    // Setup logs a capability report — what was enabled, what was skipped and why.
    // Useful to read at startup: "why didn't credits grant?" answers in one line.
    log.Printf("quota capabilities: %+v", qctx.Capabilities())

    // 2. Register an auth-event handler (similar to the Python @on_auth_event)
    authevents.OnAuthEvent("auth.user.registered", authevents.HandlerFunc(
        func(ctx context.Context, event authevents.Event) error {
            log.Printf("new user signed up: %s", event.UserID())
            return nil
        },
    ))

    // 3. Mount on your own http.ServeMux
    mux := http.NewServeMux()
    qctx.Mount(mux, "/api")  // mounts /api/quotas/*, /api/billing/*, etc.

    // 4. Enforce quota in your own route
    mux.HandleFunc("/widgets", func(w http.ResponseWriter, r *http.Request) {
        if err := qctx.CheckBundle(r.Context(), orgFromRequest(r), "widget",
            quota.WithUserID(userFromRequest(r))); err != nil {
            quota.WriteDenial(w, err)  // writes the standard 429 body + headers
            return
        }
        // ...provision the widget...
        _ = qctx.IncrementBundle(r.Context(), orgFromRequest(r), "widget",
            quota.WithUserID(userFromRequest(r)))
    })

    http.ListenAndServe(":8080", mux)
}
```

> The `testdata/quota-config.example.json` referenced below is **planned** —
> use the Python repo's `quota-config.example.json` as the source-of-truth
> schema reference until the Go testdata is bundled in Phase 1a.

That's the whole API surface for 95% of consumers. Configuration lives
in a single typed JSON file (`quota-config.json`); see the bundled
[`testdata/quota-config.example.json`](testdata/quota-config.example.json)
for the schema.

## Idempotency for handlers that touch money

For handlers with **side effects on money or persistent state**, use
the `handlerledger.Idempotent` middleware:

```go
import "github.com/ab0t-com/ab0t-quota-go/handlerledger"

authevents.OnAuthEvent("auth.user.registered",
    handlerledger.Idempotent(handlerledger.IdempotentConfig{
        Handler: "grant_initial_credit",
        Key: func(e authevents.Event) string {
            return authevents.ComposeCreditDedupKey(
                "per_user_per_tier",
                e.UserID(), e.OrgID(), "free",
            )
        },
        // Retry: 3 attempts exponential backoff by default.
        // Pass Retry: handlerledger.NoRetry to disable.
    }, func(ctx context.Context, event authevents.Event, hctx *handlerledger.Context) error {
        if done, _ := hctx.AlreadyDone(ctx); done {
            return hctx.Skip("already granted")
        }
        if err := billing.GrantCredit(ctx, event.OrgID(), 10); err != nil {
            return err  // lib will retry per policy
        }
        return hctx.MarkDone(ctx, "txn_abc")
    }))
```

What you get for free: delivery dedup (auth retries don't double-fire),
business dedup (your `Key` function controls the policy),
exponential-backoff retry, full ledger persistence, and a CLI to query
+ replay everything.

## CLI

```bash
go install github.com/ab0t-com/ab0t-quota-go/cmd/quotactl@latest

# Register the auth-event subscription
quotactl subscribe-events --endpoint https://yours.com/api/quotas/_webhooks/auth

# Query the handler ledger
quotactl events --user-id u123
quotactl events --status failed --since 1h

# Re-fire a handler from the stored event snapshot
quotactl replay --handler grant_initial_credit --event-id evt_xxx

# Backfill users who pre-existed your handler
quotactl backfill --handler grant_initial_credit --user-ids u1,u2,u3 --org-id <eu-org>

# GDPR cascade
quotactl delete-user --user-id u123 --confirm
```

## Per-feature env-var matrix

"Two env vars" is true for quota-only mode. Full paid + events mode needs more. The lib's `Capabilities()` report logs exactly which features turned on or off and why; this is the matrix it's checking:

| Feature | Required env | Optional env | What turns off without it |
|---|---|---|---|
| Quota check + tier resolution | `AB0T_MESH_API_KEY`, config file | `AB0T_MESH_BILLING_API_KEY` | nothing — quota always works (engine + middleware) |
| `/api/billing/*` + `/api/payments/*` proxy routes | `AB0T_CONSUMER_ORG_ID` | `AB0T_MESH_PAYMENT_API_KEY` | proxy routes not mounted; `Capabilities().Paid == false` with reason |
| Auth-event receiver | `AB0T_AUTH_WEBHOOK_SECRET` | — | `/api/quotas/_webhooks/auth` not mounted; no handler dispatch |
| Auto-subscribe with auth | + `AB0T_AUTH_ADMIN_TOKEN` + `AB0T_AUTH_WEBHOOK_PUBLIC_URL` + `AB0T_AUTH_AUTH_URL` | `AB0T_AUTH_WATCH_ORG_SLUG` | receiver still works but operator must register the subscription manually |
| Stripe subscription credits | `AB0T_QUOTA_STRIPE_WEBHOOK_SECRET` | — | tier configs with `billing_model: subscription_with_credits` fail loudly at load (in v0.1.0); v0.2 ports the webhook proxy |
| Lifecycle SNS fan-out | `AB0T_MESH_SNS_LIFECYCLE_TOPIC_ARN` | — | cost recording still works; SNS publish is no-op |
| Persistent ledger (DDB) | `AB0T_QUOTA_DDB_TABLE` (or `app.state.ddb_client`) | — | falls back to Redis (72h TTL) or InMemory (loud warning) |

Full env-var inventory in `PRODUCT_SPEC.md` §12.

## How it compares to the Python lib

Same contracts, same JSON config schema, same auth-event registry
semantics, same CLI subcommands, same storage backends, same dedup
policies. Idiomatic Go where there's no direct Python analogue:

- Decorators → functional middleware (`handlerledger.Idempotent(cfg, fn)`)
- Module-level singleton registry → package-level mutex-guarded map
- `@idempotent` ctx-arg pattern → `handlerledger.Context` parameter
- Pydantic models → struct tags + custom UnmarshalJSON for `Decimal`
- `setup_quota(app)` → `quota.Setup(ctx, cfg)` returning a `QuotaContext`
- Async / await → `context.Context` propagation
- pip → Go modules at `github.com/ab0t-com/ab0t-quota-go`

The JSON config from a Python deployment loads unchanged into the Go
client; same with the LedgerStore schemas (Redis keys + DynamoDB items
share the same shape on the wire).

## Contributing — local setup

```bash
make hooks          # installs the .githooks/pre-commit (gitleaks scan)
make test           # go test ./...
make race           # go test -race -count=1 ./...
make scan           # full gitleaks scan, working tree
make scan-staged    # what the pre-commit hook runs
```

The pre-commit hook scans only staged changes with `gitleaks protect
--staged` — fast, only your diff. CI runs a full-history scan on
every push and PR (`.github/workflows/ci.yml`). Allowlist test
fixtures in `.gitleaks.toml`; per-finding fingerprints go in
`.gitleaksignore`. If gitleaks isn't installed locally the hook
prints a warning and lets the commit through — CI still gates.

## Status

Pre-1.0. API may evolve. Pin to a SHA in production until v1.0 ships.
Test coverage targets ≥ 80% on each package. CI runs against Go 1.22+.

## Where to learn more

- [`PRODUCT_SPEC.md`](PRODUCT_SPEC.md) — full file tree, every function signature, intent per file
- [`back_references.md`](back_references.md) — Python lib, mesh services, endpoints, OpenAPI specs the engineer should keep open while building
- Python source: `https://github.com/ab0t-com/ab0t-quota` — the contracts this port mirrors
