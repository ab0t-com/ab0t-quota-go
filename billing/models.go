// Package billing is the typed client for the ab0t billing service. Wire
// shapes match Python lib v0.5.2 verbatim — see back_references.md for
// the full endpoint list.
package billing

import (
	"github.com/shopspring/decimal"
)

// UsageSummary is the response from GET /billing/usage/{org_id}/summary.
//
// v0.1.10 (ticket 20261016, B-08): billing answers UsageSummaryResponse
// {org_id, start_date, end_date, period, summary, group_by} (billing
// app/models/billing.py:1082-1089). Resources / Spend / GeneratedAt are never
// sent by billing (always empty / 0) and are kept only for API stability — read
// Summary.
type UsageSummary struct {
	OrgID       string           `json:"org_id"`
	Period      string           `json:"period"`
	Resources   map[string]Usage `json:"resources"`
	Spend       decimal.Decimal  `json:"spend_usd"`
	GeneratedAt string           `json:"generated_at"`
	StartDate   string           `json:"start_date,omitempty"`
	EndDate     string           `json:"end_date,omitempty"`
	Summary     map[string]any   `json:"summary,omitempty"`
	GroupBy     []string         `json:"group_by,omitempty"`
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

// CreditGrantRequest is the input to Client.GrantCredit — a sign-up / gift grant
// into the org's credit_balance. Since v0.1.10 it is sent to billing's real
// POST /billing/{org_id}/promotional-credit route (see GrantCredit); before that
// it was posted, verbatim, to /billing/credits/grant, which billing never served.
type CreditGrantRequest struct {
	UserID  string          `json:"user_id"`
	OrgID   string          `json:"org_id,omitempty"` // REQUIRED (the org that receives the credit)
	TierID  string          `json:"tier_id"`
	Amount  decimal.Decimal `json:"amount"`
	EventID string          `json:"event_id,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	// IdempotencyKey (v0.1.10) is what billing dedups on — durably and forever.
	// Empty → derived from the grant (never from the attempt):
	// "user:{UserID}:initial_credit:{TierID}" when both are set (the Python
	// runtime's sign-up key), else "event:{EventID}". Neither available → error.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// CreditGrantResponse is Client.GrantCredit's result.
type CreditGrantResponse struct {
	GrantID string          `json:"grant_id"` // billing's payment_id ("promo:{idempotency_key}")
	Balance decimal.Decimal `json:"balance"`  // the org's credit_balance after the grant
	// Result (v0.1.10) is billing's full reply.
	Result *UpdateBalanceResponse `json:"result,omitempty"`
}

// PromotionalCreditRequest is POST /billing/{org_id}/promotional-credit
// (billing app/models/billing.py PromotionalCreditRequest). Billing adds the
// amount to the org's credit_balance, refuses over its caps (by default $500 per
// grant, $1,000 lifetime promotional credit per org → 400; 10 grants per org per
// hour → 429), and answers a repeated IdempotencyKey with the ORIGINAL result.
// The caller needs billing.write.transactions (or billing.write / billing.admin).
type PromotionalCreditRequest struct {
	Amount         decimal.Decimal `json:"amount"`
	Reason         string          `json:"reason,omitempty"` // billing default "initial_credit"
	IdempotencyKey string          `json:"idempotency_key"`  // REQUIRED, non-blank, stable across retries
}

// ApplyCreditGrantRequest is POST /billing/{org_id}/apply-credit-grant (billing
// app/models/billing.py CreditGrantRequest): a grant into an explicit bucket under
// an explicit period-boundary policy. Same permissions and idempotency contract as
// promotional credit, no lifetime cap.
type ApplyCreditGrantRequest struct {
	Amount      decimal.Decimal `json:"amount"`
	Destination string          `json:"destination"` // balance | credit_balance | subscription_credit
	Lifecycle   string          `json:"lifecycle"`   // persistent | use_it_or_lose_it | rollover_unlimited | rollover_capped
	// RolloverMax is required when Lifecycle is rollover_capped and must be absent otherwise.
	RolloverMax    *decimal.Decimal `json:"rollover_max,omitempty"`
	Source         string           `json:"source,omitempty"`
	SourceTier     string           `json:"source_tier,omitempty"`
	IdempotencyKey string           `json:"idempotency_key"` // REQUIRED, non-blank, stable across retries
	Reason         string           `json:"reason,omitempty"`
}

// UpdateBalanceResponse is billing's reply to the credit routes (billing
// app/models/billing.py UpdateBalanceResponse). Money fields are decimals
// (billing serialises them as JSON strings).
type UpdateBalanceResponse struct {
	OrgID            string          `json:"org_id"`
	OldBalance       decimal.Decimal `json:"old_balance"`
	NewBalance       decimal.Decimal `json:"new_balance"`
	Amount           decimal.Decimal `json:"amount"`
	Operation        string          `json:"operation"`
	AvailableBalance decimal.Decimal `json:"available_balance"`
	PaymentID        string          `json:"payment_id,omitempty"`
	PaymentMethod    string          `json:"payment_method,omitempty"`
	Description      string          `json:"description,omitempty"`
	FundingSource    string          `json:"funding_source,omitempty"`
	AdminUserID      string          `json:"admin_user_id,omitempty"`
	Metadata         map[string]any  `json:"metadata,omitempty"`
}

// HeartbeatRequest is the lifecycle ping body. Billing serves NO heartbeat route
// (see Client.Heartbeat).
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
