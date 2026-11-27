# Migration: ab0t-quota-go v0.2.0 → v0.3.0

A MINOR release: your code compiles unchanged, no exported symbol was removed, renamed or re-typed —
and **ONE default changed** (R0, section 4): **if your `quota-config.json` omits `enforcement.enabled`,
quotas are now ENFORCED** (and a `credit_grant` that omits `reset_on_downgrade` now resets on a
downgrade), matching the Python library. Two things are new besides.

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.3.0 && go mod tidy
/path/to/ab0t-quota-go/migrations/v0.2.0-to-v0.3.0/migrate-check.sh .    # read-only
go build ./... && go vet ./... && go test ./...
```

`migrate-check.sh` is **read-only** (it never edits anything and makes no network calls). It FLAGS every
quota config that omits `enforcement.enabled` or a `credit_grant.reset_on_downgrade` (their defaults
changed), and lists the config files that declare `pricing.products` (they will now publish a price
list) and the code that builds its own sign-up-gift handler or granter (the gift may now land on the
person's workspace). It exits `1` while anything is unreviewed (`0` when clean, `2` on a usage error;
`--advisory` always exits `0`). Fix a DEFAULTS finding by writing the key; mark any other site you have
checked with `quotago:v0.3.0-reviewed` on the line or the line above it, or `quotago:v0.3.0-reviewed-file`
anywhere in the file (JSON: a `"_comment"` key holding the marker). Fixtures: `testdata/unreviewed`
(4 findings, exit 1) and `testdata/reviewed` (exit 0).

## TL;DR

| # | Rule | Change | Your action |
|---|---|---|---|
| R0 | `DEFAULTS` | ⚠ **If your `quota-config.json` omits `enforcement.enabled`, quotas are now ENFORCED** (it loaded as `false` — every request allowed). **If a tier's `credit_grant` omits `reset_on_downgrade`, it is now `true`** (the grant's balance is reset when the org moves to a lower tier; it was `false`). Python parity; one startup warning per omitted key. A `Config` built in Go code is unaffected | To keep the old behaviour add `"enforcement": {"enabled": false}` and `"reset_on_downgrade": false`; to accept the new one write `true` (silences the warning) |
| R1 | `PRICELIST` | `quota.Setup` sends your declared prices (`pricing.products`) to billing once per start: `PUT /billing/price-list/{service_name}` with your billing key, from a goroutine (Setup never waits). Billing down → retried 30 s, 60 s, 120 s, then every 300 s until acknowledged or `Close`; 404 (no price-list route yet) → one warning, no retry; other 4xx → one error, no retry | Nothing. Opt out: `"billing_integration": {"price_list": false}` |
| R2 | `WORKSPACE` | The default sign-up handler gives the gift to the person's own workspace when the event names it (`data.workspace_org_id`), else to the event's org as before. A custom `CreditGranter` sees it as `CreditGrantRequest.OrgID` | Nothing, unless your granter assumed `OrgID` is always the org the person joined |
| — | (opt-in) | `"auth_events": {"grant_on_member_joined": true}` registers a handler on auth's `org.member.added`: a person who joins with an account they already have gets the tier's sign-up grant, **once per person per company** (`authevents.MemberJoinedGrantKey`, shared with the sign-up handler; checked fail-closed) | Only if you want it; subscribe your webhook to `org.member.added` |

## 1. The price list (R1)

Your usage events tell billing a rate. Without your price list billing had nothing to compare it with —
a forgotten or wrong rate was billed as sent. With it, billing can check every event against the
prices you declared once.

| | v0.2.0 | v0.3.0 |
|---|---|---|
| `pricing.products` | read by `meter` only | also published to billing at startup |
| Body | — | `{"products": …, "published_by": "ab0t-quota-go/<version>", "schema": "ab0t.price_list.v1", "service_name": …, "version": "sha256:…"}` — money as canonical strings |
| Same prices, another library or another start | — | the same `version` (it is the hash of the canonical prices) |
| Invalid prices | refused by `meter.New` when you build a meter | also never published (`ErrConfig`, logged) |
| Where to look | — | `q.Capabilities().PriceList`: `publishing(…)`, `published(…)`, `retrying(attempt N)`, `unsupported(404: …)`, `refused(<status>)`, `off(<why>)` |

It is skipped (and `PriceList` says why) when: no `pricing.products`, `billing_integration.price_list`
is `false`, paid mode is off, `Options.SkipBackgroundLoops` is set, no service name resolves
(`AB0T_SERVICE_NAME`, else `service_name`, else the first resource's `service`), or billing is not
wired.

## 2. The sign-up gift on the person's workspace (R2)

When the auth service names the person's own workspace in the event (`data.workspace_org_id`, for
companies that give every customer a private workspace), that workspace is the account you bill — the
gift goes there. Until the field is sent, nothing changes.

## 3. Opt in: a gift for people who join with an existing account

```json
{"auth_events": {"grant_on_member_joined": true}}
```

`auth.user.registered` is sent only when the ACCOUNT is new. A person who already has an account and
joins your company produces `org.member.added` instead. With the switch on, that event triggers the
same sign-up grant (the tier's `credit_grant` with `"trigger": "signup"`) — once per person per company:
a redelivery, a re-join after a removal, a re-join on another tier, or a sign-up plus a join of the same
person give one gift. The block is strict: any other key, or a non-boolean value, is refused at load.

Known gap (unchanged): the default sign-up handler listens to `org.created` / `user.org_assigned`. The
auth service delivers `auth.user.registered` for a new account; register `BuildDefaultCreditGrantHandler`
on it yourself if you want the gift on sign-up (`authevents.OnAuthEvent("auth.user.registered", h)`).

## 4. Two defaults now match the Python library (R0 — the breaking default change)

| Key omitted from the config FILE | Go v0.2.0 | **Go v0.3.0** | Python | To keep the v0.2.0 behaviour |
|---|---|---|---|---|
| `enforcement.enabled` | false — EVERY request allowed | **true — quotas ENFORCED** | true | `"enforcement": {"enabled": false}` |
| a tier's `credit_grant.reset_on_downgrade` | false — balance kept on a downgrade | **true — reset on a downgrade** | true | `"reset_on_downgrade": false` in that `credit_grant` |

`config.LoadConfig` applies these defaults only to keys the file leaves out; `config.Warnings()` (logged by
`quota.Setup`, `Capabilities().ConfigWarnings`) names each one with the text the Python library prints.
A `Config` or `CreditGrant` built in Go code keeps the values you give it, and a grant synthesized from a
legacy `initial_credit` keeps `false`. Before deploying, run `migrate-check.sh` (rule `DEFAULTS`) and write
both keys in every config file it lists.
