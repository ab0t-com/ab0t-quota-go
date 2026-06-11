package alerts

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/engine"
)

type stubDispatcher struct {
	calls atomic.Int32
}

func (s *stubDispatcher) Send(ctx context.Context, level Level, r engine.Result) error {
	s.calls.Add(1)
	return nil
}

func TestManager_DisabledNoOp(t *testing.T) {
	s := &stubDispatcher{}
	m := NewManager(config.AlertsConfig{Enabled: false}, s)
	m.Notify(context.Background(), engine.Result{Decision: engine.Warn})
	if s.calls.Load() != 0 {
		t.Error("disabled should not dispatch")
	}
}

func TestManager_OnlyWarnAndCritical(t *testing.T) {
	s := &stubDispatcher{}
	m := NewManager(config.AlertsConfig{Enabled: true}, s)
	m.Notify(context.Background(), engine.Result{Decision: engine.Allow})
	m.Notify(context.Background(), engine.Result{Decision: engine.Deny})
	if s.calls.Load() != 0 {
		t.Errorf("non-threshold decisions should not dispatch, got %d", s.calls.Load())
	}
	m.Notify(context.Background(), engine.Result{Decision: engine.Warn, Resource: "x", TierID: "p"})
	m.Notify(context.Background(), engine.Result{Decision: engine.Critical, Resource: "x", TierID: "p"})
	if s.calls.Load() != 2 {
		t.Errorf("warn+critical dispatch failed, got %d", s.calls.Load())
	}
}

func TestManager_CooldownSuppressesBursts(t *testing.T) {
	s := &stubDispatcher{}
	m := NewManager(config.AlertsConfig{Enabled: true, CooldownSeconds: 3600}, s)
	for i := 0; i < 5; i++ {
		m.Notify(context.Background(), engine.Result{Decision: engine.Warn, Resource: "x", TierID: "p"})
	}
	if s.calls.Load() != 1 {
		t.Errorf("cooldown should collapse 5 → 1, got %d", s.calls.Load())
	}
}

func TestWebhookDispatcher_RejectsBadScheme(t *testing.T) {
	if _, err := NewWebhookDispatcher("file:///etc/passwd"); err == nil {
		t.Error("expected scheme rejection")
	}
}

func TestWebhookDispatcher_RejectsLocalhost(t *testing.T) {
	d, _ := NewWebhookDispatcher("http://localhost/x")
	if err := d.Send(context.Background(), LevelWarning, engine.Result{}); err == nil {
		t.Error("expected SSRF rejection")
	}
}
