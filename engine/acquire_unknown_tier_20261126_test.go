package engine

// v0.2.0 — Acquire on an UNRESOLVED tier (the tier provider failed or returned blank) fails CLOSED,
// like Check and the Python runtime's acquire. Before, the preamble only handled a resolved-but-
// unmapped tier and the unresolved case fell through to gaugeLimit, which reads it as UNLIMITED: the
// atomic admission gate admitted work nobody could bill. Now Acquire returns an error wrapping
// ErrTierUnresolved (nothing admitted, nothing spent) unless the config opts out with
// enforcement.allow_unknown_tier (the old behaviour, kept knowingly).

import (
	"context"
	"errors"
	"testing"
)

func TestAcquire_UnresolvedTierFailsClosedByDefault_20261126(t *testing.T) {
	for name, prov := range map[string]tierProvider20261123{
		"provider error": {err: errors.New("auth unreachable")},
		"blank tier":     {tier: ""},
	} {
		e := unknownTierEngine(false, prov)
		res, err := e.Acquire(context.Background(), AcquireInput{OrgID: "org-1", UserID: "u-1", ResourceKey: "api.calls"})
		if !errors.Is(err, ErrTierUnresolved) || res.Admitted || res.ActivationID != "" {
			t.Fatalf("%s: Acquire = %+v, %v; want an ErrTierUnresolved error and nothing admitted", name, res, err)
		}
		// Nothing was spent: once the tier resolves, the gauge reads 0.
		e.Provider = tierProvider20261123{tier: "pro"}
		chk, cerr := e.Check(context.Background(), CheckInput{UserID: "u-1", OrgID: "org-1", ResourceKey: "api.calls", Cost: 1})
		if cerr != nil || chk.Used != 0 {
			t.Fatalf("%s: the refused acquire spent quota: Used=%v err=%v", name, chk.Used, cerr)
		}
	}
}

func TestAcquire_UnresolvedTierOptOutAdmits_20261126(t *testing.T) {
	e := unknownTierEngine(true, tierProvider20261123{err: errors.New("auth unreachable")})
	res, err := e.Acquire(context.Background(), AcquireInput{OrgID: "org-1", UserID: "u-1", ResourceKey: "api.calls"})
	if err != nil || !res.Admitted || res.Reason != "ok" || res.ActivationID == "" {
		t.Fatalf("enforcement.allow_unknown_tier=true must keep the old behaviour (admitted, unlimited): %+v %v", res, err)
	}
}

// Controls: a resolved tier is admitted under either setting; a resolved-but-unmapped tier is still
// denied tier_not_in_config (a domain answer, not an error) under either setting; enforcement
// disabled still bypasses tiering.
func TestAcquire_UnresolvedTierControls_20261126(t *testing.T) {
	for _, allow := range []bool{false, true} {
		res, err := unknownTierEngine(allow, tierProvider20261123{tier: "pro"}).Acquire(context.Background(),
			AcquireInput{OrgID: "org-1", UserID: "u-1", ResourceKey: "api.calls"})
		if err != nil || !res.Admitted {
			t.Fatalf("allow=%v resolved tier: %+v %v", allow, res, err)
		}
		res, err = unknownTierEngine(allow, tierProvider20261123{tier: "gold-not-declared"}).Acquire(context.Background(),
			AcquireInput{OrgID: "org-1", UserID: "u-1", ResourceKey: "api.calls"})
		if err != nil || res.Admitted || res.Reason != "tier_not_in_config" {
			t.Fatalf("allow=%v unmapped tier: %+v %v", allow, res, err)
		}
	}
	e := unknownTierEngine(false, tierProvider20261123{err: errors.New("auth unreachable")})
	e.Cfg.Enforcement.Enabled = false
	res, err := e.Acquire(context.Background(), AcquireInput{OrgID: "org-1", UserID: "u-1", ResourceKey: "api.calls"})
	if err != nil || !res.Admitted {
		t.Fatalf("enforcement disabled bypasses tiering (as Check): %+v %v", res, err)
	}
}
