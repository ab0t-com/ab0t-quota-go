# ab0t-quota-go

Go port of [ab0t-quota](https://github.com/ab0t-com/ab0t-quota) (Python).
A client library for ab0t mesh consumers: tier-aware quota enforcement for
your HTTP API, sign-up credit grants driven by auth events, and (optionally)
durable usage settlement with the billing service.

## What this is

Your Go service uses it to:

1. **Enforce quotas** — request rates, monthly accumulators, concurrent
   resources (gauges), per tier (`free` / `pro` / … — your names), from one
   `quota-config.json`. Counters live in your Redis (or `"memory://"` for a
   single dev process).
2. **Grant sign-up credit exactly once** — auth sends a signed event when an
   org is created or a user joins one; the library verifies it, resolves the
   tier, dedups, and grants the tier's `credit_grant` (trigger `signup`)
   through billing.
3. **Settle metered usage with billing** (paid mode) — through a durable
   outbox, so a crash or a billing outage never loses or double-charges a
   usage event. Paid mode refuses to start without the whole chain.

Every subsystem reports whether it is on and why in `q.Capabilities()`, and
`q.Healthy()` fails when a money-relevant part is missing.

## Install (pin a version)

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.1.10
go install github.com/ab0t-com/ab0t-quota-go/cmd/quotactl@v0.1.10   # the CLI
```

Pre-1.0: read the [CHANGELOG](CHANGELOG.md) (and `migrations/` when present)
before every bump.

## Minimal example

This is [`examples/basic/main.go`](examples/basic/main.go) verbatim (a test
keeps the two identical, and another boots it and drives it past its limit).
Run it from the repo root with
`QUOTA_CONFIG_PATH=examples/basic/quota-config.json go run ./examples/basic`.

```go
// Minimal ab0t-quota integration: load config, guard one route, count usage
// after the work, expose an exempt /healthz. Run from the repo root with:
//
//	QUOTA_CONFIG_PATH=examples/basic/quota-config.json go run ./examples/basic
//	curl -s -H 'X-Demo-User: alice' localhost:8080/api/echo    # 100/hour, then 429
//
// The config declares "memory://" counters (one process, dev only). Point
// storage.redis_url (or QUOTA_REDIS_URL) at your Redis before you run replicas.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ab0t-com/ab0t-quota-go/engine"
	"github.com/ab0t-com/ab0t-quota-go/quota"
)

type ctxKey string

const (
	userKey ctxKey = "user_id"
	orgKey  ctxKey = "org_id"
)

// demoAuth stands in for YOUR auth middleware: it must validate the caller's
// token first and then put the verified user and org in the request context.
// Never trust identity headers from the client in production — this demo does,
// only so it runs with curl.
func demoAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := r.Header.Get("X-Demo-User")
		ctx := context.WithValue(r.Context(), userKey, u)
		ctx = context.WithValue(ctx, orgKey, "org-"+u) // one org per demo user
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func identity(r *http.Request) (string, string, error) {
	u, _ := r.Context().Value(userKey).(string)
	o, _ := r.Context().Value(orgKey).(string)
	return u, o, nil
}

