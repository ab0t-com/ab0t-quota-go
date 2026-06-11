package providers

import (
	"context"
	"errors"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// MeshLookup is the function the consumer provides to do the mesh
// lookup. Most consumers will plug in their billing service client's
// GetUserTier method here.
type MeshLookup func(ctx context.Context, userID, orgID string) (string, error)

// MeshProvider delegates resolution to a consumer-supplied MeshLookup,
// optionally wrapped in a CacheLayer (constructed via WithCache).
type MeshProvider struct {
	Lookup      MeshLookup
	DefaultTier string
}

// NewMeshProvider builds a MeshProvider. Lookup is nil at config-load
// time; quota.Setup wires it via SetLookup before first request.
func NewMeshProvider(cfg config.TierProviderConfig) *MeshProvider {
	return &MeshProvider{DefaultTier: cfg.DefaultTier}
}

// SetLookup installs the mesh fetcher. Called by quota.Setup before the
// first request lands.
func (p *MeshProvider) SetLookup(fn MeshLookup) { p.Lookup = fn }

// GetTier delegates to the lookup; falls back to DefaultTier on errors.
func (p *MeshProvider) GetTier(ctx context.Context, userID, orgID string) (string, error) {
	if p.Lookup == nil {
		if p.DefaultTier != "" {
			return p.DefaultTier, nil
		}
		return "", errors.New("mesh provider: no Lookup wired (call SetLookup at startup)")
	}
	tier, err := p.Lookup(ctx, userID, orgID)
	if err != nil && p.DefaultTier != "" {
		return p.DefaultTier, nil
	}
	return tier, err
}
