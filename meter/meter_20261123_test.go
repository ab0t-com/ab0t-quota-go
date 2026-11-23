package meter_test

// meter.Meter + metertest.Fake (v0.1.12, additive). Same contract as the Python ab0t-quota
// ResourceMeter tests: what a client used to have to REMEMBER happens by itself — priced heartbeats, scheduled; stop carries start/stop/rate/fee;
// Session/Close stop or refund; idempotent reserve retries; prices from pricing.products; missing
// dependencies refused in New; funds events routed to hooks with the run.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/meter"
	"github.com/ab0t-com/ab0t-quota-go/meter/metertest"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
	"github.com/shopspring/decimal"
)

var t0 = time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)

const pricingJSON = `{"products":{"vm":{"variants":{
  "small":{"price_per_hour":0.25,"allocation_price":"0.01","default":true},
  "large":{"price_per_hour":"1.00","allocation_price":0}}}}}`

func newMeter(t *testing.T, f *metertest.Fake, mutate func(*meter.Config)) *meter.Meter {
	t.Helper()
	if f == nil {
		f = metertest.New(map[string]string{"org-1": "5"})
	}
	cfg := meter.Config{Billing: f, Sink: f, HeartbeatInterval: 10 * time.Millisecond}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := meter.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	return m
}

func in(over func(*meter.ReserveInput)) meter.ReserveInput {
	r := meter.ReserveInput{
		OrgID:         "org-1",
		UserID:        "u-1",
		ResourceID:    "box-1",
		ResourceType:  "vm",
		HourlyRate:    decimal.RequireFromString("0.25"),
		AllocationFee: decimal.RequireFromString("0.01"),
	}
	if over != nil {
		over(&r)
	}
	return r
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// ---------------------------------------------------------------- New fails loudly

func TestNew_RefusesMissingDependencies_20261123(t *testing.T) {
	f := metertest.New(nil)
	for name, cfg := range map[string]meter.Config{
		"no billing":      {Sink: f},
		"no sink":         {Billing: f},
		"negative beat":   {Billing: f, Sink: f, HeartbeatInterval: -time.Second},
		"negative hold":   {Billing: f, Sink: f, HoldHours: d("-1")},
		"outbox no emit":  {Billing: f, Sink: meter.OutboxSink{}},
		"outbox no topic": {Billing: f, Sink: meter.OutboxSink{Emitter: outbox.NewEmitter(outbox.NewInMemoryStore(), nil, 0, "")}},
	} {
		if _, err := meter.New(cfg); !errors.Is(err, meter.ErrConfig) {
			t.Fatalf("%s: want ErrConfig, got %v", name, err)
		}
	}
	nt := metertest.New(nil)
	nt.NoTransport = true
	if _, err := meter.New(meter.Config{Billing: nt, Sink: nt}); !errors.Is(err, meter.ErrConfig) ||
		!strings.Contains(err.Error(), "no transport") {
		t.Fatalf("no transport: %v", err)
	}
	if _, err := meter.New(meter.Config{Billing: nt, Sink: nt, AllowMissingTransport: true}); err != nil {
		t.Fatalf("knowingly accepted: %v", err)
	}
}

func TestParsePricing_RefusesIncompletePricing_20261123(t *testing.T) {
	cases := [][2]string{
		{``, "no products"},
		{`{}`, "no products"},
		{`{"products":{"vm":{"variants":{"s":{"allocation_price":0}}}}}`, "missing price_per_hour"},
		{`{"products":{"vm":{"variants":{"s":{"price_per_hour":"0.1"}}}}}`, "missing allocation_price"},
		{`{"products":{"vm":{"variants":{"s":{"price_per_hour":0,"allocation_price":0}}}}}`, "not allowed"},
		{`{"products":{"vm":{"variants":{"s":{"price_per_hour":"x","allocation_price":"0"}}}}}`, "not a number"},
		{`{"products":{"vm":{"variants":{}}}}`, "no variants"},
	}
	for _, c := range cases {
		_, err := meter.ParsePricing(json.RawMessage(c[0]))
		if !errors.Is(err, meter.ErrConfig) || !strings.Contains(err.Error(), c[1]) {
			t.Fatalf("%q: want %q, got %v", c[0], c[1], err)
		}
	}
}

func TestPriceBook_NeverGuessesADefault_20261123(t *testing.T) {
	book, err := meter.ParsePricing(json.RawMessage(pricingJSON))
	if err != nil {
		t.Fatal(err)
	}
	p, err := book.Lookup("vm", "")
	if err != nil || !p.HourlyRate.Equal(d("0.25")) || !p.AllocationFee.Equal(d("0.01")) {
		t.Fatalf("default: %+v %v", p, err)
	}
	if p, err = book.Lookup("vm", "large"); err != nil || !p.HourlyRate.Equal(d("1")) || !p.AllocationFee.IsZero() {
		t.Fatalf("large: %+v %v", p, err)
	}
	two, _ := meter.ParsePricing(json.RawMessage(`{"products":{"vm":{"variants":{
	  "a":{"price_per_hour":1,"allocation_price":0},"b":{"price_per_hour":2,"allocation_price":0}}}}}`))
	if _, err := two.Lookup("vm", ""); err == nil || !strings.Contains(err.Error(), "mark exactly one default") {
		t.Fatalf("ambiguous default must be refused: %v", err)
	}
	if _, err := two.Lookup("vm", "c"); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared variant: %v", err)
	}
	if _, err := book.Lookup("gpu", ""); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared product: %v", err)
	}
}

