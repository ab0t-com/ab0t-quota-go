# Changelog

All notable changes to `ab0t-quota-go`. Semantic versioning: a change that
requires you to do something to keep working is at least a MINOR.

## [0.1.11] — 2026-10-16

Bug fixes from an adversarial test pass over v0.1.10. **No exported symbol was removed,
changed or added.** One fix changes behaviour you could notice — marked ⚠ and listed with
what to check in [`migrations/v0.1.10-to-v0.1.11/`](migrations/v0.1.10-to-v0.1.11/MIGRATION.md)
(read-only checker included).

- ⚠ **Security (medium): the mesh API key followed an https → http redirect in cleartext.**
  The billing/payment client's redirect guard (v0.1.10) compared the host only, so an
  `https://` base URL answering a redirect to `http://` on the same host was followed and
  `X-API-Key` went out on a plaintext connection. Every hop must now stay on the same **host**,
  and keep the previous hop's **origin** (scheme, host and port, default port filled in) or be an
  **http → https upgrade** on that host. **An https → http redirect that used to be followed is
  now refused** (at any hop, including after an upgrade). An http → https upgrade is still
  followed: refusing it would protect nothing (the key already went out on the first, http request)
  and would break an `http://` base URL — but use the `https://` address directly. The error reads
  `refusing redirect … to another origin`; on the settlement path it is retried, never voided.
  (`https://h` → `https://h:443` is now followed; v0.1.10 refused it.)
- **Fixed: the capability report said "receiver will reject all events with 401" while the
  previous secret still verified.** With `AB0T_AUTH_WEBHOOK_SECRET` unset and
  `AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS` set, `WhyOff["auth_events_signed"]` now says the receiver
  accepts only events signed with the previous secret(s), and boot logs a WARN. Receiver
  behaviour is unchanged.
- **Fixed: a `.` or `..` org id escaped its path segment.** `url.PathEscape` leaves dot segments
  intact, so org `..` became `/billing/../promotional-credit`, which a normalising proxy resolves
  to another route. Every billing/payment method that puts an id in the URL now refuses a blank,
  `.` or `..` org / subscription / session id before sending (`RecordUsage` stays best-effort:
  logged, returns `nil`).
- **Fixed: `config.Decimal` read a JSON number through float64.** A money value written as a
  number in `quota-config.json` (`amount_per_period`, `initial_credit`) lost digits beyond float64
  (`9007199254740993` → `…992`). It is now read exactly; the string, number and `null` forms all
  still work. A value with up to 15 significant digits reads the same as before.
- Tests: the outbox settlement test double still used the pre-v0.1.10 classification (401/403/any
  404 voided; unlisted statuses counted as a success). It now mirrors production, and one shared
  status table (`outbox/testdata/settle_classification_20261016.json`, every defined 4xx/5xx) is
  driven through both the double and the real billing client + settler. The four known-bug tests
  run unconditionally (`QUOTA_RUN_KNOWN_BUGS` is gone).

## [0.1.10] — 2026-10-16

Bug fixes from a vendor-style review (sign-up credit, settlement money paths,
CLI honesty, client routes). **No exported symbol was removed or changed; every
API change is an addition.** Some fixed bugs changed behaviour you could notice —
they are marked ⚠ and listed with what to check in
[`migrations/v0.1.9-to-v0.1.10/`](migrations/v0.1.9-to-v0.1.10/MIGRATION.md)
(read-only checker included).

### Sign-up credit (auth events)

- **Fixed: the receiver answered `404` to every event when mounted as documented.**
  `mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())` — the
  form in the examples and the path `SubscribeOnStartup` registers — never reached
  the handler, so no sign-up credit was ever granted. The receiver now answers any
  path ending in `/_webhooks/auth`; `http.StripPrefix` (the old workaround) still works.
  Non-POST on the path is `405`, other paths `404`, a body over 1 MiB `413`.
- ⚠ **Fixed: the default credit handler granted ANY tier's `credit_grant` on
  `org.created` / `user.org_assigned`**, including a paid tier's monthly refill
  (`trigger: "subscription_invoice_paid"`) — handed out as a gift each time a user
  joined an org on that tier. Only `trigger: "signup"` grants now (Python parity).
  A grant with no trigger is still treated as sign-up, with a warning.
- **Fixed: `billing.Client.GrantCredit` posted to `/billing/credits/grant`, which billing
  never served.** It now uses billing's real `POST /billing/{org}/promotional-credit`
  with a stable idempotency key (signature unchanged; `OrgID` is required). New:
  `GrantPromotionalCredit`, `ApplyCreditGrant` (`/apply-credit-grant`),
  `UpdateBalanceResponse`, and **`quota.BillingCreditGranter(nil)`** — a built-in
  granter (`credit_balance`/`persistent` → promotional credit, any other bucket →
  apply-credit-grant). `authevents.CreditGrantRequest` gained `IdempotencyKey`
  (`user:{u}:initial_credit:{tier}` by default — the Python runtime's key, never the
  event id), `DedupKey`, `Destination`, `Lifecycle`, `RolloverMax`.
