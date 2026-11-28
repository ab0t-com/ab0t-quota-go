package providers

// v0.3.2 — behaviour pin that uses only the pre-v0.3.2 API (it compiles against v0.3.1 and goes RED there
// by assertion): with no jwt_claim_key the tier is read from "org_tier" (the Python default), and the old
// "tier" claim still works.
// Run: go test ./providers -run NeverAstrayBehaviour -count=1

import (
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

func TestNeverAstrayBehaviour_OneDefaultClaimInBothLibraries_20261128(t *testing.T) {
	p := NewJWTProvider(config.TierProviderConfig{DefaultTier: "free"})
	if got, _ := p.GetTierFromToken(makeJWT(map[string]any{"org_tier": "pro"})); got != "pro" {
		t.Fatalf("org_tier claim → %q, want pro (the Python library reads org_tier: one token, one tier)", got)
	}
	// CONTROL: the old default claim still resolves (no token that worked stops working).
	if got, _ := p.GetTierFromToken(makeJWT(map[string]any{"tier": "pro"})); got != "pro" {
		t.Fatalf("tier claim → %q, want pro", got)
	}
}
