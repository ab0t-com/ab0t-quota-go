package counters

import (
	"context"
	"sync"
	"time"
)

// Rate is a sliding-window counter. Backed by a sorted set in Redis
// (timestamp → unique id) or an in-process []int64 in memory.
//
// Wire-level: Python lib uses a Redis ZSET keyed on
// `quota:rate:{resource}:{scope}` with member = timestamp_ns string and
// score = timestamp_ns float. The Go port matches.
type Rate struct {
	Store       RateStore
	Prefix      KeyPrefix
	ResourceKey string
	Window      time.Duration
}

// RateStore is a small interface for sliding-window ops. Backed by Redis
// ZSETs in production; the in-memory implementation lives below.
type RateStore interface {
	// Record adds a timestamp + member; trims everything older than (now-window).
	Record(ctx context.Context, key string, now time.Time, window time.Duration, member string) error
	// Count returns the number of entries within the window ending at now.
	Count(ctx context.Context, key string, now time.Time, window time.Duration) (int64, error)
	// Trim removes everything older than (now-window). Optional optimization.
	Trim(ctx context.Context, key string, now time.Time, window time.Duration) error
}

// Key returns the rate counter's key.
func (r Rate) Key(scope string) string {
	return r.Prefix.Build("rate", r.ResourceKey, scope)
}

// Record adds one event to the window.
func (r Rate) Record(ctx context.Context, scope string, now time.Time, member string) error {
	return r.Store.Record(ctx, r.Key(scope), now, r.Window, member)
}

// Count returns the in-window count.
func (r Rate) Count(ctx context.Context, scope string, now time.Time) (int64, error) {
	return r.Store.Count(ctx, r.Key(scope), now, r.Window)
}

// MemoryRateStore is an in-process RateStore. Used for tests and degraded
// fallback.
type MemoryRateStore struct {
	mu      sync.Mutex
	entries map[string][]int64 // key -> unix nano timestamps
}

// NewMemoryRateStore returns an empty in-memory rate store.
func NewMemoryRateStore() *MemoryRateStore {
	return &MemoryRateStore{entries: map[string][]int64{}}
}

func (s *MemoryRateStore) trim(key string, now time.Time, window time.Duration) {
	cutoff := now.Add(-window).UnixNano()
	list := s.entries[key]
	keep := list[:0]
	for _, t := range list {
		if t >= cutoff {
			keep = append(keep, t)
		}
	}
	s.entries[key] = keep
}

func (s *MemoryRateStore) Record(_ context.Context, key string, now time.Time, window time.Duration, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = append(s.entries[key], now.UnixNano())
	s.trim(key, now, window)
	return nil
}

func (s *MemoryRateStore) Count(_ context.Context, key string, now time.Time, window time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trim(key, now, window)
	return int64(len(s.entries[key])), nil
}

func (s *MemoryRateStore) Trim(_ context.Context, key string, now time.Time, window time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trim(key, now, window)
	return nil
}
