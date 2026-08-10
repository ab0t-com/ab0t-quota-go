package alerts

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"
)

// RedisCounterMetricSink is the "if there is no metrics sink" fallback
// (ticket 20260810, P4.1). Every consumer already has the engine's Redis — this
// needs no new infrastructure, no statsd agent, no CloudWatch IAM grant. It
// increments plain counters an operator (or a future statsd/CloudWatch bridge)
// can read directly with GET — a `redis-cli GET quota:metrics:drift_detected:total`
// already works.
//
// Keys (all under Prefix, default "quota:metrics"):
//   - {prefix}:drift_detected:total               — INCR, all-time count.
//   - {prefix}:drift_detected:{org}:{resource}    — INCR, per (org, resource).
//   - {prefix}:drift_resolved:total               — INCR, all-time count.
//   - {prefix}:drift:{org}:{resource}:sustained   — consecutive detects with no
//     intervening resolve for this (org, resource); reset (DEL) on a resolve.
//     The copy an external scraper reads without importing the library.
//
// A dispatch failure (a flaky Redis op) is caught + logged, never raised —
// recording a metric must never break the reconcile pass.
type RedisCounterMetricSink struct {
	c      redis.Cmdable
	prefix string
}

// NewRedisCounterMetricSink builds the sink. prefix defaults to "quota:metrics".
func NewRedisCounterMetricSink(c redis.Cmdable, prefix string) *RedisCounterMetricSink {
	if prefix == "" {
		prefix = "quota:metrics"
	}
	return &RedisCounterMetricSink{c: c, prefix: prefix}
}

func (s *RedisCounterMetricSink) sustainedKey(org, rk string) string {
	return s.prefix + ":drift:" + org + ":" + rk + ":sustained"
}

func (s *RedisCounterMetricSink) Emit(ctx context.Context, m Metric) {
	if s.c == nil {
		return
	}
	var err error
	switch m.Name {
	case MetricDriftDetected:
		if e := s.c.Incr(ctx, s.prefix+":drift_detected:total").Err(); e != nil {
			err = e
		}
		if e := s.c.Incr(ctx, s.prefix+":drift_detected:"+m.OrgID+":"+m.ResourceKey).Err(); e != nil {
			err = e
		}
		if e := s.c.Incr(ctx, s.sustainedKey(m.OrgID, m.ResourceKey)).Err(); e != nil {
			err = e
		}
	case MetricDriftResolved:
		if e := s.c.Incr(ctx, s.prefix+":drift_resolved:total").Err(); e != nil {
			err = e
		}
		if e := s.c.Del(ctx, s.sustainedKey(m.OrgID, m.ResourceKey)).Err(); e != nil {
			err = e
		}
	}
	if err != nil {
		slog.Error("redis_counter_metric_dispatch_error", "metric", m.Name,
			"org", m.OrgID, "resource", m.ResourceKey, "err", err)
	}
}
