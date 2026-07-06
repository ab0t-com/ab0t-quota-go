# Per-service consumer tokens — one `AB0T_QUOTA_SERVICE_TOKEN` can't authenticate to two provider orgs

**Status:** [x] FIXED 2026-07-06 (commit `69836dd`) — backward-compatible; released with mlplatform onboarding
**Priority:** 🟠 P1 — blocked any consumer that meters against BOTH billing AND payment (e.g. mlplatform/Kaia)
**Component:** `ab0t-quota-go` — `mesh/urls.go`, `billing/client.go`, `payment/client.go`
**Found:** wiring mlplatform's minted keys into `AB0T_QUOTA_*` (the wiring exposed the gap)

## Problem
`mesh.URLs` carried a single `Token` (from `AB0T_QUOTA_SERVICE_TOKEN`) that BOTH `billing.New` and `payment.New` used as the `X-API-Key` (`billing/client.go`, `payment/client.go`). But the ab0t mesh mints a **separate scoped key per provider org** — billing's key is issued in the billing org and is **not valid at payment**, and vice-versa. So a consumer of both services had no way to present the right key to each: one env var, two required keys.

## Fix (backward-compatible)
- `mesh/urls.go`: added `EnvBillingTok = "AB0T_QUOTA_BILLING_TOKEN"` + `EnvPaymentTok = "AB0T_QUOTA_PAYMENT_TOKEN"`; `URLs` gained `BillingToken` + `PaymentToken`; `Resolve()` reads each, **falling back to `AB0T_QUOTA_SERVICE_TOKEN`** when unset (single-key / legacy configs keep working).
- `billing/client.go` → `httpx.New(u.Billing, u.BillingToken)`; `payment/client.go` → `httpx.New(u.Payment, u.PaymentToken)`.
- `firstNonEmpty` helper for the fallback.

## Why it's safe
Purely additive. A deployment with only `AB0T_QUOTA_SERVICE_TOKEN` set behaves exactly as before (both per-service tokens fall back to it). Builds + `mesh`/`billing`/`payment` tests pass; mlplatform builds against the updated lib.

## Driver / consumer
mlplatform (Kaia) mints two mesh keys via `authsetup run 07` (billing key `781ab354`, payment key `8217d7e0`) and wires them into `AB0T_QUOTA_BILLING_TOKEN` + `AB0T_QUOTA_PAYMENT_TOKEN`. See `shared/ab0t-setup-go/tickets/2026-07-06-privileged-consumer-provisioning-platform/` and the mesh-onboarding memory.

## Acceptance
- [x] `billing` + `payment` clients use their own token.
- [x] Both fall back to the shared `AB0T_QUOTA_SERVICE_TOKEN`.
- [x] Backward-compatible (single-token configs unchanged); tests green.
- [x] `.env.example` in consumers documents the two vars (done in mlplatform).
