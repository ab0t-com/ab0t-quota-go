---
name: ab0t-quota-go-testing
description: Verify an ab0t-quota-go integration works end-to-end and troubleshoot when it doesn't. Use when wiring tests for a new integration, writing unit tests against `quota.Setup` / engine / middleware / handlers, generating signed webhook fixtures, smoke-testing in shadow mode, writing integration tests with httptest, mocking `CreditGranter` or `TierProvider`, inspecting the ledger after a run, debugging "webhook returns 401" / "credit not granted" / "every request gets 429 with tier_unresolved" / "handler runs twice" / "Setup says CreditGrant=false", or producing a test plan handed to QA before flipping enforcement on in prod.
---

# ab0t-quota-go Testing & Troubleshooting

The library is built to be test-friendly: every external dependency
is an interface, the default backends are in-memory, and the
`Capabilities` snapshot tells you exactly what's wired. Use that.

## Phase 1 — Verify it loaded at all

After `quota.Setup(...)`, check the snapshot. If this is wrong, no
test below will pass.

```bash
# From your service's deployment shell, or in a unit test:
quotactl capabilities --config quota-config.json | jq .
```

Expected: `Engine: true`. Required for any real test:

| Capability | Expect | If false |
|------------|--------|----------|
| `Engine` | always `true` | config parse failed; read `WhyOff` |
| `Enforcement` | `true` for prod tests | `enforcement.enabled = false` in config |
| `AuthEvents` | `true` | should always be true after Setup |
| `CreditGrant` | `true` if testing money flow | you forgot to pass `CreditGranter` to Setup |
| `Billing` | `true` for end-to-end | `AB0T_QUOTA_BILLING_URL` not set |
| `LedgerBackend` | `"memory"` in v0.1.0 | stub note; v0.2 wires Redis/DDB |

Programmatic version in a test:

```go
q, err := quota.Setup(ctx, quota.Options{ConfigOverride: cfg})
require.NoError(t, err)
caps := q.Capabilities()
require.True(t, caps.Engine)
require.True(t, caps.Enforcement)
```

## Phase 2 — Unit tests with no network

Use `ConfigOverride` to skip disk + the in-memory stores (default in
v0.1.0).

```go
func TestMyHandler_RespectsQuota(t *testing.T) {
    cfg := &config.Config{
        Enforcement: config.EnforcementConfig{Enabled: true},
        TierProvider: config.TierProviderConfig{
            Type:    "static",
            Mapping: map[string]string{"alice": "pro"},
        },
        Tiers: []config.Tier{{
            TierID: "pro",
            Limits: map[string]config.TierLimit{
                "api.calls": {Limit: ptrFloat(2)},
            },
        }},
        Resources: []config.ResourceDef{{
            ResourceKey: "api.calls",
            CounterType: config.CounterGauge,
        }},
    }
    q, err := quota.Setup(t.Context(), quota.Options{ConfigOverride: cfg})
    require.NoError(t, err)
    defer q.Close(context.Background())

    // Wire your handler with q.Middleware(...) and use net/http/httptest
}

func ptrFloat(f float64) *float64 { return &f }
```

Stubs for the two interfaces you usually inject:

```go
type stubTierProvider struct{ tier string }
func (s stubTierProvider) GetTier(_ context.Context, _, _ string) (string, error) {
    return s.tier, nil
}

type stubGranter struct{ calls atomic.Int32; last authevents.CreditGrantRequest }
func (g *stubGranter) GrantCredit(_ context.Context, in authevents.CreditGrantRequest) error {
    g.calls.Add(1); g.last = in; return nil
}
```

## Phase 3 — Smoke-test the HTTP guard

Three patterns the team must run before flipping enforcement on:

### 3a. "Under the limit" — request succeeds + headers present

```go
req := httptest.NewRequest("GET", "/api/x", nil)
req = req.WithContext(context.WithValue(req.Context(), userKey, "alice"))
rec := httptest.NewRecorder()
srv.ServeHTTP(rec, req)
require.Equal(t, 200, rec.Code)
require.Equal(t, "pro", rec.Header().Get("X-Quota-Tier"))
require.NotEmpty(t, rec.Header().Get("X-Quota-Limit"))
```

### 3b. "Over the limit" — request 429s with denial body

