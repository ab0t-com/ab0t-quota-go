package counters

import (
	"context"
	"errors"
	"time"
)

// ErrRedisNotAvailable signals that the Redis backend is not wired in this
// build. v0.1.0 ships in-memory only; v0.2 will wire go-redis. Surfaces
// from Setup with an actionable message.
var ErrRedisNotAvailable = errors.New("redis backend stub — wire-level implementation arrives in v0.2; use in-memory store for now")

// NewRedisStore returns a FloatStore backed by go-redis. v0.1.0 returns
// a typed error so callers can fall back to InMemoryStore.
func NewRedisStore(_ any) (FloatStore, error) {
	return nil, ErrRedisNotAvailable
}

// NewRedisRateStore returns a RateStore backed by go-redis. Stub until v0.2.
func NewRedisRateStore(_ any) (RateStore, error) {
	return nil, ErrRedisNotAvailable
}

// redisStoreStub is unused; defined to keep symbol stability for the v0.2
// wiring patch.
type redisStoreStub struct{}

func (redisStoreStub) IncrByFloat(_ context.Context, _ string, _ float64) (float64, error) {
	return 0, ErrRedisNotAvailable
}
func (redisStoreStub) GetFloat(_ context.Context, _ string) (float64, bool, error) {
	return 0, false, ErrRedisNotAvailable
}
func (redisStoreStub) Set(_ context.Context, _ string, _ float64, _ time.Duration) error {
	return ErrRedisNotAvailable
}
func (redisStoreStub) Delete(_ context.Context, _ ...string) error { return ErrRedisNotAvailable }
func (redisStoreStub) Expire(_ context.Context, _ string, _ time.Duration) error {
	return ErrRedisNotAvailable
}
func (redisStoreStub) SetIfAbsent(_ context.Context, _ string, _ string, _ time.Duration) (bool, error) {
	return false, ErrRedisNotAvailable
}
