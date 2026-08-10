package alerts

import (
	"context"
	"log/slog"
)

// Drift-observability metrics (ticket 20260810, P4.1 — Go parity with Python
// ab0t_quota/alerts.py's QuotaMetric + MetricDispatcher).
//
// The gap this closes: Go already ALERTS on a reconcile heal (Manager.NotifyDrift),
// but that alert is COOLDOWN-gated (default 3600s). A gauge that drifts and
// re-heals every reconcile pass therefore alerts ONCE an hour and is otherwise
// invisible — there is no scrapeable, graphable signal that drift is RECURRING.
// A metric is a numeric SINK, distinct from a human-facing alert: it is emitted
// on EVERY heal (never cooldown-swallowed) so a consumer can graph drift rate
// and a sustained-drift escalation can fire.

// Metric is one numeric drift observation. Low-cardinality, scrapeable —
// distinct from the free-text alert Result.
type Metric struct {
	Name          string // "quota.drift_detected" | "quota.drift_resolved"
	OrgID         string
	ResourceKey   string
	CounterBefore float64
	CounterAfter  float64
	Delta         float64 // CounterAfter - CounterBefore (0.0 for resolved)
	Source        string  // which source won: activations | provider | provider_unreachable | provider_absent
}

// Metric name constants.
const (
	MetricDriftDetected = "quota.drift_detected"
	MetricDriftResolved = "quota.drift_resolved"
)

// MetricSink records one metric observation somewhere. Multiple sinks may be
// registered (e.g. a scrapeable Redis counter AND a structured log line). A
// sink failure must never break the reconcile pass — implementations swallow +
// log their own errors.
type MetricSink interface {
	Emit(ctx context.Context, m Metric)
}

// LogMetricSink emits metrics as a structured, grep/scrape-able log line
// (default, always active). Distinct logger name + key=value SHAPE from the
// alert's message line, so a log-based metrics pipeline (Datadog log-metrics,
// CloudWatch Logs Insights, a Prometheus log exporter) can parse it without
// string-matching a sentence meant for a human.
type LogMetricSink struct{}

func (LogMetricSink) Emit(_ context.Context, m Metric) {
	slog.Info("quota_metric",
		"name", m.Name,
		"org_id", m.OrgID,
		"resource_key", m.ResourceKey,
		"counter_before", m.CounterBefore,
		"counter_after", m.CounterAfter,
		"delta", m.Delta,
		"source", m.Source,
	)
}
