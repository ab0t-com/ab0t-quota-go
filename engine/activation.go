package engine

// TASK P5.2 — the Go activation API: Acquire / Release / Settle. Ticket
// 20260709_ab0t_quota_systemic_integrity_redesign. Port of Python
// engine.py acquire/release/settle (DECISIONS D-10, D-22).
//
// WHY THIS EXISTS (D-22): P5.1 (real Redis) + P5.3 (Python's keys) made a
// mixed Python/Go fleet mechanically possible, while Go's Spend/Release had
// NO idempotency (QI-04). A duplicate Go Release then double-applies where
// Python dedups → gauge undercount → phantom headroom. Acquire mints a
// library-owned activation_id; Release/Settle dedup on it FOREVER (no TTL, no
// caller key). That closes the hazard and lifts D-22's gate on Go prod.
//
// ⚠️ Real-Redis Lua UNVERIFIED — the acquire/transition scripts run only
// under miniredis/gopher-lua in tests. Pre-deploy gate.

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
)

// AcquireResult is the outcome of Acquire. ActivationID is the minted,
// retry-safe handle the caller carries to Release/Settle.
type AcquireResult struct {
	Admitted       bool
	ActivationID   string
	DeniedResource string
	Reason         string // "ok" | "dup" | "denied"
	Values         map[string]float64
}

// AcquireInput selects what to acquire: a bundle_name OR a single resource_key.
type AcquireInput struct {
	OrgID          string
	UserID         string // "" ⇒ org-only (no per-user partition)
	BundleName     string
	ResourceKey    string
	IdempotencyKey string
}

func (e *Engine) activationStore() activations.Store {
	if e.Activations == nil {
		e.Activations = activations.NewInMemoryStore()
	}
	return e.Activations
}

// gaugeLimit resolves (orgLimit, userLimit) for one gauge resource for this
// org/user, from the tier config. nil ⇒ unlimited / no per-user cap.
func (e *Engine) gaugeLimit(ctx context.Context, orgID, userID, rk string) (orgLimit, userLimit *float64, err error) {
	tierID, terr := e.Provider.GetTier(ctx, userID, orgID)
	if terr != nil || tierID == "" {
		return nil, nil, nil // unresolved tier ⇒ treat as unlimited here; Check gates tiering
	}
	tier, ok := e.Reg.Tier(tierID)
	if !ok {
		return nil, nil, nil
	}
	lim, ok := tier.Limits[rk]
	if !ok || lim.IsUnlimited() {
		return nil, nil, nil
	}
	orgLimit = lim.Limit
	if userID != "" {
		userLimit = lim.DerivePerUserLimit(tier.DefaultPerUserFraction)
	}
	return orgLimit, userLimit, nil
}

