// Package providers implements the tier resolution strategy.
//
// Three strategies (matches Python lib): jwt, mesh, static.
// All implement the Provider interface; the loader picks based on
// config.tier_provider.type.
package providers

import (
	"context"
	"errors"
	"fmt"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// Provider resolves user → tier_id.
type Provider interface {
	GetTier(ctx context.Context, userID, orgID string) (string, error)
}

// New picks the provider matching cfg.Type. Returns an actionable error
// when type is unknown — Setup logs the rejection.
func New(cfg config.TierProviderConfig) (Provider, error) {
	switch cfg.Type {
	case "jwt":
		return NewJWTProvider(cfg), nil
	case "static":
		return NewStaticProvider(cfg), nil
	case "mesh":
		return NewMeshProvider(cfg), nil
	case "":
		return nil, errors.New("tier_provider.type required (jwt | mesh | static)")
	}
	return nil, fmt.Errorf("tier_provider.type unknown: %q", cfg.Type)
}

// Errors common across providers.
var (
	ErrNoTier = errors.New("provider: no tier resolved")
)
