package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

func makeJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(claims)
	encPayload := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + encPayload + ".sig"
}

func TestNew_DispatchesByType(t *testing.T) {
	tests := []struct {
		typ     string
		wantErr bool
	}{
		{"jwt", false},
		{"static", false},
		{"mesh", false},
		{"", true},
		{"weird", true},
	}
	for _, tc := range tests {
		_, err := New(config.TierProviderConfig{Type: tc.typ})
		if (err != nil) != tc.wantErr {
			t.Errorf("type=%q wantErr=%v got err=%v", tc.typ, tc.wantErr, err)
		}
	}
}

func TestJWTProvider_ExtractsClaim(t *testing.T) {
	p := NewJWTProvider(config.TierProviderConfig{JWTClaimKey: "tier"})
	tok := makeJWT(map[string]any{"tier": "pro"})
	got, err := p.GetTierFromToken(tok)
	if err != nil || got != "pro" {
		t.Errorf("got=%q err=%v", got, err)
	}
}

func TestJWTProvider_FallsBackToDefault(t *testing.T) {
	p := NewJWTProvider(config.TierProviderConfig{DefaultTier: "free"})
	got, err := p.GetTierFromToken("malformed")
	if err != nil || got != "free" {
		t.Errorf("got=%q err=%v", got, err)
	}
}

func TestJWTProvider_NoToken_NoDefault_Errors(t *testing.T) {
	p := NewJWTProvider(config.TierProviderConfig{})
	_, err := p.GetTier(context.Background(), "u", "o")
	if err == nil {
		t.Error("expected error with no token + no default")
	}
}

func TestJWTProvider_ContextToken(t *testing.T) {
	p := NewJWTProvider(config.TierProviderConfig{})
	tok := makeJWT(map[string]any{"tier": "enterprise"})
	ctx := WithToken(context.Background(), tok)
	got, _ := p.GetTier(ctx, "u", "o")
	if got != "enterprise" {
		t.Errorf("got %q", got)
	}
}

func TestStaticProvider(t *testing.T) {
	p := NewStaticProvider(config.TierProviderConfig{
		Mapping:     map[string]string{"alice": "pro"},
		DefaultTier: "free",
	})
	got, _ := p.GetTier(context.Background(), "alice", "")
	if got != "pro" {
		t.Errorf("got %q", got)
	}
	got, _ = p.GetTier(context.Background(), "bob", "")
	if got != "free" {
		t.Errorf("got %q", got)
	}
}

func TestMeshProvider_RequiresLookup(t *testing.T) {
	p := NewMeshProvider(config.TierProviderConfig{})
	_, err := p.GetTier(context.Background(), "u", "o")
	if err == nil {
		t.Error("expected error without lookup")
	}
}

func TestMeshProvider_DelegatesAndFallsBack(t *testing.T) {
	p := NewMeshProvider(config.TierProviderConfig{DefaultTier: "free"})
	p.SetLookup(func(ctx context.Context, userID, orgID string) (string, error) {
		if userID == "alice" {
			return "pro", nil
		}
		return "", errors.New("network down")
	})
	got, _ := p.GetTier(context.Background(), "alice", "")
	if got != "pro" {
		t.Errorf("got %q", got)
	}
	got, _ = p.GetTier(context.Background(), "bob", "")
	if got != "free" {
		t.Errorf("fallback failed: got %q", got)
	}
}

func TestCache_HitAndMissAndInvalidate(t *testing.T) {
	calls := 0
	inner := providerFunc(func(ctx context.Context, userID, orgID string) (string, error) {
		calls++
		return "pro", nil
	})
	c := WithCache(inner, time.Minute)
	got, _ := c.GetTier(context.Background(), "alice", "")
	if got != "pro" || calls != 1 {
		t.Fatalf("first: got=%q calls=%d", got, calls)
	}
	// Hit
	got, _ = c.GetTier(context.Background(), "alice", "")
	if calls != 1 {
		t.Errorf("expected cache hit, calls=%d", calls)
	}
	// Invalidate forces refetch
	c.Invalidate("alice", "")
	_, _ = c.GetTier(context.Background(), "alice", "")
	if calls != 2 {
		t.Errorf("expected refetch after invalidate, calls=%d", calls)
	}
}