```go
// pre-fill counter to the limit
_, _ = q.Spend(ctx, engine.CheckInput{UserID: "alice", ResourceKey: "api.calls", Cost: 2})

req := httptest.NewRequest("GET", "/api/x", nil)
req = req.WithContext(context.WithValue(req.Context(), userKey, "alice"))
rec := httptest.NewRecorder()
srv.ServeHTTP(rec, req)
require.Equal(t, 429, rec.Code)

var body map[string]any
require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
require.Equal(t, "exceeded", body["reason"])
require.NotEmpty(t, body["upgrade_url"])
```

### 3c. "Exempt path" — `/healthz` always passes, no headers

```go
req := httptest.NewRequest("GET", "/healthz", nil)
rec := httptest.NewRecorder()
srv.ServeHTTP(rec, req)
require.Equal(t, 200, rec.Code)
require.Empty(t, rec.Header().Get("X-Quota-Tier"))
```

## Phase 4 — Smoke-test the webhook receiver

```go
import "github.com/ab0t-com/ab0t-quota-go/authevents"

func TestWebhook_GrantsOnSignup(t *testing.T) {
    gr := &stubGranter{}
    cfg := newCfgWithCreditGrant()  // see Phase 2 pattern
    q, _ := quota.Setup(t.Context(), quota.Options{
        ConfigOverride: cfg,
        CreditGranter:  gr,
    })
    defer q.Close(context.Background())

    body := []byte(`{
        "event_type":"org.created",
        "event_id":"evt-test-1",
        "data":{"user_id":"alice","org_id":"acme"}
    }`)
    secret := "shh"
    t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", secret)

    req := httptest.NewRequest("POST", authevents.WebhookPath, bytes.NewReader(body))
    req.Header.Set("X-Event-Signature", authevents.SignBody(body, secret))
    rec := httptest.NewRecorder()
    q.WebhookHandler().ServeHTTP(rec, req)

    require.Equal(t, 200, rec.Code)
    require.Equal(t, int32(1), gr.calls.Load())
    require.Equal(t, "alice", gr.last.UserID)
}
```

### 4a. Idempotency — replay same event, grant only once

```go
// Replay the exact same body — receiver should dedup
for i := 0; i < 5; i++ {
    rec := httptest.NewRecorder()
    q.WebhookHandler().ServeHTTP(rec, freshReqFromBody(body, sig))
    require.Equal(t, 200, rec.Code)
}
require.Equal(t, int32(1), gr.calls.Load(), "idempotent — only one grant")
```

### 4b. Bad signature — 401

```go
req := httptest.NewRequest("POST", authevents.WebhookPath, bytes.NewReader(body))
req.Header.Set("X-Event-Signature", "sha256=deadbeef")
rec := httptest.NewRecorder()
q.WebhookHandler().ServeHTTP(rec, req)
require.Equal(t, 401, rec.Code)
require.Contains(t, rec.Body.String(), "invalid signature")
```

### 4c. Unknown event_type — 200 + ignored

```go
body := []byte(`{"event_type":"unknown.thing","event_id":"e1"}`)
// ...sign and send...
require.Equal(t, 200, rec.Code)
require.Contains(t, rec.Body.String(), `"ignored"`)
```

## Phase 5 — Bash smoke tests against a live service

These prove the wire is correct end-to-end. Run from a deploy shell
or laptop while pointed at staging:

```bash
SVC=https://gateway-staging.your-domain.com
SECRET="$AB0T_AUTH_WEBHOOK_SECRET"
USER_TOKEN="$(get_jwt_for alice)"

# 1. Healthz is exempt
curl -sI $SVC/healthz | head -1
# expect: HTTP/1.1 200 OK
# expect: no X-Quota-* headers in response

# 2. Authenticated request gets headers
curl -sI -H "Authorization: Bearer $USER_TOKEN" $SVC/api/x | grep X-Quota
# expect: X-Quota-Tier, X-Quota-Limit, X-Quota-Used, X-Quota-Reason

# 3. Webhook with bad sig → 401
curl -s -o /dev/null -w "%{http_code}\n" \
  -X POST $SVC/api/quotas/_webhooks/auth \
  -H 'X-Event-Signature: sha256=deadbeef' \
  -H 'Content-Type: application/json' \
  -d '{"event_type":"org.created","event_id":"e1"}'
# expect: 401

# 4. Webhook with good sig → 200
BODY='{"event_type":"org.created","event_id":"e-test-'$RANDOM'","data":{"user_id":"alice","org_id":"acme"}}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | sed 's/^.* //')
curl -s -X POST $SVC/api/quotas/_webhooks/auth \
  -H "X-Event-Signature: sha256=$SIG" \
  -H 'Content-Type: application/json' \
  -d "$BODY"
# expect: {"status":"ok","ran":1,...}  (or "ignored" if no handler for type)

# 5. Replay same body → still 200, but inner handler doesn't re-run
# (verify via billing-service: balance increased by amount_per_period once)
```