// ---------------------------------------------------------------- Reserve

func TestReserve_RefusesSilentPathsBeforeMoneyMoves_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	for name, over := range map[string]func(*meter.ReserveInput){
		"no price":     func(r *meter.ReserveInput) { r.HourlyRate = decimal.Zero },
		"negative":     func(r *meter.ReserveInput) { r.HourlyRate = d("-1") },
		"negative fee": func(r *meter.ReserveInput) { r.AllocationFee = d("-0.01") },
		"blank org":    func(r *meter.ReserveInput) { r.OrgID = " " },
		"blank res":    func(r *meter.ReserveInput) { r.ResourceID = "" },
		"no type":      func(r *meter.ReserveInput) { r.ResourceType = "" },
	} {
		if _, err := m.Reserve(context.Background(), in(over)); !errors.Is(err, meter.ErrConfig) {
			t.Fatalf("%s: want ErrConfig, got %v", name, err)
		}
	}
	if n := len(f.ReserveCalls()); n != 0 {
		t.Fatalf("%d reserve calls reached billing", n)
	}
}

func TestReserve_SizesHoldAndFreshRequestIDPerLaunch_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	a, err := m.Reserve(context.Background(), in(nil))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.Reserve(context.Background(), in(func(r *meter.ReserveInput) { r.ResourceID = "box-2" }))
	calls := f.ReserveCalls()
	if !calls[0].EstimatedCost.Equal(d("0.26")) || calls[0].OperationType != "vm" || calls[0].SessionID != "box-1" {
		t.Fatalf("%+v", calls[0])
	}
	if calls[0].RequestID == "" || calls[0].RequestID == calls[1].RequestID || a.ReservationID == b.ReservationID {
		t.Fatal("each launch needs its own request id and reservation")
	}
	if r, _ := f.Reservation(a.ReservationID); r.Status != "reserved" {
		t.Fatalf("status %s", r.Status)
	}
}

func TestReserve_FromDeclaredPricing_20261123(t *testing.T) {
	book, err := meter.ParsePricing(json.RawMessage(pricingJSON))
	if err != nil {
		t.Fatal(err)
	}
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, func(c *meter.Config) { c.Pricing = book })
	run, err := m.Reserve(context.Background(), meter.ReserveInput{OrgID: "org-1", UserID: "u", ResourceID: "b", Product: "vm", Variant: "large"})
	if err != nil {
		t.Fatal(err)
	}
	if !run.HourlyRate.Equal(d("1")) || run.ResourceType != "vm" || run.Metadata["product"] != "vm" || run.Metadata["variant"] != "large" {
		t.Fatalf("%+v", run)
	}
	if err := run.Started(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if ev := f.Events(meter.EventStarted, run.ReservationID)[0]; ev.Metadata["product"] != "vm" {
		t.Fatalf("product must ride the event metadata: %+v", ev.Metadata)
	}
	_, _ = run.Stop(context.Background(), meter.StopInput{})
	if _, err := m.Reserve(context.Background(), meter.ReserveInput{OrgID: "org-1", UserID: "u", ResourceID: "b", Product: "vm", HourlyRate: d("1")}); !errors.Is(err, meter.ErrConfig) {
		t.Fatalf("product + rate must be refused: %v", err)
	}
	bare := newMeter(t, f, nil)
	if _, err := bare.Reserve(context.Background(), meter.ReserveInput{OrgID: "org-1", UserID: "u", ResourceID: "b", Product: "vm"}); !errors.Is(err, meter.ErrConfig) {
		t.Fatalf("product without Pricing must be refused: %v", err)
	}
}

