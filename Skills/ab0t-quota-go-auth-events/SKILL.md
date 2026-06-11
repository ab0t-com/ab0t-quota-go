---
name: ab0t-quota-go-auth-events
description: Receive and handle ab0t auth-service webhook events in Go via the ab0t-quota-go library. Use when mounting the webhook receiver, registering handlers with `authevents.OnAuthEvent`, wrapping handlers with `authevents.Idempotent` for delivery dedup + retry + ledger persistence, implementing a CreditGranter, composing business dedup keys, auto-subscribing on startup, debugging 401/HMAC failures, or handling org.created / user.org_assigned / payment.succeeded events.
---

# ab0t-quota-go Auth Events

The library ships a webhook receiver + handler registry + idempotency
machinery. Mount the receiver, register handlers, done.

## Mount the receiver

```go
import "github.com/ab0t-com/ab0t-quota-go/authevents"

mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())
// path becomes /api/quotas/_webhooks/auth
```

The receiver does HMAC verification (over the **raw body** — never
re-canonicalize JSON), envelope parsing (v1 + v2 aliases), and
dispatches to registered handlers.

## Register a plain handler

```go
authevents.OnAuthEvent("org.created", authevents.HandlerFunc(
    func(ctx context.Context, ev authevents.Event) error {
        log.Println("org created:", ev.GetOrgID())
        return nil
    },
))
```

Errors are logged but always return 200 — auth retries on non-200 and
that would compound. Use `Idempotent` (below) when you need retries.

## Register an idempotent handler

When the handler does work that must not happen twice (grant credits,
send mail, charge cards):

```go
import "github.com/ab0t-com/ab0t-quota-go/handlerledger"

h := authevents.Idempotent(handlerledger.IdempotentConfig{
    Handler: "payment_credit_topup",  // stable name; appears in ledger
    Key: func(e handlerledger.Event) string {
        return "topup:" + e.GetUserID() + ":" + e.GetEventID()
    },
    Retry: handlerledger.DefaultRetry(),  // 3 attempts, exponential, 1s→30s
}, func(ctx context.Context, ev handlerledger.Event, hctx *handlerledger.Context) error {
    // Business dedup check happens AFTER hctx.DedupKey is set by Key fn.
    already, _ := hctx.AlreadyDone(ctx)
    if already { return hctx.Skip("already processed business-key") }

    if err := doRealWork(ctx, ev); err != nil {
        return err  // retried per Retry policy
    }

    _ = hctx.MarkDone(ctx, "side-effect-id")
    return hctx.Success("side-effect-id")
})
authevents.OnAuthEvent("payment.succeeded", h)
```

What `Idempotent` adds on top of a plain handler:
- **Delivery dedup** — second arrival of the same `event_id` returns the cached outcome
- **Business dedup** — your `Key` function gates real side-effects
- **Retry with backoff** — transient errors retry up to N attempts
- **Ledger persistence** — every attempt + outcome is recorded

## The default credit-grant handler

For the common case (give new users their tier's credit grant), use the
built-in:

```go
import "github.com/ab0t-com/ab0t-quota-go/authevents"

type myGranter struct{ billing *billing.Client }

func (g myGranter) GrantCredit(ctx context.Context, in authevents.CreditGrantRequest) error {
    _, err := g.billing.GrantCredit(ctx, billing.CreditGrantRequest{
        UserID: in.UserID, OrgID: in.OrgID, TierID: in.TierID,
        Amount: in.Amount, EventID: in.EventID,
    })
    return err
}

q, _ := quota.Setup(ctx, quota.Options{
    CreditGranter: myGranter{billing: yourBillingClient},
})
```

Setup registers the default handler on `org.created` AND
`user.org_assigned`. It resolves the user's tier (via your
TierProvider), composes the business dedup key (per the tier's `dedup`
policy), and calls your `GrantCredit` exactly once per (user, tier)
pair.

## Auto-subscribe on startup

Tell ab0t-auth to start sending events to your URL:

```go
q, _ := quota.Setup(ctx, quota.Options{
    AutoSubscribeAuthEvents: true,
    CreditGranter: myGranter{...},
})
```

Required env:
- `AB0T_AUTH_AUTH_URL` — auth service URL
- `AB0T_AUTH_ADMIN_TOKEN` — admin token (POSTs subscription)
- `AB0T_AUTH_WEBHOOK_PUBLIC_URL` — your public URL
- `AB0T_AUTH_WEBHOOK_SECRET` — shared HMAC secret

Idempotent: `Setup` GETs existing subscriptions first; only POSTs a
create on no-match.

## Event shape

`authevents.Event` is the parsed payload. Helpers:

| Method | Source priority |
|--------|-----------------|
| `GetEventID()` | `event_id` → `id` → SHA256(body)[:32] |
| `GetEventType()` | `event_type` → `type` |
| `GetUserID()` | `data.user_id` → top-level `user_id` |
| `GetOrgID()` | `data.org_id` → `data.organization_id` → top-level `tenant_id` |
| `Raw()` | exact bytes received (for replay / re-verification) |

## Business dedup key shapes

`authevents.ComposeCreditDedupKey(policy, userID, orgID, tierID)` returns:

| Policy | Key shape |
|--------|-----------|
| `per_user_per_tier` (default) | `credit_granted:user:{user_id}:{tier_id}` |
| `per_org_per_tier` | `credit_granted:org:{org_id}:{tier_id}` |
| `per_user_global` | `credit_granted:user:{user_id}` |
| `per_org_global` | `credit_granted:org:{org_id}` |

Match Python lib v0.5.2 verbatim — safe to share Redis with a Python
service.

## Wire contract

| Status | Body | Meaning |
|--------|------|---------|
| 200 | `{"status":"ok","ran":N,"event_type":"..."}` | handlers ran |
| 200 | `{"status":"ignored","event_type":"..."}` | no handlers for this type |
| 400 | `{"detail":"invalid json"}` | body not JSON |
| 401 | `{"detail":"invalid signature"}` | HMAC mismatch |

Auth sees 200 even on internal handler failure (so it doesn't retry-storm).
Handler retries happen inside the ledger pipeline.

## Headers accepted

| Header | Purpose |
|--------|---------|
| `X-Event-Signature` | canonical signature (preferred) |
| `X-Webhook-Signature` | legacy publisher fallback |

Both accept `sha256=<hex>` and bare `<hex>` formats — for replay/backfill
CLI compatibility.

## Common errors

| Symptom | Cause |
|---------|-------|
| every event 401s | `AB0T_AUTH_WEBHOOK_SECRET` mismatch or unset |
| events return 200 + `"status":"ignored"` | no handler registered for that event_type |
| same event runs twice | handler is plain `HandlerFunc` — use `Idempotent` |
| credit granted twice with idempotent handler | `Key` returns different strings — make it deterministic |
| TierProvider error at startup | default handler requires explicit TierProvider (prevents Python BUG #1) |
