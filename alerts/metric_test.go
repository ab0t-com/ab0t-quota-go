package alerts

import (
	"context"
	"sync"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// captureSink records emitted metrics for assertions.
type captureSink struct {
	mu      sync.Mutex
	metrics []Metric
}

func (s *captureSink) Emit(_ context.Context, m Metric) {
	s.mu.Lock()
	s.metrics = append(s.metrics, m)
	s.mu.Unlock()
}
func (s *captureSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.metrics) }
func (s *captureSink) last() Metric {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metrics[len(s.metrics)-1]
}
func (s *captureSink) countName(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.metrics {
		if m.Name == name {
			n++
		}
	}
	return n
}

func newMgr(t *testing.T, threshold int, sink MetricSink) (*Manager, *captureDispatcher) {
	t.Helper()
	d := &captureDispatcher{}
	m := NewManager(config.AlertsConfig{Enabled: true, CooldownSeconds: 3600, DriftSustainedThreshold: threshold}, d)
	m.MetricSinks = []MetricSink{sink}
	return m, d
}

// The core P4.1 parity: EVERY reconcile heal emits a scrapeable drift_detected
// metric with correct before/after/delta — never cooldown-swallowed (unlike the
// human alert).
func TestRecordDrift_EmitsMetricEveryHeal(t *testing.T) {
	sink := &captureSink{}
	m, _ := newMgr(t, 3, sink)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		m.RecordDrift(ctx, "org1", "sandbox.concurrent", "activations", 5, 1)
	}
	if got := sink.countName(MetricDriftDetected); got != 5 {
		t.Fatalf("drift_detected metric must fire on EVERY heal (never cooldown-gated); got %d want 5", got)
	}
	last := sink.last()
	if last.OrgID != "org1" || last.ResourceKey != "sandbox.concurrent" || last.Source != "activations" {
		t.Errorf("metric fields wrong: %+v", last)
	}
	if last.CounterBefore != 5 || last.CounterAfter != 1 || last.Delta != -4 {
		t.Errorf("before/after/delta wrong: before=%v after=%v delta=%v", last.CounterBefore, last.CounterAfter, last.Delta)
	}
}

// Sustained drift crosses the threshold → a distinct CRITICAL alert.
func TestRecordDrift_SustainedThresholdFiresCriticalAlert(t *testing.T) {
	sink := &captureSink{}
	m, d := newMgr(t, 3, sink)
	ctx := context.Background()

	m.RecordDrift(ctx, "o", "r", "activations", 3, 1) // 1
	m.RecordDrift(ctx, "o", "r", "activations", 3, 1) // 2
	if d.count() != 0 {
		t.Fatalf("below threshold must NOT alert; got %d", d.count())
	}
	m.RecordDrift(ctx, "o", "r", "activations", 3, 1) // 3 == threshold → alert
	if d.count() != 1 || d.sent[0].level != LevelCritical {
		t.Fatalf("threshold crossing must raise ONE critical alert; got %d", d.count())
	}
	if d.sent[0].result.Reason != "drift_sustained" {
		t.Errorf("reason=%q want drift_sustained", d.sent[0].result.Reason)
	}
	// Still emits the metric on every heal even while the alert cools down.
	if sink.countName(MetricDriftDetected) != 3 {
		t.Errorf("want 3 drift_detected metrics, got %d", sink.countName(MetricDriftDetected))
	}
}

// A resolve clears the sustained counter and emits a paired resolved metric —
// but ONLY when the pair was actually drifting.
func TestRecordDriftResolved_OnlyWhenWasDrifting(t *testing.T) {
	sink := &captureSink{}
	m, _ := newMgr(t, 3, sink)
	ctx := context.Background()

	// Perpetually-healthy: resolve on a never-drifted pair → NO metric (no noise).
	m.RecordDriftResolved(ctx, "o", "healthy")
	if sink.countName(MetricDriftResolved) != 0 {
		t.Fatalf("resolve on a never-drifted pair must emit nothing; got %d", sink.countName(MetricDriftResolved))
	}

	// Drift then resolve → one resolved metric + counter reset.
	m.RecordDrift(ctx, "o", "r", "activations", 3, 1)
	m.RecordDrift(ctx, "o", "r", "activations", 3, 1)
	m.RecordDriftResolved(ctx, "o", "r")
	if sink.countName(MetricDriftResolved) != 1 {
		t.Fatalf("drift→resolve must emit exactly one resolved metric; got %d", sink.countName(MetricDriftResolved))
	}
	// After resolve the sustained counter is cleared: 3 fresh re-drifts to alert.
	d2 := &captureDispatcher{}
	m.Dispatcher = d2
	m.RecordDrift(ctx, "o", "r", "activations", 3, 1) // 1
	m.RecordDrift(ctx, "o", "r", "activations", 3, 1) // 2
	if d2.count() != 0 {
		t.Errorf("sustained counter should have reset after resolve; alerted too early")
	}
	m.RecordDrift(ctx, "o", "r", "activations", 3, 1) // 3 → alert again
	if d2.count() != 1 {
		t.Errorf("expected a fresh sustained alert after reset; got %d", d2.count())
	}
}

// Metrics emit even when alert dispatch is disabled (a metric is not a page).
func TestRecordDrift_MetricIndependentOfEnabled(t *testing.T) {
	sink := &captureSink{}
	d := &captureDispatcher{}
	m := NewManager(config.AlertsConfig{Enabled: false}, d)
	m.MetricSinks = []MetricSink{sink}
	m.RecordDrift(context.Background(), "o", "r", "activations", 2, 0)
	if sink.countName(MetricDriftDetected) != 1 {
		t.Errorf("metric must fire even with alerts disabled; got %d", sink.countName(MetricDriftDetected))
	}
	if d.count() != 0 {
		t.Errorf("disabled manager must not dispatch a human alert; got %d", d.count())
	}
}

func TestNewManager_DefaultDriftThreshold(t *testing.T) {
	m := NewManager(config.AlertsConfig{Enabled: true}, &captureDispatcher{})
	if m.DriftSustainedThreshold != 3 {
		t.Errorf("default drift-sustained threshold should be 3, got %d", m.DriftSustainedThreshold)
	}
}