// Acquire atomically checks ALL of a bundle's (or a single resource's) gauge
// limits and, only if every one passes, spends them all — in ONE atomic op —
// then mints an activation_id and persists an OPEN record. Fully retry-safe
// create path (kills QI-03 TOCTOU and QI-04's non-idempotent release, by
// minting identity). Non-gauge resources are NOT gated here.
func (e *Engine) Acquire(ctx context.Context, in AcquireInput) (AcquireResult, error) {
	// Enforcement preamble (D-14/D-48/D-31). The atomic admission gate MUST
	// honour the same guards Check does. Acquire previously bypassed them,
	// silently ADMITTING (1) when an operator flipped the global kill switch
	// and (2) when a bundle name / resource_key was an undeclared typo — both
	// the forbidden fail-OPEN direction. Mirrors Python engine.acquire.
	var resourceKeys []string
	label := in.BundleName
	unknown := false
	switch {
	case in.BundleName != "":
		resourceKeys = e.Reg.Bundle(in.BundleName)
		unknown = !e.Reg.HasBundle(in.BundleName)
	case in.ResourceKey != "":
		resourceKeys = []string{in.ResourceKey}
		label = in.ResourceKey
		_, ok := e.Reg.Resource(in.ResourceKey)
		unknown = !ok
	default:
		return AcquireResult{}, fmt.Errorf("engine: Acquire requires BundleName or ResourceKey")
	}

	// Global kill switch — fail closed (mirrors Check).
	if e.Cfg != nil && e.Cfg.Enforcement.GlobalKillSwitch {
		slog.Warn("acquire_denied", "org", in.OrgID, "bundle", label, "reason", "global_kill_switch")
		return AcquireResult{Admitted: false, DeniedResource: label, Reason: "global_kill_switch"}, nil
	}

	// QG-08 / D-14 — a resolved-but-unknown tier (mis-mapped org) is a config
	// error, not an admission. DENY + alert at the acquire gate too (Check
	// already does; Acquire must agree — D-48 matrix). Not softened by shadow.
	// Only when enforcement is enabled (enabled=false bypasses, like Check).
	if e.Cfg == nil || e.Cfg.Enforcement.Enabled {
		if tierID, terr := e.Provider.GetTier(ctx, in.UserID, in.OrgID); terr == nil && tierID != "" {
			if _, ok := e.Reg.Tier(tierID); !ok {
				e.emit(ctx, QuotaEvent{Kind: EventTierNotInConfig, OrgID: partitionOrg(in.UserID, in.OrgID), ResourceKey: label})
				slog.Warn("acquire_denied", "org", in.OrgID, "bundle", label, "reason", "tier_not_in_config", "tier", tierID)
				return AcquireResult{Admitted: false, DeniedResource: label, Reason: "tier_not_in_config"}, nil
			}
		}
	}

	// Unknown bundle / unregistered resource — a config typo must NOT silently
	// disable enforcement (D-14/D-48). Deny in enforce mode; allow+warn under
	// shadow_mode / enforcement-off / unknown_bundle=allow_warn. Always loud.
	if unknown {
		allow := (e.Cfg != nil && (!e.Cfg.Enforcement.Enabled || e.Cfg.Enforcement.ShadowMode)) ||
			(e.Cfg != nil && e.Cfg.Enforcement.UnknownBundleAllowWarn())
		outcome := "deny"
		if allow {
			outcome = "allow_warn"
		}
		slog.Warn("unknown_bundle", "org", in.OrgID, "bundle", label,
			"note", "not declared/registered", "outcome", outcome)
		if !allow {
			return AcquireResult{Admitted: false, DeniedResource: label, Reason: "unknown_bundle"}, nil
		}
		resourceKeys = nil // allowed under shadow/disabled → nothing to gate
	}

	// K-8: per-request charset guard for the keyspace-enabled path (spec §2.3).
	if err := e.ksGuard(in.OrgID); err != nil {
		return AcquireResult{}, err
	}

	// Resolve gauge specs (skip non-gauge resources — they don't gate concurrency).
	type gspec struct {
		rk   string
		spec counters.AcquireSpec
	}
	var gs []gspec
	for _, rk := range resourceKeys {
		rd, ok := e.Reg.Resource(rk)
		if !ok || rd.CounterType != config.CounterGauge {
			continue
		}
		orgLimit, userLimit, err := e.gaugeLimit(ctx, in.OrgID, in.UserID, rk)
		if err != nil {
			return AcquireResult{}, err
		}
		g := e.Factory.Gauge(rk)
		op := g.OrgKeyPair(in.OrgID)
		sp := counters.AcquireSpec{
			OrgKey:    op.P,
			OrgKey2:   op.S,
			HasUser:   in.UserID != "",
			Delta:     1,
			OrgLimit:  orgLimit,
			UserLimit: userLimit,
		}
		if in.UserID != "" {
			up, sq := g.UserKeyPair(in.OrgID, in.UserID), g.UserSeqKeyPair(in.OrgID, in.UserID)
			sp.UserKey, sp.UserKey2 = up.P, up.S
			sp.SeqKey, sp.SeqKey2 = sq.P, sq.S
		}
		gs = append(gs, gspec{rk: rk, spec: sp})
	}
	if len(gs) == 0 {
		// Nothing to gate — admit without an activation (parity with Python).
		return AcquireResult{Admitted: true, Reason: "ok", Values: map[string]float64{}}, nil
	}

	acq, ok := e.Factory.Floats.(counters.GaugeAcquirer)
	if !ok {
		return AcquireResult{}, fmt.Errorf("engine: Acquire requires a GaugeAcquirer store (got %T)", e.Factory.Floats)
	}
	specs := make([]counters.AcquireSpec, len(gs))
	for i, g := range gs {
		specs[i] = g.spec
	}
	hasIdem := in.IdempotencyKey != ""
	// idem key shares the first gauge's namespace (Python: specs[0] gauge).
	idemPair := e.Factory.Gauge(gs[0].rk).IdemKeyPair(in.OrgID, in.IdempotencyKey)
	idemKey := idemPair.P
	// K-8 dual: latches claimed on BOTH shapes, seed-if-absent, mutate-both.
	dual := idemPair.S != ""
	runAcquire := func(specs []counters.AcquireSpec) (counters.AcquireOutcome, error) {
		if dual {
			d, derr := e.dualOps()
			if derr != nil {
				return counters.AcquireOutcome{}, derr
			}
			return d.DualAtomicAcquire(ctx, idemPair, hasIdem, 0, e.ks().PrimaryIsV2(), specs)
		}
		return acq.AtomicAcquire(ctx, idemKey, hasIdem, 0, specs)
	}

	// D-55: honour enabled/shadow like Check. enforcement.enabled=false →
	// bypass limits entirely (admit + spend). This clears the caps up front so
	// the atomic spend can never deny.
	enabled := e.Cfg == nil || e.Cfg.Enforcement.Enabled
	shadow := e.Cfg != nil && e.Cfg.Enforcement.ShadowMode
	if !enabled {
		for i := range specs {
			specs[i].OrgLimit, specs[i].UserLimit = nil, nil
		}
	}

	out, err := runAcquire(specs)
	if err != nil {
		return AcquireResult{}, err
	}
	if !out.Admitted {
		denied := label
		if out.DeniedIndex >= 1 && out.DeniedIndex <= len(gs) {
			denied = gs[out.DeniedIndex-1].rk
		}
		// D-55: shadow_mode observes, never refuses. Log the would-deny, then
		// re-run with the caps cleared so the acquire ADMITS + SPENDS. The
		// first (denied) call spent nothing and claimed no idem key (the Lua
		// denies before SET NX), so the re-run applies exactly once.
		if shadow {
			slog.Warn("shadow_would_deny", "org", in.OrgID, "bundle", label, "user", in.UserID, "denied_resource", denied)
			for i := range specs {
				specs[i].OrgLimit, specs[i].UserLimit = nil, nil
			}
			out, err = runAcquire(specs)
			if err != nil {
				return AcquireResult{}, err
			}
		} else {
			slog.Info("acquire_denied", "org", in.OrgID, "bundle", label, "user", in.UserID, "denied_resource", denied)
			return AcquireResult{Admitted: false, DeniedResource: denied, Reason: "denied"}, nil
		}
	}

	// Read post-spend values + record touches (recent-activity guard, D-62).
	values := map[string]float64{}
	for _, g := range gs {
		v, _, _ := e.Factory.Floats.GetFloat(ctx, g.spec.OrgKey)
		values[g.rk] = v
		e.recordTouch(in.OrgID, g.rk)
	}

	res := AcquireResult{Admitted: true, Reason: "ok", Values: values}
	if out.Dup {
		// A replay of an already-claimed acquire: admitted, but do NOT mint a
		// second activation (the spend did not re-apply).
		res.Reason = "dup"
		return res, nil
	}

	// Mint + persist an OPEN activation (best-effort — the spend already landed).
	id := activations.MintActivationID()
	spend := make(map[string]float64, len(gs))
	for _, g := range gs {
		spend[g.rk] = g.spec.Delta
	}
	var userPtr *string
	if in.UserID != "" {
		u := in.UserID
		userPtr = &u
	}
	act := activations.Activation{
		ActivationID: id, OrgID: in.OrgID, UserID: userPtr,
		ResourceKey: label, State: activations.StateOpen, Spend: spend,
		OpenedAt: e.now().UTC().Format(time.RFC3339Nano),
	}
	if err := e.activationStore().PutOpen(ctx, act); err != nil {
		// FAIL-CLOSED (D-27/GT-01). The counter is already spent (an over-count
		// vs the ledger). We must NOT report this acquire as admitted: if we did
		// and the caller provisioned, the ledger would be MISSING the row and the
		// reconciler would later drive the counter DOWN to Σ open activations —
		// BELOW the live resource = under-count / phantom headroom, the one
		// forbidden direction (D-31). Failing the acquire leaves an orphaned
		// OVER-count, which only ever DENIES capacity (safe) and heals to Σ open.
		// Mirrors Python engine.acquire's persist-failure handling.
		slog.Error("acquire_persist_failed", "org", in.OrgID, "bundle", label, "err", err,
			"note", "FAILING the acquire (fail-closed, D-27); orphaned counter spend heals to Σ open; caller MUST NOT provision")
		return AcquireResult{Admitted: false, DeniedResource: label, Reason: "persist_failed"},
			fmt.Errorf("engine: acquire persist failed (fail-closed, D-27): %w", err)
	}
	res.ActivationID = id
	return res, nil
}

