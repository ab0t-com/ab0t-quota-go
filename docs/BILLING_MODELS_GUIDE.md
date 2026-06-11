# Billing Models Guide

Every billing relationship you might want, expressed as a
`quota-config.json` block. Ported from the Python ab0t-quota guide;
config is wire-compatible across runtimes.

Read [BILLING_GLOSSARY.md](BILLING_GLOSSARY.md) first if any term
below feels foreign.

---

## TL;DR

You declare your tier policy as JSON. The library reads it and the
Go code is the same regardless of which archetype you pick.

```jsonc
{
  "tier_id": "pro",
  "billing_model": "subscription_with_credits",
  "price": {"amount_per_period": "10.00", "currency": "USD", "period": "month"},
  "credit_grant": {
    "trigger": "subscription_invoice_paid",
    "amount_per_period": "10.00",
    "lifecycle": "use_it_or_lose_it",
    "destination": "subscription_credit"
  },
  "limits": {
    "api.requests": 100000,
    "spend.usd": 1000.00
  }
}
```

Every paid invoice grants +$10 of credit. At renewal, unused credit is
forfeit. Spending drains the credit first; only after exhaustion do
they need to top up. Change `billing_model` + `credit_grant` to switch
relationships — zero Go code changes.

---

## What the library does

When you call `quota.Setup(ctx, opts)`:

- **Quota enforcement** — every resource you track has a tier-defined
  limit. The engine rejects requests that exceed cap.
- **Spend tracking** — `q.Spend(ctx, in)` increments the right
  counter; for accumulators it's per-period.
- **Credit grants** — when an auth event resolves to a tiered user,
  the library calls your `CreditGranter`; you in turn call billing
  to land the grant.
- **Tier-change side effects** — on downgrade, the previous tier's
  `subscription_credit` is reset (configurable per tier).
- **Top-up** — payment-service handles Stripe Checkout for
  `account_funding`; balance flows through ab0t-billing.

## What you do

- **Declare your tiers** in `quota-config.json`
- **Wire `q.Middleware`** for HTTP enforcement
- **Wire `q.WebhookHandler()`** to receive auth events
- **Implement `CreditGranter`** so credit grants land in your billing

## What the library does NOT do

- It doesn't pick pricing for you (you do, in `price.amount_per_period`)
- It doesn't pick a billing relationship for you (you pick a `billing_model`)
- It doesn't run Stripe Checkout (payment-service's job)
- It doesn't enforce defaults silently — every default applies only
  when you omit a field

---

## Tier fields

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `tier_id` | string | yes | Stable identifier; lowercase snake_case |
| `display_name` | string | yes | Shown to users |
| `sort_order` | int | yes | Free=0; paid tiers ascending. Used for downgrade detection |
| `billing_model` | enum | no (default `consumption_only`) | See list below |
| `price` | object | for subscription models | `amount_per_period`, `currency`, `period` |
| `credit_grant` | object | for credit-bearing tiers | See lifecycle table |
| `limits` | object | yes | resource_key → number or full TierLimit |
| `features` | []string | no | Feature flags this tier unlocks |
| `upgrade_url` | string | no | Surface in 429 denial body |
| `initial_credit` | decimal | no | DEPRECATED — use `credit_grant` with `trigger: signup` |

---

## Archetype A — Pure subscription (capacity unlock only)

Classic SaaS. Pay flat, get higher limits. No consumption tracking.
Example: Slack, Linear, Notion.

```jsonc
{
  "tier_id": "team",
  "display_name": "Team",
  "sort_order": 1,
  "billing_model": "subscription_unlock_only",
  "price": {"amount_per_period": "20.00", "currency": "USD", "period": "month"},
  "limits": {
    "users.total": 25,
    "history.retention_days": 365
  }
}
```

**User experience:** subscribe → higher caps → spend within them →
renew next month.

Use this when: consumption isn't your cost driver, and you want
predictable per-customer revenue.

---

## Archetype B — Pure consumption (pay-as-you-go)

API-style products. No tier hierarchy; users top up, spend down.
Example: Anthropic API, OpenAI API.

