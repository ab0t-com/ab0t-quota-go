// Package registry holds the in-process index of ResourceDefs + tier
// limits. Populated by quota.Setup from a *config.Config; engine reads
// from it on every check.
package registry

import (
	"fmt"
	"sync"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// Registry is a thread-safe view onto a parsed config. The engine asks it
// "give me the resource def for `sandbox.concurrent`" and "give me the
// per-tier limit for tier=pro resource=sandbox.concurrent".
type Registry struct {
	mu        sync.RWMutex
	resources map[string]config.ResourceDef // resource_key
	tiers     map[string]config.Tier        // tier_id
	bundles   map[string][]string
}

// New builds a Registry from a parsed (validated) Config.
func New(cfg *config.Config) *Registry {
	r := &Registry{
		resources: make(map[string]config.ResourceDef, len(cfg.Resources)),
		tiers:     make(map[string]config.Tier, len(cfg.Tiers)),
		bundles:   make(map[string][]string, len(cfg.ResourceBundles)),
	}
	for _, res := range cfg.Resources {
		r.resources[res.ResourceKey] = res
	}
	for _, tier := range cfg.Tiers {
		r.tiers[tier.TierID] = tier
	}
	for k, v := range cfg.ResourceBundles {
		r.bundles[k] = append([]string(nil), v...)
	}
	return r
}

// Resource returns the ResourceDef or false if unknown.
func (r *Registry) Resource(key string) (config.ResourceDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.resources[key]
	return v, ok
}

// MustResource returns the ResourceDef or panics — for engine code paths
// where the key is already validated.
func (r *Registry) MustResource(key string) config.ResourceDef {
	v, ok := r.Resource(key)
	if !ok {
		panic(fmt.Sprintf("registry: resource_key %q unknown", key))
	}
	return v
}

// Tier returns the Tier or false.
func (r *Registry) Tier(id string) (config.Tier, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.tiers[id]
	return v, ok
}

// TierLimit returns the tier-level limit for one resource. Unknown tier
// or resource → zero-value TierLimit (effectively unlimited) and false.
func (r *Registry) TierLimit(tierID, resourceKey string) (config.TierLimit, bool) {
	t, ok := r.Tier(tierID)
	if !ok {
		return config.TierLimit{}, false
	}
	lim, ok := t.Limits[resourceKey]
	return lim, ok
}

// Bundle returns the resource keys in a bundle, or nil if unknown.
func (r *Registry) Bundle(name string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.bundles[name]
	if src == nil {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// AllResources returns every registered ResourceDef. Stable order (insertion).
func (r *Registry) AllResources() []config.ResourceDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]config.ResourceDef, 0, len(r.resources))
	for _, v := range r.resources {
		out = append(out, v)
	}
	return out
}

// AllTiers returns every registered Tier.
func (r *Registry) AllTiers() []config.Tier {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]config.Tier, 0, len(r.tiers))
	for _, v := range r.tiers {
		out = append(out, v)
	}
	return out
}
