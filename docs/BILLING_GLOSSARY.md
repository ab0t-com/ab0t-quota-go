# Billing & Quota Glossary

Plain-English definitions of every term used in ab0t-quota-go docs.
If you've never built a billing system before, read this first.

---

## Core concepts

**Tier** — a named bucket of capabilities. The product page typically
shows 3–4: `free`, `starter`, `pro`, `enterprise`. Each tier has a
price (zero or non-zero) and a set of limits. A user is *on* a tier at
any moment; moving between tiers is an upgrade or downgrade.

**Limit** — a maximum value for one resource on one tier. "Pro users
can create up to 25 concurrent sandboxes" expresses a limit of 25 on
the `sandbox.concurrent` resource for the `pro` tier.

**Resource** — anything you meter. Three flavors:
- **Gauge** — "how many right now?" (concurrent sandboxes, open WebSocket connections)
- **Accumulator** — "how much this period?" (USD spend per month, API calls per hour)
- **Rate** — "how fast?" (requests per minute, sliding window)

**Cap** — synonym for limit. Used informally.

**Quota** — the enforcement of limits at runtime. "Quota engine" = the
component that decides Allow/Deny.

**Metering** — recording usage. Every spend / check counts toward a
counter. The act of metering is separate from the act of enforcing —
you can meter without enforcing (shadow mode) or enforce without
metering (feature gating).

---

## Money concepts

**Subscription** — recurring monthly/yearly charge. The user is on the
hook for it every period until they cancel. Stripe calls it
`subscription`. Predictable revenue for you.

**Consumption** — pay for what you use. Burns down as you use the
product. Stripe calls it `usage-based`. Predictable for the user.

**Hybrid** — both. "$29/month gives you $29 of usage credit; overage
billed at $0.001/call." This is the dominant pattern for infra
products.

**Credit** — pre-paid balance the user can spend. You can grant
credits to a user (signup gift, monthly subscription refill, promo
code), and they consume them on spend. Credits show as a positive
dollar number in the user's account.

**Balance** — total credit available to spend right now. In
ab0t-quota's billing service the balance has three buckets:
- `balance` — top-ups, refunds (real money the user paid)
- `credit_balance` — promo, signup, manual grants (non-revenue)
- `subscription_credit` — current month's subscription allowance

Spend drains them in a defined order (usually subscription_credit
first, so use-it-or-lose-it works).

**Credit grant** — the act of adding to one of the credit buckets. The
library has a `credit_grant` config block per tier that says **when**
to grant (signup, subscription_invoice_paid, manual), **how much**
(amount_per_period), and **what happens to leftovers** (lifecycle).

**Lifecycle** of a credit grant — what happens to unused credit at
each new grant:
- `persistent` — never expires (signup gift you keep forever)
- `use_it_or_lose_it` — resets to zero on next grant (typical monthly subscription)
- `rollover_capped` — leftover banks, but capped at N periods (enterprise-friendly)
- `rollover_unlimited` — leftover banks forever (rare)

**Top-up** — user explicitly buys credit (Stripe Checkout
`account_funding`). Distinct from a subscription charge.

**Reservation / commit** — for resources with uncertain cost (LLM
calls, sandbox sessions): reserve a worst-case amount, run the work,
commit the actual amount. Avoids overcharging when the cost is
unknown ahead of time. ab0t-quota wraps this in `BudgetChecker`.

**Refund** — return money to a user. Reverses a previous commit.

**Proration** — partial-period charging. "Upgrade mid-month, pay only
the rest of the month at the new rate." Stripe handles the math; the
quota lib mostly cares about the resulting tier change.

**Overage** — usage past the subscription's bundled allowance. Either
hard-stopped (deny when budget hits zero) or soft-billed (charge the
card on file for the excess).

---

## Subscription mechanics

**Plan** — Stripe's name for "this is what $29/month buys you." A
catalog entry that maps `pro` → `price_id_xyz`. Lives in the payment
service, NOT in ab0t-quota.

**Subscription** — Stripe's record of "this customer is on plan X
starting on date Y, renewing on date Z." Has a `subscription_id` and
state (`active`, `past_due`, `canceled`, `trialing`).

**Invoice** — the bill for one period. Stripe creates one per
subscription renewal. The `invoice.paid` webhook is the trigger for
"customer just paid you, top up their credit." This is the single
most important webhook to wire correctly.

**Checkout session** — the Stripe-hosted payment page. The user is
redirected there, pays, and Stripe redirects them back. The session_id
lives only for ~24h.

**Customer portal** — Stripe-hosted self-service: cancel, update card,
change plan. Don't build your own.

**Webhook** — Stripe POSTs to your endpoint when something happens
(invoice paid, subscription canceled, payment method updated). Signed
with HMAC. Your endpoint must return 200 fast or Stripe retries.

**Idempotency key** — a unique ID Stripe (and you) use to dedupe
operations. Stripe webhooks can fire twice; your handler must produce
the same outcome both times.

---

## Identity & scoping