func TestReserve_TypedRefusals_20261123(t *testing.T) {
	poor := metertest.New(map[string]string{"org-1": "0.10"})
	_, err := newMeter(t, poor, nil).Reserve(context.Background(), in(nil))
	var ife *meter.InsufficientFundsError
	if !errors.As(err, &ife) {
		t.Fatalf("want InsufficientFundsError, got %v", err)
	}
	body := ife.APIError()
	if body["error"] != "budget_exceeded" || body["available_balance"] != "0.1" || body["required_topup"] != "0.16" {
		t.Fatalf("%v", body)
	}
	lim := metertest.New(map[string]string{"org-1": "5"})
	lim.LimitedOrgs["org-1"] = "daily_limit_exceeded"
	_, err = newMeter(t, lim, nil).Reserve(context.Background(), in(nil))
	var rle *meter.ReservationLimitedError
	if !errors.As(err, &rle) || rle.Reason != "daily_limit_exceeded" {
		t.Fatalf("want ReservationLimitedError, got %v", err)
	}
}

func TestReserve_TransientRetriesOnceWithSameRequestID_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	f.ReserveFailures = []error{errors.New("dial tcp: connection refused")}
	m := newMeter(t, f, nil)
	run, err := m.Reserve(context.Background(), in(func(r *meter.ReserveInput) { r.RequestID = "launch-42" }))
	if err != nil {
		t.Fatal(err)
	}
	calls := f.ReserveCalls()
	if len(calls) != 2 || calls[0].RequestID != "launch-42" || calls[1].RequestID != "launch-42" || run.RequestID != "launch-42" {
		t.Fatalf("%+v", calls)
	}
	f2 := metertest.New(map[string]string{"org-1": "5"})
	f2.ReserveFailures = []error{billing.ErrInvalidRequest}
	if _, err := newMeter(t, f2, nil).Reserve(context.Background(), in(nil)); err == nil || len(f2.ReserveCalls()) != 1 {
		t.Fatal("a non-transient error must not be retried")
	}
}

func TestReserve_SameRequestIDSameReservation_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	a, _ := m.Reserve(context.Background(), in(func(r *meter.ReserveInput) { r.RequestID = "launch-1" }))
	b, _ := m.Reserve(context.Background(), in(func(r *meter.ReserveInput) { r.RequestID = "launch-1" }))
	if a.ReservationID != b.ReservationID || !f.Held("org-1").Equal(d("0.26")) {
		t.Fatal("a retried reserve must not be a second hold")
	}
}

// ---------------------------------------------------------------- heartbeats carry the price

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStarted_SchedulesPricedHeartbeats_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	run, err := m.Start(context.Background(), in(nil), t0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(f.Heartbeats(run.ReservationID)) >= 2 })
	for _, hb := range f.Heartbeats(run.ReservationID) {
		if hb.HourlyRate != "0.25" || hb.AllocationFee != "0.01" || hb.StartedAt != "2026-11-23T06:00:00.000000Z" {
			t.Fatalf("heartbeat not priced: %+v", hb)
		}
	}
	if len(f.UnpricedHeartbeats()) != 0 {
		t.Fatal("unpriced heartbeat sent")
	}
	if r, _ := f.Reservation(run.ReservationID); r.Status != "running" {
		t.Fatalf("status %s", r.Status)
	}
	_, _ = run.Stop(context.Background(), meter.StopInput{})
}

func TestHeartbeat_EmitFailureDoesNotKillTheLoop_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	run, _ := m.Start(context.Background(), in(nil), t0)
	f.FailNextEmits(errors.New("outbox store down"), 1)
	waitFor(t, func() bool { return run.HeartbeatFailures() == 1 && run.HeartbeatsSent() >= 1 })
	_, _ = run.Stop(context.Background(), meter.StopInput{})
}

// ---------------------------------------------------------------- Stop / Abort / Close / Session

