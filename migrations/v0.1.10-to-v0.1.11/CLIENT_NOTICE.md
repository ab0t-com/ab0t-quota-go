# Client notice — ab0t-quota-go v0.1.11 (API key no longer follows an https→http redirect)

*Copy/paste to any team that imports `github.com/ab0t-com/ab0t-quota-go`.*

---

**Subject: ab0t-quota-go v0.1.11 — security fix: the billing/payment key is never sent over a downgraded redirect. Your code compiles unchanged; run the read-only checker.**

Hi — v0.1.11 is a bug-fix release. No exported API was removed, changed or added.

**Security fix (medium).** The library's billing/payment client refused a redirect to another host,
but compared the host only. If your `AB0T_QUOTA_BILLING_URL` / `AB0T_QUOTA_PAYMENT_URL` is `https://…`
and that address answered a redirect to `http://` on the same host, the client followed it and sent
your mesh `X-API-Key` in cleartext. v0.1.11 follows a redirect only on the same host, keeping the
same scheme and port or upgrading from `http://` to `https://`; a downgrade is always refused.
If you have ever seen such a redirect from your billing/payment address, rotate the key.

**What you might notice**

| When | v0.1.10 | v0.1.11 |
|---|---|---|
| The billing/payment URL redirects `https://` → `http://` | followed, key sent in cleartext | refused (`refusing redirect … to another origin`) |
| The billing/payment URL is `http://` and redirects to `https://` | followed | followed (unchanged) — but set the `https://` URL directly: an `http://` URL sends the key in cleartext first |
| Only `AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS` is set (no current secret) | capability said "receiver will reject all events" (false) | says it accepts only the previous secret(s); behaviour unchanged |
| An org / subscription / session id that is blank, `.` or `..` | request sent to another route | refused before sending |
| A money amount written as a long JSON number in `quota-config.json` | rounded through float64 | read exactly as written |

A refused redirect on the settlement path is retried, never voided — no money event is lost while
you fix the URL.

## What to do

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.1.11 && go mod tidy
$KIT/migrations/v0.1.10-to-v0.1.11/migrate-check.sh .
go build ./... && go test ./...
```

The checker is read-only and lists what to look at (details in `MIGRATION.md` next to it).
After deploying, if billing or payment calls fail with `another origin`, put the final `https://`
address in `AB0T_QUOTA_BILLING_URL` / `AB0T_QUOTA_PAYMENT_URL`.

**Rollback:** pin back to `@v0.1.10` and redeploy (this re-opens the cleartext-key redirect).
