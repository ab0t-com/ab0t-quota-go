package counters

import (
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// Factory bundles a FloatStore + RateStore + KeyPrefix and constructs
// per-resource counters on demand. Used by the engine.
type Factory struct {
	Floats FloatStore
	Rates  RateStore
	Prefix KeyPrefix
}

// NewMemoryFactory returns a Factory backed by in-memory stores. Test
// fallback or degraded operation.
func NewMemoryFactory(prefix KeyPrefix) *Factory {
	return &Factory{
		Floats: NewInMemoryStore(),
		Rates:  NewMemoryRateStore(),
		Prefix: prefix,
	}
}

// NewRedisFactory returns a Factory backed by durable go-redis stores
// (TASK P5.1). client is a *redis.Client (or any redis.Cmdable). Returns
// ErrRedisNotAvailable when client is nil so the caller degrades loudly to
// NewMemoryFactory rather than silently.
func NewRedisFactory(client redis.Cmdable, prefix KeyPrefix) (*Factory, error) {
	floats, err := NewRedisStore(client)
	if err != nil {
		return nil, err
	}
	rates, err := NewRedisRateStore(client)
	if err != nil {
		return nil, err
	}
	return &Factory{Floats: floats, Rates: rates, Prefix: prefix}, nil
}

// Counter returns a Counter for resource. Use only when you want a plain
// scalar — for per-period or sliding-window semantics, use Accumulator or
// Rate.
func (f *Factory) Counter(resourceKey string) Counter {
	return Counter{Store: f.Floats, Prefix: f.Prefix, ResourceKey: resourceKey}
}

// Gauge returns a Gauge for resource.
func (f *Factory) Gauge(resourceKey string) Gauge {
	return Gauge{Store: f.Floats, Prefix: f.Prefix, ResourceKey: resourceKey}
}

// Accumulator returns an Accumulator for resource. reset must be a valid
// ResetPeriod from the resource def.
func (f *Factory) Accumulator(resourceKey string, reset config.ResetPeriod) Accumulator {
	return Accumulator{Store: f.Floats, Prefix: f.Prefix, ResourceKey: resourceKey, Reset: reset}
}

// Rate returns a Rate for resource.
func (f *Factory) Rate(r config.ResourceDef) Rate {
	return Rate{
		Store:       f.Rates,
		Prefix:      f.Prefix,
		ResourceKey: r.ResourceKey,
		Window:      time.Duration(r.WindowSeconds) * time.Second,
	}
}

// Idempotency returns the IdempotencyStore.
func (f *Factory) Idempotency() IdempotencyStore {
	return IdempotencyStore{Store: f.Floats, Prefix: f.Prefix}
}