func TestStop_CarriesEverythingBillingPricesWith_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, func(c *meter.Config) { c.Now = func() time.Time { return t0.Add(2 * time.Hour) } })
	run, _ := m.Start(context.Background(), in(nil), t0)
	ok, err := run.Stop(context.Background(), meter.StopInput{Reason: "user_stopped"})
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	stops := f.Events(meter.EventStopped, run.ReservationID)
	if len(stops) != 1 {
		t.Fatalf("%d stops", len(stops))
	}
	s := stops[0]
	if s.StartedAt != "2026-11-23T06:00:00.000000Z" || s.StoppedAt != "2026-11-23T08:00:00.000000Z" ||
		s.HourlyRate != "0.25" || s.AllocationFee != "0.01" || s.Reason != "user_stopped" {
		t.Fatalf("%+v", s)
	}
	if r, _ := f.Reservation(run.ReservationID); r.Status != "committed" {
		t.Fatalf("status %s", r.Status)
	}
	n := len(f.Heartbeats(""))
	time.Sleep(40 * time.Millisecond)
	if len(f.Heartbeats("")) != n {
		t.Fatal("heartbeats continued after stop")
	}
	if ok, err := run.Stop(context.Background(), meter.StopInput{}); !ok || err != nil || len(f.Events(meter.EventStopped, "")) != 1 {
		t.Fatal("Stop must be idempotent")
	}
}

func TestStop_DeletedAndNeverStarted_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	run, _ := m.Start(context.Background(), in(nil), t0)
	_, _ = run.Stop(context.Background(), meter.StopInput{Deleted: true})
	if len(f.Events(meter.EventDeleted, "")) != 1 || len(f.Events(meter.EventStopped, "")) != 0 {
		t.Fatal("Deleted must emit resource.deleted")
	}
	never, _ := m.Reserve(context.Background(), in(func(r *meter.ReserveInput) { r.ResourceID = "box-2" }))
	if ok, err := never.Stop(context.Background(), meter.StopInput{}); ok || err != nil || never.Status() != meter.StatusAborted {
		t.Fatalf("never-started stop must refund: %v %v %s", ok, err, never.Status())
	}
	if r, _ := f.Reservation(never.ReservationID); r.Status != "refunded" {
		t.Fatalf("status %s", r.Status)
	}
}

func TestStop_EmitErrorKeepsRunRetryable_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	// no loop beat may consume the injected failure before Stop does
	m := newMeter(t, f, func(c *meter.Config) { c.HeartbeatInterval = time.Hour })
	run, _ := m.Start(context.Background(), in(nil), t0)
	f.FailNextEmits(errors.New("outbox store down"), 1)
	if _, err := run.Stop(context.Background(), meter.StopInput{}); err == nil || run.Status() != meter.StatusRunning {
		t.Fatalf("failed stop must leave the run running: %v %s", err, run.Status())
	}
	if ok, err := run.Stop(context.Background(), meter.StopInput{}); !ok || err != nil || run.Status() != meter.StatusStopped {
		t.Fatalf("retry: %v %v", ok, err)
	}
}

func TestAbort_RefusesRealRuntimeAndStaysRetryable_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	running, _ := m.Start(context.Background(), in(nil), t0)
	if _, err := running.Abort(context.Background(), ""); !errors.Is(err, meter.ErrState) {
		t.Fatalf("abort on running: %v", err)
	}
	_, _ = running.Stop(context.Background(), meter.StopInput{})
	run, _ := m.Reserve(context.Background(), in(func(r *meter.ReserveInput) { r.ResourceID = "box-2" }))
	f.RefundOutcomes = []billing.RefundOutcome{billing.RefundFailed}
	if out, _ := run.Abort(context.Background(), ""); out != billing.RefundFailed || run.Status() != meter.StatusReserved {
		t.Fatalf("%s %s", out, run.Status())
	}
	if out, _ := run.Abort(context.Background(), ""); out != billing.RefundRefunded || run.Status() != meter.StatusAborted {
		t.Fatalf("%s %s", out, run.Status())
	}
}

func TestClose_RightThingPerState_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	never, _ := m.Reserve(context.Background(), in(nil))
	if err := never.Close(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.Reservation(never.ReservationID); r.Status != "refunded" {
		t.Fatalf("never-started close: %s", r.Status)
	}
	running, _ := m.Start(context.Background(), in(func(r *meter.ReserveInput) { r.ResourceID = "box-2" }), t0)
	_ = running.Close(context.Background(), "")
	_ = running.Close(context.Background(), "")
	if r, _ := f.Reservation(running.ReservationID); r.Status != "committed" || len(f.Events(meter.EventStopped, "")) != 1 {
		t.Fatalf("running close: %s", r.Status)
	}
}

