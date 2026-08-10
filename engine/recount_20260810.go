// Ticket 20260810 — robust self-correcting quota (Go parity with the Python
// engine's recount-before-deny, read-repair, and on-demand ReconcileOrg).
// DESIGN_robust_quota.md §0: the fast Redis counter is a CACHE of a
// type-specific derived truth; staleness must never harm the user.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
)

const driftEps = 1e-9

func errUnknownResource(rk string) error {
	return fmt.Errorf("engine: unknown resource_key %q", rk)
}

// callObserved invokes the gauge truth provider, bounded by ProviderTimeout.
func (e *Engine) callObserved(ctx context.Context, orgID string) (map[string]int, error) {
	if e.ProviderTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.ProviderTimeout)
		defer cancel()
	}
	return e.ObservedUsage(ctx, orgID)
}

// callAccumulator invokes the accumulator (period-ledger) truth provider.
func (e *Engine) callAccumulator(ctx context.Context, orgID string) (map[string]float64, error) {
	if e.ProviderTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.ProviderTimeout)
		defer cancel()
	}
	return e.AccumulatorUsage(ctx, orgID)
}

// hasTruthSource reports whether a truth provider is wired for a resource's type.
func (e *Engine) hasTruthSource(res config.ResourceDef) bool {
	switch res.CounterType {
	case config.CounterGauge:
		return e.ObservedUsage != nil
	case config.CounterAccumulator:
		return e.AccumulatorUsage != nil
	}
	return false
}

func sourceFor(res config.ResourceDef) string {
	if res.CounterType == config.CounterAccumulator {
		return "ledger"
	}
	return "provider"
}

// recomputeTrueUsage returns (trueUsed, ok, err) for a resource from its TYPE
// source of truth. ok=false means no source is wired OR the provider returned no
// observation for this key (absence != zero, D-51). err != nil means the source
// was unreachable (the caller fails open).
func (e *Engine) recomputeTrueUsage(ctx context.Context, orgID string, res config.ResourceDef) (float64, bool, error) {
	switch res.CounterType {
	case config.CounterGauge:
		if e.ObservedUsage == nil {
			return 0, false, nil
		}
		m, err := e.callObserved(ctx, orgID)
		if err != nil {
			return 0, false, err
		}
		v, ok := m[res.ResourceKey]
		if !ok {
			return 0, false, nil
		}
		return float64(v), true, nil
	case config.CounterAccumulator:
		if e.AccumulatorUsage == nil {
			return 0, false, nil
		}
		m, err := e.callAccumulator(ctx, orgID)
		if err != nil {
			return 0, false, err
		}
		v, ok := m[res.ResourceKey]
		if !ok {
			return 0, false, nil
		}
		return v, true, nil
	}
	return 0, false, nil
}

