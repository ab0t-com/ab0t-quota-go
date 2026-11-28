package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// DefaultJWTClaimKey (v0.3.2) is the claim both libraries read the tier from when
// tier_provider.jwt_claim_key is not set — the Python library's default (testdata/
// money_safety_golden_20261128.json "jwt_claim"). Before v0.3.2 this library read "tier", so one
// token gave two tiers in a mixed fleet.
const DefaultJWTClaimKey = "org_tier"

// LegacyJWTClaimKey is this library's pre-v0.3.2 default claim. With no jwt_claim_key it is still
// read, after the canonical claim, and — when the provider knows the config's tiers — only when its
// value names one of them.
const LegacyJWTClaimKey = "tier"

// JWTProvider extracts the tier from a JWT claim. The token is supplied
// via context (set by the middleware) or via the explicit GetTierFromToken
// method.
//
// Needs a token issuer that puts the claim in the token: tokens issued by the ab0t auth service
// carry NO tier claim — for those use the mesh provider (billing's tier for the org).
type JWTProvider struct {
	ClaimKey    string
	DefaultTier string
	// LegacyClaimKeys (v0.3.2) are read when ClaimKey is absent: [LegacyJWTClaimKey] when the config
	// set no jwt_claim_key, so a token that worked with an earlier release resolves the same.
	LegacyClaimKeys []string
	// KnownTiers (v0.3.2) — the config's tier ids (quota.Setup fills it). When set, a legacy claim
	// counts only if it names one of them (a "tier" claim that means something else is ignored);
	// nil = the pre-v0.3.2 behaviour (the legacy claim's value is used as given).
	KnownTiers map[string]bool

	warnOnce sync.Once
}

// NewJWTProvider builds a JWTProvider.
func NewJWTProvider(cfg config.TierProviderConfig) *JWTProvider {
	if cfg.JWTClaimKey != "" {
		return &JWTProvider{ClaimKey: cfg.JWTClaimKey, DefaultTier: cfg.DefaultTier}
	}
	return &JWTProvider{ClaimKey: DefaultJWTClaimKey, DefaultTier: cfg.DefaultTier,
		LegacyClaimKeys: []string{LegacyJWTClaimKey}}
}

// ResolveJWTTier (v0.3.2) is the pure claim rule both libraries share: the claim's value (a non-empty
// string), else the first legacy claim whose value names a known tier (any value when known is nil),
// else defaultTier. source = "claim" | "legacy" | "default".
func ResolveJWTTier(claims map[string]any, claimKey, defaultTier string, legacy []string, known map[string]bool) (tier, source string) {
	if v, ok := claims[claimKey].(string); ok && v != "" {
		return v, "claim"
	}
	for _, k := range legacy {
		v, ok := claims[k].(string)
		if !ok || v == "" {
			continue
		}
		if known == nil || known[v] {
			return v, "legacy"
		}
	}
	return defaultTier, "default"
}

// JWTStartupWarning is the text both libraries print at startup for tier_provider "jwt".
func JWTStartupWarning(claim, defaultTier string) string {
	return `tier_provider "jwt" reads the token claim "` + claim + `": tokens issued by the ab0t auth service ` +
		`(https://auth.service.ab0t.com) carry no tier claim, so unless your own token issuer adds "` + claim +
		`" every customer resolves to default_tier "` + defaultTier + `" — for ab0t-issued tokens use ` +
		`"tier_provider": {"type": "mesh"} (the tier billing holds for the org)`
}

// JWTMissingClaimWarning is the text both libraries print (once per process) the first time a token
// lacks the claim.
func JWTMissingClaimWarning(claim, defaultTier string) string {
	return `tier_provider "jwt": a token carried no "` + claim + `" claim — this customer resolved to ` +
		`default_tier "` + defaultTier + `" (logged once per process)`
}

// jwtTokenKey is the context key for the bearer token.
type jwtTokenKey struct{}

// WithToken stashes the bearer token in ctx for later GetTier calls.
// The middleware calls this on each request.
func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, jwtTokenKey{}, token)
}

// TokenFrom returns the bearer token if set.
func TokenFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(jwtTokenKey{}).(string)
	return v, ok && v != ""
}

// GetTier reads the JWT from ctx and extracts ClaimKey. Returns
// DefaultTier if absent.
func (p *JWTProvider) GetTier(ctx context.Context, _, _ string) (string, error) {
	tok, ok := TokenFrom(ctx)
	if !ok {
		if p.DefaultTier != "" {
			return p.DefaultTier, nil
		}
		return "", errors.New("jwt provider: no token in context")
	}
	return p.GetTierFromToken(tok)
}

// GetTierFromToken parses the JWT and pulls ClaimKey. Validation of the
// signature is NOT the lib's job — middleware enforces auth. Here we only
// decode the payload.
func (p *JWTProvider) GetTierFromToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return p.fallback("malformed jwt")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// some publishers use padded base64
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return p.fallback("undecodable jwt payload")
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return p.fallback("non-json jwt payload")
	}
	tier, source := ResolveJWTTier(claims, p.ClaimKey, "", p.LegacyClaimKeys, p.KnownTiers)
	if source == "default" {
		p.warnOnce.Do(func() { slog.Warn(JWTMissingClaimWarning(p.ClaimKey, p.DefaultTier)) })
		return p.fallback("claim missing")
	}
	return tier, nil
}

func (p *JWTProvider) fallback(reason string) (string, error) {
	if p.DefaultTier != "" {
		return p.DefaultTier, nil
	}
	return "", errors.New("jwt provider: " + reason)
}
