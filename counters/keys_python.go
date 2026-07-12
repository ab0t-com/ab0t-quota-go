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
	"time"
)

// OrgKey returns the org-level gauge key: quota:{org}:{rk}:gauge.
func (g Gauge) OrgKey(orgID string) string {
	return g.Prefix.Build(orgID, g.ResourceKey, "gauge")
}

// UserKey returns the per-user gauge partition: quota:{org}:{rk}:gauge:user:{uid}.
// Python maintains this alongside the org gauge on every user-attributed
// increment (gauge.py:42-46). Used by per-user scoping (TASK P5.4).
func (g Gauge) UserKey(orgID, userID string) string {
	return g.Prefix.Build(orgID, g.ResourceKey, "gauge", "user", userID)
}

// UserSeqKey returns the per-(org,user,resource) CREATE-generation key:
// quota:{org}:{rk}:gauge:seq:user:{uid} (Python gauge.py _seq_user_key).
// The activation acquire path bumps it for byte-parity with a Python fleet
// (TASK P5.2). Declared in KEYS for every Lua that touches it (QI-09).
func (g Gauge) UserSeqKey(orgID, userID string) string {
	return g.Prefix.Build(orgID, g.ResourceKey, "gauge", "seq", "user", userID)
}

// IdemKey returns the acquire idempotency key: quota:{org}:{rk}:idem:{key}
// (Python gauge.py _idem_key). An empty key yields the ":idem:__unused__"
// placeholder Python uses when no idempotency key is supplied.
func (g Gauge) IdemKey(orgID, key string) string {
	if key == "" {
		return g.Prefix.Build(orgID, g.ResourceKey, "idem", "__unused__")
	}
	return g.Prefix.Build(orgID, g.ResourceKey, "idem", key)
}

// OrgPeriodKey returns the accumulator key: quota:{org}:{rk}:acc:{period}.
func (a Accumulator) OrgPeriodKey(orgID string, now time.Time) string {
	return a.Prefix.Build(orgID, a.ResourceKey, "acc", CurrentPeriod(a.Reset, now))
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
	key := a.OrgPeriodKey(orgID, now)
	v, err := a.Store.IncrByFloat(ctx, key, delta)
	if err != nil {
		return 0, err
	}
	if ttl := PeriodTTL(a.Reset); ttl > 0 {
		_ = a.Store.Expire(ctx, key, ttl)
	}
	return v, nil
}

// GetOrg returns the org-scoped accumulator period value.
func (a Accumulator) GetOrg(ctx context.Context, orgID string, now time.Time) (float64, error) {
	v, _, err := a.Store.GetFloat(ctx, a.OrgPeriodKey(orgID, now))
	return v, err
}

// OrgKey returns the rate key: quota:{org}:{rk}:rate.
func (r Rate) OrgKey(orgID string) string {
	return r.Prefix.Build(orgID, r.ResourceKey, "rate")
}

// RecordOrg records one event under the org-scoped rate key.
func (r Rate) RecordOrg(ctx context.Context, orgID string, now time.Time, member string) error {
	return r.Store.Record(ctx, r.OrgKey(orgID), now, r.Window, member)
}

// CountOrg returns the in-window count under the org-scoped rate key.
func (r Rate) CountOrg(ctx context.Context, orgID string, now time.Time) (int64, error) {
	return r.Store.Count(ctx, r.OrgKey(orgID), now, r.Window)
}
