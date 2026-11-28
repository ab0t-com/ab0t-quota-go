# Client notice — ab0t-quota-go v0.3.2

*Copy/paste to any team whose Go service depends on `github.com/ab0t-com/ab0t-quota-go`.*

---

**Subject: ab0t-quota-go v0.3.2: your sign-up gift now arrives; the library tells you when money cannot
move the way your config says.**

**ab0t-quota-go v0.3.2** is a PATCH release. Your code compiles unchanged and nothing you configured
changes meaning. Two things you were promised now happen, so read the first row.

| What | v0.3.1 | v0.3.2 | Who must act |
|---|---|---|---|
| **Sign-up gift** | the default handler listened on `org.created` / `user.org_assigned`, which auth never delivers to a webhook: **the gift never arrived** | it listens on `auth.user.registered`, the event auth delivers for a new account; **gifts start arriving** once your subscription names it | everyone with a sign-up gift: re-subscribe once (below) |
| `quotactl subscribe-events` default | `org.created,user.org_assigned` | `auth.user.registered` (+ `org.member.added` when `auth_events.grant_on_member_joined`; pass `--config`) | — |
| `Capabilities().CreditGrant` | `true` whenever a granter was passed | `false`, with the cause in `WhyOff["credit_grant"]`, when no gift can arrive (no `AB0T_AUTH_WEBHOOK_SECRET`; auto-subscribe made none; the existing subscription does not listen to `auth.user.registered`); `CreditGrantEvents`, `CreditGrantSubscription` | a health check reading it may now see false — that is the truth |
| `tier_provider: "mesh"` | no lookup: every org got `default_tier` | billing's tier for each org (`GET /billing/{org_id}/tier`, your billing key; cached 60 s when unset) | set `BILLING_SERVICE_URL=https://billing.service.ab0t.com` and `BILLING_SERVICE_API_KEY` |
| `tier_provider: "jwt"` default claim | `tier` | `org_tier` (the Python default); `tier` is still read when no `jwt_claim_key` is set | tokens issued by the ab0t auth service carry neither — use `"mesh"` |
| Price list answered 403 | never retried; "check your key" | means billing has not bound your service name to your account yet; retried every 6 h (28 times at most) | run `authsetup status` (authsetup 0.5.31) and send its price-list request block, with its `PL-…` reference, to ab0t support |
| `billing_model` | Python names were "unknown" | one name set; `free_tier` / `enterprise` / `metered_billing` still work, with a warning naming the canonical name | optional: write the canonical name |
| `credit_grant.trigger: "scheduled_period_start"` | accepted silently | accepted, with a warning: neither library fires it — that grant is never given | replace it |

## What to do

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.3.2 && go mod tidy
/path/to/ab0t-quota-go/migrations/v0.3.1-to-v0.3.2/migrate-check.sh .     # read-only
go build ./... && go vet ./... && go test ./...
```

1. **Re-subscribe your receiver once** (needs `AB0T_AUTH_ADMIN_TOKEN`):
   `quotactl subscribe-events --config quota-config.json --rotate-secret`. A subscription made from the
   old docs listens to events auth never sends. Sign-ups between the delete and the create are not
   delivered: `quotactl backfill` them.
2. **If you granted the gift some other way because it never arrived, stop** — the library grants it
   now (once per person per company), and two paths mean two gifts.
3. **Check the money path once per environment:**
   `quotactl doctor --money --live --org <an org that pays> --expect-tier <its tier>`. Anything it could
   not see says `not_checked`; nothing unseen reads `good`.
4. Your auth environment must deliver webhooks (ask ab0t support whether webhook delivery is on in your
   environment); `doctor --money` shows whether a gift event has arrived when run in-process.

The checker flags: subscriptions or handlers only on the old names (R1), `subscribe-events` calls with
the default events (R2), your own grant code (R3), alias / experimental billing models and
`scheduled_period_start` (R4), and `jwt` / `mesh` tier sources (R5). Mark a reviewed site with
`quotago:v0.3.2-reviewed`. Fixtures: `testdata/unreviewed` (findings, exit 1), `testdata/reviewed`
(exit 0).

## Erratum — the v0.3.0 notice

The v0.3.0 notice said a billing without the price-list route "answers 404: one warning, no retry". In
practice ab0t billing answers **403** until it binds your service name to your account (decision D-029),
and v0.3.0 / v0.3.1 logged it as "check the service's billing key and its permissions" — it is not a key
problem. v0.3.2 says so and retries; nothing else in that notice changes.

Questions: reply to this notice.
