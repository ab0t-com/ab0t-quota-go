---
name: ab0t-quota-go-design
description: The thinking process and mental model for standing up billing + payment + quota on a service — the ordered decisions a team makes, from "I want to charge for my API" to a live tiered quota + Stripe checkout. Use when a team is confused about how quota vs billing vs payment relate, asks "where do I even start / what do I decide first / what's the order", wants the big-picture mental model or "what does ab0t do for me vs what do I decide", needs the end-to-end decision sequence (what to meter → tiers → reset periods → enforcement mode → free vs paid → credits vs subscription → mint billing+payment consumer accounts → write quota-config.json → wire Setup+middleware+webhooks → shadow-test → go live), or wants a worked example that walks intent → live. This is the CONDUCTOR / decision-spine skill; it does not re-teach mechanics — it routes into ab0t-quota-billing-101 (vocabulary), ab0t-quota-billing-design (pricing economics), billing-payment-consumer-setup (minting the mesh accounts), and the ab0t-quota-go-* mechanics skills for execution. Not the router hub (ab0t-quota), and broader than pricing-only (ab0t-quota-billing-design).
---

# Designing a billing + payment + quota system

This skill is the **decision spine**. When someone says "I want to charge
for my service" and doesn't know where to start, walk them through the
mental model, then the ordered decisions, then point at the skill that
executes each one. Do **not** re-explain mechanics here — every gate ends
with a link to the skill that owns it.

The reason teams get confused: this is three products wearing one hat.
Untangle them first, then the order becomes obvious.

## The mental model — three layers, one job

You are metering usage → gating it by tier → charging for more. Three
independent layers do that, and only ONE of them lives in your code.

