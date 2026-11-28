package billing

import (
	"context"
	"errors"
)

// OrgTier is billing's answer to GET /billing/{org_id}/tier (fields this library reads).
type OrgTier struct {
	OrgID       string `json:"org_id"`
	TierID      string `json:"tier_id"`
	TierDisplay string `json:"tier_display,omitempty"`
}

// GetOrgTier (v0.3.2) calls GET /billing/{org_id}/tier with the service's billing key and returns the
// tier billing holds for the org — the tier a payment set. This is what tier_provider "mesh" reads
// (the Python library's AuthServiceTierProvider reads the same route). A 401/403 means the key cannot
// read other customers' tiers (it needs the cross-customer read); the error carries the status.
func (c *Client) GetOrgTier(ctx context.Context, orgID string) (string, error) {
	org, err := seg("GetOrgTier", "org id", orgID)
	if err != nil {
		return "", err
	}
	var out OrgTier
	if err := c.http.GET(ctx, "/billing/"+org+"/tier", &out); err != nil {
		return "", err
	}
	if out.TierID == "" {
		return "", errors.New("billing: GET /billing/" + org + "/tier answered without a tier_id")
	}
	return out.TierID, nil
}