// repairCounterToTruth force-sets the cached counter to its type's truth. Gauge
// per-user partitions are NOT repaired here — the Go provider is org-total-only
// (parity gap noted on the Engine struct).
func (e *Engine) repairCounterToTruth(ctx context.Context, res config.ResourceDef, orgID string, trueVal float64) error {
	switch res.CounterType {
	case config.CounterGauge:
		g := e.Factory.Gauge(res.ResourceKey)
		pair := g.OrgKeyPair(orgID)
		if err := e.Factory.Floats.Set(ctx, pair.P, trueVal, 0); err != nil {
			return err
		}
		if pair.S != "" {
			if err := e.Factory.Floats.Set(ctx, pair.S, trueVal, 0); err != nil {
				return err
			}
		}
	case config.CounterAccumulator:
		a := e.Factory.Accumulator(res.ResourceKey, res.ResetPeriod)
		pair := a.OrgPeriodKeyPair(orgID, e.now())
		ttl := counters.PeriodTTL(res.ResetPeriod)
		if err := e.Factory.Floats.Set(ctx, pair.P, trueVal, ttl); err != nil {
			return err
		}
		if pair.S != "" {
			if err := e.Factory.Floats.Set(ctx, pair.S, trueVal, ttl); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) emitDrift(ctx context.Context, orgID string, res config.ResourceDef, source string, before, after float64) {
	if e.OnDrift != nil {
		e.OnDrift(ctx, DriftInfo{
			OrgID: orgID, ResourceKey: res.ResourceKey, Source: source,
			ResourceType: string(res.CounterType), Before: before, After: after,
		})
		return
	}
	slog.Info("quota_drift_detected", "org", orgID, "resource", res.ResourceKey,
		"type", string(res.CounterType), "source", source, "before", before, "after", after)
}

func (e *Engine) emitProviderUnreachable(ctx context.Context, orgID string, res config.ResourceDef, err error) {
	if e.OnDrift != nil {
		e.OnDrift(ctx, DriftInfo{
			OrgID: orgID, ResourceKey: res.ResourceKey, Source: "provider_unreachable",
			ResourceType: string(res.CounterType),
		})
	}
	slog.Error("quota_recount_provider_unreachable", "org", orgID, "resource", res.ResourceKey, "err", err,
		"note", "ALLOWING/continuing (fail-open on drift, D-31); a stale cache must never block a user")
}

// recountBeforeDeny is P1's core: given a would-be Deny, recompute the true usage
// from the resource's TYPE truth source, repair a stale cache, and re-decide.
// Fail-OPEN on a truth-source error (never block on an unverifiable cache).
func (e *Engine) recountBeforeDeny(
	ctx context.Context, in CheckInput, res config.ResourceDef, tier config.Tier,
	limit config.TierLimit, tierID string, cachedUsed float64, denied Result,
) Result {
	if res.CounterType == config.CounterRate {
		return denied // the TTL'd window counter is already truth
	}
	if !e.hasTruthSource(res) {
		return denied // no truth source wired — keep the cache-based deny
	}
	trueUsed, ok, err := e.recomputeTrueUsage(ctx, in.OrgID, res)
	if err != nil {
		// P1.2 — truth source unreachable. The fail direction is TYPE-AWARE (not
		// uniform), because the two counter types drift in OPPOSITE directions:
		//
		//   * GAUGE — the cache drifts HIGH from lost decrements (the "5/0" class),
		//     so a stale cache must NEVER block a user. Fail-OPEN: allow + alert; any
		//     over-admission is bounded and healed by the reconciler.
		//
		//   * ACCUMULATOR — the cache IS the durable ledger-backed running total: a
		//     meter/cost counter is monotonic within its period and has NO
		//     lost-decrement drift, so "cache says over limit" means the customer
		//     genuinely accrued that spend. Failing OPEN would let spend blow past the
		//     cap during a ledger-source outage (overspend / revenue risk). So do NOT
		//     fail-open: KEEP the cache-based deny (fail-to-last-known for money). A
		//     genuinely-wrong meter cache is corrected by ReconcileOrg / read-repair,
		//     never by fail-open. Still alert (distinct reason) so it is visible.
		e.emitProviderUnreachable(ctx, partitionOrg(in.UserID, in.OrgID), res, err)
		if res.CounterType == config.CounterAccumulator {
			slog.Error("recount_source_unavailable_kept_deny", "org", in.OrgID, "resource", res.ResourceKey, "err", err,
				"note", "KEEPING the deny (money-safety, fail-to-last-known): an accumulator cache is the durable ledger sum with no lost-decrement drift; allowing on a source outage risks overspend past the cap")
			r := denied
			r.Reason = "recount_source_unavailable_kept_deny"
			return r
		}
		r := denied
		r.Decision = Allow
		r.Reason = "recount_fail_open"
		r.Used = cachedUsed
		r.Warning, r.Critical = false, false
		r.Message = ""
		return r
	}
	if !ok {
		return denied // provider reachable but no observation — keep deny (absence != zero)
	}
	org := partitionOrg(in.UserID, in.OrgID)
	if math.Abs(trueUsed-cachedUsed) > driftEps {
		if rerr := e.repairCounterToTruth(ctx, res, org, trueUsed); rerr != nil {
			slog.Error("recount_repair_failed", "org", org, "resource", res.ResourceKey, "err", rerr)
		} else {
			e.emitDrift(ctx, org, res, sourceFor(res), cachedUsed, trueUsed)
		}
	}
	repaired := e.decideOrg(in.ResourceKey, tierID, res, tier, limit, trueUsed, in.Cost)
	if repaired.Decision == Deny {
		repaired.Reason = "exceeded_recounted"
	} else if math.Abs(trueUsed-cachedUsed) > driftEps {
		repaired.Reason = "recount_repaired_allow"
	}
	return repaired
}

// ReconcileEntry is the per-resource before->after of Engine.ReconcileOrg.
type ReconcileEntry struct {
	CounterType string  `json:"counter_type"`
	Before      float64 `json:"before"`
	After       float64 `json:"after"`
	Changed     bool    `json:"changed"`
	Source      string  `json:"source,omitempty"`
	Status      string  `json:"status"`
}

// ReconcileReport is the structured result of Engine.ReconcileOrg.
type ReconcileReport struct {
	OrgID     string                    `json:"org_id"`
	Resources map[string]ReconcileEntry `json:"resources"`
}

// ReconcileOrg (P2) recomputes each resource from its TYPE source of truth and
// force-sets the cached counter to it. Powers a user-facing "Recalculate usage"
// button: idempotent, safe anytime, and it ONLY ever sets a counter to derived
// truth. GAUGE→ObservedUsage, ACCUMULATOR→AccumulatorUsage, RATE→skipped.
// Emits quota.drift_detected on every repair. If resourceKey != "", only that
// resource is reconciled.
func (e *Engine) ReconcileOrg(ctx context.Context, orgID, resourceKey string) (ReconcileReport, error) {
	report := ReconcileReport{OrgID: orgID, Resources: map[string]ReconcileEntry{}}
	var defs []config.ResourceDef
	if resourceKey != "" {
		rd, ok := e.Reg.Resource(resourceKey)
		if !ok {
			return report, errUnknownResource(resourceKey)
		}
		defs = []config.ResourceDef{rd}
	} else {
		defs = e.Reg.AllResources()
	}

	// Fetch each provider AT MOST ONCE per pass (a provider returns the whole
	// org in one call), then dispatch per resource from the cached result.
	var (
		gObs      map[string]int
		gErr      error
		aObs      map[string]float64
		aErr      error
		needGauge bool
		needAcc   bool
	)
	for _, res := range defs {
		if res.CounterType == config.CounterGauge && e.ObservedUsage != nil {
			needGauge = true
		}
		if res.CounterType == config.CounterAccumulator && e.AccumulatorUsage != nil {
			needAcc = true
		}
	}
	if needGauge {
		if gObs, gErr = e.callObserved(ctx, orgID); gErr != nil {
			slog.Error("reconcile_org_gauge_provider_unreachable", "org", orgID, "err", gErr)
		}
	}
	if needAcc {
		if aObs, aErr = e.callAccumulator(ctx, orgID); aErr != nil {
			slog.Error("reconcile_org_acc_provider_unreachable", "org", orgID, "err", aErr)
		}
	}

	now := e.now()
	for _, res := range defs {
		rk := res.ResourceKey
		entry := ReconcileEntry{CounterType: string(res.CounterType)}
		if res.CounterType == config.CounterRate {
			entry.Status = "skipped_rate"
			report.Resources[rk] = entry
			continue
		}
		if !e.hasTruthSource(res) {
			entry.Status = "no_truth_source"
			report.Resources[rk] = entry
			continue
		}
		before, err := e.currentUsage(ctx, res, orgID, now)
		if err != nil {
			return report, err
		}
		entry.Before = before
		entry.After = before

		var trueVal float64
		var have bool
		source := sourceFor(res)
		switch res.CounterType {
		case config.CounterGauge:
			if gErr != nil {
				entry.Status = "truth_unavailable"
				report.Resources[rk] = entry
				continue
			}
			v, ok := gObs[rk]
			trueVal, have = float64(v), ok
		case config.CounterAccumulator:
			if aErr != nil {
				entry.Status = "truth_unavailable"
				report.Resources[rk] = entry
				continue
			}
			trueVal, have = aObs[rk]
		}
		if !have {
			entry.Status = "no_observation"
			report.Resources[rk] = entry
			continue
		}
		entry.Source = source
		entry.After = trueVal
		if math.Abs(trueVal-before) > driftEps {
			if rerr := e.repairCounterToTruth(ctx, res, orgID, trueVal); rerr != nil {
				return report, rerr
			}
			entry.Changed = true
			entry.Status = "repaired"
			e.emitDrift(ctx, orgID, res, source, before, trueVal)
		} else {
			entry.Status = "in_sync"
			if e.OnDriftResolved != nil {
				e.OnDriftResolved(ctx, orgID, rk)
			}
		}
		report.Resources[rk] = entry
	}
	return report, nil
}

// MaybeReadRepair (P2.2) is the throttled, best-effort read-repair hook. A
// consumer's usage/status handler calls it so a plain read opportunistically
// recomputes from truth and repairs the cache. No-op when no truth source is
// wired; throttled per-org; never returns/raises (a repair failure must not
// break a read).
func (e *Engine) MaybeReadRepair(ctx context.Context, orgID string) {
	if e.ObservedUsage == nil && e.AccumulatorUsage == nil {
		return
	}
	if e.ReadRepairThrottle > 0 {
		e.rrMu.Lock()
		if e.rrLast == nil {
			e.rrLast = map[string]time.Time{}
		}
		last, ok := e.rrLast[orgID]
		if ok && e.now().Sub(last) < e.ReadRepairThrottle {
			e.rrMu.Unlock()
			return
		}
		e.rrLast[orgID] = e.now()
		e.rrMu.Unlock()
	}
	if _, err := e.ReconcileOrg(ctx, orgID, ""); err != nil {
		slog.Warn("read_repair_failed", "org", orgID, "err", err)
	}
}