- **New: secret-rotation overlap.** `AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS` (comma-separated)
  is accepted alongside the current secret; `ReceiverConfig.PreviousSecrets`,
  `ReceiverConfig.MaxBodyBytes`.
- ⚠ **`quotactl subscribe-events` fails loudly.** v0.1.9 exited `0` with an empty
  `subscription_id:` when registration failed, read only page 1 of the list, and
  re-running never applied a new secret or event list. It now uses the new strict
  `authevents.Subscribe`: any failure exits non-zero with the HTTP status and a
  remedy; a changed secret/event list on an existing subscription is an error naming
  `--rotate-secret`, which deletes and re-creates the subscription (auth cannot edit
  a secret). `SubscribeOnStartup` stays best-effort, now paginates, and logs an
  ERROR when the existing subscription's secret differs.

### Money paths (settlement fallback, metering)

- ⚠ **Fixed: a money event with no reservation id was settled under the key `""`.**
  Billing dedups that key per org forever, so the second and later such settlements
  were acknowledged as "replayed" and their money never taken. They are now voided
  and alerted (`no_settlement_key`), loudly.
- ⚠ **Fixed: a terminal event without `stopped_at` was priced up to settle time** — for
  the past-horizon fallback, hours or days after the stop. The stop now defaults to
  when the event entered the outbox.
- ⚠ **Fixed: `401`, `403` and a route `404` from billing voided money events.** A
  rotated key, a permission change or a wrong base URL now keeps them pending
  (retried); only billing's "Billing account not found" `404`, `400` and `422` void.
- **Documented, not changed:** `RecordUsage` sends an empty `Cost` / `PlatformFee` as
  absent, and billing then prices the row with its fallback rates and minimum charge.
  Always set both to `"0"` on a metering row (some callers rely on omission, so the
  library does not default them).
- **Fixed (internal HTTP client):** a redirect to another host carried the
  `X-API-Key` (now refused); a malformed base URL panicked (now an error); response
  bodies are capped at 10 MiB; org ids are path-escaped in every billing/payment URL.
- **Fixed:** the handler-ledger retry loop kept calling the handler after its context
  was cancelled; a malformed Lua reply from the counter script was a silent `429`
  instead of an error.

### Clients (billing / payment)

