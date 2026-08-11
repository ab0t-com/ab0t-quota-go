package providers

import (
	"context"
	"errors"
	"fmt"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// ErrTierLookupFailed wraps a mesh lookup failure. GetTier returns it
// ALONGSIDE the DefaultTier so the cache layer can distinguish "this value is
// a fallback default — do not persist it, serve last-known-good instead" from
// a real successful fetch.
//
// Ticket 20260810_tier_failclosed_lastknowngood (CLASS-1 DSD-01/02): the old
// code swallowed the lookup error to nil and returned (DefaultTier, nil),
// which the cache then happily persisted — enforcing a paying org at the
// cheapest tier for the whole TTL window. The error MUST propagate so the
// invented tier is never cached and a paying org is never downgraded on a
// transient billing blip.
var ErrTierLookupFailed = errors.New("mesh: tier lookup failed")

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

// GetTier delegates to the lookup. On a lookup failure it returns the
// DefaultTier as a *fallback candidate* but ALSO propagates a non-nil error
// (wrapping ErrTierLookupFailed). It never silently swallows the error to nil.
//
// The paired cache (WithCache) is what turns this into the safe behavior:
// it serves last-known-good on failure and refuses to persist the fallback.
// A MeshProvider used WITHOUT a cache simply surfaces the error to the engine,
// which fail-opens (UnknownTier proceeds) rather than enforcing the invented
// cheapest tier — still the safe direction for a paying customer.
func (p *MeshProvider) GetTier(ctx context.Context, userID, orgID string) (string, error) {
	if p.Lookup == nil {
		if p.DefaultTier != "" {
			return p.DefaultTier, nil
		}
		return "", errors.New("mesh provider: no Lookup wired (call SetLookup at startup)")
	}
	tier, err := p.Lookup(ctx, userID, orgID)
	if err != nil {
		// DSD-01/02: never return (DefaultTier, nil) — that invented tier is
		// indistinguishable from a real assignment and poisons the cache.
		return p.DefaultTier, fmt.Errorf("%w: %v", ErrTierLookupFailed, err)
	}
	return tier, nil
}
