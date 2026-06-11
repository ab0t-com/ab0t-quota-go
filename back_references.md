# Back-references — what the implementer should keep open

Quick-reference index of every repo, file, service, endpoint, and doc
that informs the Go port. The engineer working on `ab0t-quota-go`
shouldn't have to spend the first day discovering this.

Treat this as a reading list — skim it once, dive into specific entries
as each phase of [`PRODUCT_SPEC.md`](PRODUCT_SPEC.md) comes up.

> **Revision 2** (2026-06-11) incorporates review corrections. Original
> rev 1 of this file had 5 endpoint errors + 9 missing endpoints +
> nonexistent skills references; all fixed below.

---

## The Python lib (source of truth for contracts)

**Repo:** `github.com/ab0t-com/ab0t-quota`
**Local:** `/home/ubuntu/infra/infra/code/shared/ab0t-quota/`
**Status:** Production, latest tag v0.5.2.

### Top-level layout

| Path | What it contains |
|---|---|
| `ab0t_quota/__init__.py` | Public exports — mirror these as Go package-level identifiers |
| `ab0t_quota/__main__.py` | CLI entry — every flag and subcommand maps 1:1 to `cmd/quotactl/` |
| `ab0t_quota/setup.py` | `setup_quota(app)` — the one-line drop-in. The Go `quota.Setup` equivalent |
| `ab0t_quota/engine.py` | `QuotaEngine` — check/increment/decrement core. Go: `engine/engine.go` |
| `ab0t_quota/config.py` | JSON config loader + search paths. Go: `config/load.go` |
| `ab0t_quota/auth_events.py` | Registry + receiver + primitives. Go: `authevents/` package |
| `ab0t_quota/handler_ledger.py` | LedgerStore + @idempotent + ctx. Go: `handlerledger/` package |
| `ab0t_quota/billing/` | Billing **client + proxy router + lifecycle emitter + subscription credit + heartbeat**. Go: `billing/`. Note: `clients.py` holds BOTH BillingServiceClient + PaymentServiceClient (not separate `client.py`). |
| `ab0t_quota/counters/` | Counter impls — **plain Redis ops, no Lua** (rev 1 of this doc claimed Lua scripts; they don't exist). Go: `counters/` |
| `ab0t_quota/models/core.py` | TierConfig, CreditGrant, ResourceDef Pydantic models with cross-field validators. Go: `config/tier.go` etc. |
| `ab0t_quota/persistence.py` | DDB-backed quota state + snapshot worker. **Auto-creates the DDB table** (Go should NOT — operator-provisioned). Go: `persistence/` |
| `ab0t_quota/middleware.py` | `QuotaGuard` FastAPI middleware. Go: `middleware/guard.go` |
| `ab0t_quota/providers.py` | TierProvider implementations including Redis-cached `AuthServiceTierProvider` (`quota:tier:{org_id}`, TTL config). Go: `providers/`. |
| `ab0t_quota/alerts.py` | AlertManager + dispatchers. `LogAlertDispatcher` always active; `WebhookAlertDispatcher` SSRF-guarded (HTTPS-only, blocks loopback/private/reserved IPs). Go: `alerts/` |
| `ab0t_quota/registry.py` | ResourceRegistry. Go: `registry/` |
| `ab0t_quota/messages.py` | 429 message builder. Has hardcoded `ACTION_HINTS` + `UPGRADE_TIER_MAP` tagged `TODO(public-mesh-ga)`. **Go must make these config-driven from day one.** Go: `messages/builder.go` |
| `ab0t_quota/caches.py` | Bridge mode TTL caches (tier TTL from `bridge_cache.tier_ttl_seconds`; decision TTL default 1s). **Out of scope for Go v0.1.0** but listed for completeness. |
| `ab0t_quota/tiers.py` | `DEFAULT_TIERS` — built-in tier fallback when no config present. Matters only if Go preserves the search-path fallback. Recommend: drop the built-in defaults; fail loudly on missing config. |
| `ab0t_quota/bridge.py` | HTTPS-only mode. **Out of scope for Go v0.1.0** |

### Public docs

| File | When to read |
|---|---|
| `README.md` | Consumer-facing intro. Mirror the quickstart shape. |
| `docs/quickstart.md` | "5-minute setup" walkthrough. Go README should achieve the same in fewer lines. |
| `docs/deployment.md` | Runbook (operator-facing). Steps 5/5b cover credit grants + webhook subscription. |
| `docs/auth-events.md` | The auth-events pattern — including the new "Idempotency, replay, observability" section. **Read in full** before implementing `authevents/` and `handlerledger/`. |
| `docs/mesh-quota-api.md` | Bridge mode wire format. Skip unless implementing bridge mode. |
| `quota-config.example.json` | Schema reference. Bundle a copy at `testdata/quota-config.example.json` in the Go repo and assert byte-identical parse. |

### Test reference

| Path | Why useful for the port |
|---|---|
| `tests/test_handler_ledger.py` | Parametrized conformance suite — port this verbatim to `handlerledger/conformance_test.go` |
| `tests/test_auth_events.py` | Idempotent dispatch tests — port the integration flow to `authevents/authevents_test.go` |
| `tests/test_cli.py` | Sync CLI smoke tests — port shape to `cmd/quotactl/main_test.go` (use `testscript`) |
| `tests/billing/test_router.py` | Auth strict-mode test pattern. Apply to Go `billing/router.go` |

### Tickets folder

`tickets/20260428_idempotency_replay_framework/` — drove v0.5.2:
- `TICKET.md` — high-level design (read first)
- `CONTEXT.md` — implementer reference (skim)
- `REVIEW.md` — self-critique of the first design (useful tradeoff context)
- `tasklist_20260611.md` — phase-by-phase delivery log

`tickets/20260428_canonical_plans_in_lib/` — separate ticket for the upcoming `plans[]` schema + `sync-plans` CLI:
- `EVENT_DRIVEN_DESIGN.md` (note: rev 1 of this doc placed this file in the idempotency dir — wrong; it lives here)
- `TICKET.md`, `CONTEXT.md`, etc.

### Known upstream bugs (running total — also see PRODUCT_SPEC.md §Known upstream bugs)

| # | Repo | Bug | First found |
|---|---|---|---|
| 1 | ab0t-quota Python | `setup.py:938` NameError — default signup-credit handler never registers | review C8 |
| 2 | ab0t-quota Python | `resolve_billing_org` calls nonexistent `GET /users/{user_id}/organizations` | review C5 |
| 3 | auth service | v1 webhook delivery signs bytes ≠ delivered bytes (canonical vs aiohttp re-serialized) | addendum A1 |
| 4 | ab0t-quota Python | Dead config knobs: `enforcement.shadow_mode`, `enforcement.global_kill_switch`, `alerts.dispatchers[]` | review M10 |
| 5 | ab0t-quota Python | Public Redis accessor never implemented (workaround: consumers reach into `_ctx._redis`) | review §5.2 |

Each is a separate upstream ticket to file. None should be ported into the Go lib's behavior.

---

## Backend services the lib talks to

All are hosted at `<service>.service.ab0t.com` in prod and `localhost:<port>` in dev.

| Service | Prod URL | Dev port | Role |
|---|---|---|---|
| auth | `auth.service.ab0t.com` | 8001 | Identity, JWTs, org graph, event subscriptions |
| billing | `billing.service.ab0t.com` | 8002 | Balance, reserve/commit/refund, usage tracking, credit grants |
| payment | `payment.service.ab0t.com` | 8005 | Stripe checkout, subscriptions, invoices |
| audit | `audit.service.ab0t.com` | 8004 | Optional — emit events for compliance |

### Key endpoints the Go lib hits

**Auth (subscription + identity):**
- `GET /events/types` — list of subscribable event types
- `GET /events/subscriptions` — list our subscriptions; **requires `events.read`**
- `POST /events/subscriptions` — create subscription; **requires `events.subscribe`**. Fires a test event in background immediately after creation — receivers must tolerate.
- `GET /events/subscriptions/{id}/stats` — observability. Useful for the "my events stopped arriving" runbook. Webhook delivery stats are Redis-backed daily buckets retained 30 days. Circuit breaker: 5 failures in 1h → open 300s.
- `POST /events/subscriptions/{id}/test` — fire test event; **requires `events.test`**.
- `GET /users/me/organizations` — Bearer-auth, self-scoped. The Python lib's `resolve_billing_org` calls `GET /users/{user_id}/organizations` (admin-style) which **does NOT exist** — Known Upstream Bug #2. The Go port must use a different mechanism (admin list endpoint or the `me` variant via consumer-supplied JWT).
- `GET /login/{slug}` — public hosted-login HTML (used by `_resolve_org_id_from_slug` — extract orgId from `window.__AUTH_CONFIG__`).
- `GET /organizations/{org_id}/.well-known/jwks.json` — JWKS for JWT validation. Public (no auth required).

**Billing (per-org):**
- `GET    /billing/{org_id}/balance`
- `POST   /billing/{org_id}/reserve`
- `POST   /billing/{org_id}/commit`
- `POST   /billing/{org_id}/refund`
- `POST   /billing/{org_id}/promotional-credit` (legacy credit endpoint)
- `POST   /billing/{org_id}/apply-credit-grant` (new credit-grant endpoint)
- `POST   /billing/{org_id}/reset-subscription-credit`
- `GET    /billing/usage/{org_id}/summary` — **NOTE: path is `usage/{org_id}` not `{org_id}/usage`** (rev 1 had this wrong)
- `GET    /billing/usage/{org_id}/records` — paginated usage entries
- `POST   /billing/usage/{org_id}/` — record usage (called by LifecycleEmitter)
- `GET    /billing/{org_id}/transactions`
- `GET    /billing/{org_id}/tier` — read current tier (MeshTierProvider's primary endpoint)
- `PUT    /billing/{org_id}/tier` — write tier
- `POST   /billing/quota/{service}/{org}/check/{resource}` — **POST not GET** (rev 1 had this wrong). Query params: `user_id`, `increment`.
- `PUT    /billing/tier-catalog/{service_name}` — catalog publish at startup (paid mode)
- `GET    /billing/{org_id}/tier/limits?service={name}` — admin view; depends on tier-catalog publish having happened

**Payment (per-org):**
- `GET  /checkout/{org_id}/plans?include_prices=true&provider_org={pid}` — `provider_org` query param is required for multi-tenant
- `POST /checkout/init`
- `POST /checkout/{org_id}/plan/{plan_id}` — subscription / one-shot checkout
- `POST /checkout/{org_id}/session` — top-up checkout (NEW — was missing from rev 1)
- `GET  /checkout/sessions/{session_id}/verify?process_if_complete=...&verification_token=...` — **GET not POST** (rev 1 had this wrong). `verification_token` required when `process_if_complete=true` (else 403).
- `GET  /subscriptions/{org_id}` — list
- `DELETE /subscriptions/{org_id}/{subscription_id}?at_period_end=...` — **DELETE not POST**; **takes a subscription_id path param** (rev 1 missed it). The Go `PaymentClient.CancelSubscription` signature must take `(ctx, orgID, subscriptionID, atPeriodEnd)`.
- `POST /portal/{org_id}/session` — Stripe Customer Portal (NEW)
- `GET  /payment-methods/{org_id}` — list (NEW)
- `PUT  /payment-methods/{org_id}/{method_id}/default` — set default (NEW)
- `DELETE /payment-methods/{org_id}/{method_id}` — remove (NEW)
- `GET  /invoices/{org_id}` — list
- `GET  /invoices/v2/invoices/{org_id}/{invoice_id}/download` — PDF (NEW)
- `POST /webhooks/stripe` — consumer-side proxy receiver (only when C4 resolves to "port")

---

## OpenAPI specs to bundle as `testdata/`

For wire-level parity tests. Each Go client (`billing/client.go`,
`payment/client.go`) should pin its happy-path against a snapshot of
the live OpenAPI.

Fetch:
```bash
curl -sS https://auth.service.ab0t.com/openapi.json    > testdata/auth_openapi.json
curl -sS https://billing.service.ab0t.com/openapi.json > testdata/billing_openapi.json
curl -sS https://payment.service.ab0t.com/openapi.json > testdata/payment_openapi.json
```

These should be refreshed periodically; drift indicates wire-level change.

The `skills snapshot` tool (per the user's existing skill registry) can
automate this if it becomes a chore.

---

## Mesh patterns the Go lib must understand

**Auth model:**
- Inter-service calls use `X-API-Key: ab0t_sk_live_...` (no JWT for mesh)
- User-facing calls use `Authorization: Bearer <jwt>` (validated via JWKS)
- The receiver in `authevents/receiver.go` validates HMAC, not JWT

**Org topology:**
- Each consumer service has a parent service org (e.g. `sandbox-platform`)
- Plus an end-users org (e.g. `sandbox-platform-users`)
- Plus per-provider consumer sub-orgs (`payment-customer-sandbox-platform`, etc.)
- Workspace-per-user creates a child org under end-users on registration
- User's `org_id` in JWT may be parent OR workspace — `ResolveBillingOrg` figures out which to bill

**Credit-grant semantics (verified in Python v0.5.2):**
- `credit_granted:user:{user_id}:{tier_id}` is the canonical Redis dedup key
- `user:{user_id}:initial_credit:{tier_id}` is the canonical billing idempotency_key
- Both have backwards-compat pins in the Python tests — Go must match exactly

---

## Related Python repos (might inform the Go port)

| Repo | Local path | Why |
|---|---|---|
| `ab0t-com/auth` | `/home/ubuntu/infra/infra/code/auth/output/` | Event subscription system + workspace_provisioning live here. Read `appv2/events/webhook.py` for exact HMAC header format. Read `appv2/event_handlers/workspace_provisioning.py` for org-creation semantics. |
| `ab0t-com/billing` | `/home/ubuntu/infra/infra/code/billing/output/` | `/billing/{org}/balance` etc. handler code — the contract Go's `billing.Client` calls. |
| `ab0t-com/payment` | `/home/ubuntu/infra/infra/code/payment/output/` | Stripe webhook handler + checkout — `payment.Client` mirrors. |
| `ab0t-com/auth_wrapper` (Python) | git URL in requirements.txt | Python lib for JWT validation + AuthGuard. The Go equivalent would be `ab0t-com/ab0t-auth-go` (does not yet exist — write our own JWT validation for now). |
| `ab0t-com/client-service-setup-cli` | external CLI | Operator runbook for onboarding a service. Skim if implementing the auto-subscribe path. |

---

## Consumer reference

The first Go consumer is hypothetical, but for reference of what a
"real" consumer looks like in Python, see:

- `/home/ubuntu/infra/infra/code/resource/output/sandbox-platform/app/quota.py` — full integration, including `@idempotent` adoption (lines ~190-230)
- `/home/ubuntu/infra/infra/code/resource/output/sandbox-platform/app/main.py` — `setup_quota(app)` callsite
- `/home/ubuntu/infra/infra/code/resource/output/sandbox-platform/quota-config.json` — real-world config

---

## CLI parity expectations

The Python CLI in `ab0t_quota/__main__.py` has these subcommands.
`cmd/quotactl/` must match flag names and exit codes exactly.

| Subcommand | Python flags | Notes for Go |
|---|---|---|
| `subscribe-events` | `--auth-url --endpoint --org-id --name` | hardcodes `event_types=["auth.user.registered","auth.user.login"]` (this differs from runtime auto-subscribe which uses registered types) |
| `events` | `--user-id --status --handler --event-id --since --limit --format` | requires `--user-id` OR `--status` OR (`--handler` + `--event-id`); exit 2 otherwise. `--since` accepts `Nh/Nd/Nm/ISO`. |
| `replay` | `--handler --event-id --webhook-url` | signs payload with **bare hex** (no `sha256=` prefix). Appends `/api/quotas/_webhooks/auth` to a bare public URL. |
| `backfill` | `--handler --user-ids --org-id --event-type --webhook-url` | `--handler` is logging-only. Synthesizes `{"event_type", "event_id": "backfill_{uid}_{ts}", "data": {"user_id", "org_id", "_synthetic": true}}`. |
| `delete-user` | `--user-id --confirm` | `--confirm` is required (irreversible) |

Exit codes: 0 success, 1 op failure (e.g. missing ledger row), 2 usage error. cobra defaults to 1 on usage errors — Go CLI must override with `SilenceUsage: true` and custom error handling for argparse-equivalent 2-on-usage.

Env vars the CLI reads:
- `AB0T_MESH_API_KEY` — fallback auth via `X-API-Key` for `subscribe-events` (when `AB0T_AUTH_ADMIN_TOKEN` is absent)
- `AB0T_QUOTA_DDB_TABLE`
- `QUOTA_REDIS_URL` / `REDIS_URL`
- `AB0T_AUTH_WEBHOOK_PUBLIC_URL`
- `AB0T_AUTH_WEBHOOK_SECRET`
- `AB0T_AUTH_ADMIN_TOKEN`
- `AB0T_AUTH_AUTH_URL` / `AUTH_SERVICE_URL`

---

## Internal docs at ab0t.com worth bookmarking

- `https://docs.ab0t.com/mesh/architecture` — mesh org topology
- `https://docs.ab0t.com/auth/events` — webhook delivery spec
- `https://docs.ab0t.com/billing/credit-grants` — credit_grant config schema reference

(If any of these are private/dev-only, skip them in the public Go repo's README and link only to public consumers.)

---

## Claude skills useful while implementing

### Active in this environment (use via the Skill tool)

| Skill | When to invoke |
|---|---|
| `auth-service-api-reference` | Subscription endpoints + JWT validation specifics. |
| `billing-service-api-reference` | Every endpoint the Go `billing` package calls. Pin field types + JSON shapes from here. |
| `payment-service-api-reference` | Every endpoint the Go `payment` package calls. |
| `uj-test-harness` | If we add UJ-style bash tests at consumer integration time. |
| `mesh-service-accounts` | When onboarding the Go lib as a new mesh consumer (consumer sub-orgs, API keys, gateway pattern). |
| `idempotent-ops-script-design` | Cross-checks the CLI + replay design. |
| `nosql-access-patterns-audit` | Audit DDB access patterns before shipping. |

### Skills inside the Python repo at `ab0t_quota/Skills/`

These are bundled with the Python repo (not in the active skill registry but readable as Markdown):

| Path | Why |
|---|---|
| `Skills/quota-service-integration/` | Step-by-step "how to wire ab0t-quota into a new service" — the closest Python analog to the Go quickstart |
| `Skills/quota-paid-tier-onboarding/` | Paid-tier wiring (credit grants + checkout + webhook) — read before Phase 3 |
| `Skills/billing-payment-integration/` | Field-level billing+payment integration patterns |
| `Skills/quota-billing-module/` | Library-side billing module internals |
| `Skills/quota-multi-tenant/` | Multi-tenant semantics (org_id propagation, isolation) |
| `Skills/quota-tier-management/` | Tier-resolution + change semantics (`InvalidateTierCache`) |
| `Skills/quota-troubleshooting/` | Common errors and diagnosis — port lessons into the Go runbook |

### Skills mentioned in rev 1 but not in the registry

Rev 1 referenced `ab0t-quota-auth-events` and `ab0t-quota-idempotent-handlers`. These exist as project-local skills under `ab0t-quota/.claude/skills/` (created during the v0.5.2 work) but are not loaded into Claude's active registry from there. If a `.claude/skills/` install path is added later, they'd be useful — until then, read them directly as MD files in that directory.
