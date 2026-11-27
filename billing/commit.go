package billing

// Typed reservation commit (v0.2.0, additive). Same contract as the Python client's
// commit_reservation_result / CommitOutcome / CommitResult (ab0t_quota/billing/clients.py): same
// route, same request validation, same classification table.
//
// The meter never uses this (its commit is the lifecycle stop event). It is for a caller that holds
// funds with ReserveFunds and settles the reservation itself with POST /billing/{org_id}/commit.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/shopspring/decimal"
)

// Values billing's /commit accepts for on_insufficient_funds. Empty = billing's default (refuse).
const (
	OnInsufficientFundsRefuse     = "refuse"
	OnInsufficientFundsRecordDebt = "record_debt"
)

// CommitOutcome is what a reservation commit established. Classified from the HTTP status and
// billing's stable error `code` — never the free text.
//
//	CommitCommitted          2xx — the usage is charged; no debt was recorded
//	CommitRecordedDebt       2xx with unfunded_overrun > 0 — charged in FULL, the part the org could
//	                         not fund recorded as debt (only when you sent OnInsufficientFundsRecordDebt)
//	CommitInsufficientFunds  402 — refused: the overrun exceeds the hold + the org's funds. NOTHING was
//	                         charged and the hold is still live (it expires at $0 if nobody acts): retry
//	                         with OnInsufficientFundsRecordDebt or decide otherwise (your policy)
//	CommitAlreadySettled     409 RESERVATION_ALREADY_SETTLED — the money is accounted; stop
//	CommitNotActive          409 RESERVATION_NOT_ACTIVE — refunded/released/expired: NO money was taken
//	                         for this usage; investigate (SettleActivation lands late usage)
//	CommitNotFound           404 RESERVATION_NOT_FOUND, or billing's uncoded ownership 404 — no live
//	                         reservation under this id for this org (SettleActivation if usage happened)
//	CommitConflict           409 CONCURRENT_MODIFICATION / RESERVATION_COMMIT_CONFLICT — retry
//	CommitUnavailable        5xx, connect failure, timeout, transport error, unreadable 2xx body —
//	                         billing's answer is UNKNOWN; retrying the SAME commit is safe (billing
//	                         replays a completed commit or answers ALREADY_SETTLED)
//	CommitFailed             anything else (400/401/403/422, an uncoded 409, an unknown code)
type CommitOutcome string

const (
	CommitCommitted         CommitOutcome = "committed"
	CommitRecordedDebt      CommitOutcome = "recorded_debt"
	CommitInsufficientFunds CommitOutcome = "insufficient_funds"
	CommitAlreadySettled    CommitOutcome = "already_settled"
	CommitNotActive         CommitOutcome = "not_active"
	CommitNotFound          CommitOutcome = "not_found"
	CommitConflict          CommitOutcome = "conflict"
	CommitUnavailable       CommitOutcome = "unavailable"
	CommitFailed            CommitOutcome = "failed"
)

// Accounted is true when the usage's money is on billing's books (this call or an earlier one).
func (o CommitOutcome) Accounted() bool {
	return o == CommitCommitted || o == CommitRecordedDebt || o == CommitAlreadySettled
}

// HoldMayRemain is true when a live hold may still exist under this reservation (act: retry, settle
// or refund).
func (o CommitOutcome) HoldMayRemain() bool {
	return o == CommitInsufficientFunds || o == CommitConflict || o == CommitUnavailable || o == CommitFailed
}

// Retryable is true when retrying the same commit unchanged may succeed.
func (o CommitOutcome) Retryable() bool { return o == CommitConflict || o == CommitUnavailable }

type commitKey struct {
	status int
	code   string
}

// commitCodeOutcomes is the classification table (the Python _COMMIT_CODE_OUTCOMES, entry for entry).
var commitCodeOutcomes = map[commitKey]CommitOutcome{
	{409, "RESERVATION_ALREADY_SETTLED"}: CommitAlreadySettled,
	{409, "RESERVATION_NOT_ACTIVE"}:      CommitNotActive,
	{409, "CONCURRENT_MODIFICATION"}:     CommitConflict,
	{409, "RESERVATION_COMMIT_CONFLICT"}: CommitConflict,
	{404, "RESERVATION_NOT_FOUND"}:       CommitNotFound,
	// billing's /commit ownership guard answers an UNCODED 404 on purpose (no enumeration oracle)
	{404, ""}: CommitNotFound,
}

