package providers

import (
	"context"
	"sync"
	"time"
)

// Cache wraps a Provider with an in-memory TTL cache keyed on user_id|org_id.
// Used to avoid hammering the mesh on every request.
type Cache struct {
	inner Provider
	ttl   time.Duration
	mu    sync.RWMutex
	items map[string]cacheEntry
	now   func() time.Time
}

type cacheEntry struct {
	tier string
	exp  time.Time
}

// WithCache wraps p with a TTL cache. ttl <= 0 disables caching.
func WithCache(p Provider, ttl time.Duration) *Cache {
	return &Cache{inner: p, ttl: ttl, items: map[string]cacheEntry{}, now: time.Now}
}

// SetClock overrides time for tests.
func (c *Cache) SetClock(fn func() time.Time) { c.now = fn }

// GetTier checks the cache first; on miss, delegates and stashes.
func (c *Cache) GetTier(ctx context.Context, userID, orgID string) (string, error) {
	if c.ttl <= 0 {
		return c.inner.GetTier(ctx, userID, orgID)
	}
	key := userID + "|" + orgID
	c.mu.RLock()
	e, ok := c.items[key]
	c.mu.RUnlock()
	if ok && c.now().Before(e.exp) {
		return e.tier, nil
	}
	tier, err := c.inner.GetTier(ctx, userID, orgID)
	if err != nil {
		return tier, err
	}
	c.mu.Lock()
	c.items[key] = cacheEntry{tier: tier, exp: c.now().Add(c.ttl)}
	c.mu.Unlock()
	return tier, nil
}

// Invalidate drops the cache entry for (userID, orgID). Useful when an
// auth-event hints that the tier changed.
func (c *Cache) Invalidate(userID, orgID string) {
	c.mu.Lock()
	delete(c.items, userID+"|"+orgID)
	c.mu.Unlock()
}
