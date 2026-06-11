// Package engine implements the central quota check engine. Engine.Check
// returns a Result; middleware translates Result into HTTP.
package engine

import "time"

// Decision is the outcome of a quota check.
type Decision string

const (
	Allow         Decision = "allow"
	Deny          Decision = "deny"
	ShadowAllow   Decision = "shadow_allow"    // would have denied but shadow_mode=true
	Warn          Decision = "warn"            // allowed; near threshold
	Critical      Decision = "critical"        // allowed; at/over critical threshold
	UnknownTier   Decision = "unknown_tier"    // resolver returned blank or unknown; fail-open by policy
)

// Result captures everything middleware needs to write a response.
type Result struct {
	Decision     Decision
	Reason       string
	Resource     string
	TierID       string
	Used         float64
	Limit        *float64 // nil → unlimited
	Burst        float64
	Threshold    float64 // 0..1 representing how close to limit (Used/Limit)
	Warning      bool
	Critical     bool
	RetryAfter   time.Duration
	Message      string
	UpgradeURL   string

	// Debug captures structured diagnostics for log emission.
	Debug map[string]any
}

// Allowed reports whether the result should let the request proceed.
// Allow + ShadowAllow + Warn + Critical + UnknownTier all proceed; only
// Deny stops.
func (r Result) Allowed() bool { return r.Decision != Deny }

// IsUnlimited reports whether the result was for an unlimited resource.
func (r Result) IsUnlimited() bool { return r.Limit == nil }