// newMux wires the quota guard around the API. Split out of main so the
// example is tested (examples/basic/main_test.go).
func newMux(q *quota.Quota) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	guard := q.Middleware(quota.MiddlewareDeps{
		Identity: identity,
		Router: func(r *http.Request) (string, float64) {
			return "api.requests", 1 // in real apps, derive the resource from the route
		},
		Exempt: []string{"/healthz"},
	})

	mux.Handle("/api/echo", demoAuth(guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
		// The guard only CHECKS. Count the usage after the work succeeded —
		// without this the limit is never reached.
		u, o, _ := identity(r)
		if _, err := q.Spend(r.Context(), engine.CheckInput{UserID: u, OrgID: o, ResourceKey: "api.requests", Cost: 1}); err != nil {
			log.Printf("quota spend: %v", err) // the work happened: log, reconcile later
		}
	}))))
	return mux
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	q, err := quota.Setup(ctx, quota.Options{}) // QUOTA_CONFIG_PATH, else ./quota-config.json
	if err != nil {
		log.Fatal(err)
	}
	defer q.Close(context.Background())

	srv := &http.Server{Addr: ":8080", Handler: newMux(q)}
	go func() {
		log.Println("listening on :8080")
		_ = srv.ListenAndServe()
	}()
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}
```

The rules this example teaches:

- **Check before, spend after.** `q.Middleware` only *checks* (and writes the
  `X-Quota-*` headers). Usage moves only when you call `q.Spend` after the
  work succeeded; gauges go up with `q.Spend` and down with `q.Release` on
  every termination path.
- **Identity comes from your auth middleware.** The library never validates a
  token. Validate first, then hand the verified user and org to `Identity`.
- **`storage.redis_url` is required.** `"memory://"` is the explicit
  single-process declaration; anything else is your Redis (single node, no
  eviction, Lua allowed — `quotactl provision --local` starts a conforming one).

### What your customers see

| Status | When | Body |
|---|---|---|
| `429` | over the tier limit (also `tier_not_in_config`, `global_kill_switch`) | `{"detail","reason","resource","tier","used","limit","upgrade_url"}` |
| `503` | the engine could not decide (Redis down, unknown `resource_key`) and `FailOpen` is false | `{"detail":"quota engine error"}` |
| `401` | your `Identity` func returned an error | `{"detail":"identity required"}` |

Allowed responses carry `X-Quota-Resource`, `X-Quota-Tier`, `X-Quota-Reason`,
`X-Quota-Limit`, `X-Quota-Remaining`, `X-Quota-Used`, and `X-Quota-Warning`
near a threshold. `Retry-After` is not sent on a `429` today; clients should back off on their own.

## Sign-up credit (auth events)

```go
paidOff := false
q, err := quota.Setup(ctx, quota.Options{
    CreditGranter: quota.BillingCreditGranter(nil), // billing's real credit routes (v0.1.10)
    EnablePaid:    &paidOff,                         // credit grants only: no outbox needed yet
})
mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())
```

- The tier needs `"credit_grant": {"trigger": "signup", "amount_per_period": "5.00",
  "destination": "credit_balance", "lifecycle": "persistent", ...}`. Only
  `trigger: "signup"` grants on auth events (v0.1.10).
- `BillingCreditGranter(nil)` uses the billing client `Setup` builds from
  `AB0T_QUOTA_BILLING_URL` + `AB0T_QUOTA_BILLING_TOKEN` (needs
  `billing.write.transactions`). A `credit_balance`/`persistent` grant goes to
  `POST /billing/{org}/promotional-credit` (billing's gift caps apply); any
  other bucket or lifecycle to `POST /billing/{org}/apply-credit-grant`. The
  idempotency key is derived from the grant (`user:{user}:initial_credit:{tier}`
  by default), never from the delivery, so a retried or replayed event can never
  grant twice.
- Setting `AB0T_QUOTA_BILLING_URL` also asserts paid mode, which refuses to start
  without the durable outbox. For credit grants only, pass `EnablePaid: &false`
  (as above) until you wire metering.
- Mount the receiver at the path you register with auth. Since v0.1.10 it
  answers any path ending in `/_webhooks/auth`, so the full-path mount above, a
  subtree mount and `http.StripPrefix("/api/quotas", …)` all work.
- Register once, loudly: `quotactl subscribe-events --events org.created,user.org_assigned`
  (exits non-zero on any failure). `Options.AutoSubscribeAuthEvents` re-checks at
  every boot, best-effort.
- Rotate the shared secret: deploy with the new value in `AB0T_AUTH_WEBHOOK_SECRET`
  and the old one in `AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS` (both verify), run
  `quotactl subscribe-events --rotate-secret` (auth cannot edit a secret, so this
  deletes and re-creates the subscription), backfill sign-ups from the seconds in
  between with `quotactl backfill`, then drop the `_PREVIOUS` variable.
- A grant that keeps failing (billing down for longer than the ~3 s retry
  budget) ends as `failed_permanent` for that event id; re-posting the same
  event is a no-op. Fix the cause and re-send with `quotactl backfill` (new
  event ids; already-granted users are no-ops).

## Keeping usage correct (self-healing counters)

The Redis counter is a **cache**, never the authority. Each resource declares a
`counter_type`, and the engine reconciles the cache against that type's **source of
truth**. Wiring the truth source is **purely additive** — wire neither provider and
behaviour is exactly as before.

| Counter type | Source of truth | Provider on `quota.Options` |
|---|---|---|
| **Gauge** (concurrency / live level) | a **live count** of what exists now | `ObservedUsageProvider func(ctx, orgID) (map[string]int, error)` |
| **Accumulator** (metered spend) | a **re-sum of your durable event ledger** for the current period (the past is real) | `AccumulatorUsageProvider func(ctx, orgID) (map[string]float64, error)` |
| **Rate** (windowed) | the TTL'd window **is** truth (self-heals) | none |

```go
q, _ := quota.Setup(ctx, quota.Options{
    ConfigPath:               "quota-config.json",
    ObservedUsageProvider:    countLiveResources,  // GAUGE truth
    AccumulatorUsageProvider: sumPeriodLedger,     // ACCUMULATOR truth
})
```

- **Recount-before-deny** — a would-be denial is re-checked against truth first, so a
  gauge stuck high from a lost decrement never wrongly blocks a user. On a truth-source
  outage the fail direction is **type-aware**: a **gauge fails OPEN** (a stale gauge must
  never block), an **accumulator KEEPS the deny** (a ledger-backed meter must never let
  spend blow past the cap), **rate is unchanged**.
- **Read-repair** — `q.Engine.MaybeReadRepair(ctx, orgID)` in your usage/status handler
  repairs a drifted cache opportunistically (throttled, never errors).
- **Recalculate button** — `q.Engine.ReconcileOrg(ctx, orgID, "")` recomputes every
  resource from its truth (pass a `resourceKey` for one), repairs the counter, and returns
  a `ReconcileReport` of per-resource before→after. Idempotent and safe anytime — wire it
  behind `POST /api/quotas/recalculate`.

**If a number ever looks wrong:** it self-heals on the next read or denial; force it with
`ReconcileOrg` (or the Recalculate button). **Never hand-edit the Redis counter** — the
next reconcile overwrites it and it bypasses the `quota.drift_detected` metric that fires
on every repair. See [CONSUMING.md](CONSUMING.md#3-keep-usage-correct--wire-a-truth-source)
for the full contract and `Status` values.

## Idempotency for handlers that touch money

For your own handlers with side effects, wrap them with `authevents.Idempotent`:
delivery dedup on `(handler, event_id)`, optional business dedup on your `Key`,
retries with backoff, and a ledger row per outcome. Full example:
[`examples/with_idempotent/main.go`](examples/with_idempotent/main.go).

```go
h := authevents.Idempotent(handlerledger.IdempotentConfig{
    Handler: "payment_credit_topup",
    Key: func(e handlerledger.Event) string { // "" = no business dedup
        ev, _ := e.(authevents.Event)
        pid, _ := ev.Data["payment_id"].(string)
        if pid == "" {
            return ""
        }
        return "topup:" + pid
    },
}, func(ctx context.Context, e handlerledger.Event, hctx *handlerledger.Context) error {
    if done, err := hctx.AlreadyDone(ctx); err != nil {
        return err // retried
    } else if done {
        return hctx.Skip("already topped up")
    }
    // ... the side effect, with hctx.DedupKey as the provider's idempotency key ...
    _ = hctx.MarkDone(ctx, e.GetEventID())
    return hctx.Success(e.GetEventID())
})
authevents.OnAuthEvent("payment.succeeded", h)
```

A plain `authevents.HandlerFunc` has no dedup: a redelivered event runs it
again. Use it only for side effects that are safe to repeat.

## CLI

```bash
quotactl subscribe-events [--events a,b] [--prefix /api] [--org-slug S | --org-id ID] [--rotate-secret]
quotactl replay   --file EVENTS.jsonl --target https://svc/api/quotas/_webhooks/auth
quotactl backfill --input USERS.csv  --target https://svc/api/quotas/_webhooks/auth [--dry-run]
quotactl capabilities --config quota-config.json   # runs Setup: connects to Redis, may create declared DDB tables
quotactl doctor --config quota-config.json --json  # production-posture grading (same honesty note)
quotactl provision --emit compose|terraform|acl|iam  # infra artifacts; never creates cloud resources
quotactl provision --local                          # one conforming local dev Redis
quotactl --version
```

`replay` and `backfill` read the HMAC secret from `AB0T_AUTH_WEBHOOK_SECRET`
(prefer it to `--secret`, which is visible in `ps`). Every verb exits non-zero
when anything it was asked to do did not happen (v0.1.10). `events` and
`delete-user` cannot reach your service's ledger from outside and say so; run
`q.LedgerStore.QueryByUser` / `DeleteUser` inside your service.

## Environment variables

Only these names are read; generic names other services use are ignored on purpose:

| Variable | Used for |
|---|---|
| `QUOTA_CONFIG_PATH` | config file (else `./quota-config.json`, then `/etc/ab0t/quota-config.json`) |
| `QUOTA_REDIS_URL` | counter store when `storage.redis_url` is empty; `${QUOTA_*}` refs in the config file expand from env |
| `AB0T_QUOTA_BILLING_URL`, `AB0T_QUOTA_BILLING_TOKEN` | billing client; setting the URL asserts paid mode |
| `AB0T_QUOTA_PAYMENT_URL`, `AB0T_QUOTA_PAYMENT_TOKEN` | payment client |
| `AB0T_QUOTA_SERVICE_TOKEN` | fallback key for both of the above |
| `AB0T_AUTH_WEBHOOK_SECRET` | auth-event HMAC secret (unset = every event is refused with 401) |
| `AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS` | also accepted during a secret rotation (comma-separated) |
| `AB0T_AUTH_AUTH_URL`, `AB0T_AUTH_ADMIN_TOKEN`, `AB0T_AUTH_WEBHOOK_PUBLIC_URL` | `subscribe-events` / auto-subscribe |
| `AB0T_QUOTA_REDIS_DURABILITY_CONFIRMED`, `AB0T_QUOTA_REDIS_CLUSTER_CONFIRMED_DISABLED`, `AB0T_QUOTA_DDB_PITR_CONFIRMED` | operator assertions where the server cannot report it (see `examples/basic/quota-config.json` comments) |
| `AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS` | allow `metered_billing` / `one_time_purchase` |
| `AB0T_QUOTA_STRICT_IDEMPOTENCY_KEYS` | refuse malformed counter idempotency keys |
| `AWS_REGION`, `AWS_ENDPOINT_URL` (+ the AWS SDK chain) | DynamoDB / SNS when declared in config |

## How it compares to the Python lib

Same JSON config, same counter keyspace (a mixed Python/Go fleet shares
counters), same auth-event wire contract, same dedup policies and sign-up
idempotency keys. Differences are listed in
[`MIGRATION_FROM_PYTHON.md`](MIGRATION_FROM_PYTHON.md) and the CHANGELOG.

## Contributing — local setup

```bash
make hooks          # installs the .githooks/pre-commit (gitleaks scan)
make test           # go test ./...
make race           # go test -race -count=1 ./...
make scan           # full gitleaks scan, working tree
make scan-staged    # what the pre-commit hook runs
```

The pre-commit hook scans only staged changes with `gitleaks protect
--staged` — fast, only your diff. CI runs a full-history scan on
every push and PR (`.github/workflows/ci.yml`). Allowlist test
fixtures in `.gitleaks.toml`; per-finding fingerprints go in
`.gitleaksignore`. If gitleaks isn't installed locally the hook
prints a warning and lets the commit through — CI still gates.

## Status

Pre-1.0. Pin a tag. Every change is in the [CHANGELOG](CHANGELOG.md).

## Where to learn more

- [`CONSUMING.md`](CONSUMING.md) — prerequisites, versions, the CLI
- [`docs/INTEGRATION_RUNBOOK.md`](docs/INTEGRATION_RUNBOOK.md) — paid mode end to end
- [`PRODUCT_SPEC.md`](PRODUCT_SPEC.md) — design and wire contracts
- Python source: `https://github.com/ab0t-com/ab0t-quota` — the contracts this port mirrors
