package billing

// Reserve / refund / balance / zero-cost metering (v0.1.12, additive).
//
// Before v0.1.12 the Go client had no way to hold funds at all, so a Go compute consumer could not
// use billing's money path. These mirror the Python client (ab0t_quota/billing/clients.py
// reserve_funds_result, refund_reservation_outcome, get_balance, record_resource_usage) — same
// routes, same outcomes.
// No existing method changes.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/shopspring/decimal"
)

// ErrInvalidRequest marks a request the client refused to send (blank id, non-positive amount…).
// Never transient: retrying the same input cannot succeed.
var ErrInvalidRequest = errors.New("billing: invalid request")

// ReserveOutcome is what a reserve attempt established.
type ReserveOutcome string

const (
	// ReserveReserved — 2xx: a hold exists; ReservationID is set.
	ReserveReserved ReserveOutcome = "reserved"
	// ReserveInsufficientFunds — 402: the org cannot fund the hold.
	ReserveInsufficientFunds ReserveOutcome = "insufficient_funds"
	// ReserveLimited — 429: a daily/monthly cap or the per-org reserve rate limit (Reason says which).
	ReserveLimited ReserveOutcome = "limited"
)

// ReserveRequest is the body of POST /billing/{org_id}/reserve (billing's reservation request).
// Empty ToolID → "default"; empty OperationType → "api_call" (the Python client's defaults).
// RequestID is billing's idempotency key: a retry with the same id returns the SAME reservation.
// Use one per launch attempt, never one per resource (a restarted resource is a new lifetime).
type ReserveRequest struct {
	OrgID         string          `json:"org_id"`
	UserID        string          `json:"user_id"`
	ToolID        string          `json:"tool_id"`
	EstimatedCost decimal.Decimal `json:"estimated_cost"`
	SessionID     string          `json:"session_id"`
	RequestID     string          `json:"request_id,omitempty"`
	OperationType string          `json:"operation_type"`
	Metadata      map[string]any  `json:"metadata"`
}

// ReserveResult is the typed answer. A nil money pointer means billing did not say (never 0).
type ReserveResult struct {
	Outcome          ReserveOutcome
	ReservationID    string
	ReservedAmount   *decimal.Decimal
	ExpiresAt        string
	AvailableBalance *decimal.Decimal
	RequestedAmount  *decimal.Decimal
	RequiredTopup    *decimal.Decimal
	Reason           string
	RequestID        string
}

// Reserved reports whether a hold exists.
func (r *ReserveResult) Reserved() bool { return r != nil && r.Outcome == ReserveReserved }

type reserveResponse struct {
	ReservationID    string           `json:"reservation_id"`
	ReservedAmount   *decimal.Decimal `json:"reserved_amount"`
	ExpiresAt        string           `json:"expires_at"`
	AvailableBalance *decimal.Decimal `json:"available_balance"`
}

