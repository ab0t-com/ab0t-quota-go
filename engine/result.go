// Package engine implements the central quota check engine. Engine.Check
// returns a Result; middleware translates Result into HTTP.
package engine

import "time"

// Decision is the outcome of a quota check.
type Decision string

const (
	Allow       Decision = "allow"
	Deny        Decision = "deny"
	ShadowAllow Decision = "shadow_allow" // would have denied but shadow_mode=true
	Warn        Decision = "warn"         // allowed; near threshold
	Critical    Decision = "critical"     // allowed; at/over critical threshold
	// UnknownTier: the tier resolver failed or returned blank — the engine could not evaluate the
	// limit. Since v0.2.0 it does NOT proceed (Allowed() is false; the middleware answers 503)
	// unless the config opts out with enforcement.allow_unknown_tier = true.
	UnknownTier Decision = "unknown_tier"
)

// Result captures everything middleware needs to write a response.
type Result struct {
	Decision   Decision
	Reason     string
	Resource   string
	TierID     string
	Used       float64
	Limit      *float64 // nil → unlimited
	Burst      float64
	Threshold  float64 // 0..1 representing how close to limit (Used/Limit)
	Warning    bool
	Critical   bool
	RetryAfter time.Duration
	Message    string
	UpgradeURL string
	// OrgID (v0.1.10) is the org the decision is for — Engine.Check sets it, so
	// alert cooldowns and payloads are per tenant (ticket 20261016, C-11).
	OrgID string

	// AllowUnknownTier (v0.2.0) is the opt-out for an UnknownTier decision: Engine.Check copies it
	// from the config's enforcement.allow_unknown_tier, so Allowed() lets an unresolved tier proceed
	// the way it did before v0.2.0. False (the default) = an unresolved tier does not proceed.
	AllowUnknownTier bool

	// Debug captures structured diagnostics for log emission.
	Debug map[string]any
}

// Allowed reports whether the result should let the request proceed.
// Allow + ShadowAllow + Warn + Critical proceed; Deny stops.
//
// v0.2.0 (changed default): UnknownTier stops too — the tier could not be resolved, so the limit
// could not be evaluated, and admitting work then can cost money nobody can bill (fail closed; the
// Python runtime's reading). A result carrying AllowUnknownTier (config
// enforcement.allow_unknown_tier = true) keeps the pre-v0.2.0 behaviour: UnknownTier proceeds.
func (r Result) Allowed() bool {
	if r.Decision == UnknownTier {
		return r.AllowUnknownTier
	}
	return r.Decision != Deny
}

// AllowedFailClosed (v0.1.12, additive, opt-in) is Allowed() with one difference: an
// UnknownTier decision never proceeds, whatever the config says. Since v0.2.0 Allowed() does the
// same by default; AllowedFailClosed() stays for code that must fail closed even where the config
// opts out (enforcement.allow_unknown_tier).
func (r Result) AllowedFailClosed() bool { return r.Decision != Deny && r.Decision != UnknownTier }

// Unavailable (v0.2.0) reports whether a refusal means "could not evaluate" rather than "over the
// limit": an UnknownTier decision that does not proceed. The middleware answers it with 503, not 429.
func (r Result) Unavailable() bool { return r.Decision == UnknownTier && !r.Allowed() }

// IsUnlimited reports whether the result was for an unlimited resource.
func (r Result) IsUnlimited() bool { return r.Limit == nil }