func TestCache_Expiry(t *testing.T) {
	inner := providerFunc(func(ctx context.Context, userID, orgID string) (string, error) {
		return "pro", nil
	})
	c := WithCache(inner, 100*time.Millisecond)
	t0 := time.Now()
	c.SetClock(func() time.Time { return t0 })
	_, _ = c.GetTier(context.Background(), "alice", "")
	c.SetClock(func() time.Time { return t0.Add(200 * time.Millisecond) })
	calls := 0
	c.inner = providerFunc(func(ctx context.Context, _, _ string) (string, error) {
		calls++
		return "enterprise", nil
	})
	got, _ := c.GetTier(context.Background(), "alice", "")
	if calls != 1 {
		t.Errorf("expected refetch after expiry, calls=%d", calls)
	}
	if got != "enterprise" {
		t.Errorf("got %q", got)
	}
}

// providerFunc adapts a function to Provider for tests.
type providerFunc func(ctx context.Context, userID, orgID string) (string, error)

func (f providerFunc) GetTier(ctx context.Context, userID, orgID string) (string, error) {
	return f(ctx, userID, orgID)
}

// --- Ticket 20260810_tier_failclosed_lastknowngood (CLASS-1 DSD-01/02) ------
// A transient tier-lookup failure must never downgrade a paying org to the
// fallback default, and must never poison the cache with an invented tier.

// meshFor builds a MeshProvider whose lookup honors a togglable failure flag.
func meshFor(defaultTier string, good string, fail *bool) *MeshProvider {
	p := NewMeshProvider(config.TierProviderConfig{DefaultTier: defaultTier})
	p.SetLookup(func(ctx context.Context, userID, orgID string) (string, error) {
		if *fail {
			return "", errors.New("billing 503")
		}
		return good, nil
	})
	return p
}

func TestMeshProvider_PropagatesErrorWithFallback(t *testing.T) {
	// The bug was returning (DefaultTier, nil); the fix returns the default
	// AS A CANDIDATE but with a non-nil ErrTierLookupFailed so the cache knows
	// not to persist it.
	fail := true
	p := meshFor("free", "pro", &fail)
	tier, err := p.GetTier(context.Background(), "u", "o")
	if tier != "free" {
		t.Errorf("want fallback tier free, got %q", tier)
	}
	if err == nil || !errors.Is(err, ErrTierLookupFailed) {
		t.Errorf("want ErrTierLookupFailed, got %v", err)
	}
}

func TestCache_FetchFailsWithGoodCache_ServesLastKnownGood(t *testing.T) {
	fail := false
	c := WithCache(meshFor("free", "pro", &fail), 100*time.Millisecond)
	t0 := time.Now()
	c.SetClock(func() time.Time { return t0 })

	// 1) good fetch → pro; records last-known-good.
	if got, _ := c.GetTier(context.Background(), "u", "o"); got != "pro" {
		t.Fatalf("first fetch got %q", got)
	}
	// 2) expire the fast cache, then billing fails.
	c.SetClock(func() time.Time { return t0.Add(time.Second) })
	fail = true
	// 3) transient failure ⇒ serve last-known-good pro, NOT free.
	got, err := c.GetTier(context.Background(), "u", "o")
	if got != "pro" || err != nil {
		t.Fatalf("want last-known-good pro,nil; got %q,%v", got, err)
	}
	// 4) and the fast cache must NOT have been poisoned with free.
	c.mu.RLock()
	e, ok := c.items["u|o"]
	c.mu.RUnlock()
	if ok && e.tier == "free" {
		t.Errorf("cache was poisoned with fallback free: %+v", e)
	}
}

