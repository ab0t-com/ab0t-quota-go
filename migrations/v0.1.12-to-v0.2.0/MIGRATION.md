# Migration: ab0t-quota-go v0.1.12 → v0.2.0

A MINOR release: **one default changed** — a request whose tier cannot be resolved is no longer
admitted. **Your code compiles unchanged**: no exported symbol was removed, renamed or re-typed. Two
more behaviours you could notice changed with it (lifecycle events can go to billing over HTTPS; the
billing/payment env-var names). Everything else in the release is opt-in.

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.2.0 && go mod tidy
/path/to/ab0t-quota-go/migrations/v0.1.12-to-v0.2.0/migrate-check.sh .    # read-only
go build ./... && go vet ./... && go test ./...
```

`migrate-check.sh` is **read-only** (it never edits anything and makes no network calls). It lists every
site that reads a quota decision, mentions the unresolved-tier reason, or uses an old env-var name,
with what to check, and exits `1` while anything is unreviewed (`0` when clean, `2` on a usage error;
`--advisory` always exits `0`). Mark a site you have checked with `quotago:v0.2.0-reviewed` on the line
or the line above it (any comment syntax), or `quotago:v0.2.0-reviewed-file` anywhere in the file.
Fixtures: `testdata/unreviewed` (4 findings, exit 1) and `testdata/reviewed` (exit 0).

## TL;DR — what changed for you

| # | Rule | Change | Your action |
|---|---|---|---|
| R1 | `UNKNOWNTIER` | ⚠ When the tier provider fails or returns blank (`engine.UnknownTier`, reason `tier_unresolved`), `Result.Allowed()` is now **false** and the middleware answers **`503`** without calling your handler; `Engine.Acquire` returns an error wrapping `engine.ErrTierUnresolved`. Both used to admit with **no limit** | Make sure every request resolves a tier (a `default_tier`, or a cached provider). Opt out knowingly with `"enforcement": {"allow_unknown_tier": true}` |
| R2 | (no rule — INFO) | With billing wired, a durable outbox and **no** `outbox.sns_topic_arn`, paid mode now **starts** and sends lifecycle events to billing over HTTPS. It used to refuse to start | Nothing. Check `Capabilities().Resolved["lifecycle_transport"]` |
| R4 | (no rule — INFO) | Money is exact: prices with more than 6 decimal places are refused at startup; `QuotaCheckRequest.Cost` (float64) is deprecated for `CostExact`; computed amounts (the meter's hold estimate, `CostForDuration`) round UP to 6 places, as billing does | Check your prices (section 4) |
| R3 | `ENVNAMES` | The billing/payment env vars are now `BILLING_SERVICE_URL`, `BILLING_SERVICE_API_KEY`, `PAYMENT_SERVICE_URL`, `PAYMENT_SERVICE_API_KEY` — the names `authsetup connect billing` / `connect payment` print. The old `AB0T_QUOTA_BILLING_*` / `AB0T_QUOTA_PAYMENT_*` names still work | Rename when convenient; if you set both, give them the same value (the new name wins) |

## 1. An unresolved tier fails closed (R1)

An `UnknownTier` decision means the engine could not evaluate the limit: the tier provider returned an
error (auth or billing unreachable, no cached tier) or a blank tier. Before v0.2.0 that request
**proceeded with no limit** — during an outage a service that charges for work admitted unlimited work
it might never bill. v0.2.0 fails closed, the way the Python runtime does.

| | v0.1.12 | v0.2.0 |
|---|---|---|
| `res.Allowed()` for `UnknownTier` | `true` | `false` |
| `res.AllowedFailClosed()` | `false` | `false` (unchanged) |
| `res.Unavailable()` (new) | — | `true` |
| middleware (`q.Middleware` / `middleware.Guard`) | handler called, no limit | `503`, handler **not** called |
| `GuardConfig.FailOpen: true` | (did not apply) | still does not apply — it covers engine **errors** (`err != nil`), not an unresolved tier |
| `Engine.Acquire` | admitted, **no limit** (spent the gauges) | error wrapping `engine.ErrTierUnresolved`; nothing admitted or spent — answer `503` |
| `enforcement.allow_unknown_tier: true` (new) | — | restores the v0.1.12 behaviour for `Allowed()`, the middleware and `Acquire` (a `tier_unresolved_admitted` warning each time) |
| `res.Decision == engine.UnknownTier` checks in your code | unchanged | unchanged |
| a tier id that is **not in your config** (`tier_not_in_config`) | `429` deny | `429` deny (unchanged) |

The `503` body (static; the same shape as the Python middleware's `503`, plus `reason` / `resource`):

```json
{"error": "quota_service_unavailable", "detail": "Quota enforcement is temporarily unavailable.",
 "reason": "tier_unresolved", "resource": "<resource_key>"}