// ClassifyCommitError maps an error answer of billing's /commit (status + top-level `code`, "" when
// billing sent none) to its outcome. 402 is always CommitInsufficientFunds, >= 500 always
// CommitUnavailable; everything not in the table is CommitFailed.
func ClassifyCommitError(status int, code string) CommitOutcome {
	if status == 402 {
		return CommitInsufficientFunds
	}
	if status >= 500 {
		return CommitUnavailable
	}
	if o, ok := commitCodeOutcomes[commitKey{status, strings.TrimSpace(code)}]; ok {
		return o
	}
	return CommitFailed
}

// CommitRequest is one commit. Prefer the billing-priced form: StartedAt + StoppedAt (with
// HourlyRate / AllocationFee) and billing prices the usage with its own cost law; ActualUsage may
// then be omitted (the client sends the usage-record shape billing validates). ActualUsage with a
// cost is the deprecated caller-priced form.
//
// OnInsufficientFunds is YOUR policy for this commit when the usage exceeds the hold AND the org's
// funds: "" / OnInsufficientFundsRefuse = billing's default (402 → CommitInsufficientFunds, nothing
// charged); OnInsufficientFundsRecordDebt = charge in full and record the shortfall as debt
// (CommitRecordedDebt with UnfundedOverrun; billing also publishes billing.funds_exhausted with
// trigger commit_overrun). Record-debt requires the billing-priced form.
type CommitRequest struct {
	ActualUsage         map[string]any
	StartedAt           time.Time
	StoppedAt           time.Time
	HourlyRate          *decimal.Decimal
	AllocationFee       *decimal.Decimal
	OnInsufficientFunds string
	// ToolID for the usage record billing validates on the billing-priced path; "" → the
	// AB0T_SERVICE_NAME environment variable, else "default".
	ToolID string
}

// CommitResult is the typed answer. A nil money pointer means billing did not say (never read as 0).
// CommitMode is "record_debt" only when billing HONOURED that option (an older billing ignores it and
// leaves it empty). StatusCode / Code are set for error answers (0 / "" on 2xx and on a transport
// failure).
type CommitResult struct {
	Outcome         CommitOutcome
	ReservationID   string
	ReservedAmount  *decimal.Decimal
	ActualCost      *decimal.Decimal
	RefundedAmount  *decimal.Decimal
	UnfundedOverrun *decimal.Decimal
	NewBalance      *decimal.Decimal
	CommitMode      string
	StatusCode      int
	Code            string
	Raw             map[string]any
}

// Accounted — see CommitOutcome.Accounted.
func (r *CommitResult) Accounted() bool { return r != nil && r.Outcome.Accounted() }

// HoldMayRemain — see CommitOutcome.HoldMayRemain. A nil result is treated as "may remain".
func (r *CommitResult) HoldMayRemain() bool { return r == nil || r.Outcome.HoldMayRemain() }

// Retryable — see CommitOutcome.Retryable.
func (r *CommitResult) Retryable() bool { return r != nil && r.Outcome.Retryable() }

// commitWireTime is RFC 3339 with microseconds (billing's Python parser reads it everywhere).
const commitWireTime = "2006-01-02T15:04:05.000000Z07:00"