func TestCache_UnknownOrgFetchFails_ReturnsDefaultUncached(t *testing.T) {
	fail := true
	c := WithCache(meshFor("free", "pro", &fail), time.Minute)
	// Never-fetched org + failing source ⇒ default free, never blocked.
	got, err := c.GetTier(context.Background(), "u", "new")
	if got != "free" || err != nil {
		t.Fatalf("want free,nil; got %q,%v", got, err)
	}
	// Must NOT be cached (so recovery is instant).
	c.mu.RLock()
	_, cached := c.items["u|new"]
	_, lkg := c.lastGood["u|new"]
	c.mu.RUnlock()
	if cached {
		t.Error("fallback free was cached; must be uncached")
	}
	if lkg {
		t.Error("fallback free recorded as last-known-good; must not be")
	}
}

func TestCache_ExplicitDowngradeIsHonored(t *testing.T) {
	// A SUCCESSFUL fetch returning a lower tier is a real downgrade and must
	// advance both cache and last-known-good.
	tier := "pro"
	p := NewMeshProvider(config.TierProviderConfig{DefaultTier: "free"})
	p.SetLookup(func(ctx context.Context, _, _ string) (string, error) { return tier, nil })
	c := WithCache(p, 100*time.Millisecond)
	t0 := time.Now()
	c.SetClock(func() time.Time { return t0 })

	if got, _ := c.GetTier(context.Background(), "u", "o"); got != "pro" {
		t.Fatalf("got %q", got)
	}
	// expire cache; billing now confirms a downgrade to free.
	c.SetClock(func() time.Time { return t0.Add(time.Second) })
	tier = "free"
	if got, _ := c.GetTier(context.Background(), "u", "o"); got != "free" {
		t.Fatalf("explicit downgrade not honored, got %q", got)
	}
	c.mu.RLock()
	lg := c.lastGood["u|o"]
	c.mu.RUnlock()
	if lg.tier != "free" {
		t.Errorf("last-known-good should advance to confirmed free, got %q", lg.tier)
	}
}

func TestCache_SuccessUpdatesGoodCacheAndLKG(t *testing.T) {
	fail := false
	c := WithCache(meshFor("free", "starter", &fail), time.Minute)
	if got, _ := c.GetTier(context.Background(), "u", "o"); got != "starter" {
		t.Fatalf("got %q", got)
	}
	c.mu.RLock()
	e, okItem := c.items["u|o"]
	lg, okLKG := c.lastGood["u|o"]
	c.mu.RUnlock()
	if !okItem || e.tier != "starter" {
		t.Errorf("fast cache not updated: %+v ok=%v", e, okItem)
	}
	if !okLKG || lg.tier != "starter" {
		t.Errorf("last-known-good not recorded: %+v ok=%v", lg, okLKG)
	}
}

func TestCache_LastKnownGoodMaxAge_Expires(t *testing.T) {
	fail := false
	c := WithCache(meshFor("free", "pro", &fail), 10*time.Millisecond)
	c.SetLastKnownGoodMaxAge(time.Minute)
	t0 := time.Now()
	c.SetClock(func() time.Time { return t0 })
	_, _ = c.GetTier(context.Background(), "u", "o") // records lkg=pro@t0

	// Well beyond both the fast TTL and the lkg max-age, with billing down.
	c.SetClock(func() time.Time { return t0.Add(2 * time.Minute) })
	fail = true
	// lkg is now too old to serve ⇒ falls back to default free (uncached).
	got, err := c.GetTier(context.Background(), "u", "o")
	if got != "free" || err != nil {
		t.Fatalf("stale lkg should fall through to default free,nil; got %q,%v", got, err)
	}
}

func TestCache_Disabled_ServesFallbackUncached(t *testing.T) {
	// ttl<=0 disables caching; a fallback lookup failure should still surface
	// the default (never block) rather than a hard error.
	fail := true
	c := WithCache(meshFor("free", "pro", &fail), 0)
	got, err := c.GetTier(context.Background(), "u", "o")
	if got != "free" || err != nil {
		t.Fatalf("want free,nil with caching disabled; got %q,%v", got, err)
	}
}