// ReserveFunds holds funds before provisioning. 402 → ReserveInsufficientFunds (with billing's
// available_balance / requested_amount / required_topup), 429 → ReserveLimited (Reason = billing's
// error code, else "rate_limited"); EVERY other failure is returned as an error, so a caller fails
// closed and never launches unreserved.
func (c *Client) ReserveFunds(ctx context.Context, orgID string, in ReserveRequest) (*ReserveResult, error) {
	org, err := seg("ReserveFunds", "org id", orgID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	switch {
	case in.OrgID == "":
		in.OrgID = orgID
	case in.OrgID != orgID:
		return nil, fmt.Errorf("%w: ReserveFunds: body org %q != path org %q", ErrInvalidRequest, in.OrgID, orgID)
	}
	if strings.TrimSpace(in.UserID) == "" {
		return nil, fmt.Errorf("%w: ReserveFunds: user id is required", ErrInvalidRequest)
	}
	if in.EstimatedCost.Sign() <= 0 {
		return nil, fmt.Errorf("%w: ReserveFunds: estimated cost must be > 0", ErrInvalidRequest)
	}
	if in.ToolID == "" {
		in.ToolID = "default"
	}
	if in.OperationType == "" {
		in.OperationType = "api_call"
	}
	if in.Metadata == nil {
		in.Metadata = map[string]any{} // billing's model rejects null
	}
	var out reserveResponse
	if err := c.http.POST(ctx, "/billing/"+org+"/reserve", in, &out); err != nil {
		status, body := errorBody(err)
		detail, _ := body["detail"].(map[string]any)
		switch status {
		case 402:
			reason, _ := detail["error"].(string)
			if reason == "" {
				reason = "insufficient_balance"
			}
			return &ReserveResult{
				Outcome:          ReserveInsufficientFunds,
				AvailableBalance: decimalField(detail, "available_balance"),
				RequestedAmount:  decimalField(detail, "requested_amount"),
				RequiredTopup:    decimalField(detail, "required_topup"),
				Reason:           reason,
				RequestID:        in.RequestID,
			}, nil
		case 429:
			reason, _ := detail["error"].(string)
			if reason == "" {
				reason = "rate_limited"
			}
			return &ReserveResult{Outcome: ReserveLimited, Reason: reason, RequestID: in.RequestID}, nil
		}
		return nil, err
	}
	if strings.TrimSpace(out.ReservationID) == "" {
		return nil, errors.New("billing: ReserveFunds: 2xx without a reservation_id (not a hold that can be committed or refunded)")
	}
	return &ReserveResult{
		Outcome:          ReserveReserved,
		ReservationID:    out.ReservationID,
		ReservedAmount:   out.ReservedAmount,
		ExpiresAt:        out.ExpiresAt,
		AvailableBalance: out.AvailableBalance,
		RequestID:        in.RequestID,
	}, nil
}

// RefundOutcome is what a refund attempt established — the Python RefundOutcome, same values.
type RefundOutcome string

const (
	RefundRefunded        RefundOutcome = "refunded"         // 2xx — released now
	RefundAlreadyTerminal RefundOutcome = "already_terminal" // 409 RESERVATION_NOT_ACTIVE — nothing held
	RefundAlreadySettled  RefundOutcome = "already_settled"  // 409 RESERVATION_ALREADY_SETTLED — committed
	RefundNotFound        RefundOutcome = "not_found"        // 404 RESERVATION_NOT_FOUND
	RefundFailed          RefundOutcome = "failed"           // anything else: a hold MAY remain
)

// HoldMayRemain answers the question an alarm needs.
func (o RefundOutcome) HoldMayRemain() bool { return o == RefundFailed }

// RefundReservationOutcome calls POST /billing/{org_id}/refund and classifies the answer ONLY from
// billing's stable error `code` (never the free text). Never returns an error: a transport failure
// or an unknown answer is RefundFailed (fail loud — a hold may remain).
func (c *Client) RefundReservationOutcome(ctx context.Context, orgID, reservationID, reason string) RefundOutcome {
	org, err := seg("RefundReservationOutcome", "org id", orgID)
	if err != nil || strings.TrimSpace(reservationID) == "" {
		slog.Warn("billing refund refused before sending", "org", orgID, "reservation_id", reservationID, "err", err)
		return RefundFailed
	}
	if reason == "" {
		reason = "cancelled"
	}
	body := map[string]string{"reservation_id": reservationID, "reason": reason}
	if err := c.http.POST(ctx, "/billing/"+org+"/refund", body, nil); err != nil {
		status, b := errorBody(err)
		code, _ := b["code"].(string)
		switch {
		case status == 409 && code == "RESERVATION_NOT_ACTIVE":
			return RefundAlreadyTerminal
		case status == 409 && code == "RESERVATION_ALREADY_SETTLED":
			return RefundAlreadySettled
		case status == 404 && code == "RESERVATION_NOT_FOUND":
			return RefundNotFound
		}
		slog.Warn("billing refund failed — a hold may remain", "org", orgID, "reservation_id", reservationID, "err", err)
		return RefundFailed
	}
	return RefundRefunded
}

// Balance is the answer of GET /billing/{org_id}/balance (billing's balance response).
// AvailableBalance = cash + credit + subscription credit − reserved (billing's definition).
type Balance struct {
	OrgID              string          `json:"org_id"`
	Balance            decimal.Decimal `json:"balance"`
	CreditBalance      decimal.Decimal `json:"credit_balance"`
	SubscriptionCredit decimal.Decimal `json:"subscription_credit"`
	ReservedBalance    decimal.Decimal `json:"reserved_balance"`
	AvailableBalance   decimal.Decimal `json:"available_balance"`
	Currency           string          `json:"currency"`
}

// GetBalance calls GET /billing/{org_id}/balance.
func (c *Client) GetBalance(ctx context.Context, orgID string) (*Balance, error) {
	org, err := seg("GetBalance", "org id", orgID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	var out Balance
	if err := c.http.GET(ctx, "/billing/"+org+"/balance", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordResourceUsage records a METERING row that can never be priced: empty Cost / PlatformFee
// are sent as "0" (billing prices a row that omits them), an empty ResourceType is
// "compute" and an empty RequestID is generated. Unlike RecordUsage it refuses a row without a
// ToolID (the caller's own identity) instead of filing it under nobody. Billing failures stay
// best-effort (logged, nil) exactly like RecordUsage. RecordUsage itself is unchanged.
func (c *Client) RecordResourceUsage(ctx context.Context, in RecordUsageRequest) error {
	if strings.TrimSpace(in.ToolID) == "" {
		return fmt.Errorf("%w: RecordResourceUsage: ToolID (your service identity) is required", ErrInvalidRequest)
	}
	if in.Cost == "" {
		in.Cost = "0"
	}
	if in.PlatformFee == "" {
		in.PlatformFee = "0"
	}
	if in.ResourceType == "" {
		in.ResourceType = "compute"
	}
	if in.RequestID == "" {
		in.RequestID = "usage-" + randomHex(6)
	}
	return c.RecordUsage(ctx, in)
}

// IsTransient reports whether err is worth ONE retry with the same idempotency key: a 502/503/504,
// or a transport failure (no HTTP answer). A refused request (ErrInvalidRequest) and every other
// HTTP status are not.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, ErrInvalidRequest) {
		return false
	}
	var he *httpx.Error
	if errors.As(err, &he) {
		return he.Status == 502 || he.Status == 503 || he.Status == 504
	}
	return true
}

// errorBody returns the HTTP status and parsed JSON body of an *httpx.Error (0, empty map otherwise).
func errorBody(err error) (int, map[string]any) {
	var he *httpx.Error
	if !errors.As(err, &he) {
		return 0, map[string]any{}
	}
	m := map[string]any{}
	_ = json.Unmarshal([]byte(he.Body), &m)
	if m == nil {
		m = map[string]any{}
	}
	return he.Status, m
}

// decimalField reads a money value (JSON string or number) as a decimal; nil when absent/unparseable.
func decimalField(m map[string]any, key string) *decimal.Decimal {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case float64:
		s = fmt.Sprint(t)
	case json.Number:
		s = t.String()
	default:
		return nil
	}
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return &d
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0"
	}
	return hex.EncodeToString(b)
}
