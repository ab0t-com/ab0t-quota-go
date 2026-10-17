package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/shopspring/decimal"
)

// Client is the typed billing client.
type Client struct {
	http *httpx.Client
}

// New constructs a Client from a mesh.URLs bundle. Returns nil + error if
// the billing URL isn't configured; callers should treat absent billing as
// a Capability=false (logged at Setup).
func New(u mesh.URLs) (*Client, error) {
	if u.Billing == "" {
		return nil, errors.New("billing: AB0T_QUOTA_BILLING_URL not set")
	}
	return &Client{http: httpx.New(u.Billing, u.BillingToken)}, nil
}

// CheckQuota calls POST /billing/quota/check. Used by the bridge engine
// when local enforcement is disabled.
//
// WARNING (ticket 20261016, B-07): billing serves no POST /billing/quota/check —
// this call answers 404. Billing's hosted check is
// POST /billing/quota/{service_name}/{org_id}/check/{resource_key}
// (modules/quota/router.py); the Go runtime has no bridge engine yet.
func (c *Client) CheckQuota(ctx context.Context, in QuotaCheckRequest) (*QuotaCheckResponse, error) {
	var out QuotaCheckResponse
	if err := c.http.POST(ctx, "/billing/quota/check", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetUsageSummary calls GET /billing/usage/{org_id}/summary.
func (c *Client) GetUsageSummary(ctx context.Context, orgID string) (*UsageSummary, error) {
	org, err := seg("GetUsageSummary", "org id", orgID)
	if err != nil {
		return nil, err
	}
	var out UsageSummary
	if err := c.http.GET(ctx, "/billing/usage/"+org+"/summary", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GrantCredit grants a sign-up / gift credit into in.OrgID's credit_balance via
// billing's POST /billing/{org_id}/promotional-credit (GrantPromotionalCredit).
//
// v0.1.10 bug fix (ticket 20261016): v0.1.9 posted to /billing/credits/grant, a
// route billing never served — every call failed with 404 and no credit moved.
// The signature is unchanged. The idempotency key is in.IdempotencyKey, else
// derived from the grant (see CreditGrantRequest), so a retry never grants twice.
// For a grant into another bucket or under a lifecycle policy, use
// ApplyCreditGrant.
func (c *Client) GrantCredit(ctx context.Context, in CreditGrantRequest) (*CreditGrantResponse, error) {
	if strings.TrimSpace(in.OrgID) == "" {
		return nil, errors.New("billing: GrantCredit: OrgID is required (the org that receives the credit)")
	}
	key := in.IdempotencyKey
	if strings.TrimSpace(key) == "" {
		switch {
		case in.UserID != "" && in.TierID != "":
			key = "user:" + in.UserID + ":initial_credit:" + in.TierID
		case in.EventID != "":
			key = "event:" + in.EventID
		default:
			return nil, errors.New("billing: GrantCredit: no stable idempotency key — set IdempotencyKey, " +
				"or UserID+TierID, or EventID (a grant without one can be applied twice)")
		}
	}
	reason := in.Reason
	if reason == "" && in.TierID != "" {
		reason = "initial_credit_" + in.TierID
	}
	res, err := c.GrantPromotionalCredit(ctx, in.OrgID, PromotionalCreditRequest{
		Amount: in.Amount, Reason: reason, IdempotencyKey: key,
	})
	if err != nil {
		return nil, err
	}
	return &CreditGrantResponse{GrantID: res.PaymentID, Balance: res.NewBalance, Result: res}, nil
}

// GrantPromotionalCredit calls POST /billing/{org_id}/promotional-credit — add
// in.Amount to the org's credit_balance (a gift, not real money). Exactly once
// per in.IdempotencyKey: billing answers a repeat with the original result.
// Billing's refusals come back as *httpx.Error with the real status: 400 (over
// the per-grant or lifetime cap), 403 (your key cannot grant for this org), 409
// (a concurrent grant for this org, or a prior attempt with this key did not
// finish — retry with the SAME key), 422 (bad request), 429 (grant rate limit).
func (c *Client) GrantPromotionalCredit(ctx context.Context, orgID string, in PromotionalCreditRequest) (*UpdateBalanceResponse, error) {
	if err := checkGrant("GrantPromotionalCredit", orgID, in.Amount, in.IdempotencyKey); err != nil {
		return nil, err
	}
	org, err := seg("GrantPromotionalCredit", "org id", orgID)
	if err != nil {
		return nil, err
	}
	var out UpdateBalanceResponse
	if err := c.http.POST(ctx, "/billing/"+org+"/promotional-credit", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ApplyCreditGrant calls POST /billing/{org_id}/apply-credit-grant — a grant into
// in.Destination under in.Lifecycle (the tier credit_grant declaration). Same
// idempotency and error contract as GrantPromotionalCredit; no lifetime cap.
func (c *Client) ApplyCreditGrant(ctx context.Context, orgID string, in ApplyCreditGrantRequest) (*UpdateBalanceResponse, error) {
	if err := checkGrant("ApplyCreditGrant", orgID, in.Amount, in.IdempotencyKey); err != nil {
		return nil, err
	}
	if in.Destination == "" || in.Lifecycle == "" {
		return nil, errors.New("billing: ApplyCreditGrant: Destination and Lifecycle are required")
	}
	if (in.Lifecycle == "rollover_capped") != (in.RolloverMax != nil) {
		return nil, errors.New("billing: ApplyCreditGrant: RolloverMax is required for lifecycle " +
			"rollover_capped and must be absent otherwise")
	}
	org, err := seg("ApplyCreditGrant", "org id", orgID)
	if err != nil {
		return nil, err
	}
	var out UpdateBalanceResponse
	if err := c.http.POST(ctx, "/billing/"+org+"/apply-credit-grant", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func checkGrant(op, orgID string, amount decimal.Decimal, key string) error {
	switch {
	case strings.TrimSpace(orgID) == "":
		return fmt.Errorf("billing: %s: org id is required", op)
	case amount.Sign() <= 0:
		return fmt.Errorf("billing: %s: amount must be > 0", op)
	case strings.TrimSpace(key) == "":
		return fmt.Errorf("billing: %s: idempotency key is required (non-blank, the same on every retry)", op)
	}
	return nil
}

// seg escapes one path segment, so an id can never address another route. A
// blank, "." or ".." id is refused (v0.1.11): url.PathEscape leaves dot segments
// intact, and a normalising proxy resolves "/billing/../x" to "/x".
func seg(op, what, s string) (string, error) {
	v, err := httpx.PathSegment(what, s)
	if err != nil {
		return "", fmt.Errorf("billing: %s: %w", op, err)
	}
	return v, nil
}

// GetSubscription calls GET /subscriptions/{org_id}.
//
// WARNING (ticket 20261016, B-07): subscriptions live on the PAYMENT service,
// not billing — on the billing host this answers 404. Payment's
// GET /subscriptions/{org_id} returns a LIST ({subscriptions,total,has_more}),
// not one Subscription. Call payment directly until a typed method ships.
func (c *Client) GetSubscription(ctx context.Context, orgID string) (*Subscription, error) {
	org, err := seg("GetSubscription", "org id", orgID)
	if err != nil {
		return nil, err
	}
	var out Subscription
	if err := c.http.GET(ctx, "/subscriptions/"+org, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelSubscription calls DELETE /subscriptions/{org_id}/{subscription_id}.
// Per back_references.md fix C5 — DELETE, not POST.
//
// WARNING (ticket 20261016, B-07): that route is on the PAYMENT service; on the
// billing host this answers 404.
func (c *Client) CancelSubscription(ctx context.Context, orgID, subID string) error {
	org, err := seg("CancelSubscription", "org id", orgID)
	if err != nil {
		return err
	}
	sub, err := seg("CancelSubscription", "subscription id", subID)
	if err != nil {
		return err
	}
	return c.http.DELETE(ctx, "/subscriptions/"+org+"/"+sub, nil)
}

// Heartbeat calls POST /billing/heartbeat. NOTE: billing serves no such route —
// it answers 404 (ticket 20261016). The HeartbeatLoop therefore stops itself,
// with one INFO line, on the first 404/405 instead of logging a warning every
// interval forever. Kept for API stability.
func (c *Client) Heartbeat(ctx context.Context, in HeartbeatRequest) error {
	return c.http.POST(ctx, "/billing/heartbeat", in, nil)
}

// RecordUsage records a metering/analytics usage row via
// POST /billing/usage/{org_id}/. This is the METERING path, NOT the money
// path (charge = reserve->commit proration at stop/delete). To avoid cost
// fabrication / double-charging, the row MUST carry Cost="0", PlatformFee="0"
// and ReservationID (see ticket 20260615_inter_service_contract_drift,
// WORKFLOW_FINDINGS section 2). Best-effort: errors are logged and swallowed.
//
// ⚠ Cost and PlatformFee are omitempty: a row that leaves them EMPTY is sent
// without them, and billing then prices it with its fallback rates and minimum
// charge (billing app/services/usage_service.py) — debiting the customer for what
// is meant to be a count. Always set Cost: "0" and PlatformFee: "0" on a metering
// row. (Not defaulted by the library: some callers rely on omission to ask billing
// to price the row — ticket 20261016, B-09, an owner decision.)
func (c *Client) RecordUsage(ctx context.Context, in RecordUsageRequest) error {
	org, err := seg("RecordUsage", "org id", in.OrgID)
	if err != nil {
		slog.Warn("billing record usage refused", "err", err)
		return nil
	}
	if err := c.http.POST(ctx, "/billing/usage/"+org+"/", in, nil); err != nil {
		slog.Warn("billing record usage failed", "err", err, "org", in.OrgID)
		return nil
	}
	return nil
}

// SettleActivation calls POST /billing/{org_id}/settle — the durable,
// activation-scoped settlement path. **D-12: the revenue-loss close.**
//
// Billing's /commit is state-guarded on a LIVE Redis reservation hash; past the
// reservation window it 404s and the revenue is LOST FOREVER. This endpoint needs
// no live hash, so a settlement arriving arbitrarily late still lands — exactly
// once, in the correct three-bucket spend order.
//
// Ticket: billing/output/tickets/20260712_revenue_chain_integrity. This method is
// the CALLER — without it, billing's settlement endpoint is a mechanism nobody
// invokes and the money is still gone (D-64: a mechanism is not a guarantee).
//
// Idempotent: a replay returns 200 with Replayed=true and the ORIGINAL payload;
// no money moves. Status codes are billing's real contract, read from its handler
// (app/api/billing.py:720 -> app/core/reservation.py:1110):
//
//	409 - not eligible: /commit already took this money, or the reservation is
//	      still live. THE BOOKS ARE CORRECT — not an error, not a revenue loss.
//	400 - negative cost (refused; it would CREDIT the customer).
//	404 - no billing account for this org.
//	5xx - transient; the settlement did not land, or landed and the response was
//	      lost. Retry — the durable key makes that safe in both readings.
func (c *Client) SettleActivation(ctx context.Context, orgID string, in SettleActivationRequest) (*SettleActivationResponse, error) {
	org, err := seg("SettleActivation", "org id", orgID)
	if err != nil {
		return nil, err
	}
	var out SettleActivationResponse
	if err := c.http.POST(ctx, "/billing/"+org+"/settle", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
