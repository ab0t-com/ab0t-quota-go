---
name: ab0t-quota-go-middleware
description: Wrap a Go HTTP handler with the ab0t-quota guard so every request runs a quota check. Use when calling `q.Middleware(...)`, writing the Identity / Router callbacks, choosing fail-open vs fail-closed, exempting health endpoints, interpreting X-Quota-* response headers, returning custom 429 bodies, integrating with `chi`/`gorilla/mux`/`net/http`/`echo`/`gin`, or debugging why a request was denied / why no headers were written.
---

# ab0t-quota-go Middleware

The guard is a `func(http.Handler) http.Handler` returned by
`q.Middleware(MiddlewareDeps{...})`. Wrap any handler with it.

## Quick start

```go
import (
    "github.com/ab0t-com/ab0t-quota-go/quota"
)

guard := q.Middleware(quota.MiddlewareDeps{
    Identity: identityFn,
    Router:   routerFn,
    Exempt:   []string{"/healthz", "/metrics"},
})

mux.Handle("/api/", guard(yourHandler))
```

## Two callbacks you write

### Identity — extract who is calling

```go
func identityFn(r *http.Request) (userID, orgID string, err error) {
    claims, ok := r.Context().Value(jwtClaimsKey{}).(*jwt.Claims)
    if !ok { return "", "", errors.New("no claims") }
    return claims.Subject, claims.OrgID, nil
}
```

The Identity must be set by upstream auth middleware — the guard never
parses JWTs itself. Returning an error triggers fail-open or 401 based
on `FailOpen`.

### Router — pick the resource + cost for this request

```go
func routerFn(r *http.Request) (resourceKey string, cost float64) {
    switch {
    case strings.HasPrefix(r.URL.Path, "/api/sandbox/create"):
        return "sandbox.concurrent", 1
    case strings.HasPrefix(r.URL.Path, "/api/llm/chat"):
        return "llm.calls", 1
    default:
        return "", 0  // skip the guard
    }
}
```

Returning `""` for the resource skips the check for that request — use
this for "free" endpoints that don't fit your tier limits.

## Response shape

### Allowed (status 200, headers set)

```
X-Quota-Resource: sandbox.concurrent
X-Quota-Tier: pro
X-Quota-Limit: 25
X-Quota-Remaining: 18
X-Quota-Used: 7
X-Quota-Reason: under_limit
```

### Denied (status 429)

```json
{
  "detail": "Quota exceeded for sandbox.concurrent: 25/25 (tier: pro).",
  "reason": "exceeded",
  "resource": "sandbox.concurrent",
  "tier": "pro",
  "used": 25,
  "limit": 25,
  "upgrade_url": "https://billing.example.com/upgrade"
}
```

Plus all `X-Quota-*` headers and `Retry-After`.

### Warn / Critical (status 200, X-Quota-Warning header set)

Request goes through, but a header signals the consumer they're near
limit. Use this for friendly in-app banners.

## Decision tree the guard runs

1. Is the path in `Exempt`? → next handler, no headers
2. Does Router return empty `resourceKey`? → next handler, no headers
3. Does Identity error? → `FailOpen ? next : 401`
4. Does Engine error (unknown resource, lookup failed)? → `FailOpen ? next : 503`
5. Decision = Deny? → 429 with denial body
6. Decision = Warn/Critical? → next handler + warning header
7. Otherwise → next handler + headers

## fail-open vs fail-closed

| | When to use |
|---|-------------|
| `FailOpen: true` | quota is advisory, billing reconciles later; degraded mode should not break the customer |
| `FailOpen: false` (default) | quota is a hard contract (e.g. spend caps); refuse rather than overspend on a bad path |

## Hooks for instrumentation

```go
quota.MiddlewareDeps{
    OnDecision: func(r *http.Request, res engine.Result) {
        metrics.Counter("quota.decisions", "decision", string(res.Decision)).Inc()
    },
    OnWarn: func(r *http.Request, res engine.Result) {
        slog.Warn("quota near limit", "user", r.Header.Get("X-User-Id"),
            "resource", res.Resource, "threshold", res.Threshold)
    },
}
```

## Standalone Check / Spend / Release

When middleware doesn't fit (background jobs, async workers):

```go
res, err := q.Check(ctx, engine.CheckInput{
    UserID: "alice", OrgID: "acme",
    ResourceKey: "spend.usd", Cost: 1.50,
})
if !res.Allowed() { return ErrOverBudget }
_, err = q.Spend(ctx, engine.CheckInput{
    UserID: "alice", OrgID: "acme",
    ResourceKey: "spend.usd", Cost: 1.50,
})

// For gauges (concurrent sandboxes, open connections):
defer q.Release(ctx, engine.CheckInput{
    UserID: "alice", OrgID: "acme",
    ResourceKey: "sandbox.concurrent", Cost: 1,
})
```

## Framework adapters

The guard is a vanilla `func(http.Handler) http.Handler` — drop into:

| Framework | Snippet |
|-----------|---------|
| `net/http` | `mux.Handle("/api/", guard(handler))` |
| `chi` | `r.Use(guard)` |
| `gorilla/mux` | `r.Use(guard)` |
| `echo` | wrap via `echo.WrapMiddleware(guard)` |
| `gin` | wrap via `gin.WrapH(guard(...))` per route |

## Common errors

| Symptom | Cause |
|---------|-------|
| `panic: middleware.Guard: Identity required` | passed `nil` Identity in MiddlewareDeps |
| no `X-Quota-*` headers on response | path is exempt, or Router returned `""` |
| every request gets 429 with `reason=tier_unresolved` | Identity returns no user_id, or no tier resolved |
| every request gets 429 with `reason=tier_not_in_config` | TierProvider returned an id not in `config.tiers[]` |
| header says `X-Quota-Reason: shadow_would_deny` | `enforcement.shadow_mode=true` — fix config or accept it |
