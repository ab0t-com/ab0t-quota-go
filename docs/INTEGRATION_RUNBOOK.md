# Gateway Platform Integration Runbook

A step-by-step from "we've heard of ab0t-quota" to "billing is live in
production." Written for the gateway team. Read [BILLING_GLOSSARY.md](
BILLING_GLOSSARY.md) first if any of the terms below are unfamiliar.

Stages:

1. **Decide your billing shape** (1 hour, on paper)
2. **Write `quota-config.json`** (1 hour, in JSON)
3. **Wire `quota.Setup`** (15 minutes, in Go)
4. **Wire the middleware** (1 hour, in Go)
5. **Wire the webhook receiver + credit grant** (2 hours, in Go)
6. **Configure env + secrets** (15 minutes, in your deploy system)
7. **Smoke test in shadow mode** (1 day, in staging)
8. **Flip enforcement on** (15 minutes, in prod)
9. **Wire dashboards + alerts** (2 hours, in Grafana / Datadog)

Total: 1–2 engineering days end-to-end.

> **Test as you go.** Each stage below has a "verify" step. Don't skip
> them — a bug at Stage 3 costs 5 minutes to fix; the same bug
> discovered at Stage 8 in prod costs hours. The full testing playbook
> is in
> [`Skills/ab0t-quota-go-testing/SKILL.md`](../Skills/ab0t-quota-go-testing/SKILL.md);
> verify steps in each stage reference its specific patterns.

---

## Stage 1 — Decide your billing shape (on paper)

Walk this checklist **with product + finance**. The output is a one-pager
that you can show to anyone in the company:

1. **What's the product?** One sentence.
2. **What costs us money per customer?** (your AWS / vendor bill —
   sorted by line item, biggest first)
3. **What creates value differentiation?** (what makes a customer say
   "I need to upgrade?")
4. **Which billing archetype?** Pick one from
   [BILLING_MODELS_GUIDE.md](BILLING_MODELS_GUIDE.md). For a
   gateway/proxy product, that's usually **Archetype C** (subscription
   with bundled credits) or **Archetype B** (pure consumption / pay-as-
   you-go) depending on whether you have a meaningful free tier.
5. **What are the tiers?** Free, plus 2–3 paid. Each with a name, a
   price, and concrete limits.
6. **What's the free-tier cost cap?** This is the answer to "what's the
   worst a free user can cost us in a month?" Pick a number small
   enough that 1000 free users is still a small bill.
7. **What's the upgrade trigger?** When a user hits one of the free
   limits, what do they see? (Banner / modal / 429 with upgrade URL.)

Output: a Notion/Google doc with the tier table. Get it signed off
before writing code.

For inspiration look at sandbox-platform's setup at
`infra/code/resource/output/sandbox-platform/quota-config.json` — a
mature 4-tier hybrid model with all the moving parts in place.

---

## Stage 2 — Write `quota-config.json`

Drop this file in your service's repo root. It's the source of truth.

Minimal viable shape:

```json
{
  "service_name": "gateway",
  "tier_provider": {
    "type": "mesh",
    "default_tier": "free",
    "cache_ttl_seconds": 60
  },
  "storage": {
    "redis_key_prefix": "ab0t-quota-gateway"
  },
  "enforcement": {
    "enabled": true,
    "shadow_mode": true
  },
  "resources": [
    {
      "service": "gateway",
      "resource_key": "api.calls",
      "display_name": "API Calls",
      "counter_type": "accumulator",
      "reset_period": "monthly"
    },
    {
      "service": "gateway",
      "resource_key": "spend.usd",
      "display_name": "Monthly Spend",
      "counter_type": "accumulator",
      "reset_period": "monthly",
      "precision": 2
    }
  ],
  "tiers": [
    {
      "tier_id": "free",
      "display_name": "Free",
      "sort_order": 0,
      "limits": {
        "api.calls": 10000,
        "spend.usd": 5.00
      },
      "credit_grant": {
        "trigger": "signup",
        "amount_per_period": "5.00",
        "lifecycle": "use_it_or_lose_it",
        "destination": "subscription_credit",
        "dedup": "per_user_per_tier"
      }
    },
    {
      "tier_id": "pro",
      "display_name": "Pro",
      "sort_order": 2,
      "billing_model": "subscription_with_credits",
      "price": {
        "amount_per_period": "29.00",
        "currency": "USD",
        "period": "month"
      },
      "credit_grant": {
        "trigger": "subscription_invoice_paid",
        "amount_per_period": "29.00",
        "lifecycle": "use_it_or_lose_it",
        "destination": "subscription_credit"
      },
      "limits": {
        "api.calls": 500000,
        "spend.usd": 100.00
      },
      "upgrade_url": "https://billing.your-domain.com/upgrade"
    }
  ],
  "alerts": {
    "enabled": true,
    "cooldown_seconds": 3600,
    "warning_threshold": 0.80,
    "critical_threshold": 0.95
  }
}
```