func TestSession_CommitsOnReturnErrorAndPanic_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	var ids []string
	if err := m.Session(context.Background(), in(nil), func(_ context.Context, run *meter.Run) error {
		ids = append(ids, run.ReservationID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("job crashed")
	if err := m.Session(context.Background(), in(nil), func(_ context.Context, run *meter.Run) error {
		ids = append(ids, run.ReservationID)
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("fn error must be returned: %v", err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = m.Session(context.Background(), in(nil), func(_ context.Context, run *meter.Run) error {
			ids = append(ids, run.ReservationID)
			panic("kaboom")
		})
	}()
	want := []string{"completed", "failed", "failed"}
	for i, id := range ids {
		stops := f.Events(meter.EventStopped, id)
		if len(stops) != 1 || stops[0].Reason != want[i] {
			t.Fatalf("session %d: %+v", i, stops)
		}
	}
}

// ---------------------------------------------------------------- restart-safe

func TestResume_RestartsHeartbeatsFromPersistedState_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	run, _ := m.Start(context.Background(), in(nil), t0)
	raw, _ := json.Marshal(run.State()) // what the client stores on its row
	m.Shutdown()
	n := len(f.Heartbeats(""))
	time.Sleep(30 * time.Millisecond)
	if len(f.Heartbeats("")) != n {
		t.Fatal("Shutdown must stop heartbeat loops")
	}
	var st meter.RunState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	m2 := newMeter(t, f, nil)
	resumed, err := m2.Resume(st, true)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ReservationID != run.ReservationID || !resumed.StartedAt().Equal(t0) || !resumed.HourlyRate.Equal(d("0.25")) {
		t.Fatalf("%+v", resumed.State())
	}
	waitFor(t, func() bool { return len(f.Heartbeats("")) > n })
	if m2.ActiveRun(run.ReservationID) != resumed {
		t.Fatal("resumed run must be registered for funds events")
	}
	_, _ = resumed.Stop(context.Background(), meter.StopInput{})
	if _, err := m2.Resume(meter.RunState{Version: 99}, false); !errors.Is(err, meter.ErrConfig) {
		t.Fatalf("foreign state: %v", err)
	}
}

func TestHeartbeat_StatelessForASweeper_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	run, _ := m.Reserve(context.Background(), in(nil))
	if ok, _ := m.Heartbeat(context.Background(), run.State()); ok {
		t.Fatal("a reserved (not running) state must be skipped")
	}
	_ = run.Started(context.Background(), t0)
	st := run.State()
	m.Shutdown()
	if ok, err := m.Heartbeat(context.Background(), st); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	hbs := f.Heartbeats(run.ReservationID)
	if hbs[len(hbs)-1].HourlyRate != "0.25" {
		t.Fatal("sweeper heartbeat must be priced")
	}
}

// ---------------------------------------------------------------- funds events

type hookLog struct {
	mu   sync.Mutex
	seen []string
	runs []*meter.Run
}

func (h *hookLog) hook(_ context.Context, ev billing.FundsEvent, run *meter.Run) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, string(ev.Kind)+":"+ev.Trigger)
	h.runs = append(h.runs, run)
	return nil
}

func TestFunds_ExhaustedReachesHookWithRun_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	var h hookLog
	m := newMeter(t, f, func(c *meter.Config) { c.OnFundsExhausted = h.hook })
	f.Attach(m)
	run, _ := m.Start(context.Background(), in(nil), t0)
	res, err := f.TriggerFundsExhausted(context.Background(), run.ReservationID, "", nil)
	if err != nil || len(res) != 1 || res[0] != billing.DispatchHandled {
		t.Fatalf("%v %v", res, err)
	}
	if len(h.runs) != 1 || h.runs[0] != run || h.seen[0] != "funds_exhausted:heartbeat" || run.FundsState() != meter.FundsExhausted {
		t.Fatalf("%v %v %s", h.seen, h.runs, run.FundsState())
	}
	if ev := run.LastFundsEvent(); ev == nil || !ev.IsRunningResource() {
		t.Fatal("last funds event must be recorded")
	}
	if res, _ := f.TriggerFundsExhausted(context.Background(), run.ReservationID, "", nil); res[0] != billing.DispatchDuplicate {
		t.Fatalf("duplicate: %v", res)
	}
	_, _ = run.Stop(context.Background(), meter.StopInput{})
}

