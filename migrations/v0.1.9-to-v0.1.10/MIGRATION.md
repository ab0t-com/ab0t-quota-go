# Migration: ab0t-quota-go v0.1.9 → v0.1.10

A PATCH release: bug fixes plus additive API. **Your code compiles unchanged** — no exported
symbol was removed or changed. Several fixed bugs change behaviour you could notice; this kit
tells you where to look.

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.1.10 && go mod tidy
/path/to/ab0t-quota-go/migrations/v0.1.9-to-v0.1.10/migrate-check.sh .    # read-only
go build ./... && go vet ./... && go test ./...
```

`migrate-check.sh` is **read-only** (it never edits anything). It scans your `.go` files, your
`quota-config*.json` and your scripts/CI files, prints each site with what to check, and exits
`1` while anything is unreviewed (`0` when clean, `2` on a usage error; `--advisory` always
exits `0`). Mark a site you have checked with `quotago:v0.1.10-reviewed` on the line or the line
above it (any comment syntax), or `quotago:v0.1.10-reviewed-file` anywhere in the file. JSON has
no comments: put the marker in a `"_comment"` key on the line above (both runtimes ignore unknown
keys inside a tier), or run with `--advisory` once you have reviewed the config. Fixtures:
`testdata/unreviewed` (8 findings, exit 1) and `testdata/reviewed` (exit 0).

## TL;DR — what changed for you

| # | Rule | Change | Your action |
|---|---|---|---|
| R1 | `GRANTER` | A built-in billing granter exists: `quota.BillingCreditGranter(nil)` | Optional: replace a hand-written promotional-credit granter (see §1 before you switch) |
| R2 | `TRIGGER` | Only `credit_grant.trigger: "signup"` grants on auth events | Check tiers whose grant has another trigger — they no longer grant on sign-up / join |
| R3 | `NOTRIGGER` | A `credit_grant` with no trigger still grants, now with a warning | Add `"trigger": "signup"` (Python grants nothing without it) |
| R4 | `CLI` | `subscribe-events`, `replay`, `backfill` exit non-zero on failure; `events` / `delete-user` refuse | Check scripts and CI that ran them and ignored the exit code |
| R5 | `SECRET-ARGV` | `replay` / `backfill` read the secret from `AB0T_AUTH_WEBHOOK_SECRET` | Drop `--secret "$…"` from scripts (argv is visible in `ps`) |
| R6 | `GRANTCREDIT` | `billing.Client.GrantCredit` now actually grants (was always 404) | Confirm `OrgID` is set and the call is meant to move money |
| R7 | `RECORDUSAGE` (INFO) | Unchanged — reminder: an empty `Cost` is sent as absent and billing prices the row | Set `Cost: "0"`, `PlatformFee: "0"` on metering rows |
| R8 | `CONFIGPATH` | `AB0T_QUOTA_CONFIG_PATH` is not read (never was); now it warns | Rename it to `QUOTA_CONFIG_PATH` |
| R9 | `STRIPPREFIX` | The receiver answers at its full path; `http.StripPrefix` still works | Nothing; optionally simplify the mount |
| R10 | `CONFIG` | Boot warnings for configs that fail open (`Capabilities.ConfigWarnings`) | Read them once after upgrading |
| — | (no rule) | Settlement fallback: blank-key events void loudly; 401/403/route-404 retry instead of voiding; missing `stopped_at` uses the outbox time | Nothing to change; watch the `lifecycle_settle_RETRY` / void alerts after upgrading |
| — | (no rule) | Threshold alerts: cooldown per org; payload gained `org_id`, `reason` | Expect more (correct) alerts; dashboards may group by `org_id` |
| — | (no rule) | DynamoDB handler-ledger tables created by v0.1.9 lack `GSI1`/`GSI2` | If boot logs `handler-ledger table has no GSI1/GSI2`, add them (§6) |

## 1. The built-in credit granter (R1)

```go
paidOff := false
q, err := quota.Setup(ctx, quota.Options{
    CreditGranter: quota.BillingCreditGranter(nil), // uses AB0T_QUOTA_BILLING_URL / _TOKEN
    EnablePaid:    &paidOff,                         // sign-up credit only: no outbox needed
})
```

It sends billing the key in `CreditGrantRequest.IdempotencyKey`:
`user:{user}:initial_credit:{tier}` (`per_user_per_tier`), `org:{org}:initial_credit:{tier}`
(`per_org_per_tier`), `user:{user}:initial_credit`, `org:{org}:initial_credit` — the same keys the
Python runtime sends.

**Before you swap a hand-written granter for it:** if your granter used a different key shape
(the v0.1.9 client docs suggested `{org}:{user}:signup_credit:{tier}`), billing will not recognise
grants it made under the old key. Users already granted are still protected by the library's own
business dedup **when the handler ledger is durable** (Redis or DynamoDB — `capabilities` shows
`ledger`); on the in-memory ledger they are not. Either keep your granter, or switch only with a
durable ledger. A tier `credit_grant` into `subscription_credit` or with a lifecycle other than
`persistent` goes to `/billing/{org}/apply-credit-grant` (the declared bucket), not to
promotional credit.

## 2. Sign-up grants only fire for `trigger: "signup"` (R2, R3)

v0.1.9 granted **any** tier's `credit_grant` on `org.created` / `user.org_assigned`. A paid tier's
monthly refill (`subscription_invoice_paid`, required by `subscription_with_credits`) was handed out
as a gift every time someone joined an org on that tier. v0.1.10 skips it (ledger row `skipped`,
reason `credit_grant trigger is …, not signup`). If you *wanted* a sign-up gift on that tier, it
needs its own `signup` grant (a tier has one `credit_grant`: use a separate sign-up tier, or grant
from your own handler). A grant with no trigger still fires, with a boot warning.

## 3. CLI exit codes (R4, R5)

| Command | v0.1.9 | v0.1.10 |
|---|---|---|
| `subscribe-events`, registration fails | exit 0, `subscription_id:` empty | non-zero, `HTTP <status>` + remedy |
| `subscribe-events`, subscription exists with another secret / other events | exit 0, nothing changed | non-zero, names `--rotate-secret` |
| `subscribe-events --rotate-secret` | (unknown flag) | deletes + re-creates the subscription |
| `replay` / `backfill`, receiver answers non-2xx | exit 0 | non-zero, counts the failures |
| `backfill` without `--target` / secret | ran, every row failed, exit 0 | usage error (unless `--dry-run`) |
| `events`, `delete-user` | read an empty in-process stub, exit 0 | refuse, non-zero — use `q.LedgerStore` in your service |

Rotating the webhook secret: deploy with the new secret in `AB0T_AUTH_WEBHOOK_SECRET` and the old
one in `AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS`, run `quotactl subscribe-events --rotate-secret`,
backfill sign-ups from the seconds between delete and create with `quotactl backfill`, then drop
`AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS`.

## 4. Money-path fixes you do not need to act on

- A lifecycle money event with no `reservation_id` is voided + alerted (`no_settlement_key`)
  instead of being settled under a blank key that billing deduped forever.
- `401`/`403`/route-`404` from billing keep money events pending (retried) instead of voiding them.
  After a key rotation you will see `lifecycle_settle_RETRY` until the key is fixed, then they settle.
- A terminal event without `stopped_at` is priced to the time it entered the outbox, not to settle time.

## 5. Alerts

The cooldown is per org now. A webhook consumer receives `org_id` and `reason` in addition to the
v0.1.9 keys (nothing was removed).

## 6. DynamoDB handler ledger without indexes

If you declared `storage.dynamodb_table`, the library created the handler-ledger table itself. v0.1.9
created it without `GSI1`/`GSI2`, so `QueryByUser`, `QueryByStatus` and `DeleteUser` failed with
`ValidationException`. New tables get both. For an existing table, add them (online, no downtime):

```bash
aws dynamodb update-table --table-name <your table> \
  --attribute-definitions AttributeName=GSI1PK,AttributeType=S AttributeName=GSI1SK,AttributeType=S \
  --global-secondary-index-updates '[{"Create":{"IndexName":"GSI1","KeySchema":[{"AttributeName":"GSI1PK","KeyType":"HASH"},{"AttributeName":"GSI1SK","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}}}]'
# wait for GSI1 to be ACTIVE, then the same for GSI2 / GSI2PK / GSI2SK
```

## Agent rules (machine-applicable)

1. For every `R4`/`R5` finding in a shell/CI file: make the step fail on a non-zero exit (remove
   `|| true`, `set +e` around it); replace `--secret "$AB0T_AUTH_WEBHOOK_SECRET"` with nothing
   (the env is read). Replace `quotactl events` / `delete-user` steps with a call to your service.
2. For every `R8` finding: rename `AB0T_QUOTA_CONFIG_PATH` → `QUOTA_CONFIG_PATH`.
3. For every `R3` finding: add `"trigger": "signup"` to that `credit_grant`.
4. `R1`, `R2`, `R6`, `R10` need a human decision: mark the site reviewed after deciding.
5. Re-run the checker until it exits 0; then `go build ./... && go vet ./... && go test ./...`.

**Rollback:** pin `@v0.1.9` and redeploy. Nothing v0.1.10 writes is unreadable by v0.1.9 (the new
DynamoDB indexes are ignored by it).
