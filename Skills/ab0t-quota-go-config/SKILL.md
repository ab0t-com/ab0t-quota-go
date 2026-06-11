---
name: ab0t-quota-go-config
description: Author the quota-config.json file that ab0t-quota-go reads at startup. Use when defining tiers (free / pro / enterprise), declaring resources (counter_type, reset_period, window_seconds), wiring tier_provider (jwt / mesh / static), choosing dedup policy (per_user_per_tier / per_org_per_tier / per_user_global / per_org_global), setting credit_grant rules, picking billing_model, enabling shadow_mode or kill_switch, configuring storage prefix, setting up alerts thresholds + webhooks, or debugging "tier not in config" / "credit_grant required" errors.
---

# ab0t-quota-go Config Schema

`quota-config.json` is the source of truth for tiers, resources, and
runtime behavior. The Go port is wire-compatible with Python ab0t-quota
v0.5.2 — same file works for both runtimes.

## Minimum viable config

```json
{
  "service_name": "my-service",
  "tier_provider": {
    "type": "static",
    "default_tier": "free"
  },
  "storage": {
    "redis_key_prefix": "ab0t-quota"
  },
  "enforcement": {
    "enabled": true
  },
  "tiers": [
    { "tier_id": "free", "display_name": "Free", "sort_order": 1, "limits": {"api.calls": 100} }
  ],
  "resources": [
    { "service": "my-service", "resource_key": "api.calls",
      "display_name": "API Calls", "counter_type": "accumulator",
      "reset_period": "hourly" }
  ]
}
```

## tier_provider — how to resolve user → tier

| `type` | When to use | Required fields |
|--------|-------------|-----------------|
| `jwt` | Tier lives in JWT claim | `jwt_claim_key` (default `tier`), `default_tier` (fallback) |
| `static` | Fixed map (small fleet, tests) | `mapping: {user_id: tier_id}`, `default_tier` |
| `mesh` | Tier comes from billing service | nothing here; wire `SetLookup` in code |

Add `cache_ttl_seconds` to memoize lookups: `30-300` is typical.

## resources — what you meter

| `counter_type` | Semantics | Extra fields |
|----------------|-----------|--------------|
| `gauge` | Current level (concurrent sandboxes, open connections) | none |
| `accumulator` | Monotonic per-period (USD spend, API calls/hour) | `reset_period: hourly\|daily\|weekly\|monthly` |
| `rate` | Sliding window (req/sec) | `window_seconds` |

```json
{
  "service": "sandbox",
  "resource_key": "sandbox.concurrent",
  "counter_type": "gauge"
},
{
  "service": "billing",
  "resource_key": "spend.usd",
  "counter_type": "accumulator",
  "reset_period": "monthly",
  "precision": 2
},
{
  "service": "api",
  "resource_key": "api.qps",
  "counter_type": "rate",
  "window_seconds": 60
}
```

## tiers — what users can do

```json
{
  "tier_id": "pro",
  "display_name": "Pro",
  "sort_order": 2,
  "features": ["custom_domains", "ssh_access"],
  "upgrade_url": "https://billing.example.com/upgrade",
  "billing_model": "subscription_with_credits",
  "price": { "amount_per_period": "29.00", "currency": "USD", "period": "month" },
  "credit_grant": {
    "trigger": "subscription_invoice_paid",
    "amount_per_period": "25.00",
    "currency": "USD",
    "lifecycle": "use_it_or_lose_it",
    "destination": "subscription_credit",
    "dedup": "per_user_per_tier",
    "reset_on_downgrade": true,
    "reset_on_upgrade": false
  },
  "limits": {
    "sandbox.concurrent": { "limit": 25, "burst_allowance": 5, "warning_threshold": 0.80 },
    "spend.usd": { "limit": 100.00 },
    "api.qps": { "limit": 100, "per_user_limit": 10 }
  }
}
```

### Limit shape variants

```json
"limits": {
  "x": 25,                  // bare number → just a limit
  "y": null,                // null → unlimited
  "z": {                    // object → full control
    "limit": 100,
    "warning_threshold": 0.80,   // emit Warn at 80%
    "critical_threshold": 0.95,  // emit Critical at 95%
    "burst_allowance": 20,       // allow short overage
    "per_user_limit": 10         // per-user cap derived from per-org
  }
}
```

If `per_user_limit` is absent, set `default_per_user_fraction` on the
tier — the lib computes per-user as `ceil(limit × fraction)`.

## billing_model — gated values

| Value | Notes |
|-------|-------|
| `consumption_only` | usage-based, no subscription |
| `subscription_unlock_only` | flat fee, no credit grant |
| `subscription_with_credits` | flat fee + monthly credit refill (`credit_grant.trigger = subscription_invoice_paid`) |
| `metered_billing` | ⚠ experimental — requires `AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS=true` |
| `one_time_purchase` | ⚠ experimental — same gate |
| `free_tier` | no charge |
| `enterprise` | custom — usually consumption + manual top-ups |

