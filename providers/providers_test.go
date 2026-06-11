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
