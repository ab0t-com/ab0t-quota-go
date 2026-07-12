// Package billing is the typed client for the ab0t billing service. Wire
// shapes match Python lib v0.5.2 verbatim — see back_references.md for
// the full endpoint list.
package billing

import (
	"github.com/shopspring/decimal"
)

// UsageSummary is the response from GET /billing/usage/{org_id}/summary.
type UsageSummary struct {
	OrgID       string           `json:"org_id"`
	Period      string           `json:"period"`
	Resources   map[string]Usage `json:"resources"`
	Spend       decimal.Decimal  `json:"spend_usd"`
	GeneratedAt string           `json:"generated_at"`
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
	SubscriptionID     string `json:"subscription_id"`
	OrgID              string `json:"org_id"`
	TierID             string `json:"tier_id"`
	Status             string `json:"status"`
	CurrentPeriodStart string `json:"current_period_start,omitempty"`
	CurrentPeriodEnd   string `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd  bool   `json:"cancel_at_period_end,omitempty"`
}

// SettleActivationRequest is POST /billing/{org_id}/settle — the durable,
// activation-scoped settlement path (D-12).
//
// Money is a STRING on the wire (the house Decimal-as-string convention).
// A float64 would introduce binary-fraction error into a number that is about
// to be debited from a customer.
//
// ⚠️ SettlementKey is the ONLY thing standing between a retry and a
// double-charge. Billing dedups it with a DynamoDB conditional write that has
// NO TTL — the dedup is durable and eternal. Pass the RESERVATION ID: it is the
// key billing's own SQS lifecycle consumer settles under, which is what makes
// the two settlement paths dedup against each other.
//
// ⚠️ IT CARRIES THE INPUTS, NOT A COST (B-D13). Send what you OBSERVED — when it started, when it
// stopped, the rate you were quoted — and billing prices it with the one law it owns. This
// request used to carry `actual_cost`, and that single field forced every caller to reimplement
// billing's proration. A caller that cannot compute a cost cannot compute it wrong.
//
// `hourly_rate` / `allocation_fee` use `omitempty`: a value we do not have is OMITTED, never sent
// as an explicit null. B-D14 was an always-present key whose value was sometimes null, against a
// `.get(k, default)` on the other side that only defaults on an ABSENT key. Send absence as
// absence — do not build the next landmine.
type SettleActivationRequest struct {
	SettlementKey string `json:"settlement_key"`
	StartedAt     string `json:"started_at"`
	StoppedAt     string `json:"stopped_at"`
	HourlyRate    string `json:"hourly_rate,omitempty"`
	AllocationFee string `json:"allocation_fee,omitempty"`
	ReservationID string `json:"reservation_id,omitempty"`
	UsageRecordID string `json:"usage_record_id,omitempty"`
}

// SettleActivationResponse is billing's reply. Replayed=true means this exact
// settlement had ALREADY landed: no money moved and the payload is the ORIGINAL
// result read back from the durable marker, not a recomputation.
//
// The three SpentFrom* fields make the three-bucket spend order auditable from
// the outside: you can prove the customer's subscription credit was drawn before
// their cash.
type SettleActivationResponse struct {
	Status                      string `json:"status"`
	SettlementKey               string `json:"settlement_key"`
	OrgID                       string `json:"org_id"`
	ActualCost                  string `json:"actual_cost"`
	NewBalance                  string `json:"new_balance"`
	SpentFromSubscriptionCredit string `json:"spent_from_subscription_credit"`
	SpentFromCreditBalance      string `json:"spent_from_credit_balance"`
	SpentFromBalance            string `json:"spent_from_balance"`
	SettledAt                   string `json:"settled_at"`
	Replayed                    bool   `json:"replayed"`
}
