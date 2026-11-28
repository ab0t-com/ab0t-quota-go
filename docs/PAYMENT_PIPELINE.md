# Payment Pipeline — Stripe → ab0t → your service

Where does money actually flow? Four services are involved.
ab0t-quota-go is the last hop — it doesn't talk to Stripe directly.
Understanding the full path is essential for debugging.

---

## The 30-second picture

```
User's card    → Stripe        : the payment happens here
Stripe         → payment-svc   : webhooks fire (invoice.paid, subscription changes, etc.)
payment-svc    → billing-svc   : the org's tier is set in billing; money lands in the balance
auth           → your service  : sign-up / membership events (auth.user.registered, org.member.added)
                                 reach ab0t-quota-go; your CreditGranter runs for sign-up gifts
your service   → billing-svc   : the tier is read back (only if you wire a lookup; see below)
```

Each hop is independent. Each hop is retried. Each hop is idempotent.
That's the design — three layers of defense against the same dollar
landing twice or not at all.

**What this Go runtime does today (read first).** Paid Stripe events are
handled by payment, not by this library: there is no Stripe webhook proxy
here, so a tier's `subscription_invoice_paid` plan credit and its
`reset_on_downgrade` are not executed by the Go runtime. With payment's
default settings a paid subscription invoice is credited to the org's
cash balance for the amount paid. The `mesh` tier provider is not wired
by `Setup` (see the README's known gaps). Payment does not publish
invoice or tier events through auth.

---

## The four services

| Service | Owns | Talks to |
|---------|------|----------|
| **Stripe** | actual money movement, card storage, dunning | the user, payment-service |
| **ab0t-payment** (payment.service.ab0t.com) | Stripe wrapper: Checkout sessions, customer portal, subscriptions, invoices | Stripe, billing-service, your service |
| **ab0t-billing** (billing.service.ab0t.com) | balance ledger (three buckets), credit grants, spend recording, tier enforcement APIs | payment-service, auth, your service |
| **ab0t-auth** (auth.service.ab0t.com) | user identity, org membership, JWT minting, event bus | every service |
| **Your service** (gateway, sandbox, etc.) | the product the user is paying for; runs ab0t-quota-go in-process | billing-service, auth |

The gateway team's service is the rightmost one. ab0t-quota-go runs
inside that service. The other services are managed by ab0t.

---

## The seven money moments

### Moment 1 — User signs up

Trigger: `auth.user.registered` event fires from ab0t-auth.

What ab0t-quota-go does (v0.3.2+: `Setup` registers the default handler on
`auth.user.registered`; before v0.3.2 it listened on `org.created` /
`user.org_assigned`, which auth does not deliver, so no gift arrived):
1. Receives the webhook at `/api/quotas/_webhooks/auth`
2. Verifies HMAC
3. Looks up handlers for `auth.user.registered`
4. Default credit-grant handler runs (idempotent-wrapped)
5. Resolves the user's tier (typically `free`)
6. Calls your `CreditGranter.GrantCredit(ctx, req)`
7. Your `CreditGranter` calls `billing.GrantCredit(...)`
8. billing-service writes the grant to the user's `credit_balance` bucket

Ledger row: `handler=default_credit_grant, event_id=evt-N,
status=success`. Idempotent — if the event fires twice, the second
delivery sees the row and returns the cached outcome.

### Moment 2 — User starts a paid subscription

Trigger: user clicks "Upgrade" in your UI.

1. Frontend calls payment-service: `POST /checkout/sessions` with
   `plan_id=pro`
2. payment-service creates a Stripe Checkout session with
   `subscription_data.metadata = {org_id, plan_id}`
3. User is redirected to Stripe-hosted payment page
4. User pays
5. Stripe redirects back to your `success_url`
6. **Asynchronously**, Stripe fires `customer.subscription.created` to
   payment-service
7. payment-service records the subscription, maps the Stripe price to a
   tier (a map on payment's side; ask ab0t support to add your price
   ids) and sets the org's tier in billing (`PUT /billing/{org_id}/tier`)
8. ab0t-quota-go sees the new tier only if its tier provider reads
   billing: wire your own lookup (README, known gaps). ab0t auth tokens
   carry no tier claim, so `jwt` does not see it.

### Moment 3 — Stripe charges the card and fires `invoice.paid`

Trigger: Stripe successfully charges the customer's card (immediately
on subscription create, then monthly).

