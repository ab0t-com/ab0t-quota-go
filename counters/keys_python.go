package counters

// TASK P5.3 (finding QG-03, evidence E-D4) — canonical key builders that
// produce the EXACT Python v0.5.x key shapes. Ticket
// 20260709_ab0t_quota_systemic_integrity_redesign.
//
// Binding constraint (D-10 / FUTURE §2): Go adopts the EXISTING Python
// keyspace; Python's keys are NEVER changed. The reference shapes (derived
// by executing the Python lib) are:
//
//	gauge          quota:{org}:{rk}:gauge
//	gauge per-user quota:{org}:{rk}:gauge:user:{uid}
//	accumulator    quota:{org}:{rk}:acc:{period}
//	rate           quota:{org}:{rk}:rate
//
// The engine uses THESE. The older scope-based Key/PeriodKey builders in
// gauge.go / accumulator.go / rate.go / counter.go produced a different,
// NON-parity shape (quota:gauge:{rk}:{scope}); they are Deprecated and kept
// only until D-13 (peer-test consolidation) is authorized. See
// engine/goldenkey_parity_test.go for the cross-runtime gate.

import (
	"context"
	"fmt"
	"time"
)

// OrgKey returns the org-level gauge key: quota:{org}:{rk}:gauge.
// K-8: with a declared keyspace it returns the AUTHORITATIVE shape; the
// zero-value Keyspace keeps the legacy Prefix.Build path byte-identical.
// Scope validation happens at the engine boundary (engine.ksGuard).
func (g Gauge) OrgKey(orgID string) string {
	if g.Keyspace.Enabled() {
		return g.Keyspace.GaugePair(orgID, g.ResourceKey).P
	}
	return g.Prefix.Build(orgID, g.ResourceKey, "gauge")
}

// OrgKeyPair returns (primary, secondary) — Secondary "" outside dual.
func (g Gauge) OrgKeyPair(orgID string) DualPair {
	if g.Keyspace.Enabled() {
		return g.Keyspace.GaugePair(orgID, g.ResourceKey)
	}
	return DualPair{P: g.Prefix.Build(orgID, g.ResourceKey, "gauge")}
}

// UserKeyPair / UserSeqKeyPair / IdemKeyPair — dual twins of the builders below.
func (g Gauge) UserKeyPair(orgID, userID string) DualPair {
	if g.Keyspace.Enabled() {
		return g.Keyspace.UserPair(orgID, g.ResourceKey, userID)
	}
	return DualPair{P: g.Prefix.Build(orgID, g.ResourceKey, "gauge", "user", userID)}
}

func (g Gauge) UserSeqKeyPair(orgID, userID string) DualPair {
	if g.Keyspace.Enabled() {
		return g.Keyspace.SeqUserPair(orgID, g.ResourceKey, userID)
	}
	return DualPair{P: g.Prefix.Build(orgID, g.ResourceKey, "gauge", "seq", "user", userID)}
}

func (g Gauge) IdemKeyPair(orgID, key string) DualPair {
	if g.Keyspace.Enabled() {
		return g.Keyspace.IdemPair(orgID, g.ResourceKey, key)
	}
	if key == "" {
		return DualPair{P: g.Prefix.Build(orgID, g.ResourceKey, "idem", "__unused__")}
	}
	return DualPair{P: g.Prefix.Build(orgID, g.ResourceKey, "idem", key)}
}

// UserKey returns the per-user gauge partition: quota:{org}:{rk}:gauge:user:{uid}.
// Python maintains this alongside the org gauge on every user-attributed
// increment (gauge.py:42-46). Used by per-user scoping (TASK P5.4).
func (g Gauge) UserKey(orgID, userID string) string {
	return g.UserKeyPair(orgID, userID).P
}

// UserSeqKey returns the per-(org,user,resource) CREATE-generation key:
// quota:{org}:{rk}:gauge:seq:user:{uid} (Python gauge.py _seq_user_key).
// The activation acquire path bumps it for byte-parity with a Python fleet
// (TASK P5.2). Declared in KEYS for every Lua that touches it (QI-09).
func (g Gauge) UserSeqKey(orgID, userID string) string {
	return g.UserSeqKeyPair(orgID, userID).P
}