```jsonc
{
  "tier_id": "payg",
  "display_name": "Pay as you go",
  "sort_order": 0,
  "billing_model": "consumption_only",
  "credit_grant": {
    "trigger": "signup",
    "amount_per_period": "5.00",
    "lifecycle": "persistent",
    "destination": "credit_balance"
  },
  "limits": {
    "api.qps": 100
  }
}
```

Users top up via Stripe Checkout (`type=account_funding`) → balance
increases. Each call's cost drains the balance. Balance at zero →
requests blocked.

**User experience:** sign up → $5 free credit → spend → top up via
Stripe to keep going.

In Go, where you'd call this:

```go
res, _ := q.Check(ctx, engine.CheckInput{
    UserID: u, OrgID: o,
    ResourceKey: "spend.usd", Cost: estimatedCost,
})
if !res.Allowed() { return ErrOutOfBudget }

actualCost := doTheWork()
q.Spend(ctx, engine.CheckInput{
    UserID: u, OrgID: o,
    ResourceKey: "spend.usd", Cost: actualCost,
})
```

---

## Archetype C — Subscription with bundled credits

The dominant consumer-SaaS pattern. Pay $X/month, get $Y of usage
credit. Example: OpenAI ChatGPT Plus, Vercel Pro, Cloudflare Workers
Paid, Anthropic Pro.

```jsonc
{
  "tier_id": "pro",
  "display_name": "Pro",
  "sort_order": 2,
  "billing_model": "subscription_with_credits",
  "price": {"amount_per_period": "10.00", "currency": "USD", "period": "month"},
  "credit_grant": {
    "trigger": "subscription_invoice_paid",
    "amount_per_period": "10.00",
    "currency": "USD",
    "lifecycle": "use_it_or_lose_it",
    "destination": "subscription_credit",
    "reset_on_downgrade": true,
    "reset_on_upgrade": false
  },
  "limits": {
    "spend.usd": 500.00
  }
}
```

**User experience:**
- Day 1: subscribe for $10, see "$10 credit available"
- Days 1–30: spend $6 on usage, see remaining balance
- Day 30: renewal → $10 charged, balance resets to $10 ($4 forfeit)
- Mental model: "I'm paying $10/mo and getting $10 to spend"

### Variant — pay for less than you get (loss leader)

```jsonc
"credit_grant": {
  "trigger": "subscription_invoice_paid",
  "amount_per_period": "12.00",   // 20% bonus vs $10 price
  "lifecycle": "use_it_or_lose_it",
  "destination": "subscription_credit"
}
```

### Variant — rollover up to N periods (enterprise-friendly)

```jsonc
"credit_grant": {
  "trigger": "subscription_invoice_paid",
  "amount_per_period": "10.00",
  "lifecycle": "rollover_capped",
  "rollover_max_periods": 3,
  "destination": "subscription_credit"
}
```

### Variant — unlimited rollover

```jsonc
"credit_grant": {
  "trigger": "subscription_invoice_paid",
  "amount_per_period": "10.00",
  "lifecycle": "rollover_unlimited",
  "destination": "subscription_credit"
}
```

---

## Archetype D — Subscription unlock only + separate top-up

Enterprise / procurement-driven. Subscription unlocks higher limits;
the customer's AP team approves a separate top-up for actual spending.
Example: AWS Savings Plans, Snowflake commit-based pricing.

```jsonc
{
  "tier_id": "enterprise",
  "display_name": "Enterprise",
  "sort_order": 3,
  "billing_model": "subscription_unlock_only",
  "price": {"amount_per_period": "1000.00", "currency": "USD", "period": "month"},
  // NO credit_grant — this is the EXPLICIT "no credit" archetype
  "limits": {
    "api.requests": null,    // null = unlimited
    "spend.usd": null
  }
}
```

**User experience:** subscribe → procurement signs off → AP team
tops up $5000 in `account_funding` → spend draws down $5000. The
$1000 sub bought the privilege of having no caps; the $5000 is what
they actually spend.

**Why not just `consumption_only`?** Because `subscription_unlock_only`
is the EXPLICIT version. The validator rejects
`subscription_unlock_only + credit_grant` — it's a self-defense flag
that says "I really mean no credit here."

---

## Archetype E — Subscription with overage (status: schema-supported)