func TestFunds_RunNotInThisProcessIsNil_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	var h hookLog
	m := newMeter(t, f, func(c *meter.Config) { c.OnFundsLow = h.hook })
	f.Attach(m)
	other := newMeter(t, f, nil) // another replica owns the run
	run, _ := other.Start(context.Background(), in(nil), t0)
	_, _ = f.TriggerFundsLow(context.Background(), run.ReservationID, nil)
	if len(h.runs) != 1 || h.runs[0] != nil {
		t.Fatalf("%v", h.runs)
	}
	_, _ = run.Stop(context.Background(), meter.StopInput{})
}

func TestFunds_NoHookStillRecordsState_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	m := newMeter(t, f, nil)
	f.Attach(m)
	run, _ := m.Start(context.Background(), in(nil), t0)
	res, err := f.TriggerFundsExhausted(context.Background(), run.ReservationID, "", nil)
	if err != nil || res[0] != billing.DispatchHandled || run.FundsState() != meter.FundsExhausted {
		t.Fatalf("%v %v %s", res, err, run.FundsState())
	}
	_, _ = run.Stop(context.Background(), meter.StopInput{})
}

func TestFunds_AfterStopDebtRoutesWithNilRun_20261123(t *testing.T) {
	f := metertest.New(map[string]string{"org-1": "5"})
	var h hookLog
	m := newMeter(t, f, func(c *meter.Config) { c.OnFundsExhausted = h.hook })
	f.Attach(m)
	run, _ := m.Start(context.Background(), in(nil), t0)
	_, _ = run.Stop(context.Background(), meter.StopInput{})
	_, _ = f.TriggerFundsExhausted(context.Background(), run.ReservationID, billing.TriggerCommitOverrun,
		map[string]any{"unfunded_overrun": d("0.08")})
	if len(h.seen) != 1 || h.seen[0] != "funds_exhausted:commit_overrun" || h.runs[0] != nil {
		t.Fatalf("%v %v", h.seen, h.runs)
	}
}

func TestFunds_SharedQueueIgnoresOtherEvents_20261123(t *testing.T) {
	m := newMeter(t, nil, nil)
	if res, err := m.HandleBillingEvent(context.Background(), []byte(`{"event_type":"tier.changed","org_id":"o"}`)); err != nil || res != billing.DispatchIgnored {
		t.Fatalf("%v %v", res, err)
	}
}

// ---------------------------------------------------------------- the durable outbox sink

type recPub struct {
	mu   sync.Mutex
	recs []outbox.Record
}

func (p *recPub) Publish(_ context.Context, r outbox.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recs = append(p.recs, r)
	return nil
}

func TestOutboxSink_WritesTheWireShapeUnderThePythonKey_20261123(t *testing.T) {
	pub := &recPub{}
	em := outbox.NewEmitter(outbox.NewInMemoryStore(), pub, 0, "")
	if !em.HasPublisher() {
		t.Fatal("HasPublisher")
	}
	f := metertest.New(map[string]string{"org-1": "5"})
	m, err := meter.New(meter.Config{Billing: f, Sink: meter.OutboxSink{Emitter: em}, HeartbeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	run, _ := m.Start(context.Background(), in(nil), t0)
	if _, err := run.HeartbeatNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _ = run.Stop(context.Background(), meter.StopInput{})
	if len(pub.recs) != 3 {
		t.Fatalf("%d records", len(pub.recs))
	}
	hb := pub.recs[1]
	if hb.Key != run.ReservationID+":"+meter.EventHeartbeat || hb.EventType != meter.EventHeartbeat || hb.ResourceType != "vm" {
		t.Fatalf("%+v", hb)
	}
	var body map[string]any
	if err := json.Unmarshal(hb.Event, &body); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{"event_type", "resource.heartbeat"},
		{"org_id", "org-1"},
		{"reservation_id", run.ReservationID},
		{"hourly_rate", "0.25"},
		{"allocation_fee", "0.01"},
		{"started_at", "2026-11-23T06:00:00.000000Z"},
	}
	for _, kv := range want {
		if body[kv[0]] != kv[1] {
			t.Fatalf("%s = %v, want %v (body %v)", kv[0], body[kv[0]], kv[1], body)
		}
	}
	if _, ok := body["stopped_at"]; !ok {
		t.Fatal("stopped_at must always be present")
	}
	if (&outbox.Emitter{}).HasPublisher() || (*outbox.Emitter)(nil).HasPublisher() {
		t.Fatal("no publisher")
	}
}
