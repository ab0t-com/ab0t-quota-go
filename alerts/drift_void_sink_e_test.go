package alerts

// (e) — money-incident alerts must reach the REAL sink. Assert at the
// Dispatcher, not the emit (Python shipped over_limit_admitted with no
// dispatcher and its test injected its own capture, proving the emitter worked
// and saying nothing about the wiring — D-40).

import (
	"context"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

func TestNotifyDrift_ReachesDispatcher_E(t *testing.T) {
	d := &captureDispatcher{}
	m := NewManager(config.AlertsConfig{Enabled: true, CooldownSeconds: 3600}, d)
	ctx := context.Background()

	// unsettleable > 0 → CRITICAL money incident.
	m.NotifyDrift(ctx, "o1", "sandboxes", "provider", 2)
	if d.count() != 1 || d.sent[0].level != LevelCritical {
		t.Fatalf("unsettleable drift must reach dispatcher as critical, got %d sends", d.count())
	}
	if d.sent[0].result.Reason != "drift_provider" {
		t.Errorf("reason=%q", d.sent[0].result.Reason)
	}
	// A plain heal (no unsettleable) → warning; distinct cooldown key so it fires.
	m.NotifyDrift(ctx, "o1", "sandboxes", "activations", 0)
	if d.count() != 2 || d.sent[1].level != LevelWarning {
		t.Errorf("plain heal should reach dispatcher as warning, got %d sends", d.count())
	}
}

func TestNotifyVoid_ReachesDispatcher_E(t *testing.T) {
	d := &captureDispatcher{}
	m := NewManager(config.AlertsConfig{Enabled: true, CooldownSeconds: 3600}, d)
	m.NotifyVoid(context.Background(), "resv-1", "stopped", "past_retry_horizon")
	if d.count() != 1 || d.sent[0].level != LevelCritical {
		t.Fatalf("a settlement void is a critical money incident; got %d sends", d.count())
	}
}

func TestNotifyDrift_Disabled_NoDispatch_E(t *testing.T) {
	d := &captureDispatcher{}
	m := NewManager(config.AlertsConfig{Enabled: false}, d)
	m.NotifyDrift(context.Background(), "o1", "x", "provider", 5)
	if d.count() != 0 {
		t.Error("disabled manager must not dispatch")
	}
}
