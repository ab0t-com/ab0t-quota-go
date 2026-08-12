package providers

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Cache wraps a Provider with an in-memory TTL cache keyed on user_id|org_id.
// Used to avoid hammering the mesh on every request.
//
// Beyond the short-lived TTL cache, Cache keeps a separate LAST-KNOWN-GOOD
// record (written only from a successful fetch) so that a transient lookup
// failure never downgrades a paying org to the fallback default. See GetTier.
// Ticket 20260810_tier_failclosed_lastknowngood (CLASS-1 DSD-01/02).
type Cache struct {
	inner    Provider
	ttl      time.Duration
	mu       sync.RWMutex
	items    map[string]cacheEntry
	lastGood map[string]lkgEntry
	// lkgMaxAge bounds how long a last-known-good value may be served during a
	// sustained outage. Zero (the default) = never expire — the strongest
	// never-downgrade guarantee, since a real downgrade always arrives as a
	// successful fetch that overwrites the record.
	lkgMaxAge time.Duration
	// durable is an OPTIONAL durable LKG store (see lkg_store.go — Redis,
	// wire-compatible with Python's `quota:tier:lkg:{org}` record) consulted
	// when the in-process map has no record, i.e. right after a restart: the
	// exact window where the in-memory-only guarantee used to be lost (the H8
	// parity gap, PROGRAM_FOLLOWUPS "Go last-known-good durability"). Nil =>
	// prior behavior, byte-identical.
	durable LKGStore
	// failureStreaks — RC4 detection parity with Python
	// (AuthServiceTierProvider._failure_streaks): consecutive per-key fetch
	// failures, reset on any success. LKG serving keeps customers safe but
	// must never make a sustained billing outage SILENT — at every
	// SustainedFailureThreshold multiple an ERROR-level
	// `tier_lookup_failing_sustained` line fires. Detection-only: never
	// changes what is served.
	failureStreaks map[string]int
	now            func() time.Time
}

// SustainedFailureThreshold — consecutive failures at which the sustained
// ERROR escalation fires (and re-fires at every multiple). Matches Python's
// AuthServiceTierProvider.SUSTAINED_FAILURE_THRESHOLD default. Var (not
// const) so tests can shrink it.
var SustainedFailureThreshold = 5

type cacheEntry struct {
	tier string
	exp  time.Time
}

// lkgEntry is the last successfully-fetched tier and when it was fetched.
type lkgEntry struct {
	tier string
	ts   time.Time
}

// WithCache wraps p with a TTL cache. ttl <= 0 disables caching.
func WithCache(p Provider, ttl time.Duration) *Cache {
	return &Cache{
		inner:          p,
		ttl:            ttl,
		items:          map[string]cacheEntry{},
		lastGood:       map[string]lkgEntry{},
		failureStreaks: map[string]int{},
		now:            time.Now,
	}
}

// ConsecutiveFailures — RC4 observability accessor: the current consecutive
// fetch-failure streak for (userID, orgID); 0 after any success.
func (c *Cache) ConsecutiveFailures(userID, orgID string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.failureStreaks[userID+"|"+orgID]
}

// noteFetchFailure bumps the streak and escalates at every threshold multiple.
func (c *Cache) noteFetchFailure(userID, orgID string, err error) int {
	threshold := SustainedFailureThreshold
	if threshold < 2 {
		threshold = 2
	}
	key := userID + "|" + orgID
	c.mu.Lock()
	c.failureStreaks[key]++
	streak := c.failureStreaks[key]
	c.mu.Unlock()
	if streak >= threshold && streak%threshold == 0 {
		slog.Error("tier_lookup_failing_sustained",
			"user", userID, "org", orgID, "consecutive_failures", streak,
			"error", err,
			"detail", "billing tier lookups failing repeatedly; last-known-good "+
				"is holding entitlements but the source outage needs attention (RC4)")
	}
	return streak
}

// SetClock overrides time for tests.
func (c *Cache) SetClock(fn func() time.Time) { c.now = fn }

// SetLastKnownGoodMaxAge bounds how long a last-known-good tier may be served
// during a sustained lookup outage. 0 (default) = never expire. Additive;
// callers that never set it keep the strongest never-downgrade guarantee.
func (c *Cache) SetLastKnownGoodMaxAge(d time.Duration) { c.lkgMaxAge = d }

// SetDurableLKGStore attaches a durable last-known-good store (normally
// NewRedisLKGStore, sharing Python's `quota:tier:lkg:{org}` record) so the
// never-downgrade guarantee survives process restarts. Additive and optional:
// never set => in-process-only LKG, exactly the prior behavior. quota.Setup
// wires this automatically when a durable Redis backend is configured.
func (c *Cache) SetDurableLKGStore(s LKGStore) {
	c.mu.Lock()
	c.durable = s
	c.mu.Unlock()
}

