// Package metertest is an in-process fake billing for testing a client's integration WITHOUT the
// billing service (v0.1.12, additive). Same contract as the Python
// ab0t_quota.billing.testing.FakeBilling.
//
// Fake is both halves a client talks to: the meter.Biller (ReserveFunds with a balance check and
// request-id idempotency, RefundReservationOutcome, GetBalance) and the meter.EventSink (Emit records
// every lifecycle event in its wire shape). So meter.New(meter.Config{Billing: f, Sink: f}) runs end
// to end in a unit test, and the test can
//
//   - assert what reached "billing": Events, Heartbeats, UnpricedHeartbeats (heartbeats that would NOT
//     engage billing's funded hold), Reservation(id).Status (reserved | running | committed | refunded);
//   - drive the funds signals billing would send: TriggerFundsLow / TriggerFundsExhausted build
//     billing's exact wire payload, wrap it as SNS→SQS delivers it and hand it to every Attach-ed
//     target (a *meter.Meter, or a *billing.FundsEventHandler via HandlerTarget).
//
// It deliberately prices NOTHING: billing owns the cost law; a second copy here would drift. A stop
// releases the hold without charging it. Never use it in production code.
package metertest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/meter"
	"github.com/shopspring/decimal"
)

// Reservation is the fake's record of one hold.
type Reservation struct {
	ID            string
	OrgID         string
	UserID        string
	Amount        decimal.Decimal
	Status        string // reserved | running | committed | refunded
	RequestID     string
	OperationType string
	ResourceID    string
	HourlyRate    string
	StartedAt     string
	Metadata      map[string]any
}

// Target receives delivered funds events (*meter.Meter satisfies it).
type Target interface {
	HandleBillingEvent(ctx context.Context, msg []byte) (billing.DispatchResult, error)
}

// HandlerTarget adapts a *billing.FundsEventHandler to Target.
type HandlerTarget struct{ H *billing.FundsEventHandler }

// HandleBillingEvent implements Target.
func (t HandlerTarget) HandleBillingEvent(ctx context.Context, msg []byte) (billing.DispatchResult, error) {
	return t.H.Handle(ctx, msg)
}

// Fake — see the package doc. The exported knobs may be set before use.
type Fake struct {
	// LimitedOrgs: orgs whose reserve answers 429 (value = billing's reason code).
	LimitedOrgs map[string]string
	// ReserveFailures: errors returned by the next ReserveFunds calls, in order (e.g. a 503).
	ReserveFailures []error
	// RefundOutcomes: outcomes returned by the next refunds, in order (default: billing's classification).
	RefundOutcomes []billing.RefundOutcome
	// NoTransport makes TransportConfigured() false (to test meter.New's refusal).
	NoTransport bool
	// EmitErr, when set, is returned by the next Emit calls (counted down by EmitErrCount; 0 = forever).
	EmitErr      error
	EmitErrCount int

	mu           sync.Mutex
	balances     map[string]decimal.Decimal
	reservations map[string]*Reservation
	byRequest    map[string]billing.ReserveResult
	events       []meter.LifecycleEvent
	reserveCalls []billing.ReserveRequest
	targets      []Target
	next         int
}

// New builds a Fake with cash balances (org → decimal string).
func New(balances map[string]string) *Fake {
	f := &Fake{
		LimitedOrgs:  map[string]string{},
		balances:     map[string]decimal.Decimal{},
		reservations: map[string]*Reservation{},
		byRequest:    map[string]billing.ReserveResult{},
	}
	for org, amount := range balances {
		f.balances[org] = decimal.RequireFromString(amount)
	}
	return f
}

// SetBalance sets an org's cash balance.
func (f *Fake) SetBalance(org, amount string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balances[org] = decimal.RequireFromString(amount)
}

func (f *Fake) heldLocked(org string) decimal.Decimal {
	sum := decimal.Zero
	for _, r := range f.reservations {
		if r.OrgID == org && (r.Status == "reserved" || r.Status == "running") {
			sum = sum.Add(r.Amount)
		}
	}
	return sum
}

// Held is the sum of the org's open holds.
func (f *Fake) Held(org string) decimal.Decimal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heldLocked(org)
}

// Available is balance − open holds.
func (f *Fake) Available(org string) decimal.Decimal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balances[org].Sub(f.heldLocked(org))
}

// ReserveCalls returns every reserve request received.
func (f *Fake) ReserveCalls() []billing.ReserveRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]billing.ReserveRequest(nil), f.reserveCalls...)
}