1. Stripe fires `invoice.paid` to payment-service
2. payment-service verifies signature, checks idempotency
3. With payment's default settings it credits the org's cash
   `balance` with the amount paid (idempotent on the invoice id)

Nothing reaches ab0t-quota-go here: payment publishes no invoice event
through auth, and this library has no Stripe proxy, so your
`CreditGranter` is not called and the tier's `credit_grant` (plan
credit into `subscription_credit`) is not applied by the Go runtime.
Ask ab0t support how plan credit runs in your environment.

### Moment 4 — User makes an API call (the spend moment)

Trigger: HTTP request to your service.

1. Request hits ab0t-quota-go's middleware
2. Identity callback returns `(user_id, org_id)`
3. Router callback returns `(resource_key, cost)`
4. Engine asks tier provider for the user's tier (cache hit if
   recently resolved)
5. Engine reads the current period's accumulator from local Redis (or
   in-memory store in v0.1.0)
6. Math runs: under limit → Allow, over limit → Deny
7. If Allow: your handler runs, your code calls `q.Spend(ctx, in)`
   afterward
8. `q.Spend` increments the accumulator (process-local in v0.1.0)

Where billing-service's three buckets come in (when billing-service is
the source of truth on spend, future v0.2):
- `q.Spend` calls `billing.Reserve(...)` BEFORE the work runs
- billing-service drains buckets in priority order
  (`subscription_credit` first, then `credit_balance`, then `balance`)
- Work runs
- `q.Spend` calls `billing.Commit(...)` with the actual cost
- billing-service reconciles the reservation with the commit
- Refunds the difference, or charges card if commit > reserve and
  overage_policy allows

v0.1.0 of ab0t-quota-go uses the simpler model: increment a local
counter, enforce against the tier's limit. Subscription credit /
balance integration lands in v0.2.

### Moment 5 — User hits their limit

Trigger: a `q.Spend` would cross the tier's limit.

1. Middleware's pre-flight check fires
2. Engine returns `Decision: Deny`
3. Middleware writes 429 with denial body:
   ```json
   {
     "detail": "Quota exceeded for api.calls: 10000/10000 (tier: free).",
     "reason": "exceeded",
     "resource": "api.calls",
     "tier": "free",
     "used": 10000,
     "limit": 10000,
     "upgrade_url": "https://billing.your-domain.com/upgrade"
   }
   ```
4. Headers include `X-Quota-*` (no `Retry-After` is sent on a `429` today, as the README says; clients back off on their own)

Your frontend reads `upgrade_url` from the body, surfaces an upgrade
prompt to the user.

### Moment 6 — User downgrades

Trigger: user clicks "Cancel subscription" in customer portal.

1. Stripe customer portal updates the subscription
2. Stripe fires `customer.subscription.updated` to payment-service
3. payment-service sets the new tier in billing (a cancellation lands on `free`)
4. `reset_on_downgrade` is not acted on by the Go runtime today: any
   `subscription_credit` the org holds stays unless something else
   resets it
5. The user's new tier is in effect once your tier provider reads it

### Moment 7 — User tops up (Archetype B / D)

Trigger: user clicks "Add $50" in your billing page.

1. Frontend calls payment-service:
   `POST /checkout/sessions {type: account_funding, amount: 5000}`
