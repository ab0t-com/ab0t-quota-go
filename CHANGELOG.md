# Changelog

All notable changes to `ab0t-quota-go`. Semantic versioning: a change that
requires you to do something to keep working is at least a MINOR.

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
