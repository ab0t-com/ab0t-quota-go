---
name: ab0t-quota-billing-design
description: Reason about pricing, quotas, and metering BEFORE writing ab0t-quota config. Use when designing a new product's billing model, picking tier prices and limits, deciding what to meter (concurrency vs throughput vs spend), choosing between feature gating and quantity gating, modeling free-tier economics, designing credit-grant economics, deciding when to fail-open vs fail-closed, picking dedup policy for B2C vs B2B, debugging "our pricing isn't working" / "users are gaming the free tier" / "we're losing money on big customers", or porting an existing manual rate-limit / cost-cap into the quota system.
---

# Pricing & Quota Design for ab0t-quota

This skill is about **what to put in `quota-config.json` and why**, not how
to call the API. The other skills handle mechanics; this one handles
judgment.

## The frame: pricing is engineering

A pricing page is the public API of your business model. Once shipped,
it constrains every product decision downstream — what features you
build, who you sell to, how you scale costs, what your dashboards
report. Treating it as marketing copy ("$29/mo, unlimited everything!")
produces silent bankruptcies. Treating it as engineering — with SLOs,
failure modes, instrumentation, and a budget — produces a product that
funds itself.

The quota layer is where that engineering lives at runtime.

## Four decisions to make before you write JSON

### 1. What costs you money?

Open your AWS bill. Sort by line item. The biggest line is what you
must meter. Don't meter what's free; don't fail to meter what isn't.

For sandbox-platform: EC2-hours and GPU-hours dominate. So they meter
**concurrent sandboxes** (proxy for EC2-hours) and **GPU instances**
(GPU-hours) explicitly. Storage, network egress, support are smaller
lines — not metered at the tier level, absorbed into the price.

If the top line is **API calls to a paid LLM**, meter tokens or call
count. If it's **storage**, meter GB-months. If it's **per-seat SaaS
overhead** (a CRM seat, an SSO connection), meter seats. **Don't meter
"requests" by default** — they're rarely the cost driver.

### 2. What creates value differentiation?

Different from "what costs money." Slack meters per-seat because the
value is per-user, not per-message. Datadog meters per-host because
that's where you feel scale. Stripe meters per-transaction because
that's the revenue moment.

Ask: *what makes a customer say "we need to upgrade"?* That's the
resource you cap. For sandbox-platform that's **concurrent sandboxes** —
when a team hits the cap, they upgrade. When they hit GPU limits, they
upgrade. When they hit a monthly cost cap, they don't churn — they pay
more.

### 3. Subscription, consumption, or both?

| Model | What it is | When right | Sandbox-platform analog |
|-------|-----------|------------|-------------------------|
| Pure consumption | Pay only for what you use | Cost varies wildly per customer (AWS, OpenAI API) | the per-minute backbone |
| Pure subscription | Flat monthly fee, fixed quota | Predictable cost, low variance per customer (Notion) | the $29/$99/$499 floors |
| Hybrid (commitment + credits) | Subscribe to a tier, get credits, overage billed | High variance + want predictable revenue | this is what sandbox does |

Hybrid is almost always the right answer for infra products. The
**subscription is your commitment line on the income statement**
(predictable MRR); the **consumption is your safety valve** (high-volume
users pay more, low-volume users still feel they got value).

