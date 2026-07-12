package engine

// D-26 — the over-admission SINK. Assert at the SINK (a captured OnEvent
// subscriber), not merely that Spend counted: a legacy Spend crossing the
// limit must (a) COUNT (never refuse), (b) publish over_limit_admitted with
// org/resource/level, and a subscriber must RECEIVE it. Covers org scope,
// per-user scope, no-fire-under-limit, and the paired resolved event.
//
// Mirrors Python's test_over_limit_admitted_20260710.py coverage shape.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

type eventSink struct {
	mu     sync.Mutex
	events []QuotaEvent
}

func (s *eventSink) fn() EventSink {
	return func(_ context.Context, ev QuotaEvent) {
		s.mu.Lock()
		s.events = append(s.events, ev)
		s.mu.Unlock()
	}
}
func (s *eventSink) of(kind EventKind) []QuotaEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []QuotaEvent
	for _, e := range s.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func d26Engine(t *testing.T, sink *eventSink, orgLimit float64, perUser *float64) (*Engine, *counters.Factory) {
	t.Helper()
	tl := config.TierLimit{Limit: &orgLimit}
	if perUser != nil {
		tl.PerUserLimit = perUser
	}
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true}, // default legacy_increment = count_and_alert
		TierProvider: config.TierProviderConfig{Type: "static", DefaultTier: "pro", Mapping: map[string]string{"alice": "pro"}},
		Tiers:        []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{"sandboxes": tl}}},
		Resources:    []config.ResourceDef{{ResourceKey: "sandboxes", CounterType: config.CounterGauge}},
	}
	prov, _ := providers.New(cfg.TierProvider)
	fs := counters.NewMemoryFactory("quota")
	e := &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov, Factory: fs,
		Messages: messages.New(messages.Templates{}),
		Clock:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		OnEvent:  sink.fn(),
	}
	return e, fs
}

// Org scope: Spend past the org limit counts AND the sink receives
// over_limit_admitted once (on the crossing), with the right payload.
func TestOverLimitAdmitted_OrgScope_ReachesSink_D26(t *testing.T) {
	sink := &eventSink{}
	e, _ := d26Engine(t, sink, 1, nil)
	ctx := context.Background()
	in := CheckInput{OrgID: "o1", ResourceKey: "sandboxes", Cost: 1}

	if _, err := e.Spend(ctx, in); err != nil { // level 1 == limit, no fire
		t.Fatal(err)
	}
	if got := len(sink.of(EventOverLimitAdmitted)); got != 0 {
		t.Fatalf("no fire AT the limit expected, got %d", got)
	}
	v, err := e.Spend(ctx, in) // level 2 > limit 1 → crossing
	if err != nil || v != 2 {
		t.Fatalf("D-24: Spend must count past limit: v=%v err=%v", v, err)
	}
	admits := sink.of(EventOverLimitAdmitted)
	if len(admits) != 1 {
		t.Fatalf("sink should receive exactly one over_limit_admitted, got %d", len(admits))
	}
	ev := admits[0]
	if ev.OrgID != "o1" || ev.ResourceKey != "sandboxes" || ev.Scope != "org" || ev.Level != 2 || ev.Limit != 1 {
		t.Errorf("payload wrong: %+v", ev)
	}
	// Only the crossing fires — a further spend past the limit does not re-fire.
	if _, err := e.Spend(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got := len(sink.of(EventOverLimitAdmitted)); got != 1 {
		t.Errorf("only the crossing should fire; got %d admits", got)
	}
}

// Per-user scope: crossing a per-user limit (org still under) reaches the sink.
func TestOverLimitAdmitted_UserScope_ReachesSink_D26(t *testing.T) {
	pu := 1.0
	sink := &eventSink{}
	e, _ := d26Engine(t, sink, 100, &pu) // org cap high; per-user cap 1
	ctx := context.Background()
	in := CheckInput{OrgID: "o1", UserID: "alice", ResourceKey: "sandboxes", Cost: 1}

	_, _ = e.Spend(ctx, in) // user level 1 == cap, no fire
	_, _ = e.Spend(ctx, in) // user level 2 > cap 1 → crossing
	admits := sink.of(EventOverLimitAdmitted)
	if len(admits) != 1 {
		t.Fatalf("expected one per-user over_limit_admitted, got %d (%+v)", len(admits), admits)
	}
	ev := admits[0]
	if ev.Scope != "user" || ev.UserID != "alice" || ev.Level != 2 || ev.Limit != 1 {
		t.Errorf("per-user payload wrong: %+v", ev)
	}
}

// No fire strictly under the limit.
func TestOverLimit_NoFireUnderLimit_D26(t *testing.T) {
	sink := &eventSink{}
	e, _ := d26Engine(t, sink, 5, nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := e.Spend(ctx, CheckInput{OrgID: "o1", ResourceKey: "sandboxes", Cost: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(sink.of(EventOverLimitAdmitted)); got != 0 {
		t.Errorf("no over_limit_admitted expected under/at limit, got %d", got)
	}
}

// The paired de-escalation: a release that brings the gauge back to/under the
// limit fires over_limit_resolved (FUTURE §5) — a healed drift clears.
func TestOverLimitResolved_OnRelease_ReachesSink_D26(t *testing.T) {
	sink := &eventSink{}
	e, _ := d26Engine(t, sink, 1, nil)
	ctx := context.Background()
	in := CheckInput{OrgID: "o1", ResourceKey: "sandboxes", Cost: 1}

	_, _ = e.Spend(ctx, in) // 1
	_, _ = e.Spend(ctx, in) // 2 > 1 → admitted
	if len(sink.of(EventOverLimitAdmitted)) != 1 {
		t.Fatal("setup: expected an admit before testing resolve")
	}
	if err := e.Release(ctx, in); err != nil { // back to 1 == limit → resolved
		t.Fatal(err)
	}
	res := sink.of(EventOverLimitResolved)
	if len(res) != 1 {
		t.Fatalf("expected one over_limit_resolved on release to limit, got %d", len(res))
	}
	if res[0].Level != 1 || res[0].Limit != 1 || res[0].Scope != "org" {
		t.Errorf("resolved payload wrong: %+v", res[0])
	}
}