```

`X-Quota-Reason: tier_unresolved` is set. A `503` tells your client "retry later", not "you are over a
limit".

**How to not be affected:** give the tier provider an answer for every request —
`tier_provider.default_tier` for `static` / `jwt`, or the last-known-good cache for `mesh` (the cache
keeps answering through an outage). An unresolved tier then only happens when there is genuinely
nothing to go on. Note: the `static` provider maps by USER id — an org-only `Acquire` (`UserID: ""`)
resolves only through `default_tier`.

**The opt-out** — only where an admitted request costs nothing (a read-only API, a free internal tool):

```json
"enforcement": { "enabled": true, "allow_unknown_tier": true }
```

Code that must refuse an unresolved tier even under that opt-out can call `res.AllowedFailClosed()`.

## 2. HTTPS lifecycle transport (INFO)

When billing is wired (`BILLING_SERVICE_URL`), the outbox is durable and neither
`outbox.sns_topic_arn` nor `Options.SettlementPublisher` is set, `quota.Setup` now delivers lifecycle
events (`resource.started` / `heartbeat` / `stopped` / `deleted`) to
`POST {billing}/billing/{org_id}/lifecycle-events` with your billing key, instead of refusing to start.
The events still go through the durable outbox: a `408`, `429`, `5xx` or a transport failure is retried
in-call (0.5 s, then 1 s), then the event stays PENDING for the drain loop, and past the retry horizon a
terminal event is settled against billing (any other is voided with an alert). Any other `4xx`, or a
`2xx` without `"accepted": true`, is a refusal: the event is settled (terminal) or voided with an alert
at once (void reason `publish_refused`). Nothing is dropped silently. Every event now carries an
`event_id` that stays the same across retries. A configured SNS topic, or a publisher you pass, is used
exactly as before. `meter.New` accepts the HTTPS transport (`q.Outbox.HasPublisher()` is true).

| `Capabilities().Resolved["lifecycle_transport"].Value` | meaning |
|---|---|
| `sns (outbox.sns_topic_arn)` | SNS, as before |
| `custom (Options.SettlementPublisher)` | your publisher, as before |
| `https (billing lifecycle-events; no outbox.sns_topic_arn)` | new in v0.2.0 |
| `none` | no transport (billing not wired, or the SNS client failed to build) |

If you relied on the old refusal as a deployment check ("paid mode must not start without SNS"), check
`lifecycle_transport` instead.

## 3. Env-var names (R3)

| Canonical (v0.2.0) | Deprecated fallback (still read) |
|---|---|
| `BILLING_SERVICE_URL` | `AB0T_QUOTA_BILLING_URL` |
| `BILLING_SERVICE_API_KEY` | `AB0T_QUOTA_BILLING_TOKEN` |
| `PAYMENT_SERVICE_URL` | `AB0T_QUOTA_PAYMENT_URL` |
| `PAYMENT_SERVICE_API_KEY` | `AB0T_QUOTA_PAYMENT_TOKEN` |

`AB0T_QUOTA_SERVICE_TOKEN` is unchanged: the shared last fallback for both keys (not deprecated). The
canonical name wins when both are set; an old variable set to a **different** value is named in the
warning (the same value is harmless and quiet — the rollback-safe setup). The first time a process reads
an old name it logs ONE warning listing every old name in use.
`Capabilities().Resolved["billing_url"].Source` names the variable actually read. The key is the value
`authsetup connect billing` saved for you (`.api_key.key` in its credentials file).

## 4. Money is exact (INFO — no checker rule; your code compiles unchanged)

Every amount is an exact decimal with at most 6 decimal places, the same rule as the Python library
and billing; computed costs round UP (away from zero) to 6 places — at most one micro-unit ($0.000001),
exactly as billing rounds them.

| | v0.1.12 | v0.2.0 | Your action |
|---|---|---|---|
| A price in `pricing.products` with more than 6 decimal places | loaded | **refused at startup** (`ErrConfig`, naming the price) | write at most 6 decimal places (`"0.125"`) |
| An explicit `ReserveInput.HourlyRate` / `AllocationFee` / `EstimatedCost` with more than 6 places | sent | refused (`ErrConfig`) before any reserve | round it yourself, deliberately, before calling |
| The meter's hold estimate (`AllocationFee + HourlyRate × HoldHours`) | unrounded | ROUND_UP to 6 places (billing's rule) | nothing |
| `billing.QuotaCheckRequest.Cost` (float64) | sent as a JSON number | unchanged, **deprecated** | set `CostExact: &m` (a `billing.Money`) — sent as an exact string |
| JSON-number money in billing answers / funds events | read through float64 | read from the number's exact text | nothing |

Find what to look at:

```bash
grep -rn 'price_per_hour\|allocation_price' --include='*.json' .        # at most 6 decimal places
grep -rnE 'QuotaCheckRequest\{|\.Cost *=' --include='*.go' .            # prefer CostExact
grep -rnE 'HourlyRate|AllocationFee|EstimatedCost' --include='*.go' .     # explicit meter prices
```

New helpers: `billing.Money` (`ParseMoney`, `MoneyFromMicros`, `String`, `Micros`, `Add`, `Sub`, …) and
`billing.CostForDuration(rate, d)` for your own cost arithmetic — never float64.

## 5. New, opt-in (nothing to do)

- `billing.Client.CommitReservationResult` → typed `CommitResult` / `CommitOutcome` (same classification
  as the Python client), with `OnInsufficientFundsRecordDebt` for the billing-priced form.
- Org-level `billing.funds_low` (`trigger: "balance_threshold"`, no `reservation_id`) reaches `OnLow` /
  `OnFundsLow` with a nil run; `FundsEvent.IsOrgLevel()`, `FundsEvent.Threshold`.
- `pricing.products.<product>.funding_sources` — which buckets may fund the product's holds; sent on
  every reserve automatically, validated by `meter.ParsePricing`.
- `billing.NewLifecyclePublisher` for a service that builds its own `outbox.Emitter`.
- `billing.Money` / `billing.CostForDuration` — exact money (see section 4).

## Agent rules (machine-applicable)

1. For every `R1` finding that reads `.Allowed()`: decide whether an unresolved tier should proceed at
   that site. If the work costs money, keep the new default and make sure the tier resolves (step 2).
   If it must proceed, set `enforcement.allow_unknown_tier: true` in `quota-config.json` (applies to the
   whole service). Then mark the line `quotago:v0.2.0-reviewed`.
2. Open `quota-config.json`: if `tier_provider.type` is `static` or `jwt` and there is no `default_tier`,
   decide whether unmapped callers should be refused (`503`) or get a tier; add `default_tier` for the
   latter.
3. For every `R1` finding in an alert rule, dashboard or test that expects `tier_unresolved` to be
   admitted (or a `429`): it is now a `503` with `"error": "quota_service_unavailable"`. Update it, then
   mark it reviewed.
4. For every `R3` finding: rename the variable to the canonical name in the table above (same value).
   If you must keep the old name for now, mark the line reviewed.
5. Re-run the checker until it exits `0`; then `go build ./... && go vet ./... && go test ./...`.

**Rollback:** pin `@v0.1.12` and redeploy. v0.2.0 writes nothing v0.1.12 cannot read. Keep the old
env-var names (or set both) until you are sure you will not roll back — v0.1.12 does not read the
canonical names. Rolling back re-opens the unlimited admit on an unresolved tier (R1), and a paid service
without an SNS topic will refuse to start again (R2).
