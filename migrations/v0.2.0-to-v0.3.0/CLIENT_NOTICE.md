# Client notice — ab0t-quota-go v0.3.0

*Copy/paste to any team whose Go service depends on `github.com/ab0t-com/ab0t-quota-go`.*

---

**Subject: ab0t-quota-go v0.3.0: if your quota-config.json omits `enforcement.enabled`, quotas are now
ENFORCED; your declared prices reach billing; an opt-in sign-up gift for people who join with an account
they already have.**

**ab0t-quota-go v0.3.0** is a MINOR release. Your code compiles unchanged. **One default changed** (first
row) — it matches the Python library.

| What | v0.2.0 | v0.3.0 |
|---|---|---|
| ⚠ **Your `quota-config.json` omits `enforcement.enabled`** | enforcement OFF — every request allowed | **quotas ENFORCED** (default `true`). To keep the old behaviour add `"enforcement": {"enabled": false}` |
| ⚠ **A tier's `credit_grant` omits `reset_on_downgrade`** | `false` — the grant's balance is kept on a downgrade | **`true`** — reset when the org moves to a lower tier. To keep the old behaviour add `"reset_on_downgrade": false` |
| Your price list (`pricing.products`) | read by `meter` only | also sent to billing once per start (`PUT /billing/price-list/{service_name}`, your billing key), from a goroutine — Setup never waits; billing down is retried with capped backoff |
| Someone joins your company with an existing account | no sign-up gift | **opt in** with `"auth_events": {"grant_on_member_joined": true}` — auth's `org.member.added` triggers your tier's sign-up grant, once per person |
| Where the default sign-up gift lands | the event's org | the person's own workspace when the event names it (`data.workspace_org_id`) |

A `Config` you build in Go code (not loaded from a file) keeps exactly the values you give it.

## What to do

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.3.0 && go mod tidy
./migrate-check.sh .     # from migrations/v0.2.0-to-v0.3.0/ — read-only; flags a config omitting either key
```

1. **Check your config files for the two keys.** `migrate-check.sh` lists every `quota-config*.json` that
   omits `enforcement.enabled` or a `credit_grant.reset_on_downgrade`. Decide per file: keep the new
   defaults (write `true` to silence the startup warning) or keep the old behaviour (write `false`).
2. `q.Capabilities().PriceList` shows `published(sha256:…)` once billing has your prices. A billing without
   the route answers 404: one warning, no retry, nothing else changes. To keep your prices out of
   billing: `"billing_integration": {"price_list": false}`.
3. To give the sign-up gift to people who join with an existing account: the `auth_events` switch above,
   and subscribe your webhook to `org.member.added` too.
4. If you wrote your own `CreditGranter`: `CreditGrantRequest.OrgID` can now be the person's workspace.

Details: `MIGRATION.md` in the same directory.
