# Review addendum — ab0t-quota-go spec: everything else found

**Date:** 2026-06-11
**Companion to:** [`review_20260611.md`](review_20260611.md) (cross-referenced as C# / M# / minor #)
This addendum captures findings that surfaced during and after the main review pass: one new cross-service bug, wire contracts the spec must pin, whole sections the PSD is missing, and corrections too detailed for the main review.

---

## A1. NEW CRITICAL — auth's v1 webhook delivery signs different bytes than it sends

This is a third live bug, in the **auth service** this time, found while pinning the HMAC contract for the Go receiver.

**Evidence** (`auth/output/appv2/events/webhook.py`):
- Signature is computed over canonical JSON: `json.dumps(payload, sort_keys=True, separators=(',', ':'))` → HMAC-SHA256 (lines 274-281).
- But the request is sent with `session.post(endpoint, json=payload, …)` (line 106; batch path line 203). aiohttp re-serializes with `json.dumps` **defaults** — `(', ', ': ')` separators, no `sort_keys` — so **the delivered body bytes never equal the signed bytes** (the spaces alone guarantee mismatch).
- The quota lib's receiver verifies HMAC over the **raw received body** (`ab0t_quota/auth_events.py:114-121`). Therefore every secret-bearing delivery through the v1 `/events/subscriptions` pipeline should fail verification with 401, auth records a failure, and the circuit breaker opens after 5 consecutive failures for 5 minutes.

**Why the system appears to work anyway:** the parallel **events_v2 publisher** (`appv2/events_v2/publishers.py:80-135`) does it correctly — it signs the *exact* `payload_bytes` it sends (`data=payload_bytes`), under the **`X-Webhook-Signature`** header. That is precisely the "legacy publisher" header the quota receiver accepts as a fallback (`auth_events.py:149`). So real deliveries that verify successfully are almost certainly coming from the v2 path, while the v1 subscription pipeline — the one the lib's auto-subscribe, the CLI's `subscribe-events`, and the `/stats` and `/test` endpoints all target — is cryptographically broken whenever a `secret` is set.

**Caveats / verification:** this is a static read of HEAD; confirm in prod by reading `GET /events/subscriptions/{id}/stats` for the quota subscription (expect `total_failure` ≈ total if the v1 path is the live one) and by `POST /events/subscriptions/{id}/test` against a receiver with a secret.

**Consequences for the Go spec:**
1. Pin the rule **"verify HMAC over raw received body bytes; never re-canonicalize JSON in the receiver"** — Go cannot reproduce Python's canonical form anyway (Python `ensure_ascii=True` escapes non-ASCII; Go escapes `<>&` and not non-ASCII; float formatting differs). Re-canonicalization as a compatibility shim is a trap; don't.
2. The two delivery paths have **different envelopes**: v1 sends `event_id`/`event_type`/`occurred_at`; v2's `event.to_dict()` uses `id`/`event_type`/`timestamp` with `X-Webhook-Signature`. This is *why* Python's dispatcher has the `event_id or id` and `event_type or type` fallbacks (`auth_events.py:161,196-197`) — the Go `Event` parsing must keep those aliases (extends review C6).
3. File the auth-side fix (one line: send `data=<the signed canonical bytes>` in v1 delivery, or sign what aiohttp will actually send). The Go port should not be designed around the broken path, but its receiver must tolerate both publishers exactly as Python does (both headers, `sha256=` and bare-hex).

---

## A2. The 429 / HTTP wire contract is a public API the PSD never defines

`QuotaResult.to_api_error()` is the **frontend-visible 429 body**, identical across all consumers by design (`models/responses.py:96-110`):

```json
{"error": "quota_exceeded", "resource": …, "current": …, "requested": …,
 "limit": …, "remaining": …, "tier": …, "tier_display": …,
 "upgrade_url": …, "retry_after": …, "message": …}
```

Plus computed fields with exact semantics: `remaining = limit − current − requested` (post-request headroom), `utilization = round(current/limit, 4)` (pre-request). The middleware's behavior (`middleware.py:66-141`):

- **429**: body = `to_api_error()`, headers `Retry-After` (denial's retry_after or **60**), `X-Quota-Limit`, `X-Quota-Current`, `X-Quota-Resource`.
- **Allowed**: increments **after** the check and **before** the downstream handler (counts requests, not successes; increment failure logs and proceeds), then sets `X-Quota-Limit` / `X-Quota-Remaining` **as ints** on the response.
- **Fail-closed 503**: `{"error": "quota_service_unavailable", "detail": "Quota enforcement is temporarily unavailable."}`; `fail_open_error_threshold=0` means "always fail open" when fail_open is set; consecutive-error counter resets on success.
- **No org → pass through unchecked.** Unauthenticated requests bypass the rate limiter entirely. This is a deliberate Python behavior with a security flavor — the Go spec must either replicate it knowingly or offer `RequireOrg bool`.

None of this is in the PSD (`middleware/headers.go` mentions two header names; `result.go` lists three type names). Add a "wire contracts" subsection with the JSON body, header set, and ordering rules — frontends and the message builder both depend on it.

## A3. Mounted quota-API routes: one route missing, response shapes unpinned

- The spec's `Mount` comment lists `/usage, /tiers, /check/{key}` — Python also mounts **`/check-bundle/{bundle_name}`** (`setup.py:1030-1036`). Bundle checks are the primary consumer pattern (sandbox uses bundles for everything); the route must exist.
- `/tiers` response shape: `{"tiers": [{tier_id, display_name, description, features[], limits: {key: {limit, limit_display}}, upgrade_url}]}` with `limit_display` = `"Unlimited"` or `%g` formatting; sorted by `sort_order`. Public (no auth dep in Python — note that too).
- `/usage` and `/check*` return 401 `{"detail": "Unable to resolve org_id"}` when org extraction fails.
- **Missing from `quota.Config` entirely:** the equivalents of Python's `org_extractor` and `auth_dependency` for these routes. In Go this should be (a) a documented context key (`quota.OrgFromContext` / `quota.WithOrg`) that the consumer's auth middleware populates, and (b) an optional `RouteMiddleware func(http.Handler) http.Handler` to guard the mounted routes. Without this, the mounted routes have no auth story at all.
- Also missing: `RateLimitResource string` (Python's `rate_limit_resource`, default `api.requests_per_hour`) and the rule "guard is skipped with a log if that resource isn't registered" (`setup.py:312-322`).

## A4. `QuotaContext` accessor gaps

Python exposes, via `app.state`: `quota` (context), `quota_emitter` (LifecycleEmitter — sandbox's `get_emitter()` depends on it), `quota_handler_ledger` (LedgerStore — the CLI-equivalent introspection handle). The Go `QuotaContext` has `Engine()` only. Add `Emitter()`, `LedgerStore()`, and `Redis()` (review §5.2) — otherwise Go consumers can't reach the emitter for resource_started/stopped calls at all, which is the entire paid-tier integration surface for a resource-provisioning service. Also note Python's context has no single-resource `Increment` (only bundles + engine passthrough); Go adding it is fine — mark as an addition.

## A5. Setup/lifecycle behaviors not in the PSD

| Behavior | Python | PSD status |
|---|---|---|
| `on_ready` may be sync or async; exceptions are swallowed with a warning | `setup.py:408-414` | unspecified |
| `engine_mode` resolution: explicit arg → `config.engine_mode` → `"local"`; unknown value → warn + local; `byo_redis` ≡ `local` code path | `setup.py:231-245` | absent (config lists no `engine_mode` field) |
| Bridge mode deliberately does **not** mount `/tiers` (catalog owned by billing) | `setup.py:599-601` | n/a (deferred) but the config should still *parse* `engine_mode` and **error** on `"bridge"` rather than ignore it |
| Service-name resolution for catalog publish: `AB0T_SERVICE_NAME` → `config.service_name` → first resource's `service` → skip | `setup.py:735-748` | absent |
| Persistence init failure is **non-fatal** (warn + run Redis-only) | `setup.py:349-357` | unspecified — important fail-soft contract |
| Teardown order: heartbeat stop → store close (stops snapshot worker) → redis close | `setup.py:442-460` | `Close()` unspecified ordering |
| **There is no "alerts" background worker.** Alerts fire inline during `check` via `AlertManager.maybe_alert` with a Redis cooldown. `quota/lifespan.go`'s "(snapshot, heartbeat, alerts)" is wrong | `setup.py:291-304`, `engine.py:183-196` | spec error — fix the file comment |
| Auto-subscribe runs as a **fire-and-forget task after startup**, not blocking | `setup.py:421-434` | spec says "SubscribeOnStartup" — pin the non-blocking task semantics |

**Multi-replica semantics (missing section):** every replica runs the snapshot worker (duplicate snapshots are idempotent writes — benign), every replica runs the HeartbeatMonitor (duplicate synthetic-stop **SNS events** are *not* deduped; only the cost record is, via `cost:lifecycle:{resource_id}`), auto-subscribe is idempotent by endpoint match. A "running N replicas" paragraph belongs in the PSD; today an implementer would discover the SNS duplication in prod.

## A6. Request/response models have no field-level spec

The PSD names `CheckRequest`/`IncrementRequest` etc. but defines no fields. From `models/requests.py` + engine usage, pin at minimum: `QuotaCheckRequest{org_id, resource_key, increment float64 = 1.0, user_id?, metadata?}`; `QuotaIncrementRequest{…, delta float64 = 1.0, idempotency_key?}`; decrement same; `QuotaBatchCheckRequest{org_id, user_id?, checks []QuotaCheckItem{resource_key, increment}}`; `QuotaResetRequest{org_id, resource_key, new_value, admin_user_id, reason}` (reset emits a mandatory `ADMIN_QUOTA_RESET` audit log line — `engine.py:255-260`); `QuotaBatchResult{allowed, results, denied_resources, warning_resources}` + `first_denial` accessor (the consumer's 429 path uses it).

## A7. Alerts module — actual contract (PSD has names only)

From `alerts.py` + `setup.py:291-304`:
- `LogAlertDispatcher` is **always** active; `WebhookAlertDispatcher` added only when `alerts.webhook_url` is set. (The example config's `"dispatchers": ["log"]` array is **not read** by the lib — another dead config key, same class as review M10.)
- Cooldown: Redis-keyed, default 3600s, configured via `alerts.cooldown_seconds`; fires only on WARNING/CRITICAL/EXCEEDED.
- `WebhookAlertDispatcher` enforces **HTTPS-only** and blocks loopback/private/link-local/reserved IPs (SSRF guard, `alerts.py:48-83`) — port this validation verbatim; it pairs with the DDB-endpoint allowlist (review M14).

## A8. HTTP client policy: Python does NOT retry — the spec's retrying client is a real divergence

`internal/httpx/client.go` is described as a "retrying HTTP client used by all callers." Python's `httpx.AsyncClient` calls have **no retry anywhere** — one attempt, fixed timeouts. Auto-retrying POSTs in Go is dangerous where idempotency keys are optional: `reserve` (idempotent only if `request_id` set), `record_usage`, subscription/checkout creates. If Go keeps the retrying client: retry **only** idempotent methods (GET/PUT with key) or requests that carry an explicit idempotency key, and say so in the spec. Also pin the timeout inventory (Python: billing/payment clients 15s; auth event subscribe 15s; org lookups 10s; tier fetch 5s; catalog publish 5s; CLI subscribe 20s; replay/backfill 30s) and the error mapping (connect error → 503 "unreachable", timeout → 504, both as typed `BillingServiceError`/`PaymentServiceError`-equivalents with `{status_code, detail}` — the PSD defines no client error types at all).

## A9. Master env-var inventory (PSD has none; back_references is partial)

The PSD should carry one authoritative table. Complete set found in source:

| Var | Used by | Notes |
|---|---|---|
| `AB0T_MESH_API_KEY` | unified mesh credential | fallback for both per-service keys; also CLI subscribe fallback (missing from back_references) |
| `AB0T_MESH_BILLING_API_KEY` / `AB0T_MESH_PAYMENT_API_KEY` | per-upstream overrides | review M12 |
| `AB0T_CONSUMER_ORG_ID` | paid-tier router | router not mounted without it |
| `AB0T_MESH_BILLING_URL` / `AB0T_MESH_PAYMENT_URL` | dev URL overrides | "not part of consumer-facing API" per setup.py docstring — keep that framing |
| `AB0T_SERVICE_NAME` | catalog publish, bridge identity | |
| `AB0T_AUTH_AUTH_URL` (fallback **`AUTH_SERVICE_URL`**) | subscribe, org resolution | second fallback undocumented anywhere |
| `AB0T_AUTH_ADMIN_TOKEN` | subscription writes | needs `events.subscribe`+`events.read` (+`events.test`) perms |
| `AB0T_AUTH_WEBHOOK_PUBLIC_URL` / `AB0T_AUTH_WEBHOOK_SECRET` | receiver + subscribe + CLI | |
| `AB0T_AUTH_WATCH_ORG_SLUG` (fallback **`AB0T_AUTH_ORG_SLUG`**) | subscription org filter | fallback chain undocumented |
| `AB0T_QUOTA_STRIPE_WEBHOOK_SECRET` (fallback `STRIPE_WEBHOOK_SECRET`) | webhook proxy T1 | only if C4 resolves to "port" |
| `AB0T_MESH_SNS_LIFECYCLE_TOPIC_ARN` (fallback `SNS_LIFECYCLE_TOPIC_ARN`) | LifecycleEmitter | only if M3 resolves to "port SNS" |
| `AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS` | config validation | C3.3 |
| `AB0T_QUOTA_DDB_TABLE` | CLI ledger store | |
| `QUOTA_CONFIG_PATH` | config | |
| `QUOTA_REDIS_URL` → `REDIS_URL` (default `redis://localhost:6379/0`) | storage | |
| `QUOTA_REDIS_PASSWORD` → `REDIS_PASSWORD` | storage | house convention, review C3.7 |
| `QUOTA_*` (namespace) | config interpolation | C3.1 |
| `DYNAMODB_ENDPOINT` | persistence (allowlisted hosts only) | |
| `AWS_REGION` (default `us-east-1`) | persistence | |

## A10. Config forward-compatibility policy is missing

- The Python repo has an open proposal to add a top-level **`plans[]`** block (`tickets/20260428_canonical_plans_in_lib/`) and a `sync-plans` CLI subcommand (TODO comment in `__main__.py:286-289`). Pydantic tolerates unknown keys; if the Go loader uses `DisallowUnknownFields` for "validation," the first Python-side schema addition breaks every Go consumer. **State the policy: unknown top-level keys are ignored (with a debug log), `$`-prefixed keys are comments.**
- Note the real configs use *ad-hoc* `$comment_*` keys (e.g. `"$comment_resource_bundles"` in `quota-config.example.json`) — not just the two `$schema`/`$comment` fields the spec models. Tolerance must be general, not field-by-field.

## A11. Documentation plan is absent from the PSD

The Python repo ships consumer-facing docs with no Go counterpart planned: `BILLING_MODELS_GUIDE.md` (21KB — the explainer for the 4 billing models, the doc a paying consumer actually needs), `ARCHITECTURE.md`, `docs/quickstart.md`, `docs/deployment.md` (operator runbook — steps 5/5b cover mesh-credential provisioning and webhook subscription, exactly the part Go operators will fumble), `docs/auth-events.md`, `docs/WEBHOOK_AND_CREDIT_GRANT_ARCHITECTURE.md` (the two-track tier-flip vs credit-grant design + the 2026-05-18 incident trust-model lesson). Phase 7 says "docs polish" — enumerate which of these get ported/adapted. Minimum: quickstart, deployment runbook (Go-flavored), billing-models guide (link to Python's if content is shared).

## A12. Test-parity map is 4/20

`back_references.md` maps four Python test files. The full inventory worth mapping per Go package: `test_config`, `test_models` (+ `tests/billing/test_models`), `test_engine`, `test_counters`, `test_providers`, `test_middleware`, `test_messages`, `test_alerts`, `test_setup`, `test_persistence_sync_worker`, `test_persistence_validation`, `test_billing_models` (the C3 validators!), `test_tier_catalog_publish` (the omit-billing-policy regression — port if M2 resolves to "port"), `test_caches`/`test_bridge` (deferred), `tests/billing/{test_clients, test_router, test_lifecycle}`. The conformance value is concentrated in `test_billing_models` and `test_handler_ledger` — call those two out as port-verbatim.

## A13. Tooling and dependency nits

1. **`testscript` is used by the testing strategy but missing from the Dependencies table** (`github.com/rogpeppe/go-internal`). Add it; same for `stripe-go` and `aws-sdk-go-v2/service/sns` pending the C4/M3 decisions.
2. **cobra exits 1 on usage errors; argparse exits 2.** CLI parity (review M8) needs explicit `SilenceUsage` + custom exit handling. Add "exit-code parity" to acceptance criteria.
3. **Per-package ≥80% coverage isn't a `go test` flag** — needs a CI script (`go test -coverprofile` per package + threshold check). Spec asserts it; CI file should own it.
4. **`LICENSE (Proprietary, matches ab0t-quota Python)`** — the Python repo has **no LICENSE file** (verified). Nothing to match; decide the license text fresh.
5. **`Mount(mux Mux, prefix string)`** — `Mux` is an undefined type in the spec. Define it (`interface{ Handle(pattern string, h http.Handler) }`) and resolve the chi-vs-std "decision pending": Go 1.22 `http.ServeMux` method+wildcard patterns cover every route in the layout (incl. `/check/{resource_key}`); recommend **std, decided now** — the pending-decision footnote just creates drift risk.
6. **shopspring/decimal formatting**: Python `Decimal("10.00")` round-trips scale ("10.00"); shopspring normalizes (emits "10"). If billing fixtures assert string equality, pin a `MarshalJSON` that preserves two decimal places for money — or assert numeric equality in tests, not byte equality. Small, but it's exactly the kind of thing the parity matrix exists for.

## A14. Security addendum

1. **No replay protection on the webhook receiver** — no timestamp-freshness check on `X-Event-Timestamp`; the CLI's replay feature *depends* on replayability. Delivery dedup protects `@idempotent` handlers (same event_id short-circuits); **plain handlers re-execute on replayed payloads**. Document this as the threat model: anyone holding a captured signed payload can re-fire plain handlers. (Don't "fix" with a freshness window without redesigning CLI replay — it signs fresh, so it would survive; note it.)
2. Use `hmac.Equal` (constant-time) in `hmac.go` — Python uses `compare_digest`; easy to forget in Go.
3. Receiver error responses are static strings (`"invalid signature"`, `"invalid json"`) — keep them static in Go (house rule: no dynamic strings in client-facing errors). Same applies to the proxy router: Python's clients carry upstream `detail` in their exceptions; **the router layer must log it and return a generic message** — make this an explicit requirement on `router.go`, not an implementation accident.
4. Stripe webhook (if ported): signature is over the **raw body** — the Go handler must read the body before any middleware/JSON decoding touches it.
5. Credential separation: the **auth admin token** (subscription writes) and the **mesh API key** (billing/payment calls) are different trust domains; the Config struct should not encourage reusing one for the other (Python's CLI accepts the mesh key as a *fallback* for subscribe — keep it a documented fallback, not the default).

## A15. Known operational edge to document (runbook material)

Lease vs auth-retry interplay: receiver records `in_progress` (lease 60s) → handler retries inline (up to 3 × backoff, can exceed auth's 30s delivery timeout) → auth times out, marks failed, schedules redelivery → redelivery arrives **within the lease** → short-circuits as already-in-progress → returns 200 → auth marks delivered. If the original attempt then fails permanently, **auth will never redeliver** — the row sits at `failed_permanent` and `quotactl replay` is the designed recovery path. This is coherent but non-obvious; one paragraph in the auth-events doc (and a `quotactl events --status failed_permanent` mention in the runbook) saves a future incident review.

## A16. Corrections to back_references' "skills" section (supersedes review minor 4)

The two referenced skills (`ab0t-quota-auth-events`, `ab0t-quota-idempotent-handlers`) exist neither in the active skill registry **nor** in the Python repo. What does exist is the Python repo's own `Skills/` directory with seven differently-named skills: `billing-payment-integration`, `quota-billing-module`, `quota-multi-tenant`, `quota-paid-tier-onboarding`, `quota-service-integration`, `quota-tier-management`, `quota-troubleshooting`. Update back_references to point at these (repo-local, so an implementing agent can read them directly) — `quota-service-integration` and `quota-paid-tier-onboarding` are the two an implementer would actually want.

## A17. Smaller true-ups (one-liners)

- `caches.py` (bridge TTL caches: tier TTL from `bridge_cache.tier_ttl_seconds`, decision TTL default **1s**) and `tiers.py` (`DEFAULT_TIERS`) need rows in the back_references layout table even if deferred.
- PinStore in Python is DDB-only; the Go spec's three PinStore backends are a superset — fine, mark as an addition (and keep DDB's conditional-write rule: `auto` never overwrites `operator`).
- `GET /checkout/{org_id}/plans` supports `provider_org` query param (multi-tenant) — Python client passes it; Go `GetPlansOptions` should include it.
- Auth subscription create fires a **test event in the background** immediately (`events.py:99-167` note) — receivers must tolerate an immediate delivery after subscribe; worth one test.
- Webhook delivery stats are Redis-backed daily buckets retained 30 days; circuit breaker = 5 failures/1h window → open 300s (`webhook.py:71-75, 304-321`) — useful context for the runbook's "my events stopped arriving" section.
- `Setup` is specced to error on second call — Python has no such guard; fine as a Go improvement, but the *handler registry* being package-global is what actually makes double-Setup dangerous; the error message should say that.
- Python's `QuotaContext.check` raises HTTP 429 directly; Go returning `*QuotaError` is better — ship the `WriteDenial(w, err)` helper (A2) so the 429 body stays consistent across the ecosystem anyway.

---

## Consolidated "missing sections" checklist for the PSD

In priority order, the PSD needs these sections added (beyond the per-item fixes above and in the main review):

1. **Wire contracts**: 429 body, quota headers, 503 body, receiver responses (A2, A3).
2. **Env-var reference table** (A9).
3. **Org-extraction / auth-injection contract** for mounted routes and middleware (A3, review M15).
4. **HTTP client policy**: timeouts, no-retry-vs-retry rules, typed errors (A8).
5. **Multi-replica & shutdown semantics** (A5).
6. **Config forward-compat policy** (A10).
7. **Observability**: logging approach (slog?), the audit-log lines Python mandates (`ADMIN_QUOTA_RESET`, alert lines), and "quotactl as the observability surface" statement (A7, A15).
8. **Documentation deliverables list** for Phase 7 (A11).
9. **Threat model notes**: replay, SSRF guards, credential separation, raw-body rules (A14).
10. **Concurrency statement**: `QuotaContext`/`Engine` safe for concurrent use; handlers dispatched sequentially per delivery in registration order (matching Python's single-loop semantics) — currently unstated anywhere.

## Bugs to file upstream (running total from both documents)

| # | Repo | Bug | Evidence |
|---|---|---|---|
| 1 | ab0t-quota (Python) | `setup.py:938` NameError `provider` — default signup-credit handler never registers | review C8 |
| 2 | ab0t-quota (Python) | `resolve_billing_org` calls nonexistent `GET /users/{user_id}/organizations` — workspace resolution silently dead | review C5 |
| 3 | **auth service** | v1 webhook delivery signs canonical bytes but sends `json=payload` re-serialized bytes — HMAC can never verify on the v1 path | **A1 (this doc)** |
| 4 | ab0t-quota (Python) | dead config knobs: `enforcement.shadow_mode`, `enforcement.global_kill_switch`, `alerts.dispatchers[]` | review M10 + A7 |
| 5 | ab0t-quota (Python) | public Redis accessor (review item M-3) still unimplemented; consumers reach into `_ctx._redis` | review §5.2 |
