package providers

// v0.3.2 — one default JWT claim in both libraries (org_tier, the Python default; this library read
// "tier"), the old name read only when no jwt_claim_key is set and — when the tiers are known — only
// when it names one of them. Shared vectors: testdata/money_safety_golden_20261128.json "jwt_claim".
// RED before v0.3.2: ResolveJWTTier / DefaultJWTClaimKey / the shared texts do not exist, and a token
// carrying only org_tier resolved to default_tier.
// Run: go test ./providers -run NeverAstray -count=1

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

type jwtGolden struct {
	JWTClaim struct {
		Default     string `json:"default"`
		LegacyAlias string `json:"legacy_alias"`
		Cases       []struct {
			Name        string         `json:"name"`
			ClaimKey    *string        `json:"claim_key"`
			Claims      map[string]any `json:"claims"`
			Tiers       []string       `json:"tiers"`
			DefaultTier string         `json:"default_tier"`
			Tier        string         `json:"tier"`
		} `json:"cases"`
	} `json:"jwt_claim"`
	Startup string `json:"jwt_startup_warning_template"`
	Missing string `json:"jwt_missing_claim_warning_template"`
}

func loadJWTGolden(t *testing.T) jwtGolden {
	t.Helper()
	raw, err := os.ReadFile("../testdata/money_safety_golden_20261128.json")
	if err != nil {
		t.Fatal(err)
	}
	var g jwtGolden
	if err := json.Unmarshal(raw, &g); err != nil || len(g.JWTClaim.Cases) == 0 {
		t.Fatalf("golden: %v", err)
	}
	return g
}

func TestNeverAstray_JWTClaimCasesMatchTheSharedVectors_20261128(t *testing.T) {
	g := loadJWTGolden(t)
	if DefaultJWTClaimKey != g.JWTClaim.Default || LegacyJWTClaimKey != g.JWTClaim.LegacyAlias {
		t.Fatalf("claims %q/%q, golden %q/%q", DefaultJWTClaimKey, LegacyJWTClaimKey, g.JWTClaim.Default, g.JWTClaim.LegacyAlias)
	}
	for _, c := range g.JWTClaim.Cases {
		t.Run(c.Name, func(t *testing.T) {
			key, legacy := g.JWTClaim.Default, []string{g.JWTClaim.LegacyAlias}
			if c.ClaimKey != nil {
				key, legacy = *c.ClaimKey, nil
			}
			known := map[string]bool{}
			for _, tr := range c.Tiers {
				known[tr] = true
			}
			if got, _ := ResolveJWTTier(c.Claims, key, c.DefaultTier, legacy, known); got != c.Tier {
				t.Fatalf("tier %q, want %q", got, c.Tier)
			}
		})
	}
}

func TestNeverAstray_DefaultProviderReadsOrgTier_20261128(t *testing.T) {
	p := NewJWTProvider(config.TierProviderConfig{DefaultTier: "free"})
	if got, _ := p.GetTierFromToken(makeJWT(map[string]any{"org_tier": "pro"})); got != "pro" {
		t.Fatalf("org_tier claim → %q, want pro", got)
	}
	// The old default still resolves (no KnownTiers: the pre-v0.3.2 behaviour).
	if got, _ := p.GetTierFromToken(makeJWT(map[string]any{"tier": "pro"})); got != "pro" {
		t.Fatalf("tier claim → %q, want pro", got)
	}
	// With the config's tiers known (quota.Setup), a "tier" claim naming none of them is ignored.
	p.KnownTiers = map[string]bool{"free": true, "pro": true}
	if got, _ := p.GetTierFromToken(makeJWT(map[string]any{"tier": "gold"})); got != "free" {
		t.Fatalf("unknown legacy claim → %q, want the default", got)
	}
	// CONTROL: an explicit key reads only that claim.
	explicit := NewJWTProvider(config.TierProviderConfig{JWTClaimKey: "plan", DefaultTier: "free"})
	if got, _ := explicit.GetTierFromToken(makeJWT(map[string]any{"org_tier": "pro", "tier": "pro"})); got != "free" {
		t.Fatalf("explicit key → %q, want free", got)
	}
}

func TestNeverAstray_JWTTextsAreTheSharedOnes_20261128(t *testing.T) {
	g := loadJWTGolden(t)
	r := strings.NewReplacer("{claim}", "org_tier", "{default_tier}", "free")
	if got := JWTStartupWarning("org_tier", "free"); got != r.Replace(g.Startup) {
		t.Fatalf("startup text\n go: %s\npy: %s", got, r.Replace(g.Startup))
	}
	if got := JWTMissingClaimWarning("org_tier", "free"); got != r.Replace(g.Missing) {
		t.Fatalf("missing-claim text\n go: %s\npy: %s", got, r.Replace(g.Missing))
	}
}