- **Fixed:** `payment.CreatePortalSession` → `POST /portal/{org}/session`,
  `payment.ListPaymentMethods` → `GET /payment-methods/{org}` (reads
  `payment_methods`); both 404'd or returned an empty list before.
  `CheckoutSession` gained `PaymentStatus` / `AmountTotal` (dollars, as payment sends it) — fulfil on
  `PaymentStatus == "paid"`. `UsageSummary` gained `Summary`, `StartDate`,
  `EndDate`, `GroupBy` (billing's real reply).
- **Documented, not re-pointed** (their request shapes do not map): `CheckQuota`,
  `GetSubscription`, `CancelSubscription` (billing host — subscriptions live on
  payment) and `payment.CreateCheckoutSession` answer 404; their doc comments say so.
- **Heartbeat:** billing serves no heartbeat route; the loop now stops after the first
  `404` (one INFO line) instead of warning every 60 s.
- **DynamoDB handler ledger:** a self-created table now has the `GSI1`/`GSI2` indexes
  `QueryByUser`, `QueryByStatus` and `DeleteUser` use (they failed with
  `ValidationException` before — including GDPR deletion). An existing table without
  them is reported at boot with the fix.

### Operations, config and observability

- **New: `config.Warnings()` / `Capabilities.ConfigWarnings`** — logged at boot: enforcement
  off, an accumulator `reset_period` the code does not know (it never resets), a
  limit naming no resource, unknown `billing_model` / `trigger` / `dedup` / `lifecycle`
  / `destination`. Warnings only; nothing that booted refuses.
- ⚠ **Threshold-alert cooldown is per org** (was per resource+tier: one tenant muted
  everyone). Alert payloads gained `org_id` and `reason`; `engine.Result` gained `OrgID`.
- **Fixed:** an unparseable `redis_url` printed its password in the log, in
  `quotactl capabilities` and in the startup error.
- **Fixed:** `Close` now removes the credit handler `Setup` registered (repeated Setup
  calls stacked handlers); `Capabilities` publication and `BillingHealthy` are locked.
- The paid-mode refusal names why paid mode is on (e.g. `AB0T_QUOTA_BILLING_URL is set`)
  and how to opt out (`Options.EnablePaid=&false`).
- `Authorization: bearer …` (any case) is recognised.
- A set-but-ignored `AB0T_QUOTA_CONFIG_PATH` is now a warning (the variable is
  `QUOTA_CONFIG_PATH`; the examples named the wrong one).

### CLI

- ⚠ `replay` / `backfill` exit non-zero when any event is not accepted; `backfill`
  requires `--target` and a secret (unless `--dry-run`). Their `--help` no longer
  prints the live `AB0T_AUTH_WEBHOOK_SECRET` (it was the flag default).
- ⚠ `events` / `delete-user` refuse (non-zero) instead of reading an empty in-process
  stub — `delete-user` used to print "deleted 0 rows" and exit 0. Use
  `q.LedgerStore` inside your service.
- `quotactl --version` reports the module version for `go install …@vX.Y.Z` builds
  (was `dev`).

### Docs

- README rewritten against the code; its minimal example is `examples/basic/main.go`
  verbatim (test-enforced) and the example now counts usage, so it actually limits.
  The env-var table lists only names the code reads.

## [v0.1.9] — Durable last-known-good tier (H8) + sustained tier-failure detection (RC4)

Parity with `ab0t-quota` v0.6.7. Non-breaking and purely additive — a consumer
wiring nothing new behaves exactly as before.

- **H8 — durable, cross-runtime last-known-good tier.** The Go LKG can now be backed
  by Redis (`providers/lkg_store.go`), so the never-downgrade guarantee survives a
  process restart during a billing outage (previously in-process only). The record is
  **wire-compatible with Python** (`quota:tier:lkg:{org}`, JSON `{"tier","ts"}` — key
  home `counters.TierLKGKey`), so a mixed Py/Go fleet shares one LKG per org. Auto-wires
  only where Redis is already configured; unconfigured consumers are unaffected.
- **RC4 — sustained tier-lookup-failure detection.** Per-org consecutive fetch-failure
  streaks escalate to an ERROR-level `tier_lookup_failing_sustained` signal at a
  threshold; **detection-only — serving is unchanged** (last-known-good still applies).
  1:1 with the Python RC4.

## [v0.1.8] — Tier resolution never downgrades a paying org on a billing hiccup

Go parity with `ab0t-quota` v0.6.6. Non-breaking and purely additive.

- `providers/mesh.go`: `GetTier` no longer swallows a lookup error to `nil` — it
  propagates `ErrTierLookupFailed` alongside the fallback default, so the cache's
  don't-cache-on-error guard actually engages.
- `providers/cache.go`: serves **last-known-good** tier on a fetch failure (never caches
  the free fallback); an unknown org still defaults (uncached); optional
  `SetLastKnownGoodMaxAge`.
- Documented parity gap vs Python: Go last-known-good is in-process (per-instance, lost on
  restart) — a durable-store backing is a tracked follow-up.

## [v0.1.7] — Recount-before-deny, read-repair, and one-call recalculate

Go parity with `ab0t-quota` (Python). Non-breaking and purely additive.

- **Recount-before-deny** in `Engine.Check` (and the `Acquire` twin): a would-be
  deny first recomputes the true usage from the resource's *type* source — a
  **gauge** from `Engine.ObservedUsage` (live count), a **meter/accumulator** from
  `Engine.AccumulatorUsage` (durable period-ledger sum) — repairs a drifted
  counter, and only denies if the truth is really over.
- **Type-aware fail direction** on a truth-source outage (money-safety): a gauge
  fails *open*; a meter keeps the last-known deny (no overspend past the cap).
  Both alert.
- **`Engine.ReconcileOrg(org, resourceKey?)`** returns a structured before→after
  `ReconcileReport`; **`Engine.MaybeReadRepair`** is the throttled read-repair
  hook. Idempotent, safe anytime — powers a "Recalculate usage" button.
- **Detection**: engine drift routes to `alerts.Manager`, and the drift metric
  gained a `type` label (`Metric.ResourceType` / `RecordDriftTyped`).

Wire `ObservedUsageProvider` / `AccumulatorUsageProvider` into `quota.Setup` to
opt in; a service that wires neither behaves exactly as before. (Documented gap:
the observed-usage provider is org-total-only, so recount/repair operate at the
org level; a per-user denial keeps its cache decision.)

## [v0.1.5] — declared, not discovered

**⚠️ Breaking, action-required — shipped as a PATCH**, matching its Python sibling
(`ab0t-quota` 0.6.3). **The version number is not a reliable breakage signal for
this release — the migration notes below are.** Read them before upgrading.

### ⚠️ Action required — migration notes

1. **An undeclared counter store refuses to start (QUOTA-CFG-001).**
   `storage.redis_url` absent/null with no `QUOTA_REDIS_URL` was silently an
   in-memory, per-process counter; now it is a typed startup error. For
   single-process dev, declare it: `"redis_url": "memory://"`.
2. **A DECLARED but unreachable Redis now RETRIES then REFUSES —
   it no longer silently degrades to in-memory.** Boot retries the
   *unreachable* kind for up to `storage.connect_retry_seconds` (default 30;
   `0` = fail immediately), then refuses with a typed reachability error.
   Authentication failures refuse immediately — retrying a wrong password is
   just a slower wrong password. Previously the process served with counters
   local to each replica **and reported healthy**: every replica
   admitted the full limit and a restart zeroed usage. If your service now
   refuses at boot, it was previously serving unmetered — the refusal is the
   fix working. Runtime is unchanged: a Redis that fails AFTER boot still
   degrades loud-not-fatal.
3. **Invented AWS regions removed** (us-west-2/us-east-1): declare
   `storage.dynamodb_region` / `outbox.sns_region` or let the platform's
   `AWS_REGION` resolve via the SDK; nothing resolving is QUOTA-CFG-009/010.
4. **Generic `AUTH_SERVICE_URL` fallback removed** — only `AB0T_AUTH_AUTH_URL`
   is consulted for the auth-events subscription.
5. **Both `storage.redis_password` and a URL-embedded password set and
   differing now logs a WARNING naming the winning source** (the declared
   field wins — unchanged direction, D-5(a)).

### New

* **`quotactl provision`** — emit conforming infra artifacts
  (`--emit compose|terraform|acl|iam`) generated from the same registry the
  boot gates enforce, or `--local` for one verified local dev Redis. Never
  creates cloud resources. **`quotactl doctor`** — production-posture
  grading over the boot evaluators; `--json` extends the report schema with
  a posture section; reports what it could not check as `not_checked`.
  Honest asymmetry, stated in its own output: Go's `doctor` runs full
  `quota.Setup` (may create the declared tables; loads the counter script)
  and never claims read-only. Verb names, exit taxonomy (0/1/2/3/4) and
  JSON schemas are pinned cross-runtime by conformance `ST-CLI-1`; the verb
  is `provision`, not `setup` (see `CONSUMING.md`).
* **New `storage` keys**: `connect_retry_seconds` (the D-2 lever above);
  `keyspace_version` / `keyspace_dual_write` — counter key shape v1/v2 +
  migration dual-write. **Defaults v1 / no-dual: an existing consumer
  changing nothing is unaffected.** Declaring v2/dual is refused by
  `config.Validate` until the Go dual path lands (declared-but-unwired must
  refuse, never silently no-op).
* **`QUOTA-CFG-nnn` is ONE cross-runtime registry** (D-13):
  `conformance/quota-cfg-registry.json`, byte-identical with the Python
  repo, sync-checked; Go's 009/010 stand, Python adopted them. Every code
  is documented with its remedy in the Python repo's `docs/error-codes.md`.

## [v0.1.4] — 2026-07-12

**Theme: money-safety by loud refusal. The library owns the integrity machinery
(idempotency, outbox delivery, reconciliation, crash recovery) so you don't.**

Parity with the Python `ab0t-quota` 0.6.1 integrity surface — same rules, same
structure.

### ⚠️ Action required — a mis-configured service now refuses to start

1. **Paid billing needs a durable outbox.** With paid billing enabled and no
   durable store, startup **refuses**. Use DynamoDB (default) or confirmed-durable
   Redis.
2. **Wire the emit path.** Set `outbox.sns_topic_arn`. Until it's set, paid billing
   correctly refuses to start — a durable outbox with nothing publishing into it
   delivers nothing.
3. **Redis for the outbox must be durable + non-evicting**, or assert
   `outbox.redis_durability_confirmed: true` where `CONFIG` is unavailable.
4. **No custom Redis key prefix** — it forks the keyspace and breaks cross-runtime
   sharing.
5. **Operator, once per env (~30s): confirm Redis is not clustered** (multi-key Lua
   fails `CROSSSLOT` on a cluster without a keyspace migration).

### The one thing you still reason about

A custom auth-event handler with a side effect and no business idempotency key can
double-run on crash-recovery replay. The built-in credit-grant handler is safe.
Pass a key to any custom side-effecting handler.

### Compatibility

- Requires the billing service to expose the settlement endpoint (`/settle`) and
  an inputs-aware commit. Confirm your billing deployment supports these before
  adopting.

See `docs/INTEGRATION_RUNBOOK.md` and `README.md` for setup.