// ReserveFunds implements meter.Biller.
func (f *Fake) ReserveFunds(_ context.Context, orgID string, in billing.ReserveRequest) (*billing.ReserveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls = append(f.reserveCalls, in)
	if len(f.ReserveFailures) > 0 {
		err := f.ReserveFailures[0]
		f.ReserveFailures = f.ReserveFailures[1:]
		return nil, err
	}
	if in.RequestID != "" {
		if r, ok := f.byRequest[in.RequestID]; ok {
			out := r
			return &out, nil
		}
	}
	if reason, ok := f.LimitedOrgs[orgID]; ok {
		return &billing.ReserveResult{Outcome: billing.ReserveLimited, Reason: reason, RequestID: in.RequestID}, nil
	}
	avail := f.balances[orgID].Sub(f.heldLocked(orgID))
	if avail.LessThan(in.EstimatedCost) {
		topup := in.EstimatedCost.Sub(avail)
		req := in.EstimatedCost
		return &billing.ReserveResult{
			Outcome:          billing.ReserveInsufficientFunds,
			AvailableBalance: &avail,
			RequestedAmount:  &req,
			RequiredTopup:    &topup,
			Reason:           "insufficient_balance",
			RequestID:        in.RequestID,
		}, nil
	}
	f.next++
	id := fmt.Sprintf("fake_reservation_%d", f.next)
	meta := map[string]any{}
	for k, v := range in.Metadata {
		meta[k] = v
	}
	f.reservations[id] = &Reservation{
		ID:            id,
		OrgID:         orgID,
		UserID:        in.UserID,
		Amount:        in.EstimatedCost,
		Status:        "reserved",
		RequestID:     in.RequestID,
		OperationType: in.OperationType,
		Metadata:      meta,
	}
	amount := in.EstimatedCost
	after := f.balances[orgID].Sub(f.heldLocked(orgID))
	res := billing.ReserveResult{
		Outcome:          billing.ReserveReserved,
		ReservationID:    id,
		ReservedAmount:   &amount,
		AvailableBalance: &after,
		RequestID:        in.RequestID,
	}
	if in.RequestID != "" {
		f.byRequest[in.RequestID] = res
	}
	return &res, nil
}

// RefundReservationOutcome implements meter.Biller with billing's classification.
func (f *Fake) RefundReservationOutcome(_ context.Context, orgID, reservationID, _ string) billing.RefundOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.RefundOutcomes) > 0 {
		o := f.RefundOutcomes[0]
		f.RefundOutcomes = f.RefundOutcomes[1:]
		return o
	}
	r, ok := f.reservations[reservationID]
	switch {
	case !ok || r.OrgID != orgID:
		return billing.RefundNotFound
	case r.Status == "committed":
		return billing.RefundAlreadySettled
	case r.Status == "refunded":
		return billing.RefundAlreadyTerminal
	}
	r.Status = "refunded"
	return billing.RefundRefunded
}

// GetBalance mirrors billing.Client.GetBalance.
func (f *Fake) GetBalance(_ context.Context, org string) (*billing.Balance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held := f.heldLocked(org)
	return &billing.Balance{
		OrgID:            org,
		Balance:          f.balances[org],
		ReservedBalance:  held,
		AvailableBalance: f.balances[org].Sub(held),
		Currency:         "usd",
	}, nil
}

// Emit implements meter.EventSink: records the event and moves the reservation's status.
func (f *Fake) Emit(_ context.Context, ev meter.LifecycleEvent) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.EmitErr != nil {
		err := f.EmitErr
		if f.EmitErrCount > 0 {
			f.EmitErrCount--
			if f.EmitErrCount == 0 {
				f.EmitErr = nil
			}
		}
		return false, err
	}
	f.events = append(f.events, ev)
	r, ok := f.reservations[ev.ReservationID]
	if !ok {
		return true, nil
	}
	switch ev.EventType {
	case meter.EventStarted, meter.EventHeartbeat:
		if ev.EventType == meter.EventStarted && r.Status == "reserved" {
			r.Status = "running"
		}
		r.ResourceID = ev.ResourceID
		r.HourlyRate = ev.HourlyRate
		r.StartedAt = ev.StartedAt
	case meter.EventStopped, meter.EventDeleted:
		if r.Status == "reserved" || r.Status == "running" {
			r.Status = "committed" // billing would price + commit; the fake charges nothing
		}
	}
	return true, nil
}

// FailNextEmits makes the next n Emit calls return err (n <= 0: every call until cleared with nil).
// Safe to call while heartbeat goroutines are running.
func (f *Fake) FailNextEmits(err error, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.EmitErr, f.EmitErrCount = err, n
}

// TransportConfigured implements meter.TransportChecker.
func (f *Fake) TransportConfigured() bool { return !f.NoTransport }

