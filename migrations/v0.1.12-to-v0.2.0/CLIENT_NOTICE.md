# Client notice — ab0t-quota-go v0.2.0 (an unresolved tier now fails closed)

*Copy/paste to any team that imports `github.com/ab0t-com/ab0t-quota-go`.*

---

**Subject: ab0t-quota-go v0.2.0 — a request whose tier cannot be resolved is no longer admitted
unlimited. Your code compiles unchanged; run the read-only checker before you deploy.**

Hi — v0.2.0 is a MINOR release. No exported API was removed, renamed or changed. One default changed,
and two behaviours you could notice changed with it.

**1. Unresolved tier → `503`, not "unlimited".** When the library cannot resolve the caller's tier (the
tier provider failed — e.g. auth or billing unreachable with nothing cached — or returned blank),
v0.1.12 let the request through with no limit. v0.2.0 refuses it: `Result.Allowed()` is false and the
middleware answers `503` `{"error":"quota_service_unavailable", "reason":"tier_unresolved", …}` without
calling your handler; `Engine.Acquire` returns an error wrapping `engine.ErrTierUnresolved` (nothing
admitted or spent — answer it `503`). Give your tier provider a `default_tier` (or use the cached mesh provider) so this
only happens when there is genuinely nothing to go on. If an admitted request costs you nothing and you
want the old behaviour, set `"enforcement": {"allow_unknown_tier": true}` in `quota-config.json`.
`FailOpen: true` on the middleware does **not** change this (it covers engine errors only).

**2. Paid mode without SNS now starts.** With billing wired, a durable outbox and no
`outbox.sns_topic_arn`, v0.1.12 refused to start. v0.2.0 starts and sends lifecycle events to billing
over HTTPS with your billing key, through the same durable outbox (retried, never dropped silently).
SNS, or a publisher you pass in, is used exactly as before. `Capabilities().Resolved["lifecycle_transport"]`
says which transport is live.

**3. New env-var names** — the ones `authsetup connect billing` / `connect payment` print:
`BILLING_SERVICE_URL`, `BILLING_SERVICE_API_KEY`, `PAYMENT_SERVICE_URL`, `PAYMENT_SERVICE_API_KEY`.
The old `AB0T_QUOTA_BILLING_URL` / `AB0T_QUOTA_BILLING_TOKEN` / `AB0T_QUOTA_PAYMENT_URL` /
`AB0T_QUOTA_PAYMENT_TOKEN` still work (each reported once per process in a deprecation warning); when both are set the new
name wins. `AB0T_QUOTA_SERVICE_TOKEN` (the shared fallback key) is unchanged.

**4. Money is exact.** Every amount is an exact decimal with at most 6 decimal places (the same rule as
billing and the Python library). A price in `pricing.products` with more than 6 decimal places is now
refused at startup (naming the price), as is an explicit meter price with more than 6. Amounts the
library computes (the meter's hold estimate, `billing.CostForDuration`) **round UP to 6 decimal places**
— away from zero, at most one micro-unit ($0.000001) — the same rule billing uses, so the library's
number equals billing's. `billing.QuotaCheckRequest.Cost` (float64) is deprecated —
set `CostExact` (a `billing.Money`, sent as a JSON string). New: `billing.Money` and
`billing.CostForDuration` for your own cost arithmetic, so it never goes through float64.

**5. Auth-event webhooks verify the Standard Webhooks headers.** A delivery that carries `webhook-id` /
`webhook-timestamp` / `webhook-signature` (the auth service sends them) must verify them — a timestamp
more than 5 minutes from your clock is refused (401) as a replay, even when `X-Event-Signature` is
valid. Keep your servers' clocks synced (NTP); `ReceiverConfig.StandardTolerance` changes the window.

**What you might notice**

| When | v0.1.12 | v0.2.0 |
|---|---|---|
| The tier provider fails / returns blank | request admitted, no limit | `503 quota_service_unavailable`, handler not called |
| Same, with `enforcement.allow_unknown_tier: true` | — | admitted, no limit (the old behaviour) |
| Billing wired + durable outbox + no SNS topic | `quota.Setup` refused to start | starts; lifecycle events over HTTPS |
| Only the old env-var names are set | read | still read, plus one deprecation warning |
| A price with more than 6 decimal places | loaded | startup refuses it, naming the price |

New and opt-in: a typed reservation commit (`Client.CommitReservationResult`), org-level
`billing.funds_low` events (`trigger: balance_threshold`) reaching your `OnLow` hook, and
`funding_sources` per product in `pricing.products`.

## What to do

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.2.0 && go mod tidy
$KIT/migrations/v0.1.12-to-v0.2.0/migrate-check.sh .
go build ./... && go test ./...
```

The checker is read-only and lists what to look at (details in `MIGRATION.md` next to it). After
deploying, watch for `503` with `reason: tier_unresolved` — each one is a request v0.1.12 would have
admitted unlimited.

**Rollback:** pin back to `@v0.1.12` and redeploy. Keep the old env-var names set until you are sure
(v0.1.12 does not read the new ones).
