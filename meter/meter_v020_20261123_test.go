package meter_test

// v0.2.0 meter additions: funding_sources declared once per product in pricing.products (sent on
// every reserve automatically; absent = as before), the org-level billing.funds_low reaching
// OnFundsLow with a nil run, and the Fake's typed commit (same validation as the real client).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/meter"
	"github.com/ab0t-com/ab0t-quota-go/meter/metertest"
)

const fundedPricingJSON = `{"products":{
  "vm":  {"funding_sources":["subscription_credit","balance"],
          "variants":{"small":{"price_per_hour":"0.25","allocation_price":"0"}}},
  "gpu": {"variants":{"a":{"price_per_hour":"2","allocation_price":"0"}}}}}`

func TestParsePricing_FundingSources_20261123(t *testing.T) {
	book, err := meter.ParsePricing(json.RawMessage(fundedPricingJSON))
	if err != nil {
		t.Fatal(err)
	}
	if fs := book.FundingSources("vm"); len(fs) != 2 || fs[0] != "subscription_credit" || fs[1] != "balance" {
		t.Fatalf("vm funding_sources = %v", fs)
	}
	if fs := book.FundingSources("gpu"); fs != nil {
		t.Fatalf("absent funding_sources must be nil (every bucket), got %v", fs)
	}
	p, err := book.Lookup("vm", "")
	if err != nil || len(p.FundingSources) != 2 {
		t.Fatalf("Lookup must carry the product's funding_sources: %+v %v", p, err)
	}
	// the returned slice is a copy
	p.FundingSources[0] = "mutated"
	if book.FundingSources("vm")[0] != "subscription_credit" {
		t.Fatal("FundingSources must return a copy")
	}
}

func TestParsePricing_RefusesBadFundingSources_20261123(t *testing.T) {
	for name, fs := range map[string]string{
		"unknown bucket": `["cash"]`,
		"empty list":     `[]`,
		"repeated":       `["balance","balance"]`,
		"not a list":     `"balance"`,
		"not strings":    `[1]`,
	} {
		raw := `{"products":{"vm":{"funding_sources":` + fs + `,"variants":{"s":{"price_per_hour":"1","allocation_price":"0"}}}}}`
		_, err := meter.ParsePricing(json.RawMessage(raw))
		if !errors.Is(err, meter.ErrConfig) || !strings.Contains(err.Error(), "vm") {
			t.Errorf("%s: got %v — a bad funding_sources must be an ErrConfig at load naming the product", name, err)
		}
	}
	// control: null is the same as absent
	raw := `{"products":{"vm":{"funding_sources":null,"variants":{"s":{"price_per_hour":"1","allocation_price":"0"}}}}}`
	if book, err := meter.ParsePricing(json.RawMessage(raw)); err != nil || book.FundingSources("vm") != nil {
		t.Fatalf("null funding_sources: %v", err)
	}
}

