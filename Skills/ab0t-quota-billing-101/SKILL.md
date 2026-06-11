---
name: ab0t-quota-billing-101
description: Teach billing & quota concepts from zero. Use when a developer asks "what's a tier?", "what's the difference between credit_balance and subscription_credit?", "how does Stripe fit in?", "what's a webhook for?", "what does idempotent mean here?", "prepaid vs postpaid?", "what's MRR / ARR / CAC?", "consumption vs subscription?", "why three balance buckets?", or any other conceptual question that a textbook would answer but the codebase won't. Distinct from `ab0t-quota-billing-design` (which is about HOW to choose); this skill is about WHAT THINGS ARE.
---

# Billing & Quota 101

This skill is the friendly explainer. When someone asks a conceptual
question, work through it before reaching for code.

## How to use this skill

When you trigger, **don't dump everything**. Identify the specific
concept the user is asking about and answer that one. Cross-reference
the deeper docs when the user wants more.

The full reference is at
`docs/BILLING_GLOSSARY.md` — point users there for lookup-style
queries; this skill is for "explain it to me like I'm new" answers.

## The five things to teach in order

A new developer should learn in this order:

### 1. The product question — "what does this user pay for?"

Three answers exist:

- **Subscription** — recurring fee for access ("$10/month for Pro").
  Predictable revenue. Example: Notion, Slack.
- **Consumption** — pay for what you use ("$0.001 per API call").
  Predictable for the user. Example: AWS, Anthropic API.