// GetTier checks the cache first; on miss, delegates and stashes.
//
// Failure handling (DSD-01/02): a fetch failure NEVER overwrites the good
// cached tier and NEVER caches the fallback default. If a last-known-good
// tier exists it is served (never a downgrade); otherwise the provider's
// fallback default is returned for THIS request only (uncached, so the next
// request retries) — matching the Python AuthServiceTierProvider.
func (c *Cache) GetTier(ctx context.Context, userID, orgID string) (string, error) {
	if c.ttl <= 0 {
		// Caching disabled ⇒ no last-known-good to hold. Still never surface a
		// fallback lookup failure as a hard error that could be mis-read: if
		// the provider supplied a fallback default alongside the error, serve
		// it uncached (parity with the no-history case below). A genuine
		// unresolved (no default) still propagates → engine fail-opens.
		tier, err := c.inner.GetTier(ctx, userID, orgID)
		if err != nil {
			c.noteFetchFailure(userID, orgID, err) // RC4 detection
			if errors.Is(err, ErrTierLookupFailed) && tier != "" {
				return tier, nil
			}
			return tier, err
		}
		c.resetFailureStreak(userID, orgID)
		return tier, nil
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
		c.noteFetchFailure(userID, orgID, err) // RC4 detection (serving unchanged)
		// Transient failure. Do NOT cache anything; serve last-known-good.
		if lg, hit := c.lookupLastGood(key); hit {
			slog.Warn("tier_fetch_failed_serving_last_known_good",
				"user", userID, "org", orgID, "tier", lg.tier,
				"age_seconds", c.now().Sub(lg.ts).Seconds(), "error", err)
			return lg.tier, nil
		}
		// H8 parity: the in-process map is empty (fresh restart) — consult the
		// durable store before falling to the default, exactly what the Python
		// provider's Redis LKG read does. A durable hit also warms the map so
		// subsequent failures stay on the fast path.
		if lg, hit := c.lookupDurableLastGood(ctx, orgID); hit {
			c.mu.Lock()
			c.lastGood[key] = lg
			c.mu.Unlock()
			slog.Warn("tier_fetch_failed_serving_durable_last_known_good",
				"user", userID, "org", orgID, "tier", lg.tier,
				"age_seconds", c.now().Sub(lg.ts).Seconds(), "error", err)
			return lg.tier, nil
		}
		// No history. If the provider handed back a fallback default alongside
		// the failure, use it for this request but do NOT cache it — matches
		// Python's "unknown/never-fetched org ⇒ default_tier, uncached".
		if errors.Is(err, ErrTierLookupFailed) && tier != "" {
			slog.Warn("tier_fetch_failed_no_last_known_good_using_default",
				"user", userID, "org", orgID, "tier", tier, "error", err)
			return tier, nil
		}
		// No default and no history → genuinely unresolved. Propagate; the
		// engine treats UnknownTier as fail-open (proceeds), never a downgrade.
		return tier, err
	}

	// Success — the ONLY path that advances the cache and the last-known-good
	// (an explicit, confirmed downgrade lands here too and correctly moves the
	// org down).
	now := c.now()
	c.mu.Lock()
	c.items[key] = cacheEntry{tier: tier, exp: now.Add(c.ttl)}
	c.lastGood[key] = lkgEntry{tier: tier, ts: now}
	delete(c.failureStreaks, key) // RC4: streak broken by success
	durable := c.durable
	c.mu.Unlock()
	if durable != nil {
		// Best-effort write-through so the record survives a restart (and is
		// visible to Python replicas sharing the same Redis).
		durable.SetLKG(ctx, orgID, tier, now)
	}
	return tier, nil
}

// resetFailureStreak clears the RC4 streak on success (ttl<=0 branch helper;
// the cached branch clears inline under the same lock as its cache write).
func (c *Cache) resetFailureStreak(userID, orgID string) {
	c.mu.Lock()
	delete(c.failureStreaks, userID+"|"+orgID)
	c.mu.Unlock()
}

// lookupDurableLastGood consults the optional durable store, applying the same
// lkgMaxAge bound as the in-memory record. ok=false when no store is wired.
func (c *Cache) lookupDurableLastGood(ctx context.Context, orgID string) (lkgEntry, bool) {
	c.mu.RLock()
	durable := c.durable
	c.mu.RUnlock()
	if durable == nil {
		return lkgEntry{}, false
	}
	tier, ts, ok := durable.GetLKG(ctx, orgID)
	if !ok || tier == "" {
		return lkgEntry{}, false
	}
	if c.lkgMaxAge > 0 && c.now().Sub(ts) > c.lkgMaxAge {
		return lkgEntry{}, false
	}
	return lkgEntry{tier: tier, ts: ts}, true
}

// lookupLastGood returns the last-known-good entry if present and not older
// than lkgMaxAge (0 = never expires).
func (c *Cache) lookupLastGood(key string) (lkgEntry, bool) {
	c.mu.RLock()
	lg, ok := c.lastGood[key]
	c.mu.RUnlock()
	if !ok {
		return lkgEntry{}, false
	}
	if c.lkgMaxAge > 0 && c.now().Sub(lg.ts) > c.lkgMaxAge {
		return lkgEntry{}, false
	}
	return lg, true
}

// Invalidate drops the fast cache entry for (userID, orgID). Useful when an
// auth-event hints that the tier changed.
//
// It intentionally leaves the last-known-good record intact: if the next
// re-fetch fails we still serve the last good tier rather than downgrading.
// An explicit tier change is picked up by the successful re-fetch, which
// overwrites both records.
func (c *Cache) Invalidate(userID, orgID string) {
	c.mu.Lock()
	delete(c.items, userID+"|"+orgID)
	c.mu.Unlock()
}