func TestReserve_SendsDeclaredFundingSources_20261123(t *testing.T) {
	book, err := meter.ParsePricing(json.RawMessage(fundedPricingJSON))
	if err != nil {
		t.Fatal(err)
	}
	f := metertest.New(map[string]string{"org-1": "50"})
	m := newMeter(t, f, func(c *meter.Config) { c.Pricing = book })
	ctx := context.Background()
	if _, err := m.Reserve(ctx, meter.ReserveInput{OrgID: "org-1", UserID: "u", ResourceID: "b1", Product: "vm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(ctx, meter.ReserveInput{OrgID: "org-1", UserID: "u", ResourceID: "b2", Product: "gpu"}); err != nil {
		t.Fatal(err)
	}
	calls := f.ReserveCalls()
	if fs := calls[0].FundingSources; len(fs) != 2 || fs[0] != billing.FundingSubscriptionCredit || fs[1] != billing.FundingBalance {
		t.Fatalf("declared product: reserve sent %v", fs)
	}
	if calls[1].FundingSources != nil {
		t.Fatalf("undeclared product: reserve must not send funding_sources (every bucket), sent %v", calls[1].FundingSources)
	}
	// declared once: setting it on the input as well is refused
	_, err = m.Reserve(ctx, meter.ReserveInput{OrgID: "org-1", UserID: "u", ResourceID: "b3", Product: "vm",
		FundingSources: []string{billing.FundingBalance}})
	if !errors.Is(err, meter.ErrConfig) {
		t.Fatalf("Product + ReserveInput.FundingSources: %v", err)
	}
}

func TestReserve_ExplicitPriceFundingSources_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	if _, err := m.Reserve(context.Background(), in(func(r *meter.ReserveInput) {
		r.FundingSources = []string{billing.FundingCreditBalance}
	})); err != nil {
		t.Fatal(err)
	}
	if fs := f.ReserveCalls()[0].FundingSources; len(fs) != 1 || fs[0] != "credit_balance" {
		t.Fatalf("sent %v", fs)
	}
	n := len(f.ReserveCalls())
	_, err := m.Reserve(context.Background(), in(func(r *meter.ReserveInput) { r.FundingSources = []string{"cash"} }))
	if !errors.Is(err, meter.ErrConfig) || len(f.ReserveCalls()) != n {
		t.Fatalf("an unknown bucket must be refused before billing: %v (calls %d→%d)", err, n, len(f.ReserveCalls()))
	}
	// default unchanged: no funding_sources → none sent
	_, _ = m.Reserve(context.Background(), in(func(r *meter.ReserveInput) { r.ResourceID = "box-2" }))
	if fs := f.ReserveCalls()[len(f.ReserveCalls())-1].FundingSources; fs != nil {
		t.Fatalf("default must send nothing, sent %v", fs)
	}
}

func TestFunds_OrgLevelLowReachesHookWithNilRun_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	h := &hookLog{}
	m := newMeter(t, f, func(c *meter.Config) { c.OnFundsLow = h.hook })
	f.Attach(m)
	run, err := m.Start(context.Background(), in(nil), t0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.TriggerOrgFundsLow(context.Background(), "org-1", map[string]any{"threshold": "10"})
	if err != nil || len(res) != 1 || res[0] != billing.DispatchHandled {
		t.Fatalf("org-level funds_low: %v %v", res, err)
	}
	h.mu.Lock()
	seen, runs := append([]string(nil), h.seen...), append([]*meter.Run(nil), h.runs...)
	h.mu.Unlock()
	if len(seen) != 1 || seen[0] != "funds_low:balance_threshold" || runs[0] != nil {
		t.Fatalf("hook got %v runs %v — an org-level event carries no run", seen, runs)
	}
	if run.FundsState() != meter.FundsFunded {
		t.Fatalf("an org-level event must not change a run's funds state, got %s", run.FundsState())
	}
	// no hook: logged loudly, handled (never an error, never malformed)
	bare := newMeter(t, f, nil)
	if r, err := bare.HandleBillingEvent(context.Background(), metertest.SQSBody(f.OrgFundsLowPayload("org-1", nil))); err != nil || r != billing.DispatchHandled {
		t.Fatalf("no hook: %s %v", r, err)
	}
	_, _ = run.Stop(context.Background(), meter.StopInput{})
}

// Every lifecycle event carries its own event_id (evt_ + 32 hex, the Python emitter's shape).
func TestLifecycleEvents_CarryUniqueEventID_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, func(c *meter.Config) { c.HeartbeatInterval = time.Hour })
	run, err := m.Start(context.Background(), in(nil), t0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = run.HeartbeatNow(context.Background())
	_, _ = run.Stop(context.Background(), meter.StopInput{})
	seen := map[string]bool{}
	evs := f.Events("", run.ReservationID)
	if len(evs) != 3 {
		t.Fatalf("%d events", len(evs))
	}
	for _, ev := range evs {
		if !strings.HasPrefix(ev.EventID, "evt_") || len(ev.EventID) != 36 || seen[ev.EventID] {
			t.Fatalf("%s: event_id %q (seen %v)", ev.EventType, ev.EventID, seen)
		}
		seen[ev.EventID] = true
	}
	raw, _ := json.Marshal(evs[0])
	if !strings.Contains(string(raw), `"event_id":"evt_`) {
		t.Fatalf("event_id must be on the wire: %s", raw)
	}
}

