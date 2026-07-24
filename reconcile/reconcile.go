// Package reconcile is the Go port of the library reconciler + the precedence
// law (D-33/D-36). Ticket 20260709. It heals gauge drift by converging the
// Redis counter (a cache) to the authoritative source, and it makes
// un-billable usage an OBSERVABLE money incident rather than silent drift.
//
// The precedence law (D-33), three layers each authoritative over ONE thing:
//
//	observed_usage_provider  → EXISTENCE (what is actually live)
//	activation ledger        → IDENTITY + COST (which activation, whose, how much)
//	Redis counter            → nothing; it is a cache of the level
//
// Consequences the reconciler implements:
//   - No provider → converge counter to Σ open activations (the zero-config heal).
//   - Provider configured + disagreement → a BUG, not drift: converge to the
//     PROVIDER's observed set, ALERT ("N live with no activation record —
//     unsettleable usage"), and NEVER fabricate ledger rows (D-36).
//   - Fail-direction (D-31): provider unreachable → do NOTHING and alert; never
//     converge to the ledger as a fallback (that erases reality when the record
//     is what's broken).
//   - Gauges only. Accumulators are never reconciled.
//
// Refuse gate (D-37/D-39): the reconciler refuses to run on a NON-durable
// activation ledger — reconciling from a per-process / evictable partial view
// under-counts a shared counter (the forbidden direction). Absence of a
// positive durability signal is treated as non-durable (D-51).
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

// ObservedUsageProvider returns the consumer's PRODUCT-STATE truth: the count
// of actually-live resources per resource_key for an org (EXISTENCE only —
// never identity or cost). nil ⇒ no provider (ledger-only reconciliation).
type ObservedUsageProvider func(ctx context.Context, orgID string) (map[string]int, error)

// DriftAlert is a money-incident signal. UnsettleableLive > 0 means resources
// are live with no activation record — their usage cannot be settled (QB-01's
// signature). It must read as a money incident, not a counter nit (D-36).
type DriftAlert struct {
	OrgID            string
	ResourceKey      string
	Source           string // which source won: "activations" | "provider" | "provider_unreachable"
	CounterWas       float64
	ConvergedTo      float64
	UnsettleableLive int
}

// Reconciler converges gauge counters to the authoritative source.
type Reconciler struct {
	Store    activations.Store
	Factory  *counters.Factory
	Reg      *registry.Registry
	Provider ObservedUsageProvider // nil ⇒ ledger-only
	OnDrift  func(DriftAlert)      // wire to alerts (money incident)
	// Preflight (D-75) RE-VERIFIES the library's infrastructure invariants once per loop
	// pass — Redis topology / eviction policy / scripting / version / headroom, and the DDB
	// tables. It rides THIS loop deliberately: a new worker is one more thing that can be
	// dead (D-50). "An assumption machine-checked once is an assumption trusted thereafter":
	// every boot guard we own (D-32/D-71/D-72/D-73/D-76) verified the world once and then
	// trusted it forever, so a `CONFIG SET maxmemory-policy allkeys-lru` at 3am — or a
	// managed failover onto a differently-configured replica — was invisible, and the counter
	// became silently evictable (under-count → phantom headroom → over-admission, D-31).
	// A safe→unsafe transition here is LOUD, NOT FATAL: degrade health + alert, never crash —
	// a running service that suddenly refuses is its own outage. nil ⇒ no re-verification.
	Preflight func(ctx context.Context)
	// RecentlyTouched guards against force-setting a (org,rk) touched within
	// the guard window (the provider lags creation — reality wins, but not
	// instantly, D-33 pt3). nil ⇒ no guard.
	RecentlyTouched func(orgID, resourceKey string) bool
	Clock           func() time.Time
}

// ErrNonDurableLedger is returned when the reconciler is asked to run on a
// ledger that is not proven durable (D-37/D-39).
var ErrNonDurableLedger = errors.New("reconcile: refusing to run on a NON-durable activation ledger — reconciling from a partial view under-counts a shared counter (D-37/D-39); wire a durable (DDB) activation store")

