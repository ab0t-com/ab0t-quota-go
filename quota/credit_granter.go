package quota

// The built-in billing credit granter (v0.1.10, ticket
// 20261016_signup_credit_receiver_404_and_client_gaps finding 2).
//
// Before v0.1.10 every consumer had to write their own authevents.CreditGranter
// (the library's typed billing client posted to a route billing never served).
// BillingCreditGranter is that granter, on billing's real routes:
//
//	destination credit_balance (or unset) + lifecycle persistent (or unset)
//	    → POST /billing/{org}/promotional-credit   (a gift; billing's per-grant,
//	      lifetime and hourly caps apply — the anti-abuse rails a sign-up gift wants)
//	any other destination / lifecycle
//	    → POST /billing/{org}/apply-credit-grant   (the declared bucket and policy)
//
// Exactly once: the key sent to billing is CreditGrantRequest.IdempotencyKey,
// derived by the default handler from the tier's dedup policy only (never the
// event id), so a redelivery, replay or backfill of the same grant is refused by
// billing's durable dedup even if the library's own ledger has forgotten it.

import (
	"context"
	"errors"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/config"
)

// BillingCreditGranter returns an authevents.CreditGranter that applies sign-up
// grants through billing. Pass the billing client to use, or nil to use the one
// Setup builds from AB0T_QUOTA_BILLING_URL / AB0T_QUOTA_BILLING_TOKEN:
//
//	q, err := quota.Setup(ctx, quota.Options{
//	    CreditGranter: quota.BillingCreditGranter(nil),
//	})
//
// With nil and no billing URL, Setup refuses to start (a granter that can never
// grant must not look wired). Your billing key needs billing.write.transactions
// for the customer orgs it grants to.
func BillingCreditGranter(c *billing.Client) authevents.CreditGranter {
	return &billingCreditGranter{c: c}
}

type billingCreditGranter struct{ c *billing.Client }

// errGranterUnbound is returned if a nil-client granter is used outside Setup.
var errGranterUnbound = errors.New("quota.BillingCreditGranter(nil): no billing client — pass it to " +
	"quota.Setup (which binds AB0T_QUOTA_BILLING_URL), or construct it with billing.New(mesh.Resolve())")

// GrantCredit implements authevents.CreditGranter.
func (g *billingCreditGranter) GrantCredit(ctx context.Context, in authevents.CreditGrantRequest) error {
	if g.c == nil {
		return errGranterUnbound
	}
	key := in.IdempotencyKey
	if key == "" {
		if in.UserID == "" || in.TierID == "" {
			return errors.New("quota.BillingCreditGranter: no idempotency key and no user/tier to derive one")
		}
		key = authevents.BillingIdempotencyKey(string(config.DedupPerUserPerTier), in.UserID, in.OrgID, in.TierID)
	}
	dest, life := in.Destination, in.Lifecycle
	if (dest == "" || dest == string(config.CreditDestCreditBalance)) &&
		(life == "" || life == string(config.CreditLifecyclePersistent)) {
		_, err := g.c.GrantPromotionalCredit(ctx, in.OrgID, billing.PromotionalCreditRequest{
			Amount: in.Amount, Reason: "initial_credit_" + in.TierID, IdempotencyKey: key,
		})
		return err
	}
	if dest == "" {
		dest = string(config.CreditDestSubscriptionCredit) // the config schema default
	}
	if life == "" {
		life = string(config.CreditLifecycleUseItOrLoseIt) // the config schema default
	}
	_, err := g.c.ApplyCreditGrant(ctx, in.OrgID, billing.ApplyCreditGrantRequest{
		Amount: in.Amount, Destination: dest, Lifecycle: life, RolloverMax: in.RolloverMax,
		SourceTier: in.TierID, IdempotencyKey: key, Reason: "signup_grant_" + in.TierID,
	})
	return err
}