// IdemKey returns the acquire idempotency key: quota:{org}:{rk}:idem:{key}
// (Python gauge.py _idem_key). An empty key yields the ":idem:__unused__"
// placeholder Python uses when no idempotency key is supplied.
func (g Gauge) IdemKey(orgID, key string) string {
	return g.IdemKeyPair(orgID, key).P
}

// OrgPeriodKey returns the accumulator key: quota:{org}:{rk}:acc:{period}.
func (a Accumulator) OrgPeriodKey(orgID string, now time.Time) string {
	return a.OrgPeriodKeyPair(orgID, now).P
}

// OrgPeriodKeyPair — (primary, secondary) period bucket keys (K-8).
func (a Accumulator) OrgPeriodKeyPair(orgID string, now time.Time) DualPair {
	if a.Keyspace.Enabled() {
		return a.Keyspace.AccPair(orgID, a.ResourceKey, CurrentPeriod(a.Reset, now))
	}
	return DualPair{P: a.Prefix.Build(orgID, a.ResourceKey, "acc", CurrentPeriod(a.Reset, now))}
}

// AddOrg adds delta to the org-scoped accumulator period bucket and applies
// the period TTL. Mirrors Add but keyed to the Python shape.
//
// NOTE (QG-05): the TTL applied here is Go's PeriodTTL, which DIFFERS from
// Python's (only daily matches). Aligning it is QG-05's fix and needs its
// own red test; out of scope for P5.3 (key parity only).
func (a Accumulator) AddOrg(ctx context.Context, orgID string, now time.Time, delta float64) (float64, error) {
	delta, derr := accumulatorDelta(delta) // W-T3/CT-04 (D-31, Python parity)
	if derr != nil {
		return 0, derr
	}
	pair := a.OrgPeriodKeyPair(orgID, now)
	// K-8 dual: seed-if-absent + mutate-both + TTL-both atomically.
	if pair.S != "" {
		d, ok := a.Store.(DualOps)
		if !ok {
			return 0, fmt.Errorf("counters: keyspace dual-write declared but store %T "+
				"has no DualOps — refusing a silent single-shape write", a.Store)
		}
		return d.DualAccAdd(ctx, pair, delta, PeriodTTL(a.Reset), a.Keyspace.PrimaryIsV2())
	}
	v, err := a.Store.IncrByFloat(ctx, pair.P, delta)
	if err != nil {
		return 0, err
	}
	if ttl := PeriodTTL(a.Reset); ttl > 0 {
		_ = a.Store.Expire(ctx, pair.P, ttl)
	}
	return v, nil
}

// GetOrg returns the org-scoped accumulator period value (dual-read: the
// authoritative shape first, fallback to the other during dual — K-8).
func (a Accumulator) GetOrg(ctx context.Context, orgID string, now time.Time) (float64, error) {
	v, _, err := GetFloatDual(ctx, a.Store, a.OrgPeriodKeyPair(orgID, now))
	return v, err
}

// OrgKey returns the rate key: quota:{org}:{rk}:rate.
func (r Rate) OrgKey(orgID string) string {
	return r.OrgKeyPair(orgID).P
}

// OrgKeyPair — (primary, secondary) rate window keys (K-8).
func (r Rate) OrgKeyPair(orgID string) DualPair {
	if r.Keyspace.Enabled() {
		return r.Keyspace.RatePair(orgID, r.ResourceKey)
	}
	return DualPair{P: r.Prefix.Build(orgID, r.ResourceKey, "rate")}
}

// RecordOrg records one event under the org-scoped rate key. K-8 dual: rate
// windows are dual-WRITTEN from dual-on, never copied — the flip gate
// outwaits the window (spec §6.1), so both shapes fill independently.
func (r Rate) RecordOrg(ctx context.Context, orgID string, now time.Time, member string) error {
	pair := r.OrgKeyPair(orgID)
	if err := r.Store.Record(ctx, pair.P, now, r.Window, member); err != nil {
		return err
	}
	if pair.S != "" {
		return r.Store.Record(ctx, pair.S, now, r.Window, member)
	}
	return nil
}

// CountOrg returns the in-window count under the AUTHORITATIVE rate key.
func (r Rate) CountOrg(ctx context.Context, orgID string, now time.Time) (int64, error) {
	return r.Store.Count(ctx, r.OrgKeyPair(orgID).P, now, r.Window)
}
