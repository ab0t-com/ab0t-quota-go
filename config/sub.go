package config

// One-file home for the smaller sub-structs to keep file count manageable.

// StorageConfig holds Redis + DDB knobs.
type StorageConfig struct {
	RedisURL       string `json:"redis_url,omitempty"`
	RedisKeyPrefix string `json:"redis_key_prefix,omitempty"`
	RedisPassword  string `json:"redis_password,omitempty"`
	// RedisClusterConfirmedDisabled is the operator's ON-THE-RECORD assertion that
	// this Redis is NOT clustered, for a server whose topology cannot be probed
	// (some managed Redis trim INFO and disable CLUSTER). Without it, an
	// unverifiable topology REFUSES to start (D-71) — unknown fails closed.
	// It never overrides a POSITIVE cluster_enabled:1 (see quota/topology.go).
	// Env equivalent: AB0T_QUOTA_REDIS_CLUSTER_CONFIRMED_DISABLED=true.
	RedisClusterConfirmedDisabled bool `json:"redis_cluster_confirmed_disabled,omitempty"`
	// RedisDurabilityConfirmed is the operator's ON-THE-RECORD assertion that this Redis
	// does not evict (and, for the outbox, is persisted), for a server that cannot report
	// its CONFIG (ElastiCache disables it). Without it, an unverifiable eviction policy
	// REFUSES to start (D-72) — an unverified counter store is not a safe one. It never
	// overrides an allkeys-* policy the server actually reported.
	// Env equivalent: AB0T_QUOTA_REDIS_DURABILITY_CONFIRMED=true.
	RedisDurabilityConfirmed bool `json:"redis_durability_confirmed,omitempty"`
	// DDBPITRConfirmed is the operator's ON-THE-RECORD assertion that point-in-time recovery
	// is enabled on the library's DynamoDB tables, for a control plane that cannot report it
	// (DynamoDB Local answers UnknownOperationException to DescribeContinuousBackups — PITR is
	// the one thing only real AWS can confirm). Without it, an unverified backup posture on a
	// money store REFUSES to start (D-76). Env: AB0T_QUOTA_DDB_PITR_CONFIRMED=true.
	DDBPITRConfirmed               bool   `json:"ddb_pitr_confirmed,omitempty"`
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
	// LegacyIncrement governs the DEPRECATED legacy Spend path (D-24):
	//   "count_and_alert" (default) — Spend ALWAYS counts (never refuses);
	//       crossing a gauge limit emits a loud over_limit_admitted event.
	//       Rationale: Spend runs AFTER provisioning; refusing there leaves a
	//       resource existing-and-uncounted → phantom headroom. Enforce at
	//       Acquire (before provisioning), count at the fact.
	//   "enforce" — opt-in atomic check-and-refuse (only for a consumer that
	//       has verified it Spends BEFORE provisioning).
	LegacyIncrement string `json:"legacy_increment,omitempty"`
	// UnknownBundle governs how an admission gate treats an undeclared bundle
	// name / unregistered resource_key (D-14/D-48): "deny" (default) refuses
	// in enforce mode; "allow_warn" admits with a loud warning. A typo must
	// NEVER silently disable enforcement (fail-OPEN, D-31).
	UnknownBundle string `json:"unknown_bundle,omitempty"`
}

// LegacyIncrementMode returns the effective legacy-increment policy (D-24),
// defaulting to "count_and_alert".
func (e EnforcementConfig) LegacyIncrementMode() string {
	if e.LegacyIncrement == "enforce" {
		return "enforce"
	}
	return "count_and_alert"
}

// UnknownBundleAllowWarn reports whether unknown bundles/resources should be
// admitted with a warning (default false — deny in enforce mode).
func (e EnforcementConfig) UnknownBundleAllowWarn() bool {
	return e.UnknownBundle == "allow_warn"
}

// BillingConfig declares paid-mode intent (D-44/D-34). A paid service that
// cannot durably record billing must not start.
type BillingConfig struct {
	// EnablePaid asserts "this service charges money". With it true and no
	// durable outbox obtainable (and !outbox.allow_ephemeral), Setup REFUSES
	// to start (D-34). Also implied true when a billing mesh client is wired.
	EnablePaid bool `json:"enable_paid,omitempty"`
}

// OutboxConfig governs the durable lifecycle-settlement outbox (QB-01 / D-29
// / D-30 / D-32). Money-bearing lifecycle events are written to a DURABLE
// store BEFORE publish and drained from that store — so a crash between
// intent and delivery RESUMES, never silently loses the billing event.
type OutboxConfig struct {
	Enabled                  *bool   `json:"enabled,omitempty"`                    // default true
	Store                    string  `json:"store,omitempty"`                      // ddb | redis (default ddb, self-provisioned)
	AllowEphemeral           bool    `json:"allow_ephemeral,omitempty"`            // default false — dev escape (D-34)
	RedisDurabilityConfirmed bool    `json:"redis_durability_confirmed,omitempty"` // operator assertion when CONFIG unavailable (D-32)
	DDBTable                 string  `json:"ddb_table,omitempty"`                  // default ab0t_quota_outbox
	MaxRetryHorizonSeconds   float64 `json:"max_retry_horizon_seconds,omitempty"`  // default 900 (D-12)
	DrainIntervalSeconds     int     `json:"drain_interval_seconds,omitempty"`     // default 30
	MaxPerPass               int     `json:"max_per_pass,omitempty"`               // default 100 (bounded drain)
	PastHorizon              string  `json:"past_horizon,omitempty"`               // void_and_alert (default) | drop (never in prod)
	ProvisionRetryAttempts   int     `json:"provision_retry_attempts,omitempty"`   // default 3 (transient blip must not stop a deploy; absence must)
	SNSTopicARN              string  `json:"sns_topic_arn,omitempty"`              // settlement publish topic (D-56/D-63); Setup auto-wires an SNS publisher
	SNSRegion                string  `json:"sns_region,omitempty"`                 // default AWS_REGION
}

// OutboxEnabled reports whether the outbox is on (default true).
func (o OutboxConfig) OutboxEnabled() bool { return o.Enabled == nil || *o.Enabled }

// AlertsConfig configures the AlertManager.
type AlertsConfig struct {
	Enabled           bool    `json:"enabled,omitempty"`
	CooldownSeconds   int     `json:"cooldown_seconds,omitempty"` // default 3600
	WebhookURL        string  `json:"webhook_url,omitempty"`
	WarningThreshold  float64 `json:"warning_threshold,omitempty"`  // default 0.80
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
