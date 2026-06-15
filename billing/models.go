// Package billing is the typed client for the ab0t billing service. Wire
// shapes match Python lib v0.5.2 verbatim — see back_references.md for
// the full endpoint list.
package billing

import (
	"github.com/shopspring/decimal"
)

// UsageSummary is the response from GET /billing/usage/{org_id}/summary.
type UsageSummary struct {
	OrgID       string             `json:"org_id"`
	Period      string             `json:"period"`
	Resources   map[string]Usage   `json:"resources"`
	Spend       decimal.Decimal    `json:"spend_usd"`
	GeneratedAt string             `json:"generated_at"`
}

// Usage is one resource's usage snapshot.
type Usage struct {
	Used  float64  `json:"used"`
	Limit *float64 `json:"limit"`
}

// QuotaCheckRequest is POST /billing/quota/check body.
type QuotaCheckRequest struct {
	UserID      string  `json:"user_id"`
	OrgID       string  `json:"org_id"`
	ResourceKey string  `json:"resource_key"`
	Cost        float64 `json:"cost,omitempty"`
}

// QuotaCheckResponse is the typed response.
type QuotaCheckResponse struct {
	Allowed    bool     `json:"allowed"`
	Limit      *float64 `json:"limit,omitempty"`
	Used       float64  `json:"used"`
	Reason     string   `json:"reason,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	UpgradeURL string   `json:"upgrade_url,omitempty"`
}

// CreditGrantRequest is POST /billing/credits/grant.
type CreditGrantRequest struct {
	UserID  string          `json:"user_id"`
	OrgID   string          `json:"org_id,omitempty"`
	TierID  string          `json:"tier_id"`
	Amount  decimal.Decimal `json:"amount"`
	EventID string          `json:"event_id,omitempty"`
	Reason  string          `json:"reason,omitempty"`
}

// CreditGrantResponse — POST /billing/credits/grant response.
type CreditGrantResponse struct {
	GrantID string          `json:"grant_id"`
	Balance decimal.Decimal `json:"balance"`
}

// HeartbeatRequest is POST /billing/heartbeat — the lifecycle ping.
type HeartbeatRequest struct {
	ServiceName string `json:"service_name"`
	Version     string `json:"version,omitempty"`
	Capability  string `json:"capability,omitempty"`
}

// RecordUsageRequest is POST /billing/usage/{org_id}/ body. Mirrors the
// Python lib RecordUsageRequest / billing's canonical model. ResourceType is
// OPEN (public mesh); Metadata is the only propagating open channel (billing
// ignores unknown top-level fields). Cost/PlatformFee are decimal-as-string
// ("0" for metering rows).
type RecordUsageRequest struct {
	OrgID         string         `json:"org_id"`
	UserID        string         `json:"user_id"`
	ToolID        string         `json:"tool_id"`
	SessionID     string         `json:"session_id"`
	RequestID     string         `json:"request_id,omitempty"`
	InputTokens   int            `json:"input_tokens,omitempty"`
	OutputTokens  int            `json:"output_tokens,omitempty"`
	ComputeTime   float64        `json:"compute_time,omitempty"`
	ResourceType  string         `json:"resource_type,omitempty"`
	ReservationID string         `json:"reservation_id,omitempty"`
	Cost          string         `json:"cost,omitempty"`
	PlatformFee   string         `json:"platform_fee,omitempty"`
	Timestamp     string         `json:"timestamp,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// Subscription is the shape returned from GET/DELETE /subscriptions paths.
type Subscription struct {
	SubscriptionID string `json:"subscription_id"`
	OrgID          string `json:"org_id"`
	TierID         string `json:"tier_id"`
	Status         string `json:"status"`
	CurrentPeriodStart string `json:"current_period_start,omitempty"`
	CurrentPeriodEnd   string `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd  bool   `json:"cancel_at_period_end,omitempty"`
}