Note `enforcement.shadow_mode: true` — that's deliberate. We turn it
off in Stage 8 after we've watched the dashboards.

**Verify Stage 2:**

```bash
quotactl capabilities --config quota-config.json
```

You should see `"Enforcement": true, "ShadowMode": true` and the tier
list. If validation errors appear, fix them now — they get harder to
debug after Setup runs in a hot reload.

See [testing skill § Phase 1](
../Skills/ab0t-quota-go-testing/SKILL.md#phase-1--verify-it-loaded-at-all)
for the full Capabilities interpretation table.

---

## Stage 3 — Wire `quota.Setup`

In your `main.go`:

```go
package main

import (
    "context"
    "log"
    "net/http"
    "os"
    "os/signal"
    "syscall"

    "github.com/ab0t-com/ab0t-quota-go/quota"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(),
        os.Interrupt, syscall.SIGTERM)
    defer stop()

    q, err := quota.Setup(ctx, quota.Options{
        ConfigPath: "quota-config.json",
        // CreditGranter wired in Stage 5
    })
    if err != nil {
        log.Fatal(err)
    }
    defer q.Close(context.Background())

    // ... your HTTP wiring in Stage 4 ...

    log.Println("startup capabilities:", q.Capabilities())
}
```

The library reads `AB0T_QUOTA_BILLING_URL`, `AB0T_QUOTA_PAYMENT_URL`,
and a few other env vars at Setup. They're listed in
[ab0t-quota-go-setup](../Skills/ab0t-quota-go-setup/SKILL.md).

Start your service. The startup log should print one line listing
capabilities. Check `Engine: true`, `Enforcement: true`. Billing and
Payment may be `false` until Stage 6.

**Verify Stage 3:** drop in this test now (don't wait for Stage 7).
The point is to catch wiring bugs while you're still in the file:

```go
func TestSetupLoads(t *testing.T) {
    q, err := quota.Setup(t.Context(), quota.Options{
        ConfigPath: "quota-config.json",
    })
    require.NoError(t, err)
    defer q.Close(context.Background())
    require.True(t, q.Capabilities().Engine)
}
```

Full smoke patterns: [testing skill § Phase 2](
../Skills/ab0t-quota-go-testing/SKILL.md#phase-2--unit-tests-with-no-network).

---

## Stage 4 — Wire the middleware

The guard intercepts requests, runs the quota check, and either
proceeds or returns 429.

```go
guard := q.Middleware(quota.MiddlewareDeps{
    Identity: func(r *http.Request) (userID, orgID string, err error) {
        // Adapt to how your gateway extracts identity. Examples:
        //   - from a JWT in the Authorization header
        //   - from a context value set by upstream auth middleware
        //   - from an internal session cookie
        claims, ok := r.Context().Value(jwtClaimsKey{}).(*Claims)
        if !ok {
            return "", "", errors.New("no claims")
        }
        return claims.UserID, claims.OrgID, nil
    },
    Router: func(r *http.Request) (resourceKey string, cost float64) {
        switch {
        case strings.HasPrefix(r.URL.Path, "/v1/chat/completions"):
            return "api.calls", 1
        case strings.HasPrefix(r.URL.Path, "/v1/embeddings"):
            return "api.calls", 1
        case strings.HasPrefix(r.URL.Path, "/healthz"):
            return "", 0  // skip
        }
        return "", 0
    },
    Exempt:   []string{"/healthz", "/metrics", "/openapi.json"},
    FailOpen: false,  // fail-closed in prod
})

mux.Handle("/v1/", guard(yourAPIHandler))
mux.Handle("/healthz", healthzHandler)
```

See [ab0t-quota-go-middleware](../Skills/ab0t-quota-go-middleware/SKILL.md)
for the full callback contract.

After this, every request gets `X-Quota-*` headers in the response. In
shadow mode, no request is actually denied — but the would-be denials
are visible in logs and dashboards.

**Verify Stage 4:** three httptest assertions:
1. Under-limit request → 200 with `X-Quota-Tier` and `X-Quota-Limit` headers
2. Exempt path (e.g. `/healthz`) → 200 with NO `X-Quota-*` headers
3. Pre-fill spend to the cap, send a request → 200 (because shadow_mode), but log shows `shadow_would_deny`

Full patterns: [testing skill § Phase 3](
../Skills/ab0t-quota-go-testing/SKILL.md#phase-3--smoke-test-the-http-guard).

### Spending — cost-shaped operations

If your gateway proxies an LLM and you care about $-spend (not just
call count), call `Spend` after the upstream returns the cost:

```go
upstreamCost := callAnthropic(req)  // your upstream call
_, _ = q.Spend(ctx, engine.CheckInput{
    UserID: userID, OrgID: orgID,
    ResourceKey: "spend.usd", Cost: upstreamCost,
})
```

This increments the monthly accumulator. The next request from this
org will see `Used = previous + upstreamCost`.

---

## Stage 5 — Wire webhooks + credit grant

This is where money meets metering. Two things to wire:

### 5a. Mount the webhook receiver

```go
import "github.com/ab0t-com/ab0t-quota-go/authevents"

mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())
// becomes /api/quotas/_webhooks/auth
```

Your gateway service URL + this path is what you'll register with
ab0t-auth in Stage 6.

### 5b. Implement `CreditGranter`

This is the callback the library uses when an auth event resolves to a
credit grant. The body of `GrantCredit` is **the moment money lands**.

```go
import (
    "github.com/ab0t-com/ab0t-quota-go/authevents"
    "github.com/ab0t-com/ab0t-quota-go/billing"
)

type gatewayGranter struct {
    bc *billing.Client
}

func (g gatewayGranter) GrantCredit(ctx context.Context,
    in authevents.CreditGrantRequest) error {
    _, err := g.bc.GrantCredit(ctx, billing.CreditGrantRequest{
        UserID:  in.UserID,
        OrgID:   in.OrgID,
        TierID:  in.TierID,
        Amount:  in.Amount,
        EventID: in.EventID,    // ledger uses this for idempotency
        Reason:  "auth_event:" + in.Trigger,
    })
    return err
}
```

Then update Setup:

```go
q, err := quota.Setup(ctx, quota.Options{
    ConfigPath:              "quota-config.json",
    CreditGranter:           gatewayGranter{bc: q.Billing},
    AutoSubscribeAuthEvents: true,
})
```

`q.Billing` is the typed client to ab0t-billing-service. It's nil if
`AB0T_QUOTA_BILLING_URL` isn't set — fix that in Stage 6 first.

`AutoSubscribeAuthEvents` makes the library call auth at startup to
register your webhook URL. Idempotent; safe to run on every deploy.

**Verify Stage 5:** four tests, in this order:
1. Post a signed `org.created` webhook → 200, your stub `CreditGranter` called once
2. Replay the EXACT same body → 200, but `CreditGranter` NOT called again (delivery dedup)
3. Post with a bad HMAC sig → 401 with `invalid signature` body
4. Post unknown `event_type` → 200 with `"status":"ignored"`

Full patterns + helpers (`SignBody`, stub granter): [testing skill §
Phase 4](
../Skills/ab0t-quota-go-testing/SKILL.md#phase-4--smoke-test-the-webhook-receiver).

---

## Stage 6 — Configure env + secrets

In your deploy system (Kubernetes ConfigMap, ECS task def, fly.toml,
whatever):

| Var | Value | Where to get it |
|-----|-------|-----------------|
| `AB0T_QUOTA_BILLING_URL` | `https://billing.service.ab0t.com` | known |
| `AB0T_QUOTA_PAYMENT_URL` | `https://payment.service.ab0t.com` | known |
| `AB0T_QUOTA_SERVICE_TOKEN` | bearer token | ab0t-mesh registration |
| `AB0T_AUTH_AUTH_URL` | `https://auth.service.ab0t.com` | known |
| `AB0T_AUTH_ADMIN_TOKEN` | bearer token | ab0t-auth admin |
| `AB0T_AUTH_WEBHOOK_PUBLIC_URL` | `https://gateway.your-domain.com` | your service's public URL |
| `AB0T_AUTH_WEBHOOK_SECRET` | random 32 bytes | generate; shared with auth |

Generate the webhook secret:

```bash
openssl rand -hex 32
```

Store it in your secrets store (AWS Secrets Manager, GCP Secret
Manager, Vault). The same value must be set in:

1. Your gateway service's env
2. The subscription you create in ab0t-auth (handled by
   `AutoSubscribeAuthEvents`)

If they don't match, the receiver returns 401 to every event.

After this, restart your service and re-check capabilities:

```bash
quotactl capabilities --config quota-config.json
```

You should see `Billing: true`, `Payment: true`, `CreditGrant: true`,
`AutoSubscribe: true`.

**Verify Stage 6** — bash smoke tests against the live staging URL:

```bash
SVC=https://gateway-staging.your-domain.com
SECRET="$AB0T_AUTH_WEBHOOK_SECRET"

# Bad sig → 401
curl -s -o /dev/null -w "%{http_code}\n" -X POST \
  $SVC/api/quotas/_webhooks/auth \
  -H 'X-Event-Signature: sha256=deadbeef' \
  -H 'Content-Type: application/json' \
  -d '{"event_type":"org.created","event_id":"e1"}'

# Good sig → 200
BODY='{"event_type":"org.created","event_id":"smoke-'$RANDOM'","data":{"user_id":"alice","org_id":"acme"}}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | sed 's/^.* //')
curl -s -X POST $SVC/api/quotas/_webhooks/auth \
  -H "X-Event-Signature: sha256=$SIG" \
  -H 'Content-Type: application/json' \
  -d "$BODY"
```

Full Phase 5 bash test menu: [testing skill § Phase 5](
../Skills/ab0t-quota-go-testing/SKILL.md#phase-5--bash-smoke-tests-against-a-live-service).

---

## Stage 7 — Smoke test in shadow mode

Shadow mode (`enforcement.shadow_mode: true`) is the safety net. The
engine runs the math, logs would-be denials, but lets every request
through.

Watch for one calendar day:

1. **Synthetic traffic** — hit your service with a free-tier user
   doing 10,000 calls. The 10,001st should log a `shadow_would_deny`
   event. **It should not 429.**
2. **Real traffic** — let your real users flow. Watch logs for
   `shadow_would_deny`. If you see them on requests that should be
   allowed, you have a config bug (most likely a wrong limit value).
3. **Auth events** — register a new user. Watch the log line
   `credit granted via auth-event`. Confirm in billing-service that
   the org's `credit_balance` went up by the configured amount.

Useful queries during this stage:

```bash
# How many would-deny events did we have today?
grep shadow_would_deny logs/ | wc -l

# Which resources are tripping the most?
grep shadow_would_deny logs/ | jq '.resource' | sort | uniq -c

# Which users hit the limit?
grep shadow_would_deny logs/ | jq '.user_id' | sort | uniq
```

If anything looks wrong, fix the config and redeploy. Do not flip
enforcement off shadow mode until this is clean.

---

## Stage 8 — Flip enforcement on

In `quota-config.json`:

```json
"enforcement": {
  "enabled": true,
  "shadow_mode": false
}
```

Deploy. Watch the error rate, 429 count, and customer support volume
for 24 hours. The expected outcomes:

- 429s appear for free-tier users who exceed their cap (this is what
  you want)
- 429 body includes `upgrade_url` pointing at your billing page (test
  this — copy the URL from a real 429, paste in a browser)
- Paid-tier users do NOT see 429 (if they do, your subscription wiring
  isn't granting credits — check `q.Capabilities().CreditGrant` and
  recent ledger rows via `quotactl events --status success`)

If anything goes wrong, flip `shadow_mode: true` and redeploy. Takes
60 seconds; no data loss.

---

## Stage 9 — Wire dashboards + alerts

The library exports observability via:

1. **Structured logs** — every decision, every grant, every error
2. **Response headers** — `X-Quota-Used`, `X-Quota-Limit`,
   `X-Quota-Tier`, `X-Quota-Reason` on every response
3. **Capabilities snapshot** — log at startup, machine-readable via
   `q.Capabilities()`

What to dashboard:

| Metric | Source | Use it for |
|--------|--------|------------|
| 429 rate by tier | logs / response-status counter | spot free→paid pressure |
| `credit_granted` per minute | log filter | confirm webhooks firing |
| `shadow_would_deny` (if you keep shadow_mode for some tiers) | log filter | spot config drift |
| `handler_attempt_failed` | log filter | webhook handler errors |
| Engine error rate | log filter | depend on this for fail-open/closed decisions |

Alerts:

| Condition | Severity |
|-----------|----------|
| Capabilities log shows `Billing: false` for >5 min | page |
| Capabilities log shows `CreditGrant: false` for >5 min | page |
| `handler_attempt_failed` rate > 1/min for >10 min | page |
| Receiver returns 401 rate > 0.1% for >5 min | warn (HMAC mismatch) |
| Webhook 5xx rate > 1% for >5 min | warn |
| Spend close to monthly_cost cap for >5 free users | info — sales opportunity |

The library's webhook to your alerting system is configured via
`alerts.webhook_url` in the config. Set it to your Slack or PagerDuty
inbound webhook. The library SSRF-guards by default (no localhost, no
RFC1918 — flip a flag in code to allow if you really need to).

---

## Done. What you should have

- Free-tier users get N calls + $X credit; over-limit returns 429
- Paid-tier users pay $Y/month and get $Y of credit refilled monthly
- The 429 body has an `upgrade_url` linking to billing.
- The gateway service appears in ab0t-auth's subscription list
- Credit grants land in billing-service when users sign up / renew
- Dashboards show real numbers; alerts fire on real failures
- Total code: ~150 lines of Go + one JSON config

For ongoing operations:

- Add a tier? Edit JSON, redeploy.
- Change a limit? Edit JSON, redeploy.
- Add a resource? Edit JSON + add `Spend`/`Release` call at the
  business operation.
- Emergency stop? Set `enforcement.global_kill_switch: true`,
  redeploy. (Denies everything; use sparingly.)
- Customer dispute? `quotactl events --user $userid` shows their
  ledger.

---

## When you get stuck

- Capabilities says `WhyOff: <reason>` — read the reason. It's
  usually a missing env var.
- 401 on every webhook event — `AB0T_AUTH_WEBHOOK_SECRET` mismatch.
- 200 with `"status":"ignored"` — no handler registered for that
  event_type.
- 429 with `reason: tier_unresolved` — Identity returns no
  user/org, or tier provider can't resolve.
- 429 with `reason: tier_not_in_config` — your tier_provider
  returned an ID not in `config.tiers[]`. Common when a new tier
  exists in auth but you forgot to update the gateway's JSON.
- Same event fires the handler twice — handler is not idempotent;
  wrap it in `authevents.Idempotent(...)`.

See [ab0t-quota-go-setup](../Skills/ab0t-quota-go-setup/SKILL.md),
[ab0t-quota-go-middleware](../Skills/ab0t-quota-go-middleware/SKILL.md),
[ab0t-quota-go-auth-events](../Skills/ab0t-quota-go-auth-events/SKILL.md),
[ab0t-quota-go-cli](../Skills/ab0t-quota-go-cli/SKILL.md), and
[ab0t-quota-go-config](../Skills/ab0t-quota-go-config/SKILL.md) for
deeper detail on each surface.
