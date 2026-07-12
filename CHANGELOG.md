# Changelog

All notable changes to `ab0t-quota-go`. Semantic versioning: a change that
requires you to do something to keep working is at least a MINOR.

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
