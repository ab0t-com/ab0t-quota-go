# Client notice — ab0t-quota-go v0.3.1

*Copy/paste to any team whose Go service depends on `github.com/ab0t-com/ab0t-quota-go`.*

---

**Subject: ab0t-quota-go v0.3.1: changing your sign-up rule no longer gives existing customers your
sign-up gift again.**

**ab0t-quota-go v0.3.1** is a PATCH release. Your code compiles unchanged, no setting changed, and what
your `CreditGranter` sends to billing (`CreditGrantRequest.IdempotencyKey`) is the same as before.

| What | v0.3.0 | v0.3.1 |
|---|---|---|
| You change `credit_grant.dedup` (or a person's plan changes under a per-plan rule), then an existing customer's sign-up is handled again (a redelivery past the delivery window, a replay) | the new rule's key matched none of the old markers: **a second gift** | **no second gift** — the gift is once per person, per company, whatever the rule |
| Customers gifted before you upgrade | — | still recognised: a ledger marker an earlier release wrote under your current rule, or under a per-person rule (`per_user_per_tier`, `per_user_global`), counts as "already gifted" |
| `CreditGrantRequest.IdempotencyKey` | the current rule's key | **unchanged** |
| The default sign-up handler without `MemberJoinedEnabled` | no once-per-person key | always checks and writes `MemberJoinedGrantKey(company, user)` |

## What to do

```bash
go get github.com/ab0t-com/ab0t-quota-go@v0.3.1 && go mod tidy
```

1. **Nothing is required.** Your rule still works as before for new people; it can make the gift rarer
   (`per_org_global`: one gift per org) but can no longer make it happen twice for one person.
2. **Use a durable ledger** (DynamoDB or Redis). The once-per-person guard lives in your ledger; billing
   still dedups only on the current rule's key.
3. **Known limit:** a person gifted before v0.3.1 under a per-ORG rule (`per_org_per_tier`,
   `per_org_global`), whose sign-up is handled again after you switch to a per-person rule, is not
   recognised — per-org markers name an org, not a person. Everyone gifted from v0.3.1 on is.

New helper (optional): `authevents.SignupGiftOnceKeys(policy, userID, orgID, tierID, companyOrgID)`
returns the once-key, the key billing receives, and every key that means "already gifted".

Questions: reply to this notice.