"$10/mo includes $10 of usage; anything over auto-bills your card."
Example: Twilio Voice, GitHub Actions, Vercel.

```jsonc
{
  "tier_id": "pro",
  "billing_model": "subscription_with_overage",
  "price": {"amount_per_period": "10.00", "currency": "USD", "period": "month"},
  "credit_grant": {
    "trigger": "subscription_invoice_paid",
    "amount_per_period": "10.00",
    "lifecycle": "use_it_or_lose_it"
  },
  "overage_policy": {
    "enabled": true,
    "payment_method": "card_on_file",
    "max_overage_per_period": 1000
  }
}
```

Library accepts the declaration. Off-session Stripe charging is wired
through payment-service. Use this archetype when you want a soft
upper boundary instead of a hard 429.

---

## Archetype F — Seat-based / Per-user

Classic per-user SaaS. Price scales with seats. Example: Slack,
Notion, Linear.

```jsonc
{
  "tier_id": "team",
  "billing_model": "seat_based",
  "price": {"amount_per_user_per_period": "10.00", "currency": "USD", "period": "month"},
  "min_seats": 1,
  "limits": {
    "per_user.api.requests_per_day": 1000
  }
}
```

Library tracks `default_per_user_fraction` for splitting org-level
quotas across seats. Stripe `quantity` sync wires through payment-
service; the library reads the resulting tier_id from the mesh.

---

## Archetype G — Freemium (free with limits)

Every SaaS startup. Restrictive caps; users upgrade when they
outgrow. The teaser, not the gift.

```jsonc
{
  "tier_id": "free",
  "display_name": "Free",
  "sort_order": 0,
  "billing_model": "consumption_only",
  "credit_grant": {
    "trigger": "signup",
    "amount_per_period": "10.00",
    "lifecycle": "persistent",
    "destination": "credit_balance",
    "dedup": "per_user_per_tier"
  },
  "limits": {
    "api.requests_per_hour": 1000,
    "spend.usd": 10.00,
    "users.total": 3
  }
}
```

Design wisdom: **gate quantity, not features**. Free users with the
full feature set hitting low caps upgrade for capacity. Free users
with limited features churn because they don't see the value.

The `dedup: per_user_per_tier` is your anti-farming gate. Combined
with email-verification-required at the auth layer, it makes farming
the free credit economically unattractive.

---

## Archetype H — Free trial → auto-convert

```jsonc
{
  "tier_id": "pro_trial",
  "display_name": "Pro (14-day trial)",
  "billing_model": "subscription_with_credits",
  "price": {"amount_per_period": "10.00"},
  "trial": {"duration_days": 14, "requires_card": true, "auto_convert": true}
}
```

The underlying Plan in payment-service must have `trial_period_days`
set; Stripe Checkout honors it. The library treats the user as Pro
during the trial; at end of trial Stripe converts and a normal
`invoice.paid` fires the grant.

---

## Archetypes I, J, K — Volume commit, metered, referral credits

Schema-supported, light on implementation. Open a ticket when you
need:

- **Annual commit discount** — pay $1188 upfront for 12 months
- **Pure metered billing** — Stripe `usage_record_summary` push
- **Referral / promo credits** — `trigger: "manual"` works on the
  request side; admin tooling for issuing them is the missing piece

---

## Lifecycle table — what happens to leftover credit

| `lifecycle` | At next grant | Use for |
|-------------|---------------|---------|
| `persistent` | Existing balance preserved | Signup gift; one-shot grants |
| `use_it_or_lose_it` | Existing balance forfeit; new credit lands at full amount | Standard monthly subscription |
| `rollover_capped` | Existing balance kept, capped at `rollover_max_periods × amount_per_period` | Enterprise plans |
| `rollover_unlimited` | Existing balance fully retained, new credit added | Rare; commit-based contracts |

`rollover_capped` REQUIRES `rollover_max_periods`. Validation rejects
otherwise.

---

## Library defaults — what kicks in when you omit a field

If you declare only `tier_id`, `display_name`, `sort_order`, and
`limits`:

