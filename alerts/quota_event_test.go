package alerts

// D-26 — the over-admission event must reach the REAL sink. This asserts at
// the Dispatcher (where a webhook/log actually fires), proving the chain
// engine.QuotaEvent → Manager.NotifyQuotaEvent → Dispatcher.Send is wired,
// with the right severity per kind and cooldown that dedups admits without
// suppressing a resolve.

import (
	"context"
	"sync"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/engine"
)

type captureDispatcher struct {
	mu   sync.Mutex
	sent []struct {
		level  Level
		result engine.Result
	}
}

func (d *captureDispatcher) Send(_ context.Context, level Level, r engine.Result) error {
	d.mu.Lock()
	d.sent = append(d.sent, struct {
		level  Level
		result engine.Result
	}{level, r})
	d.mu.Unlock()
	return nil
}
func (d *captureDispatcher) count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.sent) }

func TestNotifyQuotaEvent_ReachesDispatcher_D26(t *testing.T) {
	d := &captureDispatcher{}
	m := NewManager(config.AlertsConfig{Enabled: true, CooldownSeconds: 3600}, d)
	ctx := context.Background()

	m.NotifyQuotaEvent(ctx, engine.QuotaEvent{
		Kind: engine.EventOverLimitAdmitted, OrgID: "o1", ResourceKey: "sandboxes", Scope: "org", Level: 2, Limit: 1,
	})
	if d.count() != 1 {
		t.Fatalf("dispatcher should have received the admit, got %d sends", d.count())
	}
	if d.sent[0].level != LevelCritical {
		t.Errorf("over_limit_admitted should be critical, got %v", d.sent[0].level)
	}
	if d.sent[0].result.Reason != string(engine.EventOverLimitAdmitted) {
		t.Errorf("result.Reason = %q", d.sent[0].result.Reason)
	}

	// Same admit again within cooldown → deduped.
	m.NotifyQuotaEvent(ctx, engine.QuotaEvent{
		Kind: engine.EventOverLimitAdmitted, OrgID: "o1", ResourceKey: "sandboxes", Scope: "org", Level: 3, Limit: 1,
	})
	if d.count() != 1 {
		t.Errorf("burst admit should be deduped by cooldown, got %d", d.count())
	}

	// A RESOLVE (distinct kind) is NOT suppressed by the admit's cooldown.
	m.NotifyQuotaEvent(ctx, engine.QuotaEvent{
		Kind: engine.EventOverLimitResolved, OrgID: "o1", ResourceKey: "sandboxes", Scope: "org", Level: 1, Limit: 1,
	})
	if d.count() != 2 {
		t.Fatalf("resolve should reach the dispatcher despite the admit cooldown, got %d", d.count())
	}
	if d.sent[1].level != LevelWarning {
		t.Errorf("over_limit_resolved should be warning, got %v", d.sent[1].level)
	}
}

func TestNotifyQuotaEvent_DisabledOrNoDispatcher_D26(t *testing.T) {
	d := &captureDispatcher{}
	m := NewManager(config.AlertsConfig{Enabled: false}, d)
	m.NotifyQuotaEvent(context.Background(), engine.QuotaEvent{Kind: engine.EventOverLimitAdmitted})
	if d.count() != 0 {
		t.Error("disabled manager must not dispatch")
	}
}
