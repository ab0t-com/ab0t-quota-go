---
name: ab0t-quota
description: Master entrypoint for ab0t-quota-go — the Go SDK for ab0t-style services that need quota enforcement, billing-credit lifecycle, and tier-based limits. Use when the user mentions ab0t-quota / ab0t-quota-go / quota library at all, asks how to add billing to a Go service, says "I want to integrate quota / billing", asks "where do I start", asks "what is ab0t-quota?", asks "tell me about quota / billing for Go", asks "how do I test the integration?" or "how do I verify it works?", asks an underspecified billing or quota question that needs routing, OR mentions ANY of: `quota.Setup`, `q.Middleware`, `q.WebhookHandler`, `quotactl`, `quota-config.json`, `CreditGranter`, `Idempotent`, `OnAuthEvent`, `BillingModel`, `credit_grant`, `tier_provider`, `dedup_policy`, `shadow_mode`, `subscription_with_credits`, `subscription_credit`, `credit_balance`, `account_funding`. Routes the user to the right specialized skill or doc based on what they're trying to do.
---

# ab0t-quota-go — Start Here

A Go SDK for ab0t-style services that need quota enforcement, billing
credit lifecycle, and tier-based limits. One `Setup` call, one JSON
config, one HTTP guard, one webhook receiver — done.

## What you're looking at

**Library packages** (import via `go get github.com/ab0t-com/ab0t-quota-go`):
`quota`, `engine`, `middleware`, `authevents`, `handlerledger`,
`counters`, `providers`, `registry`, `messages`, `alerts`, `billing`,
`payment`, `mesh`.

**CLI binary**: `quotactl` — `go install github.com/ab0t-com/ab0t-quota-go/cmd/quotactl@latest`.

**Skill tree** (7 specialized + this one): mechanics, concepts, design,
runbook.

**Doc tree** (5 long-form): glossary, models, pipeline, runbook,
learning paths.

## Route by what the user is trying to do

| Need | Skill or doc |
|------|--------------|
| First contact — "I've never seen this lib" | this skill + [`docs/README.md`](../../docs/README.md) |
| "What do these words mean?" (tier, credit, MRR, etc.) | [`ab0t-quota-billing-101`](../ab0t-quota-billing-101/SKILL.md) + [`docs/BILLING_GLOSSARY.md`](../../docs/BILLING_GLOSSARY.md) |
| "How should I price my product?" | [`ab0t-quota-billing-design`](../ab0t-quota-billing-design/SKILL.md) |
| "Which billing model fits my product?" | [`docs/BILLING_MODELS_GUIDE.md`](../../docs/BILLING_MODELS_GUIDE.md) — 11 archetypes |
| "Walk me through integration top to bottom" | [`docs/INTEGRATION_RUNBOOK.md`](../../docs/INTEGRATION_RUNBOOK.md) — 9 stages, 1–2 days |
| "Where does the money actually move?" | [`docs/PAYMENT_PIPELINE.md`](../../docs/PAYMENT_PIPELINE.md) — four-service architecture |
| Install + `quota.Setup` + env vars + Capabilities | [`ab0t-quota-go-setup`](../ab0t-quota-go-setup/SKILL.md) |
| HTTP guard, Identity, Router, headers, 429 | [`ab0t-quota-go-middleware`](../ab0t-quota-go-middleware/SKILL.md) |
| Webhook receiver, `Idempotent`, `CreditGranter`, auto-subscribe | [`ab0t-quota-go-auth-events`](../ab0t-quota-go-auth-events/SKILL.md) |
| `quota-config.json` schema | [`ab0t-quota-go-config`](../ab0t-quota-go-config/SKILL.md) |
| `quotactl` admin CLI (replay, backfill, events, etc.) | [`ab0t-quota-go-cli`](../ab0t-quota-go-cli/SKILL.md) |
| **Test it works + troubleshoot when it doesn't** | [`ab0t-quota-go-testing`](../ab0t-quota-go-testing/SKILL.md) |

## When the request is underspecified

If the user says "help me with quota", ask them which of these applies:

1. **"I'm new and don't know what billing concepts mean"** → start at
   [`docs/BILLING_GLOSSARY.md`](../../docs/BILLING_GLOSSARY.md), then
   models guide, then runbook.
2. **"I know billing, I just need to wire the Go API"** → setup +
   middleware + auth-events skills, in order.
3. **"I'm designing the pricing layer"** → billing-design skill;
   sandbox-platform at
   `infra/code/resource/output/sandbox-platform/quota-config.json` is
   the reference example.
4. **"I'm running this in prod and something's broken"** → ask which
   surface: 429s? webhooks? credit grants? Then route to the matching
   skill's "Common errors" table.
5. **"I'm just curious"** → one-paragraph below, then point at
   `docs/README.md`.

## The one-paragraph explainer

ab0t-quota-go is a Go library you `go get` into any HTTP service. Once
wired (~150 lines of Go + one JSON config), every request is checked
against the user's tier limits, every relevant auth event drives a
credit grant through your billing service, and every webhook handler is
delivery-deduped + business-deduped + ledger-persisted so retries can't
double-spend. It pairs with three ab0t services (auth, billing,
payment) but never talks to Stripe directly — payment-service handles
that. Pricing is wire-compatible with the Python `ab0t-quota` library
v0.5.2, so a mixed Python/Go fleet can share Redis prefixes, dedup
keys, and HMAC secrets.

