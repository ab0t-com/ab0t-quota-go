package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// JWTProvider extracts the tier from a JWT claim. The token is supplied
// via context (set by the middleware) or via the explicit GetTierFromToken
// method.
//
// Wire-level parity: matches Python lib's jwt provider — same default
// claim key (`tier`), same fallback to default_tier.
type JWTProvider struct {
	ClaimKey    string
	DefaultTier string
}

// NewJWTProvider builds a JWTProvider.
func NewJWTProvider(cfg config.TierProviderConfig) *JWTProvider {
	key := cfg.JWTClaimKey
	if key == "" {
		key = "tier"
	}
	return &JWTProvider{ClaimKey: key, DefaultTier: cfg.DefaultTier}
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
	v, ok := claims[p.ClaimKey].(string)
	if !ok || v == "" {
		return p.fallback("claim missing")
	}
	return v, nil
}

func (p *JWTProvider) fallback(reason string) (string, error) {
	if p.DefaultTier != "" {
		return p.DefaultTier, nil
	}
	return "", errors.New("jwt provider: " + reason)
}
