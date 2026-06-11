package counters

import (
	"context"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// Accumulator is a monotonically-increasing per-period counter (e.g. spend
// cap dollars). Like Counter but with period semantics baked in.
//
// Wire-level: matches Python's `quota:accumulator:{resource}:{scope}:{period}`.
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