## Phase 6 — Shadow-mode validation

Before flipping enforcement on (Stage 8 of the runbook), keep
`shadow_mode: true` for a calendar day and watch logs:

```bash
# Count shadow-mode would-denies per resource
grep '"reason":"shadow_would_deny"' /var/log/gateway/*.log \
  | jq -r '"\(.resource) \(.tier)"' | sort | uniq -c | sort -rn

# Spot users who'd be denied
grep '"reason":"shadow_would_deny"' /var/log/gateway/*.log \
  | jq -r .user_id | sort | uniq

# Confirm credit grants firing
grep 'credit granted via auth-event' /var/log/gateway/*.log \
  | jq -r '"\(.user_id) \(.tier_id) \(.amount)"'
```

If the would-deny rate is much higher than expected, your tier limits
are too tight (or wrong). Fix before flipping.

## Phase 7 — Failure-mode tests (catch them before prod)

Each row is "a way it breaks + a test that catches it":

| Failure | Test | Verifies |
|---------|------|----------|
| HMAC mismatch | curl with bad sig → 401 | secret wiring symmetric |
| Receiver delivery dedup race | 100 parallel POSTs of same event_id | handler runs ≤ 1 time |
| `CreditGranter` returns transient error | inject error, expect retry | retry config respected |
| `CreditGranter` returns permanent error | inject error N times, expect failed_permanent | ledger row visible via `quotactl events --status failed_permanent` |
| Tier provider returns empty | stub returns "" | handler returns Skip, no false denial |
| Concurrent spend | 100 goroutines Spend(1) | final counter == 100 (race-safe) |
| `shadow_mode` honored | over-limit request | 200 returned, log shows shadow_would_deny |
| Missing env var | unset `AB0T_QUOTA_BILLING_URL` | Capabilities.Billing=false + WhyOff explains |
| Forward-compat config | add unknown top-level key | Setup succeeds; key in Extra map |

The library's own test suite covers most of these — see
`authevents/concurrency_test.go`, `engine/concurrency_test.go`,
`quota/degraded_test.go`. Copy the patterns into your integration test
file.

## Phase 8 — Diagnostic tooling

### Ledger inspection

```bash
# Last 50 attempts for a specific user
quotactl events --user u-alice --limit 50 | jq .

# All permanent failures (needs human attention)
quotactl events --status failed_permanent

# Currently in-progress (handlers running now)
quotactl events --status in_progress
```

v0.1.0 caveat: the ledger is process-local (in-memory). The CLI hits
its own empty store. Inspect via the service's admin RPC, or wait for
v0.2 which wires Redis/DDB.

### Replay missed events

```bash
quotactl replay \
  --file missed-events.jsonl \
  --target https://your-svc/api/quotas/_webhooks/auth \
  --secret "$AB0T_AUTH_WEBHOOK_SECRET"
```

Safe because the receiver's delivery dedup short-circuits already-
processed events.

### Backfill legacy users

```bash
quotactl backfill --input legacy-users.csv \
  --target https://your-svc/api/quotas/_webhooks/auth \
  --secret "$AB0T_AUTH_WEBHOOK_SECRET" --dry-run
# review, then run without --dry-run
```

Same dedup safety — already-credited users are no-ops.

## Troubleshooting by symptom

### "Every request returns 429 with `reason: tier_unresolved`"

→ Identity callback returns no `user_id`/`org_id`. Check upstream auth
middleware is setting the context value the callback reads. Test:

```go
res, err := q.Engine.Provider.GetTier(ctx, "alice", "acme")
require.NoError(t, err)
require.NotEmpty(t, res)  // should be "pro" or default tier
```

### "Every request returns 429 with `reason: tier_not_in_config`"

