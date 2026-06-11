package providers

import (
	"context"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// StaticProvider resolves tiers via a static user_id → tier_id map.
// Falls back to DefaultTier. Useful for tests + small fixed user lists.
type StaticProvider struct {
	Mapping     map[string]string
	DefaultTier string
}

// NewStaticProvider builds a StaticProvider.
func NewStaticProvider(cfg config.TierProviderConfig) *StaticProvider {
	return &StaticProvider{Mapping: cfg.Mapping, DefaultTier: cfg.DefaultTier}
}

// GetTier returns the mapped tier or DefaultTier.
func (p *StaticProvider) GetTier(_ context.Context, userID, _ string) (string, error) {
	if v, ok := p.Mapping[userID]; ok && v != "" {
		return v, nil
	}
	if p.DefaultTier != "" {
		return p.DefaultTier, nil
	}
	return "", ErrNoTier
}
