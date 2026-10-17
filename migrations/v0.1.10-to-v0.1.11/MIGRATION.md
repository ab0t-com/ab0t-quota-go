# Migration: ab0t-quota-go v0.1.10 → v0.1.11

A PATCH release: four bug fixes. **Your code compiles unchanged.** No exported symbol was
removed, changed or added. Two fixes change behaviour you could notice; this kit tells you where
to look.

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.1.11 && go mod tidy
/path/to/ab0t-quota-go/migrations/v0.1.10-to-v0.1.11/migrate-check.sh .    # read-only
go build ./... && go vet ./... && go test ./...
```

`migrate-check.sh` is **read-only** (it never edits anything and makes no network calls). It scans
your files for billing/payment base URLs, your `quota-config*.json` and anything matching the old
capability text, prints each site with what to check, and exits `1` while anything is unreviewed
(`0` when clean, `2` on a usage error; `--advisory` always exits `0`). Mark a site you have checked
with `quotago:v0.1.11-reviewed` on the line or the line above it (any comment syntax), or
`quotago:v0.1.11-reviewed-file` anywhere in the file. JSON has no comments: put the marker in a
`"_comment"` key on the line above, or run with `--advisory` once you have reviewed the config.
Fixtures: `testdata/unreviewed` (3 findings, exit 1) and `testdata/reviewed` (exit 0).

## TL;DR — what changed for you

| # | Rule | Change | Your action |
|---|---|---|---|
| R1 | `REDIRECT` | ⚠ The billing/payment client follows a redirect only on the **same host**, keeping the same scheme and port or **upgrading http → https**. An `https://` base URL that redirects to `http://` on the same host **used to be followed — sending your `X-API-Key` in cleartext — and is now refused.** An `http://` base URL that redirects to `https://` is still followed (your key already went out in cleartext on the first request — use the `https://` address). | If calls fail with `refusing redirect … to another origin`, set the base URL to the final `https://` address. If your base URL is `http://`, change it to `https://` |
| R2 | `MONEYNUMBER` | A money value written as a JSON **number** in `quota-config.json` is read exactly (v0.1.10 rounded it through float64) | Only values with more than ~15 significant digits change — to what you wrote. Prefer strings for money |
| R3 | `WHYOFFTEXT` | With `AB0T_AUTH_WEBHOOK_SECRET` unset and `AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS` set, the capability report no longer says "reject all events" (it was false) | Match on the `WhyOff` key `auth_events_signed`, not its text |
| — | (no rule) | A blank, `.` or `..` org / subscription / session id is refused before any request | Nothing; such an id addressed another route |

## 1. Redirects stay on one origin (R1)

v0.1.10 refused a redirect to another **host**, but compared the host alone. So
`AB0T_QUOTA_BILLING_URL=https://billing.example` answering `302 Location: http://billing.example/…`
(a misconfigured proxy, a TLS terminator's default, or anyone able to inject a redirect) was followed,
and the mesh key went out on a plaintext connection. v0.1.11 requires every hop to stay on the first
request's **host** and to keep the previous hop's **origin** — scheme, host and port, with the default
port filled in (`https://h` and `https://h:443` are the same origin) — or to be an **http → https
upgrade** on that host. A downgrade is refused at any hop, also after an upgrade.

What changes:

| Base URL | Redirects to | v0.1.10 | v0.1.11 |
|---|---|---|---|
| `https://billing.example` | `http://billing.example/…` | followed — **key sent in cleartext** | refused |
| `http://billing.example` | `https://billing.example/…` | followed (the key had already gone out in cleartext on the first request) | followed (unchanged — but use the `https://` URL) |
| `http://billing.example` | `https://billing.example/…` then `http://billing.example/…` | followed | refused at the second hop |
| `https://billing.example` | `https://billing.example:443/…` | refused | followed |
| any | same scheme, host and port | followed | followed (unchanged) |
| any | another host or port | refused | refused (unchanged) |

The error reads `httpx: refusing redirect from <origin> to another origin (<origin>): it would carry
the API key …`. For billing settlement it is retried (`lifecycle_settle_RETRY`), never voided, so no
money event is lost while you fix the URL. Check an address without the library:

```bash
curl -sI "$AB0T_QUOTA_BILLING_URL/health" | grep -iE '^(HTTP|location)'
```

A `3xx` with a `Location` on another host, another port, or from `https://` to `http://` means: put the
final `https://` URL in the variable. A base URL that is `http://` keeps working but sends your key in
cleartext on every first request — change it to `https://`.

## 2. Money in quota-config.json is exact (R2)

`amount_per_period` (tier `credit_grant` and `price`) and `initial_credit` accept a JSON string
(`"5.00"`), a JSON number (`5`, `5.5`, `1e2`) or `null`, as before. v0.1.10 read the number form
through float64: `9007199254740993` became `9007199254740992`, `0.123456789012345678901` became
`0.12345678901234568`. v0.1.11 reads the digits you wrote. A value with up to 15 significant digits
reads the same in both versions. A non-number in a direct `UnmarshalJSON` call (e.g. `NaN`) is now an
error (v0.1.10 panicked).

## 3. Capability text with only the previous secret (R3)

| `AB0T_AUTH_WEBHOOK_SECRET` | `…_PREVIOUS` | Receiver | `WhyOff["auth_events_signed"]` |
|---|---|---|---|
| set | any | accepts current (+ previous) | absent (unchanged) |
| unset | unset | rejects all with `401` | `… receiver will reject all events with 401` (unchanged) |
| unset | set | accepts events signed with the previous secret(s), rejects the rest with `401` | v0.1.10: "reject all events" (false). v0.1.11: `… receiver accepts only events signed with the previous secret(s) in AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS and rejects the rest with 401; set the current secret`, plus a boot WARN |

Behaviour is unchanged; only the report now tells the truth.

## 4. Ids that are not one path segment

Every billing/payment method that puts an id in the URL (`GetUsageSummary`, `GrantPromotionalCredit`,
`ApplyCreditGrant`, `GrantCredit`, `GetSubscription`, `CancelSubscription`, `RecordUsage`,
`SettleActivation`, `CreatePortalSession`, `ListPaymentMethods`, `VerifyCheckoutSession`) refuses a
blank, `.` or `..` id with an error and sends nothing. `url.PathEscape` left `.`/`..` intact, so org `..`
became `/billing/../promotional-credit`, which a normalising proxy resolves to another route.
`RecordUsage` stays best-effort (logs, returns `nil`). Any other id is escaped into one segment as before.

## Agent rules (machine-applicable)

1. For every `R1` finding: if the value is `http://`, change it to the `https://` address. Then run the
   `curl -sI` check above against it. If it redirects to another host or port, or to `http://`, replace
   the value with the final `https://` URL; otherwise mark the line `quotago:v0.1.11-reviewed`.
2. For every `R2` finding: confirm the written number is the intended amount (it is now used exactly);
   optionally rewrite it as a JSON string.
3. For every `R3` finding: key the alert/test on `auth_events_signed` being present in `WhyOff`, or mark
   it reviewed if it only covers the no-secret-at-all case.
4. Re-run the checker until it exits 0; then `go build ./... && go vet ./... && go test ./...`.

**Rollback:** pin `@v0.1.10` and redeploy. v0.1.11 writes nothing v0.1.10 cannot read. Rolling back
re-opens the cleartext-key redirect (R1).
