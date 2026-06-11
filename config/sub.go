package config

// One-file home for the smaller sub-structs to keep file count manageable.

// StorageConfig holds Redis + DDB knobs.
type StorageConfig struct {
	RedisURL                       string `json:"redis_url,omitempty"`
	RedisKeyPrefix                 string `json:"redis_key_prefix,omitempty"`
	RedisPassword                  string `json:"redis_password,omitempty"`
	DynamoDBTable                  string `json:"dynamodb_table,omitempty"`
	DynamoDBRegion                 string `json:"dynamodb_region,omitempty"`
	PersistenceEnabled             *bool  `json:"persistence_enabled,omitempty"`
	PersistenceSyncIntervalSeconds int    `json:"persistence_sync_interval_seconds,omitempty"`
}

// EnforcementConfig governs runtime enforcement behavior.
//
// shadow_mode and global_kill_switch are present in some real configs but
// the Python lib never reads them (Known Upstream Bug #4). The Go port
// honors shadow_mode (check-and-log-but-allow).
type EnforcementConfig struct {
	Enabled          bool `json:"enabled"`
	ShadowMode       bool `json:"shadow_mode,omitempty"`
	GlobalKillSwitch bool `json:"global_kill_switch,omitempty"`
}

// AlertsConfig configures the AlertManager.
type AlertsConfig struct {
	Enabled          bool   `json:"enabled,omitempty"`
	CooldownSeconds  int    `json:"cooldown_seconds,omitempty"`  // default 3600
	WebhookURL       string `json:"webhook_url,omitempty"`
	WarningThreshold float64 `json:"warning_threshold,omitempty"`  // default 0.80
	CriticalThreshold float64 `json:"critical_threshold,omitempty"` // default 0.95
}

// TierProviderConfig selects which provider strategy to use.
type TierProviderConfig struct {
	Type        string            `json:"type"` // jwt | mesh | static
	JWTClaimKey string            `json:"jwt_claim_key,omitempty"`
	DefaultTier string            `json:"default_tier,omitempty"`
	Mapping     map[string]string `json:"mapping,omitempty"` // for static
	CacheTTLSec int               `json:"cache_ttl_seconds,omitempty"`
}

// BillingIntegrationConfig wires plan/price → tier mappings.
type BillingIntegrationConfig struct {
	PlanToTier        map[string]string `json:"plan_to_tier,omitempty"`
	StripePriceToTier map[string]string `json:"stripe_price_to_tier,omitempty"`
}

// ReconciliationConfig: reserved for future use.
type ReconciliationConfig struct {
	Enabled         bool `json:"enabled,omitempty"`
	IntervalSeconds int  `json:"interval_seconds,omitempty"`
}
