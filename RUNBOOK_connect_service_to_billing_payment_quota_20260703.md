# RUNBOOK — take a service from zero → billing + payment consumer accounts → ab0t-quota → accepting payments/quota

**Date**: 2026-07-03
**Applies to**: any mesh service that needs to meter usage, enforce quota, and/or accept payments on behalf of its users.
**Worked example**: connect (integration) — has billing, needs payment (ticket c3a8).
**Owner-gated boundaries are called out inline. This runbook is copy-pasteable up to those gates.**

Related:
- SKILL `billing-payment-consumer-setup` (`shared/ab0t-setup-go/Skills/`) — the mint how-to this runbook operationalizes.
- SKILL `mesh-service-accounts` — general consumer mechanism. SKILL `billing-payment-integration` — runtime proxy/Stripe wiring. SKILL `ab0t-quota` (+ `ab0t-quota-go-*`) — the library.
- DISCUSSION: `intergration/output/tickets/open/2026-07-03-c3a8-.../DISCUSSION.md` — why + the full chain.
- Migration: `~/infra/infra/ops/tickets/20260703_mesh_client_authsetup_migration/`.

---

## The chain (what this runbook builds)

```
authsetup run 07  ──▶ billing X-API-Key + payment X-API-Key  (~/.authmesh/<svc>/{billing,payment}-consumer.prod.json)
        │
        ▼ wire to env
BILLING_SERVICE_API_KEY / PAYMENT_SERVICE_API_KEY  (Go: AB0T_QUOTA_{BILLING,PAYMENT}_URL + AB0T_QUOTA_SERVICE_TOKEN)
        │
        ▼ client library holds the keys
Go → ab0t-quota-go (quota.Setup, Middleware, WebhookHandler, CreditGranter)
Py → ab0t-quota + drop-in billing_client.py / payment_client.py (X-API-Key)
        │
        ▼ runtime
quota enforced on every request  +  checkout → payment-svc → Stripe  +  webhooks → payment-svc → billing-svc credit → tier/credit buckets
        │
        ▼
service enforces quota AND accepts payments — zero custom Stripe code
```
Money direction (never reversed): `Stripe → payment-svc → billing-svc → auth event → your service`. The library is the last hop; it never calls Stripe.

---

## Prerequisites

```bash
AUTHSETUP=~/.local/bin/authsetup                 # or shared/ab0t-setup-go/setup-go/release/authsetup-linux-amd64
SVC=<service-name>                               # e.g. connect-service
CFG=/home/ubuntu/infra/infra/code/<svc-repo>/output/setup/config
CREDS=~/.authmesh/$SVC
```
- Service already onboarded to the mesh (its service org adopted; `run 01` done). Verify: `$AUTHSETUP --env prod --config-dir "$CFG" status`.
- Provider admin cred files reachable:
  - billing: `billing/output/setup/credentials/billing.json` (prod org-B — NOT `-dev`, NOT an archived twin).
  - payment: `~/.authmesh/payment/payment.prod.json` (adopted provider cred).
- **Never print an `X-API-Key` or password value.** Validate keys via the API, not by echoing them.

---

## Step 1 — author the consumer configs

Create `$CFG/clients.d/billing.json` and `$CFG/clients.d/payment.json`, modeled byte-for-structure on
`resource/output/sandbox-platform/setup/config/clients.d/{billing,payment}.json`.

Decisions to lock before writing (per c3a8):
1. **`client.service_id`** — keep billing and payment under the SAME identity (connect: `integration-service` for both, until a coordinated rename).
2. **Permissions** — start from the table in the `billing-payment-consumer-setup` SKILL. Drop `payment.admin` unless the service manages plans/products itself. Remember: billing uses `cross_tenant`, payment uses `cross_org`.

Fill: `provider.credentials_path` (Step-1 prereqs), `provider.service_url`, `customer_org_name/slug`, `service_account_email/password` (secret), `api_key.name` (`<svc>-{billing,payment}-backend`).

**Seed-to-adopt** (only if the consumer already exists): hand-seed `$CREDS/<provider>-consumer.prod.json` with the existing key so `run 07` adopts. Net-new (e.g. connect→payment) needs no seed.

## Step 2 — validate + dry-run (mutates nothing)

```bash
$AUTHSETUP --env prod --config-dir "$CFG" validate
$AUTHSETUP --env prod --config-dir "$CFG" --creds-dir "$CREDS" --dry-run run 07
```
Read the plan:
- **net-new consumer** → expect exactly "create sub-org + SA + key" for that one provider. Good.
- **existing consumer** → expect "sub-org exists + key already provisioned (reuse)". Good.
- **existing consumer but plan says CREATE** → the post-reset **membership gap** (see Step 2a). **ABORT** — do not run for real.

`run 07` reconciles every `clients.d/*.json`. To mint one at a time, temporarily `.hold`-suffix the other file.