ab0t-quota's `credit_grant` is the hybrid primitive: `trigger:
subscription_invoice_paid` + `amount_per_period` = "every renewal, top
up the user's prepaid balance to N." That credit is then burned by
consumption events.

### 4. What's your cost-cap safety valve?

Every consumption product needs an answer to: *"a customer accidentally
runs a $50,000 job overnight, what happens?"* If the answer is "we eat
it" or "we email them apologetically", you're not done.

ab0t-quota's **accumulator counter** is the lever. Sandbox-platform
declares `sandbox.monthly_cost` as a `counter_type: accumulator,
reset_period: monthly` and gives each tier a cap: free=$10,
starter=$100, pro=$1000, enterprise=unlimited. A background cost
manager ticks hourly and increments. When the accumulator hits the cap,
new creates are denied.

This is not a feature, it's an SLO: *"no free-tier user can incur more
than $10 of cost in a calendar month."* Write it in your runbook. It's
also a sales message: enterprise unlimited is a tangible perk.

## Resource taxonomy — pick the right counter_type

| `counter_type` | Question it answers | Good for |
|----------------|---------------------|----------|
| `gauge` | "How many right now?" | Concurrency limits, open seats, live sessions |
| `accumulator` | "How much this period?" | Spend caps, monthly API quota, hourly token allowance |
| `rate` | "How fast?" | DDoS guard, fairness, fan-out throttle |

Common mistake: using `rate` for what should be `accumulator`. "Max 100
requests per hour" is two different things depending on intent:

- Anti-abuse → `rate`, `window_seconds: 3600` (sliding)
- Quota allowance → `accumulator`, `reset_period: hourly` (resets on the hour)

The behavior diverges at the boundary. Pick deliberately.

## Resource bundles — meter the umbrella concern

Sandbox-platform defines bundles like:

```json
"resource_bundles": {
  "sandbox":      ["sandbox.concurrent"],
  "sandbox_gpu":  ["sandbox.concurrent", "sandbox.gpu_instances"]
}
```

The middleware can `check_for_bundle("sandbox_gpu")` to atomically
verify a request fits BOTH caps. Without bundles, you'd have N
sequential checks with race conditions between them.

Use a bundle whenever an operation consumes multiple resources at once.
"Start a GPU sandbox" should not be allowed if the user has GPU budget
but no concurrency budget.

## Free tier — teaser, not gift

The job of the free tier is **lead generation**, not customer service.
Optimize for it.

Bad free tier:
- Unlimited usage, throttled to "best effort"
- Full feature set, limited only by a fairness limit
- No card required, no cap

Good free tier (the sandbox pattern):
- **Hard limit on quantity** (1 sandbox, 0 GPUs, 2 browsers)
- **Hard cap on monthly cost** ($10 — enough to try, not enough to
  abuse)
- **Time-bounded credit grant** (one-time $10 on email-verified signup,
  per_user_per_tier dedup so multi-account farming is detected)
- Full feature access — **gate quantity, not features**

Gating features (e.g. "free users can't use custom domains") creates
permanent reasons to be unhappy. Gating quantity creates a clear
"upgrade when you need more" prompt. The former breeds churn; the
latter breeds revenue.

### Anti-farming: dedup policy choice

| Policy | Use when |
|--------|----------|
| `per_user_per_tier` (default) | B2C — credit follows the human; signup-farming is the threat |
| `per_org_per_tier` | B2B — credit follows the org; users come and go |
| `per_user_global` | Onboarding gift that follows the human across tiers |
| `per_org_global` | Migration / one-time enterprise credit |

For sandbox-platform: `per_user_per_tier` plus
`REQUIRE_EMAIL_VERIFICATION_FOR_PROMO=true`. A determined attacker
can still farm, but the cost of a verified email + an unblocked
provider raises their break-even above $10.

## Tier psychology — what each tier signals

A four-tier ladder is the dominant pattern because it covers four
distinct buyer modes:

| Tier | Buyer says | What they're really buying |
|------|-----------|----------------------------|
| Free | "I want to try" | A reason to come back tomorrow |
| Mid-low ($29) | "I'm using this for a side project" | Removal of trial friction |
| Mid-high ($99) | "This is now a team thing" | Permission to scale |
| Enterprise ($499+) | "We need a contract" | A salesperson's phone number |

Notice what each tier **gives** in sandbox-platform:

- Starter ($29): 5x concurrency, the FIRST GPU, **subscription credits
  that refresh monthly** — the "I'm a real user" signal.
- Pro ($99): 5x more, 5 GPUs, ten times the monthly cap — the "I have a
  team" signal.
- Enterprise ($499): unlimited concurrency, **3-period credit rollover**
  — the "we don't want to think about it" signal. Rollover is a small
  technical feature with a huge sales-pitch payoff.

The price gaps (3x, 3.4x, 5x) follow the "good/better/best" rule:
**enterprise must feel ~5x bigger** than the second-from-top, or the
sales pitch collapses ("why not just buy two Pros?").

## Failure modes — fail-open vs fail-closed

This is a runtime decision, not a config decision. But it's a pricing
decision in disguise.

| Path | Default | Why |
|------|---------|-----|
| Pre-flight check at creation | **fail-closed** (deny if engine errors) | A 503 here is a customer waiting; a silent bypass here is a customer overspending. |
| Lifecycle hook on resource exit | **fail-open + loud log** | If we fail to decrement, we leak quota — annoying but not catastrophic. Better to log + page than block shutdown. |
| Health checks, `/metrics` | **exempt entirely** | Never gate observability. |
| Auth-event webhook handlers | **idempotent + ledger** | Auth retries on non-200; rely on dedup, not silence. |

Sandbox-platform encodes this exactly: `check_sandbox_creation`
raises 429 on engine error (fail-closed); `on_sandbox_terminated`
logs and proceeds on error (fail-open).

## Identity model — user, org, or both

The metering scope question: when alice@acme.com starts a sandbox, what
counter increments?

- **`org:acme`** — the team pays; users come and go; one cap pools
  across all teammates. (Sandbox-platform default.)
- **`user:alice`** — alice pays; her team can't borrow her quota.
- **Both** — increment org for billing AND user for fairness ("no one
  user can hog the team's GPU pool").

The Identity callback in middleware returns `(userID, orgID)`. The
engine uses `orgID` as the primary scope and `userID` for per-user
fairness limits. `default_per_user_fraction: 0.4` on a tier means each
user gets ceil(40% × tier-cap) — useful for "5 users, 10-sandbox limit,
no one user can pin 8 of them."

### PinStore — the user→org stable map

When `auth.user.registered` fires, the user has no org yet. When
`org.created` fires, the org has no users yet. The default
credit-grant handler needs both. The **PinStore** records "the first
billing org observed for this user" and keeps it stable forever —
operator overrides win, auto-mappings stick. This avoids the bug where
a user signs up, gets credit, joins a different org, and gets credit
*again*.

Use it when your auth model has user-before-org-before-billing-org
sequencing. Skip it if every user has an immediate billing org.

## When NOT to use quota

| Use case | Right tool |
|----------|-----------|
| Anti-DDoS / abuse | A real rate limiter or WAF; quota is overkill |
| Authn / authz | The auth service; quota assumes you're already authed |
| Per-request circuit-breaker | A circuit breaker library; quota doesn't model failures |
| Feature flags | LaunchDarkly / GrowthBook; quota gates quantity, not boolean access |
| Streaming throttle | Token bucket in the streaming layer; quota's rate counter is too coarse |

Quota is a **budget**, not a gate. If the question is "is this allowed
at all?" → auth. If it's "how much?" → quota.

## The audit — questions to answer before shipping

For every quota-config.json before it goes to prod:

1. **Where does the money go?** Map every resource_key to a line on the
   cost spreadsheet. If a metered resource has no cost, why is it
   metered? If a cost line has no resource, what stops a bad actor from
   running it up?
2. **What does the free tier cost us per signup?** Multiply the cap by
   your unit cost. If it's >$1 and you don't have a CAC budget, tighten
   the cap.
3. **What's the per-tier gross margin?** For each paid tier, compute
   `subscription_revenue - (max_consumption × unit_cost)`. If negative
   at the cap, you're selling at a loss to the heaviest user. Either
   raise the price or lower the cap.
4. **What happens when Redis is down?** With v0.1.0 the answer is "in-
   memory store, counters reset on restart, capabilities log warns."
   Decide if that's acceptable. (For sandbox-platform: yes, because the
   cost cap is enforced by the hourly cost_manager tick, not by
   per-request increments.)
5. **What's our worst-case month?** Pick the heaviest user, assume
   they max out, multiply by 30 days. Can you absorb it?
6. **Do we have shadow mode?** Set `enforcement.shadow_mode=true`
   before the first day of enforcement. Watch logs for would-deny
   events. Fix the false positives. Then flip to enforce.

## Sandbox-platform case study

A complete pricing engine reads like this:

```
4 tiers: free / starter / pro / enterprise
5 resources: sandbox.concurrent, sandbox.gpu_instances,
             sandbox.browser_sessions, sandbox.desktop_sessions,
             sandbox.monthly_cost
