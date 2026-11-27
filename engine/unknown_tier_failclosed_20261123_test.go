package engine

// v0.2.0 changed default: an UnknownTier decision (the tier resolver failed or returned blank) does
// NOT proceed — Result.Allowed() is false — unless the config opts out with
// enforcement.allow_unknown_tier. Every other decision reads exactly as before.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

type tierProvider20261123 struct {
	tier string
	err  error
}

func (p tierProvider20261123) GetTier(context.Context, string, string) (string, error) {
	return p.tier, p.err
}

func unknownTierEngine(allow bool, prov tierProvider20261123) *Engine {
	lim := 5.0
	cfg := &config.Config{
		Enforcement: config.EnforcementConfig{Enabled: true, AllowUnknownTier: allow},
		Tiers:       []config.Tier{{TierID: "pro", Limits: map[string]config.TierLimit{"api.calls": {Limit: &lim}}}},
		Resources:   []config.ResourceDef{{ResourceKey: "api.calls", CounterType: config.CounterGauge}},
	}
	return &Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov,
		Factory:  counters.NewMemoryFactory("quota"),
		Messages: messages.New(messages.Templates{}),
		Clock:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
}

func TestResultAllowed_Table_20261123(t *testing.T) {
	cases := []struct {
		d          Decision
		optOut     bool
		allowed    bool
		failClosed bool
		unavail    bool
	}{
		{Allow, false, true, true, false},
		{Warn, false, true, true, false},
		{Critical, false, true, true, false},
		{ShadowAllow, false, true, true, false},
		{Deny, false, false, false, false},
		{Deny, true, false, false, false}, // the opt-out never admits a real denial
		{UnknownTier, false, false, false, true},
		{UnknownTier, true, true, false, false}, // opt-out: pre-v0.2.0 behaviour; AllowedFailClosed still refuses
	}
	for _, c := range cases {
		r := Result{Decision: c.d, AllowUnknownTier: c.optOut}
		if r.Allowed() != c.allowed || r.AllowedFailClosed() != c.failClosed || r.Unavailable() != c.unavail {
			t.Errorf("%s (opt-out %v): Allowed=%v AllowedFailClosed=%v Unavailable=%v, want %v %v %v", c.d, c.optOut,
				r.Allowed(), r.AllowedFailClosed(), r.Unavailable(), c.allowed, c.failClosed, c.unavail)
		}
	}
}

func TestCheck_UnresolvedTierFailsClosedByDefault_20261123(t *testing.T) {
	for name, prov := range map[string]tierProvider20261123{
		"provider error": {err: errors.New("auth unreachable")},
		"blank tier":     {tier: ""},
	} {
		res, err := unknownTierEngine(false, prov).Check(context.Background(),
			CheckInput{UserID: "u-1", OrgID: "org-1", ResourceKey: "api.calls", Cost: 1})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Decision != UnknownTier || res.Reason != "tier_unresolved" || res.Allowed() || !res.Unavailable() {
			t.Fatalf("%s: %+v — an unresolved tier must not proceed by default", name, res)
		}
	}
}

func TestCheck_UnresolvedTierOptOut_20261123(t *testing.T) {
	res, err := unknownTierEngine(true, tierProvider20261123{err: errors.New("auth unreachable")}).Check(
		context.Background(), CheckInput{UserID: "u-1", OrgID: "org-1", ResourceKey: "api.calls", Cost: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != UnknownTier || !res.AllowUnknownTier || !res.Allowed() || res.Unavailable() || res.AllowedFailClosed() {
		t.Fatalf("enforcement.allow_unknown_tier=true must restore the old behaviour: %+v", res)
	}
}

// Control: a resolved tier is unaffected by either setting (the flip touches only UnknownTier).
func TestCheck_ResolvedTierUnaffected_20261123(t *testing.T) {
	for _, allow := range []bool{false, true} {
		res, err := unknownTierEngine(allow, tierProvider20261123{tier: "pro"}).Check(context.Background(),
			CheckInput{UserID: "u-1", OrgID: "org-1", ResourceKey: "api.calls", Cost: 1})
		if err != nil || res.Decision == UnknownTier || !res.Allowed() || res.AllowUnknownTier {
			t.Fatalf("allow=%v: %+v %v", allow, res, err)
		}
	}
}

func TestEnforcementConfig_AllowUnknownTierJSON_20261123(t *testing.T) {
	var on, off config.EnforcementConfig
	if err := json.Unmarshal([]byte(`{"enabled":true,"allow_unknown_tier":true}`), &on); err != nil || !on.AllowUnknownTier {
		t.Fatalf("allow_unknown_tier=true: %+v %v", on, err)
	}
	if err := json.Unmarshal([]byte(`{"enabled":true}`), &off); err != nil || off.AllowUnknownTier {
		t.Fatalf("absent = false (fail closed): %+v %v", off, err)
	}
}
