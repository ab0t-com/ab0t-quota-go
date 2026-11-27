package engine

// Result.AllowedFailClosed (v0.1.12, additive, opt-in). Since v0.2.0 Allowed() ALSO refuses an
// UnknownTier by default (fail closed, the same as the Python runtime); the opt-out
// (enforcement.allow_unknown_tier) is covered by its own test.

import "testing"

func TestAllowedFailClosed_20261123(t *testing.T) {
	cases := []struct {
		d          Decision
		allowed    bool
		failClosed bool
	}{
		{Allow, true, true},
		{Warn, true, true},
		{Critical, true, true},
		{ShadowAllow, true, true},
		{Deny, false, false},
		{UnknownTier, false, false}, // v0.2.0: unknown tier = deny by default
	}
	for _, c := range cases {
		r := Result{Decision: c.d}
		if r.Allowed() != c.allowed {
			t.Fatalf("%s: Allowed() = %v, want %v", c.d, r.Allowed(), c.allowed)
		}
		if r.AllowedFailClosed() != c.failClosed {
			t.Fatalf("%s: AllowedFailClosed() = %v, want %v", c.d, r.AllowedFailClosed(), c.failClosed)
		}
	}
}
