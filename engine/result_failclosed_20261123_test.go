package engine

// Result.AllowedFailClosed (v0.1.12, additive, opt-in). Allowed() is
// unchanged (UnknownTier still proceeds); AllowedFailClosed() reads an unresolved tier the way the
// Python runtime does — as a refusal.

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
		{UnknownTier, true, false},
	}
	for _, c := range cases {
		r := Result{Decision: c.d}
		if r.Allowed() != c.allowed {
			t.Fatalf("%s: Allowed() changed (%v) — it must stay as it was", c.d, r.Allowed())
		}
		if r.AllowedFailClosed() != c.failClosed {
			t.Fatalf("%s: AllowedFailClosed() = %v, want %v", c.d, r.AllowedFailClosed(), c.failClosed)
		}
	}
}
