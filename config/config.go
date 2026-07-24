package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the root quota-config.json shape.
//
// Unknown top-level keys are preserved in Extra for forward-compat
// (PRODUCT_SPEC §13.6). `$`-prefixed keys (e.g. `$comment_*`) are
// always ignored.
type Config struct {
	Schema             string                   `json:"$schema,omitempty"`
	ServiceName        string                   `json:"service_name,omitempty"`
	EngineMode         string                   `json:"engine_mode,omitempty"` // local | byo_redis (alias) | bridge (rejected)
	Tiers              []Tier                   `json:"tiers"`
	Resources          []ResourceDef            `json:"resources"`
	ResourceBundles    map[string][]string      `json:"resource_bundles,omitempty"`
	TierProvider       TierProviderConfig       `json:"tier_provider"`
	Storage            StorageConfig            `json:"storage"`
	Alerts             AlertsConfig             `json:"alerts,omitempty"`
	Enforcement        EnforcementConfig        `json:"enforcement"`
	BillingIntegration BillingIntegrationConfig `json:"billing_integration,omitempty"`
	Billing            BillingConfig            `json:"billing,omitempty"`
	Outbox             OutboxConfig             `json:"outbox,omitempty"`
	Reconciliation     ReconciliationConfig     `json:"reconciliation,omitempty"`
	Pricing            json.RawMessage          `json:"pricing,omitempty"`      // out of scope for v0.1.0; raw for forward-compat
	BridgeCache        json.RawMessage          `json:"bridge_cache,omitempty"` // out of scope; raw

	// Extra captures unknown top-level keys. Never used at runtime; exists
	// so a Python schema addition doesn't break Go consumers.
	Extra map[string]json.RawMessage `json:"-"`
}

// TierByID returns the tier with the given id, or nil if absent.
func (c *Config) TierByID(id string) *Tier {
	for i := range c.Tiers {
		if c.Tiers[i].TierID == id {
			return &c.Tiers[i]
		}
	}
	return nil
}

// Validate runs all cross-field validators in order. Honors the
// experimental-billing-models gate.
func (c *Config) Validate() error {
	experimental := envBool("AB0T_QUOTA_ALLOW_EXPERIMENTAL_BILLING_MODELS", false)

	if c.EngineMode != "" && c.EngineMode != "local" && c.EngineMode != "byo_redis" && c.EngineMode != "bridge" {
		// unknown → warn-and-fall-back; bridge is rejected
		c.EngineMode = "local"
	}
	if c.EngineMode == "bridge" {
		return fmt.Errorf("config: engine_mode %q is out of scope for v0.1.0", c.EngineMode)
	}

	// K-1 (ST-KEYSPACE-1): only defined keyspace versions — an unknown version
	// silently falling back to a shape would orphan every live counter.
	if v := c.Storage.KeyspaceVersion; v != 0 && v != 1 && v != 2 {
		return fmt.Errorf("config: storage.keyspace_version must be 1 or 2 (got %d) — "+
			"an unknown keyspace version would orphan every live counter", v)
	}
	if (c.Storage.KeyspaceVersion == 2 || c.Storage.KeyspaceDualWrite) && c.ServiceName == "" {
		return fmt.Errorf("config: keyspace v2 keys carry a service segment — declare " +
			"service_name before storage.keyspace_version=2 or keyspace_dual_write")
	}
	// K-8: keyspace_version/keyspace_dual_write are now CONSUMED by Setup
	// (engine + counters route through counters.Keyspace; boot guards
	// QUOTA-CFG-011/012 run via counters.CheckBootKeyspace). D-KS-8's
	// declared-but-unwired refusal is lifted by the change that wired it.

	if len(c.Tiers) == 0 {
		return fmt.Errorf("config: tiers[] is required")
	}
	seenTier := map[string]bool{}
	for i := range c.Tiers {
		if err := c.Tiers[i].Validate(experimental); err != nil {
			return err
		}
		if seenTier[c.Tiers[i].TierID] {
			return fmt.Errorf("config: duplicate tier_id %q", c.Tiers[i].TierID)
		}
		seenTier[c.Tiers[i].TierID] = true
	}

	seenRes := map[string]bool{}
	for i := range c.Resources {
		if err := c.Resources[i].Validate(); err != nil {
			return err
		}
		if seenRes[c.Resources[i].ResourceKey] {
			return fmt.Errorf("config: duplicate resource_key %q", c.Resources[i].ResourceKey)
		}
		seenRes[c.Resources[i].ResourceKey] = true
	}

	for bundle, keys := range c.ResourceBundles {
		for _, k := range keys {
			if !seenRes[k] {
				return fmt.Errorf("config: resource_bundle %q references unknown resource_key %q", bundle, k)
			}
		}
	}
	return nil
}

func envBool(name string, def bool) bool {
	v := os.Getenv(name)
	switch v {
	case "1", "true", "True", "TRUE", "yes", "YES":
		return true
	case "0", "false", "False", "FALSE", "no", "NO":
		return false
	}
	return def
}
