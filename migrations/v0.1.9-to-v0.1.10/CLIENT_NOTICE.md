# Client notice — ab0t-quota-go v0.1.10 (sign-up credit works as documented; money-path fixes)

*Copy/paste to any team that imports `github.com/ab0t-com/ab0t-quota-go`.*

---

**Subject: ab0t-quota-go v0.1.10 — sign-up credit fixed, settlement and CLI fixes. Your code compiles unchanged; run the read-only checker.**

Hi — v0.1.10 is a bug-fix release. No exported API was removed or changed.

**Sign-up credit.** In v0.1.9 the auth-event receiver, mounted as the README and examples showed
(`mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())`), answered `404` to every
event, and the typed billing client granted credit through a route billing does not serve — so no
sign-up credit was granted unless you used the `http.StripPrefix` workaround and your own granter.
Both now work as documented, and there is a built-in granter: `quota.BillingCreditGranter(nil)`.
Your workaround keeps working.

**What you might notice**

| When | v0.1.9 | v0.1.10 |
|---|---|---|
| A paid tier's `credit_grant` (monthly refill) and a user joins an org on it | granted as a gift | not granted (only `trigger: "signup"` grants on auth events) |
| `quotactl subscribe-events` / `replay` / `backfill` fail | exit 0 | non-zero |
| `quotactl events` / `delete-user` | empty result / "deleted 0 rows", exit 0 | refuse (they cannot reach your ledger) |
| Billing answers 401/403 to a settlement (rotated key) | money event voided | kept pending, retried |
| A money event with no `reservation_id` | settled under a blank key (later ones lost silently) | voided + alerted |
| Threshold alerts for two orgs at once | one alert (the other muted) | one per org |

New and optional: `AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS` for secret rotation without rejected events,
`quotactl subscribe-events --rotate-secret`, `config.Warnings()` / `Capabilities.ConfigWarnings`.

## What to do

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.1.10 && go mod tidy
$KIT/migrations/v0.1.9-to-v0.1.10/migrate-check.sh .
go build ./... && go test ./...
```

The checker is read-only and lists what to look at (details in `MIGRATION.md` next to it).
After deploying, read the boot log line `ab0t-quota capabilities` and any `quota config:` warnings.

**Rollback:** pin back to `@v0.1.9` and redeploy.