## Minimal example

```go
import "github.com/ab0t-com/ab0t-quota-go/quota"

q, err := quota.Setup(ctx, quota.Options{
    ConfigPath:    "quota-config.json",
    CreditGranter: myGranter{billing: yourBillingClient},
})
if err != nil { log.Fatal(err) }
defer q.Close(context.Background())

mux.Handle("/api/", q.Middleware(deps)(yourHandler))
mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())
```

That's the whole shape. Everything else is detail.

## Env vars at a glance

| Var | Purpose |
|-----|---------|
| `AB0T_QUOTA_BILLING_URL` | typed billing client target |
| `AB0T_QUOTA_PAYMENT_URL` | typed payment client target |
| `AB0T_QUOTA_SERVICE_TOKEN` | mesh bearer token |
| `AB0T_AUTH_AUTH_URL` | auth service URL for auto-subscribe |
| `AB0T_AUTH_ADMIN_TOKEN` | admin token for subscription POSTs |
| `AB0T_AUTH_WEBHOOK_PUBLIC_URL` | your service's public URL |
| `AB0T_AUTH_WEBHOOK_SECRET` | HMAC secret shared with auth |
| `AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS` | gate for `metered_billing` / `one_time_purchase` |

`Setup` reads these at startup; missing ones produce a `WhyOff` entry
in the Capabilities snapshot rather than a hard error.

## Capabilities — read this when things look wrong

`q.Capabilities()` returns a struct that tells you which subsystems are
wired. On startup, the lib emits one structured log line with the same
info. When integration questions come up, the answer often lives there:

```go
caps := q.Capabilities()
// caps.Engine, caps.Enforcement, caps.ShadowMode
// caps.Billing, caps.Payment
// caps.Alerts, caps.AlertsWebhook
// caps.AuthEvents, caps.CreditGrant, caps.AutoSubscribe
// caps.LedgerBackend (string: "memory" in v0.1.0)
// caps.FloatStore   (string: "memory" in v0.1.0)
// caps.WhyOff       (map[string]string explaining each off subsystem)
```

Or from a shell:

```bash
quotactl capabilities --config quota-config.json | jq .
```

If something looks off, `WhyOff` is the first place to look — every
"off" capability has a string explanation.

## Three things newcomers always get wrong

1. **Webhook secret mismatch** — `AB0T_AUTH_WEBHOOK_SECRET` must be the
   same value in your service env AND in the auth-side subscription.
   Symptom: every event returns 401.
2. **`CreditGranter` not supplied** — without one, the default
   credit-grant handler isn't registered. Symptom: capabilities log
   shows `CreditGrant: false` with `WhyOff["credit_grant"]: "no
   CreditGranter supplied"`.
3. **`shadow_mode` left on after rollout** — easy to forget. Symptom:
   no 429s ever fire even though counters are clearly over limit.

The full troubleshooting list lives in each skill's "Common errors"
table. Route by symptom.

## If the user is from the gateway team

This is the path:

1. [`docs/BILLING_GLOSSARY.md`](../../docs/BILLING_GLOSSARY.md) — terminology
2. [`docs/BILLING_MODELS_GUIDE.md`](../../docs/BILLING_MODELS_GUIDE.md) — pick the right archetype
3. [`docs/PAYMENT_PIPELINE.md`](../../docs/PAYMENT_PIPELINE.md) — see where money flows
4. [`docs/INTEGRATION_RUNBOOK.md`](../../docs/INTEGRATION_RUNBOOK.md) — 9 stages, follow top to bottom
5. The five mechanical skills as reference while wiring
6. [`ab0t-quota-go-testing`](../ab0t-quota-go-testing/SKILL.md) — verify each stage works before moving to the next
7. [`ab0t-quota-billing-design`](../ab0t-quota-billing-design/SKILL.md) when picking limits

Expected time to production: 1–2 engineering days end-to-end with
shadow mode for a calendar day in the middle. Use the testing skill
**throughout** — not just at the end. Every stage of the runbook has a
verification step that the testing skill spells out.

## Reference integration

The most mature in-tree example is **sandbox-platform** at
`infra/code/resource/output/sandbox-platform/`. Read its
`quota-config.json` to see a working 4-tier hybrid model (free /
starter $29 / pro $99 / enterprise $499) with concurrency gauges, GPU
gating, monthly cost accumulators, and credit grants. It's a Python
integration but the JSON is the same on both runtimes.

## Versioning + distribution

- Pure-source library: `go get github.com/ab0t-com/ab0t-quota-go@v0.1.0`
- CLI binary: `go install .../cmd/quotactl@v0.1.0` OR download from GitHub releases
- The git tag IS the release — there's no separate publish step
- Semver respected; breaking changes go in new majors

See [`CONSUMING.md`](../../CONSUMING.md) at the repo root for the
full install matrix (both paths, including SHA256 verification).