// BuildCommitBody returns the /commit body for (orgID, reservationID, in), or an error wrapping
// ErrInvalidRequest for a request billing would refuse or could not price. Used by
// Client.CommitReservationResult and by meter/metertest.Fake, so both refuse the same mistakes
// before any network call.
func BuildCommitBody(orgID, reservationID string, in CommitRequest) (map[string]any, error) {
	if strings.TrimSpace(reservationID) == "" {
		return nil, fmt.Errorf("%w: commit needs a reservation_id", ErrInvalidRequest)
	}
	switch in.OnInsufficientFunds {
	case "", OnInsufficientFundsRefuse, OnInsufficientFundsRecordDebt:
	default:
		return nil, fmt.Errorf("%w: OnInsufficientFunds must be \"\", %q or %q (got %q)", ErrInvalidRequest,
			OnInsufficientFundsRefuse, OnInsufficientFundsRecordDebt, in.OnInsufficientFunds)
	}
	if in.StartedAt.IsZero() != in.StoppedAt.IsZero() {
		return nil, fmt.Errorf("%w: StartedAt and StoppedAt go together: send both (billing prices the usage) or neither",
			ErrInvalidRequest)
	}
	billingPriced := !in.StartedAt.IsZero()
	if in.OnInsufficientFunds == OnInsufficientFundsRecordDebt && !billingPriced {
		return nil, fmt.Errorf("%w: OnInsufficientFunds=%q needs a billing-priced commit: send StartedAt and StoppedAt "+
			"(and HourlyRate / AllocationFee). Billing refuses to record debt for a caller-computed cost "+
			"(422 RECORD_DEBT_REQUIRES_BILLING_PRICING)", ErrInvalidRequest, OnInsufficientFundsRecordDebt)
	}
	if !billingPriced && len(in.ActualUsage) == 0 {
		return nil, fmt.Errorf("%w: a commit needs either StartedAt + StoppedAt (billing prices it — preferred) or "+
			"ActualUsage with a cost (deprecated caller-priced path)", ErrInvalidRequest)
	}
	body := map[string]any{"reservation_id": reservationID}
	if len(in.ActualUsage) > 0 {
		usage := make(map[string]any, len(in.ActualUsage))
		for k, v := range in.ActualUsage {
			usage[k] = v
		}
		body["actual_usage"] = usage
	} else {
		// Billing's /commit validates actual_usage as a usage record even when it prices the commit
		// itself; on that path the cost below is ignored (billing uses the inputs).
		tool := strings.TrimSpace(in.ToolID)
		if tool == "" {
			tool = strings.TrimSpace(os.Getenv("AB0T_SERVICE_NAME"))
		}
		if tool == "" {
			tool = "default"
		}
		body["actual_usage"] = map[string]any{
			"org_id": orgID, "user_id": "", "tool_id": tool, "session_id": "",
			"cost": "0", "platform_fee": "0", "reservation_id": reservationID,
		}
	}
	if billingPriced {
		body["started_at"] = in.StartedAt.UTC().Format(commitWireTime)
		body["stopped_at"] = in.StoppedAt.UTC().Format(commitWireTime)
		if in.HourlyRate != nil {
			body["hourly_rate"] = in.HourlyRate.String()
		}
		if in.AllocationFee != nil {
			body["allocation_fee"] = in.AllocationFee.String()
		}
	}
	if in.OnInsufficientFunds != "" {
		body["on_insufficient_funds"] = in.OnInsufficientFunds
	}
	return body, nil
}

// CommitReservationResult commits a reservation via POST /billing/{org_id}/commit and says WHAT
// happened — see CommitOutcome. A request billing would refuse or could not price is an error
// wrapping ErrInvalidRequest, returned before any network call. Every billing answer and every
// transport failure is a result (CommitUnavailable when the answer is unknown), never an error.
func (c *Client) CommitReservationResult(ctx context.Context, orgID, reservationID string, in CommitRequest) (*CommitResult, error) {
	org, err := seg("CommitReservationResult", "org id", orgID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	body, err := BuildCommitBody(orgID, reservationID, in)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.http.POST(ctx, "/billing/"+org+"/commit", body, &raw); err != nil {
		var he *httpx.Error
		if !errors.As(err, &he) {
			// No HTTP answer (connect failure, timeout, refused redirect) or an unreadable 2xx body:
			// billing's answer is unknown — never read as committed.
			slog.Warn("billing commit answer unknown", "reservation_id", reservationID, "err", err)
			return &CommitResult{Outcome: CommitUnavailable, ReservationID: reservationID}, nil
		}
		status, b := errorBody(err)
		code, _ := b["code"].(string)
		code = strings.TrimSpace(code)
		outcome := ClassifyCommitError(status, code)
		if outcome == CommitUnavailable || outcome == CommitFailed {
			slog.Warn("billing commit not accounted", "reservation_id", reservationID, "status", status,
				"code", code, "outcome", string(outcome))
		}
		return &CommitResult{Outcome: outcome, ReservationID: reservationID, StatusCode: status, Code: code, Raw: b}, nil
	}
	if len(raw) == 0 {
		slog.Warn("billing commit answer unknown (2xx without a body)", "reservation_id", reservationID)
		return &CommitResult{Outcome: CommitUnavailable, ReservationID: reservationID}, nil
	}
	data := map[string]any{}
	if err := json.Unmarshal(raw, &data); err != nil || data == nil {
		data = map[string]any{} // valid JSON that is not an object: no fields to read
	}
	exact := decodeExact(raw) // v0.2.0: money from the exact number text, never via float64
	res := &CommitResult{
		Outcome:         CommitCommitted,
		ReservationID:   reservationID,
		ReservedAmount:  decimalField(exact, "reserved_amount"),
		ActualCost:      decimalField(exact, "actual_cost"),
		RefundedAmount:  decimalField(exact, "refunded_amount"),
		UnfundedOverrun: decimalField(exact, "unfunded_overrun"),
		NewBalance:      decimalField(exact, "new_balance"),
		Raw:             data,
	}
	if id := text(data["reservation_id"]); id != "" {
		res.ReservationID = id
	}
	if mode := text(data["commit_mode"]); mode != "" {
		res.CommitMode = mode
	}
	if res.UnfundedOverrun != nil && res.UnfundedOverrun.Sign() > 0 {
		res.Outcome = CommitRecordedDebt
	}
	return res, nil
}
