// Package counters implements the live quota counter primitives.
//
// Counter types:
//
//	Counter      — per-period incrementing float, INCRBYFLOAT + TTL
//	Gauge        — set/incr/decr value, no TTL (e.g. concurrent sandboxes)
//	Rate         — sliding-window using a Redis sorted set
//	Accumulator  — pin a single key (replay-resistant) used for idempotency
//
// All counters share the FloatStore interface, which the auto-selector
// resolves to InMemoryStore or RedisStore at Setup time.
//
// Wire-level parity: the live parity key builders are in keys_python.go
// (OrgKey / UserKey / OrgPeriodKey), which produce the EXACT Python v0.5.x
// shapes and are what the engine uses (TASK P5.3). The scope-based Key /
// PeriodKey builders on the types below are the DEPRECATED pre-P5.3 shapes
// (finding QG-03) retained until D-13. See engine/goldenkey_parity_test.go
// for the cross-runtime gate.
package counters

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// FloatStore is the storage abstraction. Two implementations live in this
// package: InMemoryStore + RedisStore.
type FloatStore interface {
	IncrByFloat(ctx context.Context, key string, delta float64) (newValue float64, err error)
	GetFloat(ctx context.Context, key string) (value float64, found bool, err error)
	Set(ctx context.Context, key string, value float64, ttl time.Duration) error
	Delete(ctx context.Context, keys ...string) error
	Expire(ctx context.Context, key string, ttl time.Duration) error
	SetIfAbsent(ctx context.Context, key string, value string, ttl time.Duration) (set bool, err error)
	// DecrByFloorZero subtracts amount (a positive magnitude) and floors
	// the result at zero ATOMICALLY, returning the floored value. This is
	// the gauge-release invariant: a gauge must never go negative (a
	// double-fired release would otherwise manufacture free quota headroom
	// — finding QG-06). Mirrors Python gauge.py:79-81, but atomic rather
	// than the read-then-SET race Python carries (QI-02).
	DecrByFloorZero(ctx context.Context, key string, amount float64) (newValue float64, err error)
}

// KeyPrefix is the optional namespace placed at the head of every key.
// In multi-service Redis deployments, set this to a service-unique value
// (e.g. "ab0t-quota") to avoid collisions.
type KeyPrefix string

// Build composes a namespaced key. Empty prefix → no leading colon.
func (p KeyPrefix) Build(parts ...string) string {
	if p == "" {
		return strings.Join(parts, ":")
	}
	return string(p) + ":" + strings.Join(parts, ":")
}

// Counter is a per-period incrementing scalar (USD spend, API calls, etc.).
// Backed by a single Redis float; cleared via TTL.
//
// NOTE (QG-07): Python has NO plain "counter" type — only gauge,
// accumulator, and rate. This type is Go-only and is NOT part of the
// cross-runtime parity contract; the engine does not use it. Do not rely on
// its key shape matching any Python key.
type Counter struct {
	Store       FloatStore
	Prefix      KeyPrefix
	ResourceKey string
}

// PeriodKey returns the period's key for the given scope + period bucket.
// Scope is typically org_id or "user:{user_id}@{org_id}".
//
// NOTE (QG-07): Go-only shape quota:counter:{rk}:{scope}:{period}; NOT a
// Python parity key (Python has no plain counter). Not used by the engine.
func (c Counter) PeriodKey(scope, period string) string {
	return DeprecatedScopeKey(c.Prefix, "counter", c.ResourceKey, scope, period)
}

// Increment adds delta and returns the new value. delta may be negative.
func (c Counter) Increment(ctx context.Context, scope, period string, delta float64) (float64, error) {
	key := c.PeriodKey(scope, period)
	return c.Store.IncrByFloat(ctx, key, delta)
}

// Get returns the current value (0 if absent).
func (c Counter) Get(ctx context.Context, scope, period string) (float64, error) {
	v, _, err := c.Store.GetFloat(ctx, c.PeriodKey(scope, period))
	return v, err
}

// SetTTL applies the period TTL to the key. Idempotent.
func (c Counter) SetTTL(ctx context.Context, scope, period string, ttl time.Duration) error {
	return c.Store.Expire(ctx, c.PeriodKey(scope, period), ttl)
}

// Reset removes the counter for this period.
func (c Counter) Reset(ctx context.Context, scope, period string) error {
	return c.Store.Delete(ctx, c.PeriodKey(scope, period))
}

// CurrentPeriod returns the current bucket label for a ResetPeriod.
// Mirrors Python's bucket math; uses UTC.
func CurrentPeriod(reset config.ResetPeriod, now time.Time) string {
	n := now.UTC()
	switch reset {
	case config.ResetHourly:
		return n.Format("2006-01-02T15")
	case config.ResetDaily:
		return n.Format("2006-01-02")
	case config.ResetWeekly:
		y, w := n.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	case config.ResetMonthly:
		return n.Format("2006-01")
	}
	return "all"
}

// PeriodTTL returns the TTL for a period bucket: the period length plus a
// 1-day dashboard buffer, matching Python EXACTLY (accumulator.py:39-50).
// Parity is load-bearing (finding QG-05): a shorter Go TTL would expire a
// shared accumulator early mid-period → silent under-billing. Monthly uses
// Python's fixed 31-day base.
func PeriodTTL(reset config.ResetPeriod) time.Duration {
	const buffer = 86400 * time.Second // 1-day dashboard buffer
	switch reset {
	case config.ResetHourly:
		return 3600*time.Second + buffer // 25h
	case config.ResetDaily:
		return 86400*time.Second + buffer // 48h
	case config.ResetWeekly:
		return 604800*time.Second + buffer // 8d
	case config.ResetMonthly:
		return 2678400*time.Second + buffer // 31d + 1d = 32d
	}
	return 0
}
