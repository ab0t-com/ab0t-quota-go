package billing

import (
	"context"
	"errors"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
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
	return &Client{http: httpx.New(u.Billing, u.Token)}, nil
}

// CheckQuota calls POST /billing/quota/check. Used by the bridge engine
// when local enforcement is disabled.
func (c *Client) CheckQuota(ctx context.Context, in QuotaCheckRequest) (*QuotaCheckResponse, error) {
	var out QuotaCheckResponse
	if err := c.http.POST(ctx, "/billing/quota/check", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetUsageSummary calls GET /billing/usage/{org_id}/summary.
func (c *Client) GetUsageSummary(ctx context.Context, orgID string) (*UsageSummary, error) {
	var out UsageSummary
	if err := c.http.GET(ctx, "/billing/usage/"+orgID+"/summary", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GrantCredit calls POST /billing/credits/grant. This is what the default
// credit-grant handler hooks up to via the CreditGranter interface.
func (c *Client) GrantCredit(ctx context.Context, in CreditGrantRequest) (*CreditGrantResponse, error) {
	var out CreditGrantResponse
	if err := c.http.POST(ctx, "/billing/credits/grant", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSubscription calls GET /subscriptions/{org_id}.
func (c *Client) GetSubscription(ctx context.Context, orgID string) (*Subscription, error) {
	var out Subscription
	if err := c.http.GET(ctx, "/subscriptions/"+orgID, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelSubscription calls DELETE /subscriptions/{org_id}/{subscription_id}.
// Per back_references.md fix C5 — DELETE, not POST.
func (c *Client) CancelSubscription(ctx context.Context, orgID, subID string) error {
	return c.http.DELETE(ctx, "/subscriptions/"+orgID+"/"+subID, nil)
}

// Heartbeat calls POST /billing/heartbeat.
func (c *Client) Heartbeat(ctx context.Context, in HeartbeatRequest) error {
	return c.http.POST(ctx, "/billing/heartbeat", in, nil)
}