| Field | Default | Means |
|-------|---------|-------|
| `billing_model` | `consumption_only` | No subscription side-effects |
| `price` | none | Free tier |
| `credit_grant` | none | No grants fire |
| In `credit_grant`, `lifecycle` | `use_it_or_lose_it` | Consumer-SaaS norm |
| In `credit_grant`, `destination` | `subscription_credit` | For subscription grants |
| In `credit_grant`, `dedup` | `per_user_per_tier` | Anti-farming default |
| `currency` | `USD` | |
| `period` | `month` | |
| `reset_on_downgrade` | `true` | Voluntary downgrade wipes credit |
| `reset_on_upgrade` | `false` | Upgrade preserves existing credit |

**No default is consumer-specific.** Defaults reflect the modal SaaS
expectation; override per-tier as needed.

---

## Required wiring per archetype

### For any `subscription_*` model

1. **payment-service** must set
   `subscription_data.metadata = {"org_id": ..., "plan_id": ...}` on
   Stripe Checkout sessions
2. **ab0t-quota-go** must receive paid-invoice events
   (`invoice.paid`, `invoice.payment_succeeded`) routed through auth's
   event system
3. **Your `CreditGranter`** must call `q.Billing.GrantCredit(...)`
   with the right org/tier/amount

### For any model with `credit_grant`

4. **Your `quota-config.json`** must declare it. No code-level config
   exists; everything is JSON.

### For downgrade reset

5. **Your service** must propagate tier changes (this typically
   happens automatically via Stripe → payment-service → auth's tier
   mesh)

### For top-up (Archetype B / D)

6. **payment-service** mounts `/api/payments/topup` — call from your
   frontend's "Add credits" button
7. **Your service** doesn't do anything special; the credit lands via
   the normal billing flow

---

## Common gotchas

### "My tier upgrade isn't granting credit"

Check that:
1. The Plan in payment-service has the right `plan_id` set at checkout
2. `subscription_data.metadata.org_id` is populated
3. Your `quota-config.json` tier has
   `billing_model: subscription_with_credits` AND
   `credit_grant.trigger: subscription_invoice_paid`
4. The webhook receiver returns 200 to test events (`quotactl replay`)
5. The Stripe Invoice has `metadata.org_id` set
6. Your `CreditGranter` doesn't error (check
   `quotactl events --status failed_permanent`)

### "Downgrade isn't resetting subscription_credit"

Check that:
1. The old tier has `credit_grant.reset_on_downgrade: true` (default)
2. `sort_order` is set on both tiers and new < old
3. The recorded `subscription_credit_source_tier` matches the old tier
   (safety check)

### "Spending isn't draining `subscription_credit`"

Check the spend order in billing-service. The bucket priority is
typically: `subscription_credit` first (use it or lose it),
then `credit_balance`, then `balance`. If billing-service's spend
order is wrong, subscription_credit accumulates and never drains.

### "Why is `subscription_credit` always 0 even with credit_grant?"

Either:
- The paid-invoice webhook isn't reaching the library (delivery
  unwired, or Stripe webhook not subscribed to the event)
- `subscription_data.metadata` wasn't set on Stripe Checkout
- Your `CreditGranter` returned an error
  (`quotactl events --status failed_permanent`)
- Cache TTL — billing-service's balance cache may be stale; force
  refresh

### "My consumption_only tier rejects `price`"

Intentional. For consumption with a fixed fee, use
`subscription_unlock_only`. `consumption_only` means literally that —
no recurring price.

---

## Migration from `initial_credit`

The deprecated form:

```jsonc
"initial_credit": "10.00"
```

Is auto-synthesized into:

```jsonc
"credit_grant": {
  "trigger": "signup",
  "amount_per_period": "10.00",
  "lifecycle": "use_it_or_lose_it",
  "destination": "subscription_credit"
}
```

The new explicit form is preferred. Migrate by replacing
`initial_credit` with the full `credit_grant` block and choosing the
right `lifecycle` for your customer-trust posture.

---

## Read next

- [INTEGRATION_RUNBOOK.md](INTEGRATION_RUNBOOK.md) — step-by-step
  gateway integration
- [BILLING_GLOSSARY.md](BILLING_GLOSSARY.md) — terminology
- `../examples/basic/quota-config.json` — minimal working config