// Events returns recorded events, optionally filtered ("" = any).
func (f *Fake) Events(eventType, reservationID string) []meter.LifecycleEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []meter.LifecycleEvent
	for _, e := range f.events {
		if (eventType == "" || e.EventType == eventType) && (reservationID == "" || e.ReservationID == reservationID) {
			out = append(out, e)
		}
	}
	return out
}

// Heartbeats returns recorded heartbeats ("" = all reservations).
func (f *Fake) Heartbeats(reservationID string) []meter.LifecycleEvent {
	return f.Events(meter.EventHeartbeat, reservationID)
}

// UnpricedHeartbeats are heartbeats billing would treat as TTL-only (no rate or no start): the silent
// opt-out of the funded hold. A correct integration keeps this empty.
func (f *Fake) UnpricedHeartbeats() []meter.LifecycleEvent {
	var out []meter.LifecycleEvent
	for _, e := range f.Heartbeats("") {
		if e.HourlyRate == "" || e.StartedAt == "" {
			out = append(out, e)
		}
	}
	return out
}

// Reservation returns a copy of the fake's record.
func (f *Fake) Reservation(id string) (Reservation, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.reservations[id]
	if !ok {
		return Reservation{}, false
	}
	return *r, true
}

// Attach registers a delivery target for triggered funds events.
func (f *Fake) Attach(t Target) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = append(f.targets, t)
}

// FundsPayload is billing's wire payload for a funds event (event_type, trigger, org_id,
// timestamp + the trigger's fields). fields override.
func (f *Fake) FundsPayload(kind billing.FundsEventKind, reservationID, trigger string, fields map[string]any) map[string]any {
	if trigger == "" {
		trigger = billing.TriggerHeartbeat
	}
	f.mu.Lock()
	r := f.reservations[reservationID]
	var rc Reservation
	if r != nil {
		rc = *r
	}
	f.mu.Unlock()
	p := map[string]any{
		"event_type":     kind.EventType(),
		"trigger":        trigger,
		"org_id":         rc.OrgID,
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		"reservation_id": reservationID,
	}
	hold := rc.Amount.String()
	switch trigger {
	case billing.TriggerHeartbeat:
		state, accrued, runway := "low", "0", 900
		if kind == billing.FundsExhausted {
			state, accrued, runway = "exhausted", hold, 0
		}
		p["resource_id"] = rc.ResourceID
		p["state"] = state
		p["accrued"] = accrued
		p["hold_amount"] = hold
		p["available"] = "0"
		p["hourly_rate"] = rc.HourlyRate
		p["started_at"] = rc.StartedAt
		p["runway_seconds"] = runway
	case billing.TriggerCommitOverrun:
		p["actual_cost"] = "0"
		p["reserved_amount"] = hold
		p["unfunded_overrun"] = "0"
		p["new_balance"] = "0"
	case billing.TriggerSettlementDebt:
		p["settlement_key"] = reservationID
		p["actual_cost"] = "0"
		p["new_balance"] = "0"
	}
	for k, v := range fields {
		if d, ok := v.(decimal.Decimal); ok {
			v = d.String()
		}
		p[k] = v
	}
	return p
}

// SQSBody wraps a billing event the way an SNS→SQS subscription delivers it (the SQS message Body).
func SQSBody(payload map[string]any) []byte {
	inner, _ := json.Marshal(payload)
	env, _ := json.Marshal(map[string]string{"Type": "Notification", "Message": string(inner)})
	return env
}

// Deliver hands one payload to every attached target and returns their results.
func (f *Fake) Deliver(ctx context.Context, payload map[string]any) ([]billing.DispatchResult, error) {
	f.mu.Lock()
	targets := append([]Target(nil), f.targets...)
	f.mu.Unlock()
	body := SQSBody(payload)
	var out []billing.DispatchResult
	for _, t := range targets {
		res, err := t.HandleBillingEvent(ctx, body)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// TriggerFundsLow delivers billing.funds_low (trigger heartbeat) for reservationID.
func (f *Fake) TriggerFundsLow(ctx context.Context, reservationID string, fields map[string]any) ([]billing.DispatchResult, error) {
	return f.Deliver(ctx, f.FundsPayload(billing.FundsLow, reservationID, billing.TriggerHeartbeat, fields))
}

// TriggerFundsExhausted delivers billing.funds_exhausted with the given trigger ("" = heartbeat).
func (f *Fake) TriggerFundsExhausted(ctx context.Context, reservationID, trigger string, fields map[string]any) ([]billing.DispatchResult, error) {
	return f.Deliver(ctx, f.FundsPayload(billing.FundsExhausted, reservationID, trigger, fields))
}