**User** — one human (alice@acme.com). Identified by `user_id`.

**Org** — one team/company (acme.com). Identified by `org_id`.
Multiple users can belong to one org. **Billing is almost always
per-org**, even for products where the user experience is per-user.

**Tenant** — synonym for org in some contexts (Auth0, Cognito,
sometimes Stripe). ab0t treats `tenant_id` as a synonym for `org_id`
in event envelopes.

**Workspace** — sometimes used for "the billing org" specifically,
when users can belong to multiple orgs but get charged to one.

**Scope** — the granularity of metering. "Per-org" scope means a
team's usage pools. "Per-user" scope means each user has their own
counter.

**Pin store** — ab0t-quota's record of "this user's billing org is
X, and that's stable." Used so a user who signs up before joining
an org still gets credit consistently when they later join one.

---

## ab0t mesh & service architecture

**Service** — one HTTP API (auth, billing, payment, sandbox,
gateway, your-thing). They all run as independent processes.

**Mesh** — the network of services that authenticate each other via
API keys. The library calls "the mesh" when it talks to billing or
payment on your behalf.

**Consumer** — your service, from the mesh's POV. ab0t-quota-go runs
inside your service and makes mesh calls.

**Provider** — the upstream service you're calling. Billing-service is
a provider; payment-service is a provider.

**API key** — `AB0T_MESH_API_KEY` is your service's identity. It
authenticates you to billing, payment, etc.

**Bridge mode** — the lib calls the mesh HTTPS-only, no local state.
10–100ms per check. Easy to deploy, slower per-request. Not in v0.1.0.

**Engine-local mode** — the lib runs the quota engine in your
process with Redis state. <5ms per check. v0.1.0 default.

**Sidecar** — a separate process running alongside your service that
proxies mesh calls. Not used by ab0t-quota-go; mentioned for context.

---

## Operational terms

**Shadow mode** — "check but don't deny." The engine runs the math,
logs the would-be decision, and lets the request through. Use during
rollout to find false positives without breaking customers.

**Kill switch** — emergency disable. `enforcement.global_kill_switch:
true` makes the engine deny everything. Rare.

**Fail-open** — when the engine errors, default to allow. Use for
non-critical paths where breaking a customer is worse than letting
them overspend slightly.

**Fail-closed** — when the engine errors, default to deny. Use for
spend caps where overspend is worse than a temporary 503.

**Cooldown** — alert deduplication window. "Don't alert about the same
condition more than once per hour." Configured per alert manager.

**Ledger** — the durable log of "this handler attempted this event,
outcome was X." Used to make webhook handlers idempotent. ab0t-quota's
ledger has 5 states: `in_progress`, `success`, `skipped`, `failed`,
`failed_permanent`.

**Replay** — re-send an event through the receiver. Safe if handlers
are idempotent. The CLI's `quotactl replay` does this.

**Backfill** — synthesize events for users who existed before the
system was wired up. The CLI's `quotactl backfill` posts synthetic
signup events; the receiver dedups so already-credited users are no-ops.

**Capabilities snapshot** — at startup, the lib logs which subsystems
are wired (billing, payment, alerts, credit_grant) and which aren't
(and why). Read this first when something doesn't work.

---

## Common abbreviations

| Abbrev | Meaning |
|--------|---------|
| MRR | Monthly Recurring Revenue — total subscription revenue / month |
| ARR | Annual Recurring Revenue — MRR × 12 |
| CAC | Customer Acquisition Cost — what you spend to get one paying user |
| LTV | Lifetime Value — total revenue from one customer over their tenure |
| ARPU | Average Revenue Per User |
| CoGS | Cost of Goods Sold — your AWS bill, OpenAI bill, etc. |
| GM | Gross Margin — `(revenue - CoGS) / revenue`, expressed as % |
| TAM | Total Addressable Market — how big the opportunity is |
| PLG | Product-Led Growth — free tier acquires; product drives upgrades |
| SaaS | Software as a Service — the recurring-revenue product model |
| RBAC | Role-Based Access Control — separate from quotas (auth's job) |
| HMAC | Hash-based Message Authentication Code — webhook signatures |
| TTL | Time To Live — cache or key expiration |

---

## Things that look related but aren't

| | Different from | Because |
|--|----------------|---------|
| Quota | Rate limit | Quota is a budget; rate limit is anti-abuse. Both can be implemented with the same counters, but their failure modes differ. |
| Quota | Auth | Auth says "are you allowed to call this endpoint?" Quota says "and do you have budget to do it?" |
| Quota | Feature flag | Feature flag is boolean (have it / don't). Quota is quantitative (how much). |
| Tier | Plan | Tier is your internal label (`pro`). Plan is Stripe's catalog entry that maps to it. |
| Credit | Refund | Credit is non-real-money balance. Refund is real money returned. |
| Subscription | Top-up | Subscription is recurring. Top-up is one-time. |

Read [BILLING_MODELS_GUIDE.md](BILLING_MODELS_GUIDE.md) next.
