# Architecture review — should the quota library mount endpoints into consumer services?

**Date:** 2026-06-11
**Scope:** the system design of ab0t-quota (Python v0.5.2) and, by extension, the planned ab0t-quota-go port. Companion to [`review_20260611.md`](review_20260611.md) and [`review_20260611_addendum.md`](review_20260611_addendum.md), which audit the spec *as written*. This document asks the prior question: **is the spec porting the right architecture?**

---

## 0. The question

ab0t-quota's signature move is that `setup_quota(app)` *mounts things into the consumer's HTTP service*: ~25 routes (quota reads, a billing/payments BFF proxy, a Stripe webhook handler with money side-effects, an auth-event webhook receiver, a static checkout page), two middlewares, three background workers, and several outbound integrations. The Go spec faithfully reproduces this shape.

The question: is "the library injects a commerce stack into every consumer" the right engineering for a *mesh* of services — or an artifact of how the first consumer got built? The Go port is the natural fork point to decide, because whatever shape ships in `ab0t-quota-go` v0.1.0 will be re-ported to every future language and re-deployed into every future consumer.

**Short answer:** the *quota engine* belongs in a client library; most of the rest does not. The library currently bundles four different planes with four different owners, lifecycles, and trust levels, and the bundling — not any single component — is the source of most of the bugs, parity burden, and operational risk found in the audit. There is a smaller, better-bounded architecture that keeps the drop-in promise, and the Go port can ship it *first* rather than inherit the debt.

---

## 1. What the library actually is today: four planes in one package

Decomposing `setup_quota(app, enable_paid=True)` by what each piece *is*, rather than where it lives:

| Plane | Components | Hot path? | Trust level | Natural owner |
|---|---|---|---|---|
| **1. Enforcement plane** | engine, counters (Redis), tier provider, registry, bundles, middleware, persistence/snapshot | yes (per provisioning op / per request) | consumer's own data | **the consumer process** — this is genuinely a client-library problem |
| **2. Commerce BFF plane** | ~20 proxy routes `/api/billing/*`, `/api/payments/*` (checkout, portal, payment methods, top-up, invoices), checkout-intent store, `/checkout/success` template | no (human-paced UI) | consumer's mesh API keys; user-facing | a frontend/BFF concern — *currently* forced into the consumer by the JWT audience model |
| **3. Money-event plane** | Stripe webhook proxy (signature verify → invoice credit grant → downgrade reset → forward), auth-event receiver + default signup-credit handler, handler ledger, subscription credit logic | no (event-paced) | **Stripe secrets, credit-granting authority** | payment/billing services — the mesh's own docs flag this ("Alternative C") |
| **4. Control/ops plane** | tier-catalog publish, auto-subscribe, plan sync (proposed), `quotactl` CLI, backfill/replay | no (deploy-paced) | admin tokens | CI/ops scripts, not app startup |

The README sells plane 1 ("quota enforcement, sub-5ms"). The VISION sells planes 1–4 as one drop-in ("fill out a config file, get a fully working commercial system"). Both are honest — but the *package boundary* treats four planes as one deployable, and that's the design decision worth re-examining.

## 2. Evidence: how the bundle behaves in production

These are observations from the audit, not hypotheticals:

1. **The only production consumer turns half of it off.** sandbox-platform runs `enable_rate_limit=False` (uses slowapi instead) and `enable_quota_api=False` (rebuilt the quota read routes itself because the lib's auth-injection model didn't fit its `CurrentUser`/`CostsReader` deps) — `sandbox-platform/app/quota.py:146-148`. What it actually uses: the engine (via its own 430-line wrapper), the paid proxy, the webhook receiver, the emitter. The two most "library-like" mounted surfaces were rejected by the first real user; the parts it kept are precisely planes 2–3 — the ones that arguably shouldn't be in a library at all.
2. **The hot-path justification is weaker than the README claims.** Quota checks fire on *provisioning operations* (sandbox create, `main.py:343-346`) — a few per minute, not per request. The one true per-request path (rate-limit middleware) is disabled in production. Today, nothing deployed needs in-process sub-5ms; a 5–25ms HTTP check (the documented bridge-mode latency) would serve the live workload without observable difference. (This does **not** mean the engine should go — future high-QPS consumers like an LLM gateway would need it — but it means in-process enforcement is a *mode for a consumer class*, not the architecture's load-bearing premise.)
3. **The money-event plane is where the live bugs cluster.** All three production bugs found in this audit sit in planes 3–4: the default credit-grant handler that never registers (`setup.py:938` NameError), the dead workspace-resolution call (nonexistent auth endpoint), and the auth v1 webhook signature-vs-body mismatch. None are in the enforcement plane. Complex, distributed, money-touching logic was pushed into a library where it runs unobserved in N consumer processes — and broke silently in three places. The 2026-05-18 incident (Stripe secret-list drift between lib proxy and payment-service) is the same class: a trust boundary smeared across deployables.
4. **The parity tax is a direct cost of the data-plane coupling.** Because consumers in different languages write the *same Redis keys with the same float formatting*, the Go port must replicate `INCRBYFLOAT` semantics, key shapes, period-key formats, canonical-JSON hashes — 17 parity-matrix rows of wire-level trivia (see the review). Each future language pays it again. If enforcement state were behind a service API (or per-consumer Redis), the parity surface would be one HTTP contract.
5. **Library-version skew = behavioral skew for money logic.** A fix to the credit-grant flow ships by re-releasing the lib and redeploying *every consumer*. With server-side money logic, it ships once. For quota *enforcement* semantics, skew between consumers is tolerable (each consumer's quotas are its own); for *credit-granting* semantics it is not (it's the platform's money).
6. **Privilege sprawl.** Today every paid consumer's env holds: a billing write-capable mesh key, a payment key, a Stripe webhook secret, an auth **admin token** (events.subscribe), a webhook HMAC secret. The mesh's own security model (gateway pattern, least privilege, consumer keys read-mostly) is undermined by the library requiring credit-granting and subscription-admin credentials in every consumer.

## 3. Why it was built this way (steelman)

The current design is not careless; it follows from three real constraints:

- **C-1: The JWT audience model.** Mesh JWTs are single-audience (`aud=sandbox-platform` is rejected by billing). A browser holding a sandbox JWT *cannot* call billing/payment directly. Something server-side in the consumer must proxy with the consumer's API key — hence the BFF plane. This is the strongest constraint and it's structural, not accidental.
- **C-2: Webhooks need a public, consumer-owned endpoint.** Auth events and Stripe events must land somewhere addressable; the consumer is already public; the library mounting the receiver gives signature verification and idempotency for free.
- **C-3: The drop-in promise as product strategy.** VISION.md: a new mesh service fills out config and gets a commercial system with zero custom billing code. For a one-team platform racing to onboard services, one `setup_quota()` call genuinely delivers that. The bundle *is* the product.

Any alternative must satisfy C-1/C-2's constraints and preserve C-3's outcome ("a new consumer gets commerce in a day") — otherwise it's an aesthetic refactor that loses the plot.

## 4. Requirements, restated without the current design baked in

- **R1** Enforce per-org/per-user resource quotas with tier-aware limits; provisioning-op latency budget ≤ ~50ms, high-QPS option for future per-request consumers.
- **R2** Tier/limit/credit policy is *consumer-owned config*, no code deploy to change (and per locked decision D5: consumer pricing never stored centrally).
- **R3** Consumer frontends can display balance/usage/invoices and run checkout/portal flows, despite single-audience JWTs (C-1).
- **R4** Signup credits fire on auth events; subscription credits fire on invoice events; both idempotent, observable, replayable.
- **R5** Works for internal mesh services *and* future external/third-party consumers (no shared infra assumption).
- **R6** Multi-language: adding a language costs a thin client, not a re-implementation of money logic.
- **R7** A new consumer reaches working commerce in ≤ a day (preserve C-3).

## 5. Options

### Option A — Status quo: fat embedded library (what the Go spec ports)
Everything in §1 in-process. **Pros:** proven with one consumer; zero new services; latency floor. **Cons:** §2 in full — parity tax per language, money logic × N deployables, privilege sprawl, version skew, every consumer is a payment-adjacent service. Porting it to Go costs ~4 weeks now and repeats per language.

### Option B — Quota as a service for everyone ("bridge mode by default")
Billing already hosts the mesh quota API (`POST /billing/quota/{service}/{org}/check/{resource}`, increment/decrement/usage, catalog publish) and the lib already ships the bridge client + caches. Thin SDKs per language (~500 lines). **Pros:** kills the parity tax; one enforcement implementation; trivially multi-language. **Cons:** billing becomes a hot-path availability dependency for every consumer request (needs explicit fail-open/closed policy per resource); high-QPS rate counting over HTTP is expensive; doesn't by itself solve planes 2–4 (the proxy/webhook/money problems remain wherever they live).

### Option C — Split the planes (recommended; detailed in §6)
Keep plane 1 as the embedded library (with engine-local *and* bridge as first-class modes per consumer class). Move plane 3's trust boundaries server-side into payment/billing. Shrink plane 2 to what C-1 strictly requires (or dissolve it via token-exchange/widgets). Move plane 4 to deploy-time tooling. **Pros:** each plane gets its natural owner; Go port scope shrinks ~40%; money logic ships once; fixes the bug-cluster class structurally. **Cons:** requires changes in payment/billing services (cross-team work); two consumers' worth of migration; more moving parts to *describe* even though fewer to *deploy per consumer*.

### Option D — Sidecar enforcement (Envoy-style local agent)
A quota sidecar per consumer pod; consumers call localhost gRPC. **Rejected for now:** this platform runs docker-compose on a small fleet; a sidecar adds operational machinery to solve a problem (polyglot in-process parity) that Option C's bridge mode already solves more cheaply. Revisit if/when the EKS migration (per the ops pipeline roadmap) makes sidecar injection free.

### Option E — Gateway-level enforcement
Move quota to the API Gateway (8010) / Traefik layer. **Partial fit only:** generic request-rate limits belong there eventually, but the interesting quotas here are *domain-resource* quotas (concurrent sandboxes, monthly spend) that are checked at provisioning decision points inside the consumer's business logic, with per-user partitions and bundle semantics. A gateway can't see those. Keep as a far-future destination for `api.requests_per_hour` only — which, notably, production already enforces with slowapi at the route level instead of this library.

### Comparison against requirements

| | R1 latency | R2 policy | R3 frontend | R4 events | R5 external | R6 polyglot | R7 day-one |
|---|---|---|---|---|---|---|---|
| A status quo | ✅ | ✅ | ✅ | ⚠️ works, bug-prone ×N | ⚠️ byo_redis only | ❌ full re-port each | ✅ |
| B service-only | ⚠️ fail-policy needed; rate counters costly | ✅ | ➖ unchanged | ➖ unchanged | ✅ | ✅ | ✅ |
| C split planes | ✅ (mode per class) | ✅ | ✅ (smaller surface) | ✅ ships once | ✅ | ✅ thin port | ✅ (config + 1 call, same promise) |
| D sidecar | ✅ | ✅ | ➖ | ➖ | ⚠️ | ✅ | ❌ ops burden |
| E gateway | ❌ domain quotas invisible | ❌ | ➖ | ➖ | ➖ | ✅ | ➖ |

## 6. Recommended target architecture (Option C, concrete)

```
                           ┌─────────────────────────────────────────────┐
                           │                AUTH SERVICE                  │
                           │  events ──► ONE publisher, signs the bytes   │
                           └───────────────┬─────────────────────────────┘
                                           │ internal mesh event (HMAC)
   Stripe ──► PAYMENT SERVICE ─────────────┤
              │ owns Stripe trust boundary │
              │ verifies sig ONCE          ▼
              │ invoice.paid ──► normalized internal event ──► consumer's
              │ subscription.updated ─┘                        single receiver
              │
              ▼
        BILLING SERVICE
        │ balance / reserve / commit / credit grants (idempotent, as today)
        │ mesh quota API (bridge mode data plane)
        │ tier catalog (quota policy only — D5 preserved)
        │
        ▼
 ┌─────────────────────────── CONSUMER SERVICE ───────────────────────────┐
 │  ab0t-quota-go  (the actual library)                                   │
 │  ┌──────────────────────────────┐  ┌─────────────────────────────────┐ │
 │  │ ENFORCEMENT (plane 1)        │  │ EVENTS (plane 3, thin half)     │ │
 │  │ engine: local│byo_redis│bridge│  │ ONE receiver /_webhooks/mesh   │ │
 │  │ check/increment/bundles      │  │ handler registry + ledger      │ │
 │  │ middleware (opt-in)          │  │ policy applied LOCALLY from    │ │
 │  │ read API (opt-in)            │  │ quota-config (D5 intact)       │ │
 │  └──────────────────────────────┘  └─────────────────────────────────┘ │
 │  ┌──────────────────────────────┐                                      │
 │  │ COMMERCE READS (plane 2,     │   NOT in the library anymore:        │
 │  │ shrunk): balance/usage/      │   ✗ Stripe webhook handling          │
 │  │ invoices proxy OR token-     │   ✗ credit-grant decision execution  │
 │  │ exchange helper (3-5 routes) │   ✗ checkout/portal/methods proxy    │
 │  └──────────────────────────────┘   (Stripe-hosted + widgets instead)  │
 └─────────────────────────────────────────────────────────────────────────┘
        quotactl (plane 4): subscribe, catalog-publish, replay — CI/deploy step
```

### 6.1 Plane 1 — keep embedded, make modes first-class
The engine, counters, tier providers, bundles, middleware, and optional read API stay a client library — this is the legitimately library-shaped problem, and it's the part the Go spec already specifies well (after the review's C1–C3 fixes). Two changes:
- **Mode is a consumer-class decision, declared in config**: `engine_mode: local` (shared infra, internal mesh), `byo_redis` (external consumer, own Redis), `bridge` (low-QPS or no-infra consumers — *the recommended default for new consumers*, given §2.2's evidence that provisioning-op QPS doesn't need in-process). The Go port should implement **bridge mode in v0.1.0, not defer it** — it is the mode that kills the cross-language parity tax, and the server side already exists in billing.
- **Library, not framework**: the Go lib returns handlers/middleware for the consumer to mount (`qctx.Routes()`, `qctx.Middleware()`), never composes lifespans or registers global state invisibly. (The Go spec's `Mount(mux, prefix)` is already closer to this than Python — keep going: no package-global handler registry as the primary API, per review §5.7.)

### 6.2 Plane 3 — one trust boundary per secret, money logic ships once
- **Stripe trust moves entirely into payment-service** (the lib docs' own "Alternative C"). Payment verifies Stripe signatures (it already does), then emits **normalized internal mesh events** — `payment.invoice.paid {org_id, plan_id, invoice_id}`, `payment.subscription.changed {org_id, old_price, new_price}` — to each subscribed consumer's receiver, signed with the *mesh* webhook scheme. Consumers never hold Stripe secrets. The secret-list-drift incident class disappears; so does per-consumer Stripe SDK dependency.
- **Credit policy stays consumer-side (D5 preserved).** The consumer's thin handler — shipped by the library, the same `Idempotent`-wrapped pattern as today — receives `payment.invoice.paid`, reads its *local* `quota-config.json` `credit_grant`, and calls billing's `apply-credit-grant` with the same idempotency key (`invoice:{id}:credit_grant`). Nothing about who *decides* the amount changes; only who *verifies Stripe* changes. This resolves the apparent tension with locked decision D5: policy resolution remains consumer-side; the platform centralizes only transport and signature verification.
- **One receiver, one envelope, one signature scheme** for auth events *and* payment events: `POST {prefix}/_webhooks/mesh`. This is also the structural fix for bug A1 (the auth v1 sign-vs-send mismatch): a single, tested publisher implementation signing the exact bytes it sends, instead of two divergent publishers and a receiver that tolerates both. The handler-ledger framework (delivery dedup, business dedup, retry, replay) is the best part of the current design — keep it exactly, it now serves both event sources.
- **Signup-credit default handler** stays a library feature (it's policy-from-config, the right kind of drop-in) — but the Go implementation gets the conformance test that would have caught the Python `provider` NameError.

### 6.3 Plane 2 — shrink the BFF to what C-1 actually forces
Inventory what the 20 proxy routes do and what *minimally* requires a consumer-side hop:

| Today's proxy routes | Actually needed consumer-side? |
|---|---|
| checkout session create, anonymous checkout, `/checkout/success` page | **No** — Stripe Checkout is already Stripe-hosted; payment-service can own session creation directly with `success_url` pointing back to the consumer. The verification-token dance already lives in payment. |
| customer portal | **No** — Stripe-hosted; payment-service mints the session. |
| payment methods CRUD, top-up | **Mostly no** — same pattern: payment-service endpoints + a short-lived scoped token (below). |
| balance, usage summary/records, transactions, invoices list/pdf, subscriptions list | **This is the real C-1 residue** — read-only display data the frontend needs. |

Two ways to serve the residue; both smaller than today:
- **(a) Keep a read-only proxy** — 5–6 GET routes, reader-permission key only (no write-capable payment key in consumers), error-masked. This is the conservative move and is what the Go `billing/router.go` should shrink to.
- **(b) Token exchange (the better endgame):** auth-service issues short-lived, read-scoped, billing-audience tokens in exchange for a valid consumer JWT (`POST /auth/token-exchange`, RFC 8693-shaped). The frontend then calls billing/payment *directly*; the consumer-side proxy disappears entirely; the existing `authmesh-widgets` program ships a `billing-balance` widget that does this out of the box — which also gives every consumer a consistent billing UI for free (stronger drop-in than today's JSON proxy). This is an auth-service feature request, not a quota-lib feature — file it; until it lands, (a).

Either way: **checkout/portal/anonymous-account-creation routes leave the consumer process.** A consumer service should not be able to create accounts and move money as a side effect of importing a quota library.

### 6.4 Plane 4 — deploy-time, not startup-time
Catalog publish, plan sync, and event subscription are operator state changes — idempotent, audited, run-on-deploy (house pattern: idempotent ops scripts with explicit phases). Move them from startup side effects into `quotactl` invocations in the deploy pipeline (`quotactl catalog-publish`, `quotactl subscribe-events` — the latter already exists). Startup keeps a *verification* (warn if subscription/catalog stale), not a *mutation*. This removes the auth admin token from consumer runtime env entirely — it lives with deploy credentials, matching the platform's runtime-vs-ops IAM separation convention.

## 7. What this means for ab0t-quota-go specifically

The Go port is the cheapest possible moment to adopt this: nothing exists yet, and the Python lib doesn't have to change first (the planes can be split in Go while Python continues as-is for sandbox-platform).

**v0.1.0 scope under Option C** (replaces PRODUCT_SPEC §"Implementation order"):
1. `config` + `engine` + `counters` + `providers` + `registry` + `messages` + `middleware` — as specced, with the review's C1–C3 fixes. *(unchanged)*
2. **`bridge` client mode** — promote from "out of scope" to v0.1.0. It's a small HTTP client against billing's existing API and it is the multi-language strategy. *(changed)*
3. `authevents` receiver + registry + `handlerledger` — as specced (with C6/C7 fixes), but designed around the **unified mesh envelope** from day one (accept today's two auth publisher formats as documented legacy inputs).
4. `billing` client: balance/usage/transactions/tier reads + reserve/commit/refund + credit-grant calls. **Read-only proxy router (option a), 5–6 routes.** *(shrunk from ~20)*
5. `payment` client: plans + subscriptions/invoices reads only. **No checkout/portal/methods proxy, no Stripe webhook handling, no `stripe-go` dependency, no checkout-intent store, no HTML templates.** *(removed)*
6. `cmd/quotactl`: as specced, plus `catalog-publish`. *(grown by one subcommand)*
7. **Config gate:** `LoadConfig` errors on `billing_model: subscription_with_credits` until the payment-service event fan-out exists — explicit, loud, honest about what v0.1.0 supports (this also resolves review C4 cleanly: option (b) now, option (a) becomes unnecessary).

Net effect on the 4-week estimate: roughly −1.5 weeks (proxy + Stripe machinery + templates) +0.5 week (bridge client) ⇒ **~3 weeks, with a smaller blast radius and no money-moving code in v0.1.0 at all.**

**Cross-service asks this creates** (file as tickets; none block Go v0.1.0):
1. payment-service: internal event fan-out for `invoice.paid` / `subscription.changed` (Alternative C — already sketched in their architecture doc).
2. auth-service: fix the v1 publisher sign-vs-send bug (needed regardless); longer-term, token-exchange endpoint for §6.3(b).
3. billing-service: confirm the mesh quota API's rate-counter performance envelope for bridge-mode consumers (it exists; it needs a load number).

## 8. Migration path (Python side, non-blocking, reversible)

- **Phase 0** — ADR recording the plane split (this doc → decision); no code.
- **Phase 1** — Go v0.1.0 ships per §7. New Go consumers onboard with bridge or byo_redis mode. Python lib untouched.
- **Phase 2** — payment-service event fan-out lands. Python lib gains a handler for the internal `payment.invoice.paid` event behind a flag; the Stripe proxy path stays as fallback (mirrors the existing `ENABLE_LEGACY_SUBSCRIPTION_INVOICE_CREDIT` phase-out pattern the team already uses).
- **Phase 3** — sandbox-platform flips to the internal event; its Stripe webhook secret is deleted; lib's Stripe proxy marked deprecated.
- **Phase 4** — token-exchange / widgets replace the read proxy where frontends adopt them; the proxy quietly stays for the rest (it's small now).
- **Rollback at every phase** is "keep using the old path" — both paths are idempotent on the same keys (`invoice:{id}:credit_grant`), so double-delivery during transition is safe *by the system's own dedup design*. That's the payoff of the ledger framework, and the strongest reason to migrate event-by-event rather than big-bang.

## 9. Honest counterarguments, and when to keep Option A

- **"It works and there's one team."** True, and if the platform freezes at one paid consumer + one language, Option A's costs never compound and this whole document is premature optimization. The trigger that makes it compound is exactly the thing being specced: a second language. If the Go port is real, the fork in the road is now.
- **"More services = more failure modes."** The proposal adds *zero* new services (payment/billing/auth all exist; they gain endpoints). It *removes* deployable money-logic copies (N consumers → 1 payment-service).
- **"Bridge mode makes billing a single point of failure for enforcement."** Engine-local already makes shared Redis exactly that for internal consumers; nothing gets worse, and per-resource fail-open/closed policy (which the review demands be specified anyway, M15/A2) covers both modes with one mechanism. High-QPS consumers keep engine mode.
- **"The drop-in promise dies."** It doesn't — it gets *stronger*. New consumer, Option C: `go get`, one config file, `quota.Setup`, `quotactl subscribe-events && quotactl catalog-publish` in deploy. Commerce UI via hosted Stripe + widgets instead of 20 mounted JSON routes the consumer still had to build a frontend for anyway. The day-one experience is equal; the day-300 experience (upgrades, audits, incidents) is dramatically better.
- **"D5 forbids centralizing billing policy."** Addressed structurally in §6.2 — policy *evaluation* stays consumer-side with local config; only Stripe *signature verification* centralizes. D5's rationale (don't leak consumer pricing into a central store; don't put billing in the policy-resolution path) is fully preserved.

## 10. Decisions requested (ADR-style)

| # | Decision | Recommendation |
|---|---|---|
| D-A | Go v0.1.0 ports the full proxy/Stripe surface vs. ships the plane-split scope (§7) | **plane-split scope** |
| D-B | Bridge mode in Go v0.1.0 | **yes** (it's the polyglot strategy) |
| D-C | Stripe trust boundary: per-consumer (status quo) vs. payment-service fan-out (Alternative C) | **payment-service**, phased per §8 |
| D-D | Commerce reads: keep read-only proxy vs. token-exchange/widgets | **proxy now, token-exchange as auth-service ticket** |
| D-E | Catalog publish + subscribe: startup side effect vs. deploy step | **deploy step** (`quotactl`), startup verifies only |
| D-F | Unified mesh event envelope (one publisher contract for auth + payment events) | **yes** — and it's the durable fix for bug A1 |
| D-G | Default mode for *new* consumers | **bridge**, engine-local reserved for measured high-QPS need |

If D-A through D-C land as recommended, update `PRODUCT_SPEC.md` §"Repository layout" (drop `payment/router.go` checkout/portal surface, drop Stripe deps, add `bridge/`), §"Out of scope" (move bridge *in*, move Stripe proxy *out* with a pointer to the payment-service ticket), and §"Implementation order" (per §7). The two review documents' findings remain valid for everything that stays.
