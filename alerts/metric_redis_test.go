package alerts

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The scrapeable counters an operator reads with GET must actually land in
// Redis, under the documented keys, and reset on a resolve.
func TestRedisCounterMetricSink_ScrapeableKeys(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c.Close()
	sink := NewRedisCounterMetricSink(c, "")
	ctx := context.Background()

	sink.Emit(ctx, Metric{Name: MetricDriftDetected, OrgID: "o1", ResourceKey: "sandbox.concurrent"})
	sink.Emit(ctx, Metric{Name: MetricDriftDetected, OrgID: "o1", ResourceKey: "sandbox.concurrent"})

	if v, _ := c.Get(ctx, "quota:metrics:drift_detected:total").Result(); v != "2" {
		t.Errorf("drift_detected:total = %q, want 2", v)
	}
	if v, _ := c.Get(ctx, "quota:metrics:drift_detected:o1:sandbox.concurrent").Result(); v != "2" {
		t.Errorf("per-pair counter = %q, want 2", v)
	}
	if v, _ := c.Get(ctx, "quota:metrics:drift:o1:sandbox.concurrent:sustained").Result(); v != "2" {
		t.Errorf("sustained = %q, want 2", v)
	}

	// Resolve clears the sustained key and bumps resolved:total.
	sink.Emit(ctx, Metric{Name: MetricDriftResolved, OrgID: "o1", ResourceKey: "sandbox.concurrent"})
	if v, _ := c.Get(ctx, "quota:metrics:drift_resolved:total").Result(); v != "1" {
		t.Errorf("drift_resolved:total = %q, want 1", v)
	}
	if exists, _ := c.Exists(ctx, "quota:metrics:drift:o1:sandbox.concurrent:sustained").Result(); exists != 0 {
		t.Errorf("sustained key should be deleted on resolve")
	}
}

func TestRedisCounterMetricSink_CustomPrefix(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c.Close()
	sink := NewRedisCounterMetricSink(c, "myapp:qm")
	sink.Emit(context.Background(), Metric{Name: MetricDriftDetected, OrgID: "o", ResourceKey: "r"})
	if v, _ := c.Get(context.Background(), "myapp:qm:drift_detected:total").Result(); v != "1" {
		t.Errorf("custom-prefix key = %q, want 1", v)
	}
}