// ErrNoRecentActivityGuard is returned when the reconciler has no
// recent-activity guard (D-62). A nil guard is NOT "no guard configured" — it
// is "reconcile against a provider that lags reality", which force-sets a
// just-created resource down to zero on every fast create → under-count →
// phantom headroom (the 20260626 production incident). The guard is not a
// knob; only its WINDOW is.
var ErrNoRecentActivityGuard = errors.New("reconcile: refusing to run without a recent-activity guard — a provider lags creation, so converging a just-created (org,resource) down is under-count/phantom headroom (D-62); wire RecentlyTouched (engine.TouchGuard)")

// GuardOK reports whether a recent-activity guard is wired (D-62).
func (r *Reconciler) GuardOK() (bool, string) {
	if r.RecentlyTouched == nil {
		return false, "no recent-activity guard — reconciling against a provider that lags reality under-counts on every fast create (D-62)"
	}
	return true, "guarded"
}

// Durability reports whether the activation ledger is proven durable, and why.
// Absence of a positive Durable() signal is non-durable (D-51).
func (r *Reconciler) Durability() (bool, string) {
	d, ok := r.Store.(interface{ Durable() bool })
	if !ok {
		return false, "activation store does not report durability — treated as NON-durable (D-51)"
	}
	if !d.Durable() {
		return false, "activation store is not durable (in-memory / unconfirmed cache) — D-37/D-39"
	}
	return true, "durable"
}

// ReconcileOrg converges every GAUGE resource for an org. Refuses on a
// non-durable ledger. Returns the alerts it raised.
func (r *Reconciler) ReconcileOrg(ctx context.Context, orgID string) ([]DriftAlert, error) {
	if durable, why := r.Durability(); !durable {
		slog.Error("reconciler_refused", "org", orgID, "reason", why)
		return nil, fmt.Errorf("%w: %s", ErrNonDurableLedger, why)
	}
	if ok, why := r.GuardOK(); !ok {
		slog.Error("reconciler_refused", "org", orgID, "reason", why)
		return nil, fmt.Errorf("%w: %s", ErrNoRecentActivityGuard, why)
	}

	// Σ open activations per resource_key (the ledger's per-resource open set).
	opens, err := r.Store.ListOpen(ctx, orgID, 100000)
	if err != nil {
		return nil, err
	}
	openByRK := map[string]int{}
	for _, a := range opens {
		for rk := range a.Spend {
			openByRK[rk]++
		}
	}

	// Provider observed set (EXISTENCE). On error → do nothing + alert (D-31).
	var observed map[string]int
	providerFailed := false
	if r.Provider != nil {
		observed, err = r.Provider(ctx, orgID)
		if err != nil {
			providerFailed = true
			slog.Error("reconciler_provider_unreachable", "org", orgID, "err", err,
				"note", "doing NOTHING and alerting — will not converge to the ledger (that erases reality when the record is broken, D-31)")
		}
	}

	var alerts []DriftAlert
	for _, res := range r.Reg.AllResources() {
		if res.CounterType != config.CounterGauge {
			continue // accumulators/rates are never reconciled (D-33)
		}
		rk := res.ResourceKey
		if r.RecentlyTouched != nil && r.RecentlyTouched(orgID, rk) {
			continue // reality wins, but not instantly (D-33 pt3)
		}
		if providerFailed {
			alerts = append(alerts, r.emit(DriftAlert{OrgID: orgID, ResourceKey: rk, Source: "provider_unreachable"}))
			continue
		}

		openN := openByRK[rk]
		var target float64
		var source string
		unsettleable := 0
		if r.Provider != nil {
			// QG-09 / D-51 — absence is UNKNOWN, not zero. A missing key in the
			// observed map is "the provider made no observation for this
			// resource", NOT "zero live". Reading it as an affirmative 0 would
			// converge a live gauge DOWN to 0 — under-count / phantom headroom,
			// the forbidden direction. Only an EXPLICIT value converges; absence
			// SKIPS (counter untouched) + alerts under a distinct source.
			obs, present := observed[rk]
			if !present {
				slog.Warn("reconcile_skip_provider_absent", "org", orgID, "resource", rk,
					"note", "provider returned no observation for this resource — absence is UNKNOWN, not zero (D-51); gauge untouched")
				alerts = append(alerts, r.emit(DriftAlert{OrgID: orgID, ResourceKey: rk, Source: "provider_absent"}))
				continue
			}
			target, source = float64(obs), "provider"
			if obs != openN {
				// Disagreement = a BUG (the record lost or never had a row).
				// Converge to the provider (reality), NEVER fabricate a row.
				if obs > openN {
					unsettleable = obs - openN
				}
			} else {
				target, source = float64(openN), "activations"
			}
		} else {
			target, source = float64(openN), "activations"
		}

		// K-8: derive keys through the FACTORY so the reconciler reads/writes
		// the engine's declared keyspace (a v2-blind reconciler "heals" the
		// shape nobody reads, spec §6.3). Dual: read with fallback, force-set
		// BOTH shapes (Python gauge.reset parity).
		g := r.Factory.Gauge(rk)
		pair := g.OrgKeyPair(orgID)
		cur, _, _ := counters.GetFloatDual(ctx, r.Factory.Floats, pair)
		if cur == target && unsettleable == 0 {
			continue // no drift, nothing to alert
		}
		// Force-set the cache to the authoritative level; log which source won.
		if err := r.Factory.Floats.Set(ctx, pair.P, target, 0); err != nil {
			return alerts, err
		}
		if pair.S != "" {
			if err := r.Factory.Floats.Set(ctx, pair.S, target, 0); err != nil {
				return alerts, err
			}
		}
		slog.Info("reconciled_gauge", "org", orgID, "resource", rk, "was", cur, "converged_to", target, "source", source)
		alerts = append(alerts, r.emit(DriftAlert{
			OrgID: orgID, ResourceKey: rk, Source: source,
			CounterWas: cur, ConvergedTo: target, UnsettleableLive: unsettleable,
		}))
	}
	return alerts, nil
}

