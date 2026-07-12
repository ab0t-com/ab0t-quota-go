package counters

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// accumulatorDelta guards the accumulator's D-31 fail direction (W-T3/CT-04,
// Python parity: AccumulatorCounter.increment applies finite_magnitude). An
// accumulator is spend — a negative delta would ERASE recorded spend (the
// counter class that "cannot be decremented"), and a non-finite delta fails
// loud rather than corrupting the bucket.
func accumulatorDelta(delta float64) (float64, error) {
	if math.IsNaN(delta) || math.IsInf(delta, 0) {
		return 0, fmt.Errorf("counters: accumulator delta must be finite, got %v (D-31)", delta)
	}
	return math.Abs(delta), nil
}

// Accumulator is a monotonically-increasing per-period counter (e.g. spend
// cap dollars). Like Counter but with period semantics baked in.
//
// Wire-level: the Python parity key is `quota:{org}:{rk}:acc:{period}` —
// built by OrgPeriodKey in keys_python.go, which the engine uses. PeriodKey
// below is the DEPRECATED pre-P5.3 shape (finding QG-03). NOTE also QG-05:
// the period TTL here (PeriodTTL) differs from Python's.
type Accumulator struct {
	Store       FloatStore
	Prefix      KeyPrefix
	ResourceKey string
	Reset       config.ResetPeriod
}

// PeriodKey returns the key for the current period bucket.
func (a Accumulator) PeriodKey(scope string, now time.Time) string {
	period := CurrentPeriod(a.Reset, now)
	return a.Prefix.Build("accumulator", a.ResourceKey, scope, period)
}

// Add adds delta to the current period bucket and returns the new value.
// Also sets the TTL so the key expires after the period ends.
func (a Accumulator) Add(ctx context.Context, scope string, now time.Time, delta float64) (float64, error) {
	delta, derr := accumulatorDelta(delta) // W-T3/CT-04 (D-31, Python parity)
	if derr != nil {
		return 0, derr
	}
	key := a.PeriodKey(scope, now)
	v, err := a.Store.IncrByFloat(ctx, key, delta)
	if err != nil {
		return 0, err
	}
	if ttl := PeriodTTL(a.Reset); ttl > 0 {
		_ = a.Store.Expire(ctx, key, ttl)
	}
	return v, nil
}

// Get returns the current period bucket value.
func (a Accumulator) Get(ctx context.Context, scope string, now time.Time) (float64, error) {
	v, _, err := a.Store.GetFloat(ctx, a.PeriodKey(scope, now))
	return v, err
}

// Reset removes the current period bucket.
func (a Accumulator) ResetPeriod(ctx context.Context, scope string, now time.Time) error {
	return a.Store.Delete(ctx, a.PeriodKey(scope, now))
}
