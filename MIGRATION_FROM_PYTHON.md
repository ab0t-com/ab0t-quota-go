# Migration from Python ab0t-quota v0.5.2

This is a side-by-side. Each row is a callsite a Python consumer has;
the right column is the Go equivalent.

## Setup

| Python | Go |
|--------|----|
| `from ab0t_quota import setup_quota` | `import "github.com/ab0t-com/ab0t-quota-go/quota"` |
| `q = setup_quota(app, config_path="...")` | `q, err := quota.Setup(ctx, quota.Options{ConfigPath: "..."})` |
| (lifespan auto-wired by FastAPI) | `defer q.Close(context.Background())` |
| `app.middleware('http')(q.middleware)` | `mux.Handle("/api/", q.Middleware(deps)(yourHandler))` |
| `app.include_router(q.router, prefix="/api/quotas")` | `mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())` |

## Quota check from app code

```python
# Python
from ab0t_quota import check_quota
result = await check_quota(user_id="alice", resource_key="sandbox.concurrent")
if not result.allowed:
    raise HTTPException(status_code=429, detail=result.detail)
```

```go
// Go
res, err := q.Check(ctx, engine.CheckInput{
    UserID:      "alice",
    ResourceKey: "sandbox.concurrent",
})
if err != nil {
    return err
}
if !res.Allowed() {
    middleware.WriteDenial(w, res)
    return
}
```

## Spend / Release

```python
await q.spend(user_id="u", resource_key="r", cost=1)
await q.release(user_id="u", resource_key="r", cost=1)
```

```go
_, err := q.Spend(ctx, engine.CheckInput{UserID: "u", ResourceKey: "r", Cost: 1})
err = q.Release(ctx, engine.CheckInput{UserID: "u", ResourceKey: "r", Cost: 1})
```

## Auth-event handler registration

```python
from ab0t_quota import on_auth_event

@on_auth_event("payment.succeeded")
async def handle_payment(event):
    ...
```

```go
import "github.com/ab0t-com/ab0t-quota-go/authevents"

authevents.OnAuthEvent("payment.succeeded", authevents.HandlerFunc(
    func(ctx context.Context, ev authevents.Event) error {
        // your work
        return nil
    },
))
```

## @idempotent (new in v0.5.2)

```python
from ab0t_quota import idempotent, HandlerContext

@on_auth_event("payment.succeeded")
@idempotent(
    name="credit_topup",
    dedup_key=lambda e: f"topup:{e.user_id}:{e.event_id}",
    retry=RetryConfig(attempts=3, backoff="exponential"),
)
async def handle_topup(event, ctx: HandlerContext):
    ...
    return ctx.success(side_effect_id=charge_id)
```

```go
import (
    "github.com/ab0t-com/ab0t-quota-go/authevents"
    "github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

h := authevents.Idempotent(handlerledger.IdempotentConfig{
    Handler: "credit_topup",
    Key: func(e handlerledger.Event) string {
        return "topup:" + e.GetUserID() + ":" + e.GetEventID()
    },
    Retry: handlerledger.DefaultRetry(),
}, func(ctx context.Context, ev handlerledger.Event, hctx *handlerledger.Context) error {
    // your work
    return hctx.Success(chargeID)
})
authevents.OnAuthEvent("payment.succeeded", h)
```

Differences worth knowing:

- Python attaches metadata to the function via `setattr`; Go can't, so
  `Idempotent(...)` returns `*authevents.IdempotentHandler` (a concrete
  type the receiver type-switches on).
- Python's `HandlerContext` is positional kwarg; Go's is the third
  argument `*handlerledger.Context`.
- Both use `ctx.success(...)` / `ctx.skip(...)` to short-circuit the
  retry loop with an explicit terminal status.

## CreditGranter — the consumer integration point

In Python, the default credit-grant handler calls
`billing_client.grant_credit(...)` directly (with the known BUG #1
NameError around `tier_provider`).

In Go, you supply a `CreditGranter` interface explicitly:

```go
type myGranter struct{ billing *billing.Client }

func (g myGranter) GrantCredit(ctx context.Context, in authevents.CreditGrantRequest) error {
    _, err := g.billing.GrantCredit(ctx, billing.CreditGrantRequest{
        UserID:  in.UserID,
        OrgID:   in.OrgID,
        TierID:  in.TierID,
        Amount:  in.Amount,
        EventID: in.EventID,
    })
    return err
}

q, err := quota.Setup(ctx, quota.Options{
    ConfigPath:    "quota-config.json",
    CreditGranter: myGranter{billing: yourBillingClient},
})
```

If you don't supply a `CreditGranter`, the default handler is NOT
registered — `Capabilities.CreditGrant=false` and `WhyOff` explains why.
No silent NameError at first event.

## CLI

```bash
# Python
python -m ab0t_quota events --user alice
python -m ab0t_quota replay events.jsonl --target https://svc/webhook --secret $SECRET
python -m ab0t_quota backfill users.csv --target https://svc/webhook --secret $SECRET

# Go
quotactl events --user alice
quotactl replay --file events.jsonl --target https://svc/webhook --secret "$SECRET"
quotactl backfill --input users.csv --target https://svc/webhook --secret "$SECRET"
```

Both CLIs ship the same `--bare` flag for the bare-hex signature variant
some legacy publishers use.

## Config file

`quota-config.json` is the same file. Both libs read the same schema,
the same env-var interpolation (`${QUOTA_*}`), and the same forward-
compat Extra map for unknown top-level keys. A Python service and a Go
service on the same `quota-config.json` will behave identically.

One difference: the Go port honors `enforcement.shadow_mode` (Python
ignores it — Known Upstream Bug #4). If you have shadow_mode in your
config and you want it actually shadow-modeing, the Go port is the
right runtime.

## What's not in the Go port (yet)

- Redis FloatStore / RateStore (in-memory only; Capabilities reports it)
- Persistent ledger backends (Redis + DDB) — in-memory only
- Persistence snapshot worker — out of scope for v0.1.0

All four wire in v0.2 — typed `ErrRedisNotAvailable` errors mark the
seams.