// ReleaseActivation idempotently marks an activation RELEASED and returns its
// spent gauges to zero, flooring at zero (reuses DecrByFloorZero). Keyed ONLY
// on the minted activation_id — no caller key, no TTL — so a reused
// resource-id can never collide (QI-05) and a duplicate delivery is a no-op
// (QI-04/D-22). Returns true iff THIS call performed the release.
//
// NB: named ReleaseActivation, not Release, because the pre-activation legacy
// Release(CheckInput) surface is kept (deprecated) and Go has no overloading.
// This IS the Python `release(activation_id)` API.
func (e *Engine) ReleaseActivation(ctx context.Context, activationID string) (bool, error) {
	row, err := e.activationStore().MarkReleased(ctx, activationID)
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, nil // already released or unknown — replay-safe by construction
	}
	// The store transition ran exactly once, so a plain (floored) decrement
	// per spent gauge is safe.
	for rk, delta := range row.Spend {
		rd, ok := e.Reg.Resource(rk)
		if !ok || rd.CounterType != config.CounterGauge {
			continue
		}
		g := e.Factory.Gauge(rk)
		if _, err := e.decrFloor(ctx, g.OrgKeyPair(row.OrgID), delta); err != nil {
			return true, err
		}
		if row.UserID != nil && *row.UserID != "" {
			if _, err := e.decrFloor(ctx, g.UserKeyPair(row.OrgID, *row.UserID), delta); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

// Settle records an activation's final cost, idempotent on activation_id
// (QB-02). Returns true iff THIS call recorded it.
func (e *Engine) Settle(ctx context.Context, activationID, cost string) (bool, error) {
	// QG-10 / D-47 — reject NaN / inf / negative BEFORE any ledger write. A
	// knob that lets NaN into a money ledger is one nobody may turn: NaN
	// poisons the accumulator irrecoverably. The activation stays OPEN.
	if f, perr := strconv.ParseFloat(strings.TrimSpace(cost), 64); perr != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return false, fmt.Errorf("engine: settle cost %q is not a valid non-negative finite amount (D-47)", cost)
	}
	row, err := e.activationStore().MarkSettled(ctx, activationID, cost)
	if err != nil {
		return false, err
	}
	if row != nil {
		return true, nil // this call recorded the settlement
	}
	// QG-11 / D-46 — already settled: first wins, but a DIFFERING re-settle is
	// the caller's cost mismatch and must be LOUD, never silently discarded.
	if existing, gerr := e.activationStore().Get(ctx, activationID); gerr == nil &&
		existing != nil && existing.Cost != nil && *existing.Cost != cost {
		slog.Warn("settle_conflict", "activation", activationID, "recorded_cost", *existing.Cost, "new_cost", cost)
		e.emit(ctx, QuotaEvent{Kind: EventSettleConflict, OrgID: existing.OrgID, ResourceKey: existing.ResourceKey})
	}
	return false, nil
}

// OpenActivations lists an org's OPEN activations (the drift alarm — QB-03).
func (e *Engine) OpenActivations(ctx context.Context, orgID string, limit int) ([]activations.Activation, error) {
	return e.activationStore().ListOpen(ctx, orgID, limit)
}
