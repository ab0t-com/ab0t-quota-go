# Architecture

## One paragraph

`ab0t-quota-go` is a drop-in Go SDK for ab0t-style services that need
**quota enforcement** + **billing-credit lifecycle**. The consumer calls
`quota.Setup(ctx, opts)` once during startup, wraps their HTTP handler in
`q.Middleware(...)`, mounts `q.WebhookHandler()` for auth-event
delivery, and is done — every request is checked against tier limits,
every relevant auth event drives a credit grant through the consumer's
billing service, and every dispatched handler is delivery-deduped +
business-deduped + ledger-persisted so retries can't double-spend.

## Three paragraphs (theory of operation)

### Enforcement path (the hot path)

A request comes in. Middleware extracts (user_id, org_id) via the
consumer's Identity function and resolves a `resource_key` + cost via
the Router. Engine asks the TierProvider for a tier_id, looks up the
tier's limit for that resource in the Registry, then asks the Counters
factory for the live usage on a per-(org, resource, period) key. Math
runs: under limit → Allow, between limit and burst → Allow with a Warn
header, over burst → Deny with the consumer-overridable message
template. Shadow mode flips Deny → ShadowAllow (allow + log) so
operators can turn enforcement on safely.

### Webhook path (the warm path)

Auth events (`org.created`, `user.org_assigned`, plus consumer-defined
types) arrive at `<your-prefix>/api/quotas/_webhooks/auth`. The receiver
verifies HMAC over the **raw body** (never re-canonicalize JSON), parses
the v1 or v2 envelope, looks up handlers in the package-level Registry,
and dispatches. Handlers wrapped in `authevents.Idempotent(...)` get
the ledger pipeline: `RecordAttempt` claims a lease for the (handler,
event_id) pair, the inner runs with retry, `RecordOutcome` writes one of
success/skipped/failed_permanent. A second arrival of the same event_id
finds the row and returns the cached outcome without re-running. This is
the same mechanism that makes the default credit-grant handler safe to
auto-retry — once 1) the row exists with status=success or 2) the
business dedup key (e.g. `credit_granted:user:u1:tier_pro`) is set, the
grant won't apply twice.

### Capability surface (the smart-default path)

`Setup` returns a `Capabilities` snapshot AND emits one structured log
line listing which subsystems are on and which are off (and why). It is
explicit by design: if `AB0T_QUOTA_BILLING_URL` isn't set, billing isn't
wired and `Capabilities.WhyOff["billing"]` says so. The intent is to
make degraded operation visible — no surprise "I thought billing was
on." This is the Go port's counter to Python's `BUG #1` (missing
`tier_provider` swallowed silently until first event); here it
short-circuits at Setup with an explicit constructor error.

## Module dependency graph

```
quota
 ├── engine        (decision math)
 │    ├── registry        (resource + tier index)
 │    ├── counters        (Redis-shaped float counters; in-memory fallback)
 │    ├── providers       (jwt | static | mesh with TTL cache)
 │    └── messages        (config-driven copy)
 ├── middleware    (HTTP guard + header writers + denial responder)
 ├── authevents    (webhook receiver + registry + Idempotent + default credit-grant handler)
 │    └── handlerledger   (delivery dedup + business dedup + retry + ledger persistence)
 ├── alerts        (manager + log + webhook dispatchers, SSRF-guarded)
 ├── billing       (typed REST client)
 ├── payment       (typed REST client)
 ├── mesh          (env-var → URLs)
 └── internal/httpx (shared HTTP client)

cmd/quotactl       admin CLI — subscribe-events, events, replay, backfill,
                   delete-user, capabilities
```

Arrows go down only (no upward imports). `authevents` and `handlerledger`
have separate Event abstractions to keep the cycle broken:
`handlerledger.Event` is an interface; `authevents.Event` is a struct
that satisfies it. The receiver type-switches on
`*authevents.IdempotentHandler` to give wrapped handlers the ledger
pipeline; plain HandlerFunc handlers skip it.

## Storage abstraction

Two stores have pluggable backends:

| Interface              | Default        | Future                 |
|------------------------|----------------|------------------------|
| `counters.FloatStore`  | InMemoryStore  | Redis (v0.2)           |
| `counters.RateStore`   | MemoryRateStore| Redis ZSET (v0.2)      |
| `handlerledger.LedgerStore` | InMemoryLedgerStore | Redis + DDB (v0.2) |
| `authevents.PinStore`  | MemoryPinStore | Redis + DDB (v0.2)     |

Stubs for the persistent backends exist with typed
`ErrRedisNotAvailable` errors so `Setup` can fall back cleanly. The
`Capabilities` snapshot reports `LedgerBackend = "memory"` so operators
notice.

## Wire-level parity

This Go port is intentionally bit-for-bit compatible with the Python
`ab0t-quota` library v0.5.2 on the wire:

- HMAC formats: sha256 hex with optional `sha256=` prefix; constant-time
  comparison.
- Signature header alternates: `X-Event-Signature` (canonical),
  `X-Webhook-Signature` (legacy fallback).
- Event envelopes: v1 (`event_type`, `event_id`, `occurred_at`) + v2
  aliases (`type`, `id`, `timestamp`); `data.*` + top-level fallbacks
  for `user_id`/`org_id`.
- Business dedup key shapes (all 4 policies — `per_user_per_tier`,
  `per_org_per_tier`, `per_user_global`, `per_org_global`).
- Redis key namespaces: `{prefix}:counter:...`, `{prefix}:gauge:...`,
  `{prefix}:rate:...`, `{prefix}:accumulator:...`,
  `{prefix}:idempotency:...`.
- Billing/payment endpoint paths and methods (see `back_references.md`
  C5 fixes vs the inital PSD).
- Env var names: `AB0T_QUOTA_*`, `AB0T_AUTH_*`, etc.

A mixed Python/Go deployment can share the same Redis prefix, same DDB
table (once wired), and same HMAC secret without coordination.

## Known upstream bugs structurally prevented

| ID | Python bug | Go counter |
|----|-----------|------------|
| #1 | `setup.py:938` NameError because `tier_provider` not wired into default handler | `BuildDefaultCreditGrantHandler` REQUIRES TierProvider as a constructor arg; surfaces at startup, not at first event |
| #3 | Auth v1 webhook signs canonical bytes but ships aiohttp-reserialized bytes | Receiver verifies over raw body; cautionary comment + documented |
| #4 | `shadow_mode` flag exists in config but never read | Engine actively flips `Deny → ShadowAllow` when `shadow_mode=true` |

Bugs #2 and #5 are in publisher code (auth service); the Go port
documents them in `back_references.md` so consumers know to disable
those code paths in production.