### Step 2a — membership-gap repair (OWNER-GATED)
If dry-run falsely plans a CREATE for an existing consumer: the sub-org was made by a dead admin generation, so the current provider admin isn't a member. **Repair (owner runs — it's a permission grant on live prod):**
```
# the sub-org's own service account (already admin) invites the current provider admin
POST /organizations/<sub-org-id>/invite  {email: <provider>-admin@ab0t.com, role: admin}
```
Re-run the dry-run → it flips to "sub-org exists + reuse". The live runtime key is untouched throughout. (Evidence: migration worklog lines 62-66, 110-113.)

## Step 3 — mint (real run)

```bash
$AUTHSETUP --env prod --config-dir "$CFG" --creds-dir "$CREDS" run 07
```
Rate limit: auth 5/min — a mint is ~5 calls; one clean window. Output:
`$CREDS/{billing,payment}-consumer.prod.json` (0600).

## Step 4 — validate the minted key(s)

```bash
# read the key from the cred file into a var WITHOUT echoing it; then:
POST /auth/validate-api-key  {api_key: "<X-API-Key>"}
#   → { valid: true, permissions: [ <your configured set> ] }
```
Confirm perms == config. Confirm any **pre-existing** consumer key_ids are UNCHANGED (the non-destructive invariant).

## Step 5 — wire the keys into the service env (announce; deploy = OWNER)

Stage in `production/.env.production` **and** its `.template` (add new vars to the template so they reach prod; the live `.env.production` is often gitignored). Announce the exact name/file/line you changed.

```
BILLING_SERVICE_URL=https://billing.service.ab0t.com     BILLING_SERVICE_API_KEY=<X-API-Key>
PAYMENT_SERVICE_URL=<internal payment-service-prod:8005 | payment.service.ab0t.com>   PAYMENT_SERVICE_API_KEY=<X-API-Key>
```
**Deploy (distribute env + recreate container) = owner/ops.** This runbook stops at staging.

## Step 6 — set up the client library

### 6a — Go service → ab0t-quota-go
```bash
go get github.com/ab0t-com/ab0t-quota-go
```
Env (the mesh consumer credential feeds the library):
```
AB0T_QUOTA_BILLING_URL=$BILLING_SERVICE_URL
AB0T_QUOTA_PAYMENT_URL=$PAYMENT_SERVICE_URL
AB0T_QUOTA_SERVICE_TOKEN=<mesh consumer credential>
```
Wire (see `ab0t-quota-go-setup` / `-middleware` / `-auth-events`):
```go
q, _ := quota.Setup(ctx, quota.Options{
    ConfigPath:    "quota-config.json",              // tier/limits/credit-grant schema (ab0t-quota-go-config)
    CreditGranter: myGranter{billing: billingClient},// grants credits on auth events, idempotently
})
mux.Handle("/api/", q.Middleware(deps)(handler))                        // per-request quota enforcement → 429
mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())   // receives auth/billing events
```

### 6b — Python service → drop-in clients (connect is Python)
Follow `billing-payment-integration`: `app/billing_client.py` + `app/payment_client.py` send `X-API-Key`; add the proxy routes (`/api/payments/plans|checkout|topup|portal`, `/api/webhooks/stripe`). Connect already does this for billing (`app/dependencies.py`, `app/api/proxy.py`); add the payment client symmetrically.

## Step 7 — verify end-to-end

- **Quota**: hit a guarded route past the tier limit → expect 429 with a human message.
- **Balance/usage**: `GET /billing/{org}/balance` through the proxy → 200 (3-bucket shape).
- **Checkout**: `POST /api/payments/checkout/{plan}` → returns a Stripe URL.
- **Webhook loop**: complete a test checkout (Stripe test mode) → `invoice.paid`/`checkout.session.completed` → payment-svc credits billing → balance/tier updates.
- **No 401/403** on proxied billing/payment calls (401 = missing/expired key; 403 = missing `cross_tenant`/`cross_org`).
- Register the Stripe webhook URL + events per `billing-payment-integration`.

---

## Owner-gated boundaries (this runbook never crosses these)
1. **Membership-gap invite** (Step 2a) — a permission grant on live prod.
2. **Real `run 07` in some flows** — the auto-mode classifier denies non-dry-run prod mutation; the owner runs it.
3. **Deploy** (Step 5) — distributing env + recreating the container.
4. **Stripe dashboard** webhook registration.

Everything else (author configs, validate, dry-run, stage env, write client code, wire quota-config) is safe to do here.

## Quick reference — what lands where
| Artifact | Location |
|---|---|
| Consumer config | `<svc>/output/setup/config/clients.d/{billing,payment}.json` |
| Minted key | `~/.authmesh/<svc>/{billing,payment}-consumer.prod.json` (0600) |
| Env | `<svc>/output/production/.env.production` + `.template` |
| Go env | `AB0T_QUOTA_{BILLING,PAYMENT}_URL`, `AB0T_QUOTA_SERVICE_TOKEN` |
| Py client | `app/{billing,payment}_client.py` (X-API-Key) |
| Quota schema | `quota-config.json` |