2. payment-service creates Stripe Checkout for one-time payment
3. User pays
4. Stripe fires `payment_intent.succeeded` to payment-service
5. payment-service publishes `account.funding.completed` to auth
6. billing-service updates the user's `balance` bucket directly (no
   credit-grant handler needed — this is real money, not credit)

This bucket is real money — refundable, withdrawable in some
jurisdictions, never expires.

---

## Why three buckets in billing-service

| Bucket | Source | Lifecycle | Spend priority |
|--------|--------|-----------|----------------|
| `subscription_credit` | monthly subscription grant | per-period; reset rules apply | drained first |
| `credit_balance` | signup grants, promos, manual admin grants | persistent (or per `lifecycle`) | drained second |
| `balance` | top-ups (real money) | never expires; refundable | drained last |

Why this order? **Use-it-or-lose-it pressure**. Drain the bucket that
expires first; preserve the bucket that's real money. A user who
forgot to spend their $10 subscription credit this month forfeits it
— but their $50 top-up is still there. This nudges users to actively
use what they're paying for.

---

## Webhook delivery semantics

Every webhook in the pipeline is:

- **Signed** with HMAC SHA-256 over the raw body
- **Idempotent** — handlers dedup by event_id at the ledger layer
- **Retried** on non-2xx response (by Stripe, by payment-service, by
  auth)
- **Logged at every hop** — you can trace a single dollar from card
  charge to your gateway's credit grant

For ab0t-quota-go specifically:
- Receiver always returns 200 on a valid signature, even if the
  handler errors internally (auth retries non-200; that would
  compound)
- Handler errors are retried inside the ledger pipeline (delivery
  dedup means a retried delivery sees the in-progress row and waits)
- After N permanent failures (`Retry.Attempts` exhausted), the row
  goes to `failed_permanent` and `quotactl replay` is the recovery
  tool

---

## What you watch in prod

| Signal | What it means |
|--------|---------------|
| `credit granted via auth-event` log line | happy path — money landed |
| `handler attempt failed` log line | transient — retry will happen |
| ledger row in `failed_permanent` | something needs human attention |
| `subscription_invoice_paid_skip` log | event reached us but no action needed (no tier, no grant configured) |
| 401 on receiver | HMAC secret mismatch between auth and your service |
| 200 with `"status":"ignored"` | event arrived but no handler registered for the type |
| receiver latency > 1s | handler is doing too much work synchronously; offload |

For mainline dashboards, see [INTEGRATION_RUNBOOK.md](
INTEGRATION_RUNBOOK.md#stage-9--wire-dashboards--alerts).

---

## Debugging a missing credit grant

A customer says: "I paid but never got my credits." Walk this:

1. **Stripe Dashboard** — confirm the invoice was paid. Note the
   `invoice_id` and `subscription_id`. If unpaid → it's a Stripe
   issue (card declined etc.), not yours.
2. **payment-service logs** — search for the `invoice_id`. Confirm
   `invoice.paid` webhook was received and signed. If 401 →
   Stripe-secret mismatch in payment-service.
3. **auth event log** — search for the `invoice_id`. Confirm
   `subscription.invoice.paid` was published. If missing → bug in
   payment-service.
4. **Your service's ledger** — `quotactl events --user $userid
   --status success` to find recent grants. If the row exists with
   `status=success` → credit DID land; check billing-service's view
   of the bucket. If `status=failed_permanent` → your `CreditGranter`
   returned an error; check its logs.
5. **billing-service** — `GET /billing/balance/{org_id}` to see the
   three buckets. If the bucket didn't increase but your ledger says
   success → bug in billing-service's grant endpoint or a stale
   cache.

The full trail leaves footprints at every hop. Follow them.

---

## Glossary cross-refs

Reach for [BILLING_GLOSSARY.md](BILLING_GLOSSARY.md) when:
- you don't remember which is `subscription_credit` vs `credit_balance`
- you don't remember what reservation/commit means
- you see a term in here you don't recognize