| Layer | Question it answers | Where it runs | You touch it via |
|-------|--------------------|--------------|------------------|
| **Quota** | "Is this request within the user's limit?" | **In your service** | `ab0t-quota-go` library (Setup + Middleware + webhook) |
| **Billing** | "How much money/credit does this org have?" | ab0t **billing-service** (a ledger) | HTTP with an `X-API-Key` (or the lib's typed client) |
| **Payment** | "Charge the card." | ab0t **payment-service** (a Stripe wrapper) | HTTP with an `X-API-Key` |

**Money only flows one direction, and the library is the LAST hop:**

```
Stripe  →  payment-service  →  billing-service  →  auth event  →  your service (ab0t-quota-go)
(card)     (charges card)      (credits ledger)   (broadcasts)    (grants credit, enforces limits)
```

Your library **never calls Stripe.** It receives the downstream effects
as auth webhooks. If a customer says "I paid but got no credit," you
debug that chain right-to-left — the library is where it ends, not
starts. (Full walk: `docs/PAYMENT_PIPELINE.md`.)

### What YOU decide vs what ab0t does FOR you

This table is the whole point. Confusion comes from thinking you must
build the right column.

| You decide (policy) | ab0t runs (plumbing) |
|--------------------|----------------------|
| What to meter + limits per tier | The counters, Redis, per-request check, 429 |
| Tier prices + billing model | Stripe checkout, subscription lifecycle, card retries |
| Credit-grant rules (how much, when, expiry) | Delivery-deduped, ledgered, retried grant execution |
| Free-vs-paid, enforce-vs-shadow | Webhook receipt, HMAC verify, idempotency |
| The `quota-config.json` (one JSON file) | Everything below the config |

You write ~1 JSON file + ~150 lines of Go glue. You do **not** write
Stripe code, a ledger, a webhook deduper, or a rate limiter. That's the
"large product add" the platform absorbs for you.

For vocabulary (tier, credit_balance vs subscription_credit, MRR,
accumulator, idempotency), send them to `ab0t-quota-billing-101` first —
don't teach terms inline here.

## The decision sequence

Decisions **1–6 happen on paper, before any JSON or code.** Steps 7–11
are execution, in order, each behind its own skill. Doing 7 before 1–6 is
the #1 way teams stall — you can't mint accounts or write config for a
pricing model you haven't chosen.

### 1. Do you even need quota? · gate
If the question is "is this allowed at all?" that's **auth**, not quota.
Quota is a **budget** ("how much?"), not a gate ("may I?"). Anti-DDoS →
WAF/rate-limiter. Feature flags → a flag service. See the "When NOT to
use quota" table in `ab0t-quota-billing-design`. If you're metering a
cost or selling capacity, continue.

### 2. What do you meter? · resources + counter_type
Open the vendor/AWS bill; the **biggest line is your primary resource.**
Meter cost drivers, **not "requests"** by default. For each resource pick
a `counter_type`:

| Intent | `counter_type` |
|--------|---------------|
| "How many right now?" (concurrency, seats, live sessions) | `gauge` |
| "How much this period?" (spend, calls/month, tokens/day) | `accumulator` (needs `reset_period`) |
| "How fast?" (fairness, abuse) | `rate` (needs `window_seconds`) |

Depth: `ab0t-quota-billing-design` §"What costs you money" + resource
taxonomy. Schema: `ab0t-quota-go-config` §resources.

### 3. What are the tiers? · the ladder
Name the buckets a user can be "on." The dominant shape is a 4-rung
ladder: **free / low / high / enterprise**, each a distinct buyer mode
(try / side-project / team / contract). Set each rung's **limit per
resource** from step 2. Gate **quantity, not features** — feature gates
breed churn; quantity gates breed upgrades. Depth: `ab0t-quota-billing-design`
§tier psychology. Archetype catalog: `docs/BILLING_MODELS_GUIDE.md`.

### 4. Reset periods & scope · counter semantics
For each `accumulator`, pick `reset_period` (hourly/daily/weekly/monthly)
— usually monthly to match the billing period. `gauge`s never reset
(they move on Spend/Release). `rate`s use a sliding `window_seconds`.
Then pick the **identity scope**: does the counter increment on
`org:acme` (team pools quota — B2B default) or `user:alice` (personal
quota)? This also sets your **dedup policy** for credit grants:
`per_org_per_tier` (B2B) vs `per_user_per_tier` (B2C, guards signup
farming). Depth: `ab0t-quota-billing-design` §identity model + anti-farming.

### 5. Free vs paid, and the billing model · the money shape
For each paid tier choose a `billing_model`:

| Shape | `billing_model` | Use when |
|-------|----------------|----------|
| Flat fee unlocks bigger limits, no credits | `subscription_unlock_only` | simple capacity SaaS |
| Flat fee + monthly credit refill (hybrid) | `subscription_with_credits` | infra products (usual winner) |
| Pure pay-as-you-go | `consumption_only` | cost varies wildly per customer |

**Credits vs subscription** is the fork most teams fumble: a
*subscription* is "pay for access/limits"; *credits* are "prepaid
balance you burn per use." Hybrid (`subscription_with_credits`) gives you
predictable MRR **and** fair scaling — the flat fee is your revenue line,
the credit is the usage allowance that refills each renewal. Then design
the **free-tier gift** (a `credit_grant` with `trigger: signup`, gated on
email verification) and the **cost-cap safety valve** (an accumulator cap
per tier so a runaway job can't bankrupt you). Depth:
`docs/BILLING_MODELS_GUIDE.md` (11 archetypes A–K), `ab0t-quota-billing-design`
§free tier + cost-cap.

### 6. Enforcement mode · block vs allow-over, and shadow first
- **Fail-closed** (deny on engine error) at creation/spend gates — a
  silent bypass means overspend.
- **Fail-open + loud log** at lifecycle/release hooks — failing to
  decrement leaks quota but shouldn't block shutdown.
- **Ship with `enforcement.shadow_mode: true` first.** Shadow logs
  would-deny events without blocking anyone. Watch a calendar day, fix
  false positives, *then* flip to enforce.
Depth: `ab0t-quota-go-middleware` (fail-open/closed) + `ab0t-quota-go-config`
(shadow_mode).

---
Decisions locked. Now **build**, in this order:

### 7. Mint the billing + payment consumer accounts · accounts FIRST
Your service needs its own scoped `X-API-Key` to call billing and
payment on its users' behalf. This is a mesh step, **not** part of the Go
library, and it's the piece the mechanics skills assume already exists.
Do it before writing config/code. Run `authsetup run 07` per
**`billing-payment-consumer-setup`** (billing needs `cross_tenant`,
payment needs `cross_org`). End-to-end copy-paste path incl. dry-run,
membership-gap gotcha, and env wiring:
`RUNBOOK_connect_service_to_billing_payment_quota_20260703.md`.

### 8. Write `quota-config.json` · encode decisions 2–6
One JSON file: `resources`, `tiers` (with `limits`, `price`,
`billing_model`, `credit_grant`), `tier_provider` (usually `mesh` — tier
comes from billing), `enforcement`. This is the source of truth; the
library reads it at startup. Skill: **`ab0t-quota-go-config`**. Copy an
archetype from `docs/BILLING_MODELS_GUIDE.md` and adapt.

### 9. Wire the library · Setup + middleware + webhooks
~150 lines of Go:
```go
q, _ := quota.Setup(ctx, quota.Options{
    ConfigPath:    "quota-config.json",
    CreditGranter: myGranter{billing: billingClient}, // grants credit on auth events, idempotently
})
mux.Handle("/api/", q.Middleware(deps)(handler))                      // per-request enforcement → 429
mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())  // receives auth/billing events
```
Skills: **`ab0t-quota-go-setup`** (Setup + env vars + Capabilities),
**`ab0t-quota-go-middleware`** (the guard), **`ab0t-quota-go-auth-events`**
(webhook receiver + CreditGranter + Idempotent). For a Python service,
swap the library for the drop-in clients per `billing-payment-integration`.

### 10. Shadow-test · prove it before enforcing
With `shadow_mode: true`, drive real traffic and read would-deny logs.
Verify: checkout returns a Stripe URL; a test-mode payment flows Stripe →
payment → billing → auth event → your CreditGranter → `subscription_credit`
bucket; guarded routes would-deny past the limit. Skill:
**`ab0t-quota-go-testing`**; stage-by-stage: `docs/INTEGRATION_RUNBOOK.md`
§Stage 7. Inspect wiring anytime with `quotactl capabilities` (**`ab0t-quota-go-cli`**).

### 11. Flip enforcement on · go live
Set `shadow_mode: false`, redeploy, watch 429 rate + credit-grant
success. `docs/INTEGRATION_RUNBOOK.md` §Stage 8–9 (enforce + dashboards).

## Worked example — "I want to charge for my summarization API"

A team runs `POST /summarize` (calls a paid LLM). Walking the gates:

1. **Need quota?** Yes — the LLM tokens cost real money; cap them.
2. **Meter what?** The bill is dominated by LLM tokens → primary resource
   `summarize.tokens`, `counter_type: accumulator`. Add `summarize.requests`
   (`rate`, `window_seconds: 60`) purely for abuse fairness.
3. **Tiers?** `free / pro / enterprise`. Gate quantity: free = 50k
   tokens/mo, pro = 5M, enterprise = unlimited. Same feature set on all.
4. **Reset & scope?** `summarize.tokens` resets `monthly`. Scope on
   `org` (teams share a pool). Dedup `per_org_per_tier`.
5. **Money shape?** Pro is `subscription_with_credits`: $29/mo, refills
   $25 of credit each renewal. Free gets a `signup` credit grant of $1,
   email-verification gated. Cost cap: `spend.usd` accumulator, free=$1,
   pro=$100 — the runaway-job safety valve.
6. **Enforce?** Fail-closed at `/summarize`; ship shadow_mode first.

Resulting `quota-config.json` (abbreviated — schema in `ab0t-quota-go-config`):
```json
{
  "service_name": "summarizer",
  "tier_provider": { "type": "mesh", "default_tier": "free" },
  "enforcement": { "enabled": true, "shadow_mode": true },
  "resources": [
    { "service": "summarizer", "resource_key": "summarize.tokens",
      "counter_type": "accumulator", "reset_period": "monthly" },
    { "service": "summarizer", "resource_key": "spend.usd",
      "counter_type": "accumulator", "reset_period": "monthly", "precision": 2 }
  ],
  "tiers": [
    { "tier_id": "free", "display_name": "Free", "sort_order": 1,
      "billing_model": "free_tier",
      "limits": { "summarize.tokens": 50000, "spend.usd": { "limit": 1.00 } },
      "credit_grant": { "trigger": "signup", "amount_per_period": "1.00",
        "destination": "credit_balance", "lifecycle": "persistent",
        "dedup": "per_user_per_tier" } },
    { "tier_id": "pro", "display_name": "Pro", "sort_order": 2,
      "billing_model": "subscription_with_credits",
      "price": { "amount_per_period": "29.00", "currency": "USD", "period": "month" },
      "credit_grant": { "trigger": "subscription_invoice_paid",
        "amount_per_period": "25.00", "destination": "subscription_credit",
        "lifecycle": "use_it_or_lose_it", "dedup": "per_org_per_tier",
        "reset_on_downgrade": true },
      "limits": { "summarize.tokens": 5000000, "spend.usd": { "limit": 100.00 } } },
    { "tier_id": "enterprise", "display_name": "Enterprise", "sort_order": 3,
      "billing_model": "enterprise",
      "limits": { "summarize.tokens": null, "spend.usd": null } }
  ]
}
```

**Then:** mint accounts (step 7 → `billing-payment-consumer-setup`), drop
that JSON in (step 8), wire Setup + a `Middleware` on `/summarize` that
Spends `summarize.tokens` after each call + a `CreditGranter` (step 9),
shadow-test a Stripe test checkout (step 10), flip enforce (step 11).
Intent → live, no Stripe code written.

## Mental-model traps (why teams get confused)

| Trap | Correction |
|------|-----------|
| "Quota holds the money" | No — **billing-service** holds money; quota holds *limits*. |
| "I need to write Stripe code" | No — **payment-service** owns Stripe; you never see a card. |
| "Meter requests" | Meter **cost drivers** (tokens, GPU-hours, seats). Requests rarely drive cost. |
| "Subscription == credits" | Subscription = pay for access; credits = prepaid burn. Hybrid uses both. |
| "Write the JSON first" | Decide 1–6 on paper first; JSON just encodes the decisions. |
| "Mint accounts last" | Accounts (step 7) come **before** config/code — the lib assumes the key exists. |
| "Enforce on day one" | Shadow first; enforcing untested config denies real customers. |

## The chain at a glance

```
Decide (paper)                    Build (in order)
1 need quota?      ─┐   7 mint billing+payment accounts  → billing-payment-consumer-setup
2 what to meter    ─┤   8 write quota-config.json         → ab0t-quota-go-config
3 tiers            ─┼─▶ 9 wire Setup+middleware+webhooks  → ab0t-quota-go-{setup,middleware,auth-events}
4 reset & scope    ─┤  10 shadow-test                     → ab0t-quota-go-testing
5 money shape      ─┤  11 flip enforcement / dashboards   → docs/INTEGRATION_RUNBOOK.md §8–9
6 enforce mode     ─┘
        │                        │
   ab0t-quota-billing-design     library never calls Stripe; it's the last hop
   ab0t-quota-billing-101 (terms)
```

Route pricing depth → `ab0t-quota-billing-design`; vocabulary →
`ab0t-quota-billing-101`; execution → the `ab0t-quota-go-*` mechanics
skills; the hub router `ab0t-quota` reaches any of them.
