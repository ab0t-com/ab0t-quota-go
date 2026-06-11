package counters

import (
	"context"
	"sync"
	"time"
)

// InMemoryStore is a process-local FloatStore. Used for tests and as the
// degraded fallback when no Redis is configured. Loud warning is emitted at
// quota.Setup; this package emits no logs (callers do).
type InMemoryStore struct {
	mu      sync.Mutex
	floats  map[string]float64
	strings map[string]string
	expiry  map[string]time.Time
	now     func() time.Time
}

// NewInMemoryStore returns an empty in-memory store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		floats:  map[string]float64{},
		strings: map[string]string{},
		expiry:  map[string]time.Time{},
		now:     time.Now,
	}
}

// SetClock overrides time.Now for deterministic tests.
func (s *InMemoryStore) SetClock(fn func() time.Time) { s.now = fn }

func (s *InMemoryStore) sweep() {
	now := s.now()
	for k, t := range s.expiry {
		if now.After(t) {
			delete(s.floats, k)
			delete(s.strings, k)
			delete(s.expiry, k)
		}
	}
}

func (s *InMemoryStore) IncrByFloat(_ context.Context, key string, delta float64) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	s.floats[key] += delta
	return s.floats[key], nil
}

func (s *InMemoryStore) GetFloat(_ context.Context, key string) (float64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	v, ok := s.floats[key]
	return v, ok, nil
}

func (s *InMemoryStore) Set(_ context.Context, key string, value float64, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.floats[key] = value
	if ttl > 0 {
		s.expiry[key] = s.now().Add(ttl)
	}
	return nil
}

func (s *InMemoryStore) Delete(_ context.Context, keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		delete(s.floats, k)
		delete(s.strings, k)
		delete(s.expiry, k)
	}
	return nil
}

func (s *InMemoryStore) Expire(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ttl <= 0 {
		delete(s.expiry, key)
		return nil
	}
	s.expiry[key] = s.now().Add(ttl)
	return nil
}

func (s *InMemoryStore) SetIfAbsent(_ context.Context, key, value string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	if _, exists := s.strings[key]; exists {
		return false, nil
	}
	s.strings[key] = value
	if ttl > 0 {
		s.expiry[key] = s.now().Add(ttl)
	}
	return true, nil
}