func TestFake_CommitReservationResult_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	ctx := context.Background()
	r, err := f.ReserveFunds(ctx, "org-1", billing.ReserveRequest{UserID: "u", EstimatedCost: d("1"), RequestID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	start, stop := t0, t0.Add(90*time.Minute)
	rate := d("0.25")
	priced := billing.CommitRequest{StartedAt: start, StoppedAt: stop, HourlyRate: &rate}

	c1, err := f.CommitReservationResult(ctx, "org-1", r.ReservationID, priced)
	if err != nil || c1.Outcome != billing.CommitCommitted || c1.ActualCost != nil || !c1.ReservedAmount.Equal(d("1")) {
		t.Fatalf("first commit: %+v %v (the fake prices nothing: ActualCost stays nil)", c1, err)
	}
	if res, _ := f.Reservation(r.ReservationID); res.Status != "committed" {
		t.Fatalf("status %s", res.Status)
	}
	if c2, _ := f.CommitReservationResult(ctx, "org-1", r.ReservationID, priced); c2.Outcome != billing.CommitAlreadySettled || c2.Code != "RESERVATION_ALREADY_SETTLED" {
		t.Fatalf("second commit: %+v", c2)
	}
	if c3, _ := f.CommitReservationResult(ctx, "org-1", "nope", priced); c3.Outcome != billing.CommitNotFound || c3.StatusCode != 404 {
		t.Fatalf("unknown reservation: %+v", c3)
	}
	r2, _ := f.ReserveFunds(ctx, "org-1", billing.ReserveRequest{UserID: "u", EstimatedCost: d("1"), RequestID: "y"})
	_ = f.RefundReservationOutcome(ctx, "org-1", r2.ReservationID, "")
	if c4, _ := f.CommitReservationResult(ctx, "org-1", r2.ReservationID, priced); c4.Outcome != billing.CommitNotActive {
		t.Fatalf("refunded reservation: %+v", c4)
	}
	if n := len(f.CommitCalls()); n != 4 {
		t.Fatalf("CommitCalls = %d, want 4", n)
	}
	if b := f.CommitCalls()[0]; b["org_id"] != "org-1" || b["started_at"] != "2026-11-23T06:00:00.000000Z" || b["hourly_rate"] != "0.25" {
		t.Fatalf("recorded body %v", b)
	}
}

func TestFake_CommitScriptedAndValidation_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	ctx := context.Background()
	r, _ := f.ReserveFunds(ctx, "org-1", billing.ReserveRequest{UserID: "u", EstimatedCost: d("1"), RequestID: "x"})
	priced := billing.CommitRequest{StartedAt: t0, StoppedAt: t0.Add(time.Hour), OnInsufficientFunds: billing.OnInsufficientFundsRecordDebt}

	f.CommitOutcomes = []billing.CommitOutcome{billing.CommitInsufficientFunds, billing.CommitCommitted}
	c1, _ := f.CommitReservationResult(ctx, "org-1", r.ReservationID, priced)
	if c1.Outcome != billing.CommitInsufficientFunds || !c1.HoldMayRemain() {
		t.Fatalf("scripted 402: %+v", c1)
	}
	if res, _ := f.Reservation(r.ReservationID); res.Status != "reserved" {
		t.Fatalf("a refused commit must leave the hold live, status %s", res.Status)
	}
	c2, _ := f.CommitReservationResult(ctx, "org-1", r.ReservationID, priced)
	if c2.Outcome != billing.CommitCommitted || c2.CommitMode != "record_debt" || c2.UnfundedOverrun == nil || !c2.UnfundedOverrun.IsZero() {
		t.Fatalf("scripted commit with record_debt: %+v", c2)
	}
	if res, _ := f.Reservation(r.ReservationID); res.Status != "committed" {
		t.Fatalf("status %s", res.Status)
	}
	// the same local validation as the real client — refused before anything is recorded
	before := len(f.CommitCalls())
	callerPriced := billing.CommitRequest{ActualUsage: map[string]any{"cost": "1"}, OnInsufficientFunds: billing.OnInsufficientFundsRecordDebt}
	if _, err := f.CommitReservationResult(ctx, "org-1", r.ReservationID, callerPriced); !errors.Is(err, billing.ErrInvalidRequest) {
		t.Fatalf("record_debt caller-priced: %v", err)
	}
	if len(f.CommitCalls()) != before {
		t.Fatal("a refused commit must not be recorded as reaching billing")
	}
}