4 bundles:   sandbox, sandbox_gpu, browser, desktop
1 credit-grant pattern: per_user_per_tier with email_verified gate
1 cost cap: monthly_cost accumulator, tier-scoped
1 lifecycle hook pair per resource (create increments, terminate decrements)
1 auth-event handler: grant initial credit on signup
1 background job: hourly cost_manager ticks accumulator
```

Read that as a checklist for your product. If you're missing the cost
cap, add one. If you're missing the lifecycle pair, you'll leak quota.
If you're missing the credit-grant signup hook, your free tier has no
delight.

## Quick decision checklist

When the team asks "what should we put in the config?", walk this:

- [ ] What's the largest line on the AWS / vendor bill? → primary resource
- [ ] What's the second largest? → secondary resource (often premium feature)
- [ ] What's the upgrade trigger? → gauge limit on that resource per tier
- [ ] What's the cost cap per tier? → accumulator with `reset_period: monthly`
- [ ] What's the free signup gift? → `credit_grant.trigger: signup`, gated
- [ ] What's the enterprise differentiator? → `lifecycle: rollover_capped`
- [ ] What about expensive-feature flags? → DON'T — gate quantity, not features
- [ ] What's the failure mode if our metering layer dies? → cost cap + manual reconciliation

Then run `quotactl capabilities --config quota-config.json` and read
the `WhyOff` map — anything missing is a thing to decide deliberately,
not silently.
