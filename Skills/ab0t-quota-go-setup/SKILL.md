---
name: ab0t-quota-go-setup
description: Install and wire up the ab0t-quota-go library in a Go service. Use when adding `github.com/ab0t-com/ab0t-quota-go` as a dependency, calling `quota.Setup`, picking config search paths, interpreting the Capabilities snapshot, choosing env vars (AB0T_QUOTA_BILLING_URL, AB0T_QUOTA_PAYMENT_URL, AB0T_AUTH_WEBHOOK_SECRET), wiring Close() into graceful shutdown, or debugging "billing off" / "credit_grant off" in the startup log line.
---

# ab0t-quota-go Setup

The library is wired through one call: `quota.Setup`. Everything else is a
field on the returned `*quota.Quota`.

## Install

```bash
go get github.com/ab0t-com/ab0t-quota-go@latest
```

Public Go module — no `GOPRIVATE` needed. The git tag IS the release;
`@v0.1.0` and `@latest` both work.

## Minimum viable Setup

```go
import (
    "context"
    "github.com/ab0t-com/ab0t-quota-go/quota"
)

ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
defer stop()

q, err := quota.Setup(ctx, quota.Options{
    ConfigPath: "quota-config.json",
})
if err != nil { log.Fatal(err) }
defer q.Close(context.Background())
```

`Setup` is idempotent within a process — call it once at startup.

## Options that matter

| Field | When to set |
|-------|-------------|
| `ConfigPath` | non-default config location; empty uses search paths |
| `ConfigOverride` | tests — skip disk entirely with a parsed `*config.Config` |
| `CreditGranter` | hooking the default credit-grant handler to your billing service |
| `AutoSubscribeAuthEvents` | auto-register webhook with auth on startup |
| `Logger` | inject your `*slog.Logger`; otherwise the package default |

## Config search paths

When `ConfigPath` is empty, `LoadConfig` looks for:
1. `$AB0T_QUOTA_CONFIG_PATH` if set
2. `./quota-config.json`
3. `./config/quota-config.json`
4. `/etc/ab0t-quota/quota-config.json`

First hit wins. See [ab0t-quota-go-config](../ab0t-quota-go-config/SKILL.md)
for the schema.

## Env vars consumed at Setup

| Var | Effect when set | Effect when missing |
|-----|-----------------|---------------------|
| `AB0T_QUOTA_BILLING_URL` | wires `q.Billing` typed client | `Capabilities.Billing=false`, `WhyOff["billing"]` says why |
| `AB0T_QUOTA_PAYMENT_URL` | wires `q.Payment` typed client | `Capabilities.Payment=false` |
| `AB0T_QUOTA_SERVICE_TOKEN` | bearer for mesh calls | unauthenticated mesh calls |
| `AB0T_AUTH_WEBHOOK_SECRET` | HMAC for webhook receiver | receiver returns 401 to every event |
| `AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS` | allow `metered_billing` / `one_time_purchase` tiers | config validation rejects them |

## Reading Capabilities

`Setup` emits a single `INFO` log line at start with every capability and
its on/off state. Programmatic access:

```go
caps := q.Capabilities()
if !caps.Billing {
    log.Warn("billing off", "reason", caps.WhyOff["billing"])
}
```

Use this in a `/healthz` handler to surface intentional vs unintentional
degradation.

## Graceful shutdown

`q.Close(ctx)` stops the heartbeat loop (if started) and flushes any
buffered ledger writes. Wire it to the same SIGTERM the HTTP server
listens for.

```go
srv.Shutdown(ctx)   // first
q.Close(ctx)        // second
```

## What gets registered

Even with no `CreditGranter`, Setup registers:
- The HTTP guard available via `q.Middleware(deps)`
- The webhook receiver available via `q.WebhookHandler()`
- The engine accessible via `q.Check`, `q.Spend`, `q.Release`

With `CreditGranter`, additionally:
- A handler on `org.created` and `user.org_assigned` events that calls
  your granter when the event resolves to a tiered user.

## After Setup — verify before moving on

The pattern is: Setup → check Capabilities → write smoke test.

```go
q, err := quota.Setup(ctx, opts)
if err != nil { log.Fatal(err) }

caps := q.Capabilities()
log.Printf("capabilities: %+v", caps)

// Expected: Engine=true, AuthEvents=true, Enforcement=true,
//          Billing=true (if AB0T_QUOTA_BILLING_URL set),
//          CreditGrant=true (if CreditGranter passed).
// Any WhyOff entry means a deliberate config gap — read it.
```

If anything looks off, go straight to
[ab0t-quota-go-testing](../ab0t-quota-go-testing/SKILL.md) — its
"Troubleshooting by symptom" table is keyed to the most common
Setup-time failures (every-request-401, every-request-429,
CreditGrant=false, etc.).

## Common errors

| Error | Cause | Fix |
|-------|-------|-----|
| `validate config: tiers[] is required` | empty config | add at least one tier |
| `tier_provider.type unknown` | typo or missing | use `jwt`, `static`, or `mesh` |
| `TierProvider required` (in `BuildDefaultCreditGrantHandler`) | passing no provider | use `q.Provider` from Setup |
| `engine_mode "bridge" is out of scope for v0.1.0` | bridge mode requested | use `local` or `byo_redis` |

For the full end-to-end example, see `examples/basic/main.go` in the
library repo.