→ Provider returned a tier_id that's NOT in your `quota-config.json`.
Either add the tier to your config or fix the provider's mapping.
Test by listing what the provider returns:

```bash
quotactl capabilities --config quota-config.json | jq '.Engine'
# inspect the tier list in your config matches what auth returns
```

### "Webhooks return 401 to everything"

→ HMAC secret mismatch. The same value must be in your service's env
AND the auth-side subscription. Generate fresh:

```bash
openssl rand -hex 32 > /tmp/new-secret
# put it in your service env AND re-register the subscription:
quotactl subscribe-events  # idempotent; updates if endpoint matches
```

### "Webhooks return 200 + `status: ignored`"

→ No handler registered for that event_type. Either:
- Pass `CreditGranter` to Setup (registers default handler for
  `org.created` + `user.org_assigned`)
- Register your own with `authevents.OnAuthEvent("type", handler)`

### "Same event grants credit twice"

→ Handler is plain `HandlerFunc` not `Idempotent`-wrapped. Replace
with the pattern in
[ab0t-quota-go-auth-events](../ab0t-quota-go-auth-events/SKILL.md).
Reproduce in test by posting the same event_id twice and asserting
granter.calls == 1.

### "Credit granted but billing says balance is 0"

→ Your `CreditGranter.GrantCredit` succeeded locally but billing-svc
rejected silently. Walk the trail (see
[docs/PAYMENT_PIPELINE.md § Debugging](../../docs/PAYMENT_PIPELINE.md#debugging-a-missing-credit-grant)).

### "`Setup` says `CreditGrant: false`"

→ Read `Capabilities.WhyOff["credit_grant"]`. Almost always: no
`CreditGranter` passed in `quota.Options`. Wire it.

### "Tests pass but prod fails differently"

→ v0.1.0 uses in-memory stores. Process restarts wipe counters. If
your prod test depends on cross-restart state, that's expected. Wait
for v0.2 (Redis/DDB) or design tests to be self-contained.

## Acceptance checklist before flipping enforcement on

Run through this with QA before changing `shadow_mode: false`:

- [ ] `quotactl capabilities` shows all expected-on capabilities `true`
- [ ] `WhyOff` is empty for all required subsystems
- [ ] curl smoke tests (Phase 5) all pass against staging
- [ ] One full day of shadow_mode in staging with zero
      unexpected `shadow_would_deny` events
- [ ] Real auth event in staging fires the credit-grant handler;
      billing-service confirms balance increase
- [ ] Same auth event replayed; handler does NOT fire again;
      billing-service balance unchanged
- [ ] Force a deny in staging (set a user's spend over the cap),
      confirm 429 body has `upgrade_url`
- [ ] Dashboards show real numbers; one alert fires correctly
- [ ] Rollback drill: set `shadow_mode: true` in config, redeploy,
      confirm 429s stop within 60s

When all 9 boxes are checked, you're safe to flip in prod.

## Test plan template

For the gateway team to fill out and submit to QA. Copy this:

```markdown
# ab0t-quota-go integration test plan

## Service: <name>
## Environment: <staging | prod>
## Tester: <name>
## Date: <YYYY-MM-DD>

## 1. Capabilities check
- [ ] `quotactl capabilities` output matches expected
- [ ] Note any unexpected `WhyOff` entries: <list>

## 2. Smoke tests
- [ ] Healthz exempt
- [ ] Authenticated request returns 200 + X-Quota-* headers
- [ ] Bad signature webhook returns 401
- [ ] Good signature webhook returns 200
- [ ] Replay same webhook → handler runs once

## 3. Tier enforcement
- [ ] Free user under limit → 200
- [ ] Free user at limit → 429 with upgrade_url
- [ ] Paid user well over free limit → 200 (different tier)
- [ ] Tier change → next request reflects new limits within <60s

## 4. Shadow mode
- [ ] One day of shadow_mode in staging
- [ ] Shadow-would-deny rate matches synthetic expectation
- [ ] No false positives on paths we care about

## 5. Failure injection
- [ ] CreditGranter transient error → retry + success
- [ ] CreditGranter permanent error → failed_permanent ledger row
- [ ] Receiver concurrency: 100 parallel same-event POSTs → 1 grant

## Sign-off
QA: <signature>
On-call: <signature>
Date approved for prod flip: <YYYY-MM-DD>
```

Submit this with each integration deployment.