Validation enforces cross-field rules — e.g.
`subscription_with_credits` REQUIRES a `price` AND a `credit_grant` with
trigger=`subscription_invoice_paid`.

## credit_grant — when to give credits

```json
"credit_grant": {
  "trigger": "signup",                  // signup | subscription_invoice_paid | scheduled_period_start | manual | webhook_admin
  "amount_per_period": "25.00",
  "currency": "USD",                    // default USD
  "lifecycle": "use_it_or_lose_it",     // persistent | use_it_or_lose_it | rollover_capped
  "destination": "subscription_credit", // credit_balance | subscription_credit
  "dedup": "per_user_per_tier",         // see dedup policies below
  "reset_on_downgrade": true,
  "reset_on_upgrade": false
}
```

`rollover_capped` REQUIRES `rollover_max_periods`. Validation rejects
otherwise.

### Legacy `initial_credit` (synthesized)

```json
"initial_credit": "10.00"
```

The loader rewrites this into:

```json
"credit_grant": {
  "trigger": "signup",
  "amount_per_period": "10.00",
  "lifecycle": "use_it_or_lose_it",
  "destination": "subscription_credit",
  "dedup": "per_user_per_tier"
}
```

Prefer writing the explicit `credit_grant`. Don't set both.

## dedup policy — anti-double-spend

| Value | Key shape | Use for |
|-------|-----------|---------|
| `per_user_per_tier` (default) | `credit_granted:user:{user_id}:{tier_id}` | Most cases — anti-farming |
| `per_org_per_tier` | `credit_granted:org:{org_id}:{tier_id}` | B2B — org pools credits |
| `per_user_global` | `credit_granted:user:{user_id}` | One credit ever per user |
| `per_org_global` | `credit_granted:org:{org_id}` | One credit ever per org |

The lib composes the key; you only pick the policy. Stable across
Python + Go.

## enforcement — runtime knobs

```json
"enforcement": {
  "enabled": true,         // false → engine always Allows
  "shadow_mode": false,    // true → flips Deny to ShadowAllow + logs
  "global_kill_switch": false  // true → engine always Denies (emergency)
}
```

`shadow_mode` is honored by the Go port (Python lib ignores it — known
upstream bug).

## storage — key namespace + (future) Redis URL

```json
"storage": {
  "redis_key_prefix": "ab0t-quota",   // every key prefixed with this
  "redis_url": "redis://localhost:6379/0",
  "dynamodb_table": "ab0t-quota-ledger",
  "dynamodb_region": "us-east-1"
}
```

v0.1.0 ships in-memory backends only; `redis_url` / `dynamodb_table`
trigger a "v0.2 wires this" warning at Setup. Set them now so the
upgrade is a one-line change.

## alerts — threshold notifications

```json
"alerts": {
  "enabled": true,
  "cooldown_seconds": 3600,        // suppress dupes within this window
  "warning_threshold": 0.80,        // emit at 80% of any limit
  "critical_threshold": 0.95,
  "webhook_url": "https://hooks.slack.com/services/..."   // optional
}
```

Webhook is SSRF-guarded by default — rejects `localhost`, RFC1918
ranges, `file://`. Set `AllowPrivateNetworks` in code if you genuinely
need internal targets.

## ${QUOTA_*} env interpolation

Strings like `${QUOTA_REDIS_URL}` or `${QUOTA_REDIS_URL:-default}` are
expanded from env at load time. Only `QUOTA_`-prefixed vars are
substituted — safety.

```json
"storage": {
  "redis_url": "${QUOTA_REDIS_URL:-redis://localhost:6379/0}",
  "redis_password": "${QUOTA_REDIS_PASSWORD}"
}
```

## Forward compat

Unknown top-level keys are preserved in the `Extra` map at parse time —
your Go binary won't fail to start when a future Python schema addition
shows up. `$comment_*` keys are ignored entirely.

## Validation rules in order

1. `engine_mode` ∈ {`local`, `byo_redis`}; `bridge` rejected
2. `tiers[]` non-empty + unique `tier_id`
3. Each tier's `billing_model` cross-fields valid (price+credit_grant)
4. `resources[]` unique `resource_key`; required fields per `counter_type`
5. Every `resource_bundles[].keys[]` references a known `resource_key`

`q.Capabilities()` reports the runtime state; config validation reports
the static state. Both surface actionable error strings.

## Common errors

| Error | Fix |
|-------|-----|
| `tier %q: billing_model %q requires credit_grant with trigger %q` | add or fix the `credit_grant` block |
| `tier_provider.type unknown` | use `jwt`, `static`, or `mesh` |
| `resource %q: reset_period required for accumulator` | add `reset_period: hourly/daily/weekly/monthly` |
| `resource %q: window_seconds required for rate counters` | add `window_seconds: 60` (or similar) |
| `credit_grant.rollover_max_periods required when lifecycle is "rollover_capped"` | add `rollover_max_periods: 3` (or similar) |
| `billing_model %q is experimental` | set `AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS=true` or pick a stable model |

## Full sample

See `examples/basic/quota-config.json` in the library repo for a working
minimal config.