- **Hybrid** — both ("$10/month gets you $10 of usage; overage
  billed"). Dominant pattern for infra products.

Most infra products land on hybrid because subscription gives you
predictable MRR and consumption gives the customer fair pricing as
they scale.

### 2. The mechanism — "how does the money move?"

Four services collaborate:

```
Stripe (the card)
  → payment-service (Stripe wrapper, talks to card networks)
     → billing-service (the ledger; three balance buckets)
        → auth-service (broadcasts the tier change)
           → your service (ab0t-quota-go runs here)
```

Each hop sends webhooks. Each hop is independent. The ab0t-quota-go
library is in the rightmost box; it never talks to Stripe directly.

### 3. The bucket model — "why three balances?"

billing-service tracks each org's money in **three buckets**, drained
in priority order:

| Bucket | Source | Why it exists |
|--------|--------|---------------|
| `subscription_credit` | this month's subscription allowance | use-it-or-lose-it — encourages active use |
| `credit_balance` | signup gifts, promos, manual admin grants | non-revenue gifts; lifecycle varies |
| `balance` | top-ups (real money the user paid) | refundable; never expires |

Drain order: subscription_credit first, then credit_balance, then
balance. Why? Because subscription_credit expires at month-end, so
spending it first means the user actually USES what they're paying
for. Their real money stays as long as they want.

This is the most surprising part of the model for newcomers. Walk
through an example: "User pays $10/mo, also has $20 of leftover
top-up. They spend $5. What happens?" Answer: `subscription_credit`
drops to $5; `balance` stays at $20. At renewal, `subscription_credit`
resets to $10 ($5 forfeit). Balance is untouched.

### 4. The tier model — "how does the system know what you can do?"

A **tier** is a named bucket. Each user is "on" a tier at any moment.
The tier maps to:
- **A price** ($0 for free, $X/mo for paid tiers)
- **Limits** (numerical caps on each metered resource)
- **A `credit_grant` rule** (how much credit to add when, with what
  lifecycle)
- **A `billing_model`** (subscription, consumption, hybrid)

The tier provider — typically the auth service mesh — is the source
of truth for "what tier is this user on right now?" The library asks
it on each request (with a TTL cache).

When a user upgrades:
1. Stripe → payment-service: subscription created
2. payment-service → billing-service: tier set
3. billing-service → auth event bus: tier change broadcast
4. your service receives the event, runs the credit-grant handler
5. credit lands in `subscription_credit` bucket

The library handles steps 4–5. Steps 1–3 are someone else's
responsibility.

### 5. The webhook model — "what's idempotency, why do I care?"

A webhook is "service A POSTs to service B when something happens."
Stripe sends webhooks when invoices are paid. Auth sends webhooks when
users are created. Your service receives them.

**Webhooks fire twice.** Always. Network retries, ambiguous timeouts,
upstream bugs. If your handler is naive ("grant credit on every
delivery"), you'll grant the same credit twice.

**Idempotency** = your handler produces the same outcome whether it
runs once or twice. ab0t-quota-go's `Idempotent` wrapper achieves this
with:

- **Delivery dedup** — ledger row keyed on (handler, event_id);
  second arrival sees the row and returns cached outcome
- **Business dedup** — composed key (e.g.
  `credit_granted:user:u1:pro`); the handler checks `AlreadyDone`
  before doing real work
- **Retry policy** — transient errors retry with backoff; permanent
  failures land in `failed_permanent` for manual recovery

Use `Idempotent` for any handler that touches money or external
state. Use a plain handler only for read-only side effects (logging,
metrics).

---

## Mini-lessons by question type

### "Is this product subscription or consumption?"

Look at the tier's `billing_model`:

- `consumption_only` — no recurring fee. Pure pay-as-you-go.
- `subscription_unlock_only` — flat fee buys higher limits. No
  credit. The fee disappears into your revenue line.
- `subscription_with_credits` — flat fee + monthly credit refill.
  The hybrid.
- `subscription_with_overage` — flat fee + credits + auto-bill on
  overage. Hybrid with a soft cap.
- `seat_based` — price scales with seats. Per-user SaaS.

### "What does 'use_it_or_lose_it' mean?"

`lifecycle: use_it_or_lose_it` is the most common choice. At the next
grant (typically monthly), any unspent `subscription_credit` from the
previous period is **forfeit**. The new grant resets the bucket to
`amount_per_period`.

Why? Predictable cost for you (no liability accumulates), creates
mild urgency for the user (use it or lose it). The dominant pattern
for consumer subscriptions.

Variants:
- `rollover_capped` — keep leftover, capped at N periods. Enterprise-
  friendly.
- `rollover_unlimited` — keep leftover forever. Rare; commit-based
  contracts.
- `persistent` — for one-shot grants (signup gift). Never expires,
  never resets.

### "What's the difference between Check, Spend, and Release?"

- `Check` is a **dry-run** — "would this be allowed?" No counters
  move. Use before doing expensive work where you want to fail-fast.
- `Spend` is **record this happened** — increment the counter. Use
  after the work completed and you know the cost.
- `Release` is **decrement a gauge** — use when a long-running
  resource exits (sandbox terminated, connection closed). Only valid
  for gauge counters; no-op on accumulators.

The middleware does Check automatically; you do Spend/Release in
your handler when the work completes.

### "Why does the receiver always return 200?"

Because auth retries on non-200 responses. If your handler errored
and you returned 500, auth would retry, your handler would error
again, and you'd be in a retry loop with no observability.

Instead: receiver returns 200 always (on valid signature). Handler
errors are captured in the ledger and retried INSIDE the library
with the configured `Retry` policy. After N failures, the row lands
in `failed_permanent` and `quotactl events --status failed_permanent`
surfaces it for human attention.

Exception: 401 (bad signature) and 400 (malformed JSON) are returned
synchronously because they're configuration bugs, not transient.

### "What's a PinStore?"

Stable user→org map. When `auth.user.registered` fires, the user has
no org yet. When `org.created` fires, the org has no users yet. The
credit-grant handler needs both. The PinStore records "the first
billing org observed for this user" and keeps it forever, so the
second-arriving event can fall back to the pin instead of granting
credit to the wrong org.

Operator-set values (`source=operator`) win over auto-set values
(`source=auto`), so you can override programmatically without fear of
the auto-mapping overwriting your decision.

### "What does 'fail-open' mean?"

The middleware decides what to do when the engine itself errors
(Redis down, config bug, unknown resource). Two modes:

- **`FailOpen: true`** — let the request through, log a warning.
  Use for non-critical paths where breaking the customer is worse
  than allowing slight overspend.
- **`FailOpen: false` (default)** — return 503. Use for spend caps
  where overspend is worse than a temporary outage.

This is a tradeoff between availability and financial safety. Most
production gateways pick fail-closed at the entrance and fail-open
at lifecycle hooks (release counters), so a Redis blip can't block
shutdown.

### "What's the difference between an accumulator and a rate?"

Both count "events over time" but with different semantics:

- **Accumulator** — counts within a calendar bucket (hour, day, week,
  month). Resets at the bucket boundary. "100 API calls per month."
- **Rate** — counts within a sliding window. Never resets; old
  entries fall off the back. "100 API calls per minute, sliding."

Use accumulator for quotas (budget that resets at billing period).
Use rate for anti-abuse (DDoS guard, per-second cap).

A common mistake: using rate for "100/hour quota". A rate counter
will accept 100 calls now, then 0 for the next 59 minutes — vs an
accumulator that resets cleanly at the top of the hour.

### "What's a gauge?"

A counter that goes up AND down. "How many concurrent sandboxes does
this org have right now?" Spend increments; Release decrements.
Always reflects the present state, never a sum-over-time.

Gauges have no `reset_period` — they don't reset; they only change
via explicit Spend/Release calls. The risk: if your handler crashes
between starting a sandbox and decrementing on exit, you'll leak
quota. Mitigation: paired emit pattern (lifecycle event fires on
start AND on stop, so external observers can reconcile).

---

## When the user asks something you can't answer here

Point them at:

- **For a term you don't recognize** —
  `docs/BILLING_GLOSSARY.md` is the alphabetical lookup
- **For the choice between billing models** —
  `docs/BILLING_MODELS_GUIDE.md` walks all 11 archetypes
- **For "how do I wire it"** — `docs/INTEGRATION_RUNBOOK.md` has the
  9-stage step-by-step
- **For "where does the money flow"** —
  `docs/PAYMENT_PIPELINE.md` walks the four-service architecture
- **For Go API specifics** — the five mechanical skills
  (`ab0t-quota-go-setup` etc.)
- **For "how should I think about pricing for MY product"** —
  `ab0t-quota-billing-design` is the strategic skill

Don't try to teach everything in one response. Identify the specific
concept the user is missing and teach that one well. Use the
glossary or specific doc to follow up.