// RunLoop reconciles on an interval until ctx is cancelled. `orgs` supplies
// the orgs to reconcile each pass (the consumer's seam — the library cannot
// enumerate orgs). It FAILS LOUD and does not start when misconfigured (a
// non-durable ledger, no guard, or no org source) rather than becoming a
// silently-dead worker (D-50); Setup reflects the reason in Capabilities. Each
// pass wraps in a recover so one bad org cannot kill the loop.
func (r *Reconciler) RunLoop(ctx context.Context, interval time.Duration, orgs func() []string) {
	if durable, why := r.Durability(); !durable {
		slog.Error("reconciler NOT started", "reason", why)
		return
	}
	if ok, why := r.GuardOK(); !ok {
		slog.Error("reconciler NOT started", "reason", why)
		return
	}
	if orgs == nil {
		slog.Error("reconciler NOT started", "reason", "no org source (RunLoop orgs is nil)")
		return
	}
	if interval <= 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	slog.Info("reconciler loop started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// D-75: re-verify the world BEFORE reconciling against it, every pass.
			if r.Preflight != nil {
				r.Preflight(ctx)
			}
			r.reconcilePassLoud(ctx, orgs())
		}
	}
}

func (r *Reconciler) reconcilePassLoud(ctx context.Context, orgs []string) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("reconciler_PANIC", "panic", rec, "note", "recovered; a recurring panic is a DEAD reconciler — investigate")
		}
	}()
	for _, org := range orgs {
		if _, err := r.ReconcileOrg(ctx, org); err != nil {
			slog.Error("reconcile_org_error", "org", org, "err", err)
		}
	}
}

func (r *Reconciler) emit(a DriftAlert) DriftAlert {
	if a.UnsettleableLive > 0 {
		slog.Error("MONEY_INCIDENT_unsettleable_usage", "org", a.OrgID, "resource", a.ResourceKey,
			"live_without_activation", a.UnsettleableLive,
			"note", "resources are live with NO activation record — their usage cannot be settled (QB-01 signature)")
	}
	if r.OnDrift != nil {
		r.OnDrift(a)
	}
	return a
}
