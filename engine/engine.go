package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/activations"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

// ErrOverLimit is returned by Spend ONLY in the opt-in
// enforcement.legacy_increment="enforce" mode when a gauge spend would
// exceed the limit (D-24). The default mode never returns it (never refuses).
var ErrOverLimit = errors.New("engine: gauge spend would exceed limit (legacy_increment=enforce)")

// CheckInput is the per-request input to Engine.Check.
type CheckInput struct {
	UserID      string
	OrgID       string
	ResourceKey string
	// Cost is the proposed delta the caller wants to spend. Defaults to 1
	// for gauges/counters and to the literal $ amount for accumulators.
	Cost float64
}

// Engine performs quota checks. Constructed by quota.Setup.
type Engine struct {
	Cfg      *config.Config
	Reg      *registry.Registry
	Provider providers.Provider
	Factory  *counters.Factory
	Messages *messages.Builder
	Clock    func() time.Time

	// Activations is the store behind Acquire/Release/Settle (TASK P5.2).
	// nil ⇒ an in-memory store is used lazily (loud-not-durable default).
	Activations activations.Store

	// OnEvent is the observability sink for QuotaEvents (D-26). nil ⇒
	// events are still logged, just not forwarded. setup wires this to
	// alerts.Manager (dependency inverted — the engine never imports alerts).
	OnEvent EventSink

	// touch-tracking for the reconciler's recent-activity guard (D-62). A
	// (org, resource) touched (acquired/spent) within the guard window must
	// NEVER be force-set by the reconciler — the provider lags creation, so
	// converging a just-created resource down is under-count / phantom
	// headroom (the 20260626 production incident).
	touchMu sync.Mutex
	touches map[string]time.Time
}

func (e *Engine) recordTouch(org, rk string) {
	e.touchMu.Lock()
	if e.touches == nil {
		e.touches = map[string]time.Time{}
	}
	e.touches[org+"|"+rk] = e.now()
	e.touchMu.Unlock()
}

// RecentlyTouched reports whether (org, resourceKey) was touched within window.
// The reconciler's guard is built from this (engine.TouchGuard).
func (e *Engine) RecentlyTouched(org, rk string, window time.Duration) bool {
	e.touchMu.Lock()
	defer e.touchMu.Unlock()
	t, ok := e.touches[org+"|"+rk]
	return ok && e.now().Sub(t) < window
}

// TouchGuard returns a recent-activity guard closure bound to this engine's
// touch-tracking, for reconcile.Reconciler.RecentlyTouched (D-62). Zero-config:
// a consumer that wires the engine gets the guard without supplying anything.
func (e *Engine) TouchGuard(window time.Duration) func(org, rk string) bool {
	return func(org, rk string) bool { return e.RecentlyTouched(org, rk, window) }
}

// Check runs the quota decision for a single resource. Returns a Result
// the middleware can serialize.
func (e *Engine) Check(ctx context.Context, in CheckInput) (Result, error) {
	if in.ResourceKey == "" {
		return Result{}, errors.New("engine: ResourceKey required")
	}
	if in.Cost == 0 {
		in.Cost = 1
	}
	now := e.now()

	// Global kill-switch — fail closed.
	if e.Cfg != nil && e.Cfg.Enforcement.GlobalKillSwitch {
		return Result{
			Decision: Deny,
			Reason:   "global_kill_switch",
			Resource: in.ResourceKey,
			Message:  "Quota enforcement halted by global kill switch.",
		}, nil
	}

	// Enforcement off → always allow without computing.
	if e.Cfg != nil && !e.Cfg.Enforcement.Enabled {
		return Result{
			Decision: Allow,
			Reason:   "enforcement_disabled",
			Resource: in.ResourceKey,
		}, nil
	}

	res, ok := e.Reg.Resource(in.ResourceKey)
	if !ok {
		return Result{}, fmt.Errorf("engine: unknown resource_key %q", in.ResourceKey)
	}

	tierID, err := e.Provider.GetTier(ctx, in.UserID, in.OrgID)
	if err != nil || tierID == "" {
		return Result{
			Decision: UnknownTier,
			Reason:   "tier_unresolved",
			Resource: in.ResourceKey,
			TierID:   tierID,
			Message:  e.Messages.UnknownTier(messages.Vars{Tier: tierID}),
		}, nil
	}

	tier, ok := e.Reg.Tier(tierID)
	if !ok {
		// QG-08 / D-14 — a mis-mapped org (a resolved tier id that is not in
		// the config) is a config error, NOT an admission. DENY + alert at the
		// gate; never a silent unlimited admit (the forbidden fail-OPEN). Not
		// softened by shadow_mode — an unknown tier is not a limit decision.
		e.emit(ctx, QuotaEvent{Kind: EventTierNotInConfig, OrgID: partitionOrg(in.UserID, in.OrgID), ResourceKey: in.ResourceKey})
		return Result{
			Decision: Deny,
			Reason:   "tier_not_in_config",
			Resource: in.ResourceKey,
			TierID:   tierID,
			Message:  e.Messages.UnknownTier(messages.Vars{Tier: tierID}),
		}, nil
	}

	limit, ok := tier.Limits[in.ResourceKey]
	if !ok || limit.IsUnlimited() {
		return Result{
			Decision: Allow,
			Reason:   "unlimited",
			Resource: in.ResourceKey,
			TierID:   tierID,
		}, nil
	}

	org := partitionOrg(in.UserID, in.OrgID)
	used, err := e.currentUsage(ctx, res, org, now)
	if err != nil {
		return Result{}, fmt.Errorf("engine: usage lookup: %w", err)
	}

	// Per-user enforcement (TASK P5.4 / finding QG-04). Python maintains a
	// per-user partition for GAUGES only; enforce against the derived
	// per-user limit before the org-level math. A user over their share is
	// denied even when the org is under its cap.
	if res.CounterType == config.CounterGauge && in.UserID != "" {
		if pul := limit.DerivePerUserLimit(tier.DefaultPerUserFraction); pul != nil {
			g := e.Factory.Gauge(res.ResourceKey)
			userUsed, _, uerr := e.Factory.Floats.GetFloat(ctx, g.UserKey(org, in.UserID))
			if uerr != nil {
				return Result{}, fmt.Errorf("engine: per-user usage lookup: %w", uerr)
			}
			if userUsed+in.Cost > *pul {
				r := Result{
					Resource: in.ResourceKey,
					TierID:   tierID,
					Used:     used,
					Limit:    limit.Limit,
					Decision: Deny,
					Reason:   "per_user_exceeded",
					Message: e.Messages.Denied(messages.Vars{
						Resource: in.ResourceKey, Limit: ftoa(*pul), Used: ftoa(userUsed),
						Tier: tierID, UpgradeURL: tier.UpgradeURL,
					}),
					UpgradeURL: tier.UpgradeURL,
				}
				if e.Cfg != nil && e.Cfg.Enforcement.ShadowMode {
					r.Decision = ShadowAllow
					r.Reason = "shadow_would_deny_per_user"
				}
				return r, nil
			}
		}
	}

	// Decision math.
	cap := *limit.Limit
	burst := limit.BurstAllowance
	proposed := used + in.Cost
	hard := cap + burst

	result := Result{
		Resource:   in.ResourceKey,
		TierID:     tierID,
		Used:       used,
		Limit:      limit.Limit,
		Burst:      burst,
		UpgradeURL: tier.UpgradeURL,
	}
	if cap > 0 {
		result.Threshold = proposed / cap
	}

	switch {
	case proposed > hard:
		result.Decision = Deny
		result.Reason = "exceeded"
		result.Message = e.Messages.Denied(messages.Vars{
			Resource: in.ResourceKey, Limit: ftoa(cap), Used: ftoa(used),
			Tier: tierID, UpgradeURL: tier.UpgradeURL,
		})
	case proposed > cap:
		// Within burst — log + allow.
		result.Decision = Allow
		result.Reason = "burst_consumed"
		result.Warning = true
		result.Message = e.Messages.OverBurst(messages.Vars{
			Resource: in.ResourceKey, Limit: ftoa(cap), Used: ftoa(used), Tier: tierID,
		})
	case limit.CriticalThreshold > 0 && result.Threshold >= limit.CriticalThreshold:
		result.Decision = Critical
		result.Reason = "near_critical"
		result.Critical = true
		result.Message = e.Messages.Critical(messages.Vars{
			Resource: in.ResourceKey, Limit: ftoa(cap), Used: ftoa(used), Tier: tierID,
		})
	case limit.WarningThreshold > 0 && result.Threshold >= limit.WarningThreshold:
		result.Decision = Warn
		result.Reason = "near_warning"
		result.Warning = true
		result.Message = e.Messages.Warning(messages.Vars{
			Resource: in.ResourceKey, Limit: ftoa(cap), Used: ftoa(used), Tier: tierID,
		})
	default:
		result.Decision = Allow
		result.Reason = "under_limit"
	}

	// Shadow mode — convert Deny to ShadowAllow.
	if result.Decision == Deny && e.Cfg.Enforcement.ShadowMode {
		result.Decision = ShadowAllow
		result.Reason = "shadow_would_deny"
		result.Message = e.Messages.ShadowAllowed(messages.Vars{
			Resource: in.ResourceKey, Limit: ftoa(cap), Used: ftoa(used),
		})
	}

	return result, nil
}

// Spend applies the cost after Check returned an Allow-ish decision. The
// caller is expected to call Check + Spend in pairs; the engine doesn't
// auto-spend because the caller may want a "preflight" check.
func (e *Engine) Spend(ctx context.Context, in CheckInput) (float64, error) {
	if in.Cost == 0 {
		in.Cost = 1
	}
	// W-T3 GT-02/GT-05 (D-31, Python parity — reference executed on fakeredis):
	// a non-finite Cost fails LOUD before any store op; a negative Cost is a
	// MAGNITUDE (Python increment(-3) adds 3). Pre-fix, Spend(-3) applied the
	// raw negative — ERASING spend, the forbidden silent direction, and a
	// silent cross-runtime divergence on a shared keyspace.
	if math.IsNaN(in.Cost) || math.IsInf(in.Cost, 0) {
		return 0, fmt.Errorf("engine: Spend Cost must be finite, got %v (D-31)", in.Cost)
	}
	rawCost := in.Cost
	in.Cost = math.Abs(in.Cost)
	res, ok := e.Reg.Resource(in.ResourceKey)
	if !ok {
		return 0, fmt.Errorf("engine: unknown resource_key %q", in.ResourceKey)
	}
	org := partitionOrg(in.UserID, in.OrgID)
	now := e.now()
	switch res.CounterType {
	case config.CounterAccumulator:
		a := e.Factory.Accumulator(res.ResourceKey, res.ResetPeriod)
		return a.AddOrg(ctx, org, now, in.Cost)
	case config.CounterGauge:
		g := e.Factory.Gauge(res.ResourceKey)
		orgLimit, userLimit, _ := e.gaugeLimit(ctx, org, in.UserID, res.ResourceKey)
		// D-24: the legacy Spend path NEVER refuses by default ("count at the
		// fact") — refusing after provisioning would leave a resource
		// existing-and-uncounted (phantom headroom). The opt-in "enforce"
		// mode atomically refuses BEFORE spending, for a consumer that has
		// verified it Spends before provisioning.
		if e.Cfg != nil && e.Cfg.Enforcement.LegacyIncrementMode() == "enforce" && orgLimit != nil {
			if acq, ok := e.Factory.Floats.(counters.GaugeAcquirer); ok {
				spec := counters.AcquireSpec{OrgKey: g.OrgKey(org), Delta: in.Cost, OrgLimit: orgLimit}
				if in.UserID != "" {
					spec.HasUser = true
					spec.UserKey = g.UserKey(org, in.UserID)
					spec.SeqKey = g.UserSeqKey(org, in.UserID)
				}
				out, aerr := acq.AtomicAcquire(ctx, "", false, 0, []counters.AcquireSpec{spec})
				if aerr != nil {
					return 0, aerr
				}
				if !out.Admitted {
					return 0, ErrOverLimit
				}
				v, _, _ := e.Factory.Floats.GetFloat(ctx, g.OrgKey(org))
				return v, nil
			}
		}
		// Org gauge is authoritative for the returned value; the per-user
		// partition (TASK P5.4) is maintained alongside it, mirroring
		// Python (gauge.py:42-46). Best-effort on the user partition — an
		// error there must not lose the org increment already applied.
		newVal, err := e.Factory.Floats.IncrByFloat(ctx, g.OrgKey(org), in.Cost)
		if err != nil {
			return 0, err
		}
		e.recordTouch(org, res.ResourceKey) // recent-activity guard (D-62)
		var userVal float64
		if in.UserID != "" {
			uv, uerr := e.Factory.Floats.IncrByFloat(ctx, g.UserKey(org, in.UserID), in.Cost)
			if uerr != nil {
				return newVal, fmt.Errorf("engine: user partition increment: %w", uerr)
			}
			userVal = uv
		}
		// D-24 count_and_alert: crossing the limit is an OBSERVABLE fact
		// (D-26), not a silent undercount. Emit over_limit_admitted → the
		// sink (alerts) via OnEvent. Both org and per-user scopes.
		if orgLimit != nil && crossedUp(newVal, in.Cost, *orgLimit) {
			e.emit(ctx, QuotaEvent{Kind: EventOverLimitAdmitted, OrgID: org, ResourceKey: res.ResourceKey,
				Scope: "org", Level: newVal, Limit: *orgLimit})
		}
		if in.UserID != "" && userLimit != nil && crossedUp(userVal, in.Cost, *userLimit) {
			e.emit(ctx, QuotaEvent{Kind: EventOverLimitAdmitted, OrgID: org, UserID: in.UserID, ResourceKey: res.ResourceKey,
				Scope: "user", Level: userVal, Limit: *userLimit})
		}
		return newVal, nil
	case config.CounterRate:
		r := e.Factory.Rate(res)
		// W-T3 GT-04 (Python parity, reference executed): Cost is an EVENT
		// COUNT — rate.increment(3) records int(3) members. Pre-fix Go
		// recorded exactly ONE regardless, under-counting ⇒ the rate limit
		// was silently WIDER in Go than in Python on a shared keyspace
		// (D-31's forbidden direction). Python truncation semantics kept
		// exactly: int(0.5) == 0 events; a negative raw Cost records none
		// (rawCost predates the gauge magnitude normalisation above).
		n := int(rawCost)
		for i := 0; i < n; i++ {
			member := strconv.FormatInt(now.UnixNano(), 10) + ":" + strconv.Itoa(i)
			if err := r.RecordOrg(ctx, org, now, member); err != nil {
				return 0, err
			}
		}
		count, err := r.CountOrg(ctx, org, now)
		return float64(count), err
	}
	return 0, fmt.Errorf("engine: unsupported counter_type %q", res.CounterType)
}

// Release decrements a gauge — for instance, when a sandbox container
// exits. No-op for non-gauge resources.
func (e *Engine) Release(ctx context.Context, in CheckInput) error {
	if in.Cost == 0 {
		in.Cost = 1
	}
	// W-T3 GT-03/GT-05 (D-31, Python parity): Cost is a MAGNITUDE — Python's
	// decrement(-2) decrements by 2. Pre-fix, Release(Cost=-2) INCREASED the
	// gauge (DecrByFloorZero negates), silently diverging from a Python fleet
	// on the same call. Non-finite fails loud.
	if math.IsNaN(in.Cost) || math.IsInf(in.Cost, 0) {
		return fmt.Errorf("engine: Release Cost must be finite, got %v (D-31)", in.Cost)
	}
	in.Cost = math.Abs(in.Cost)
	res, ok := e.Reg.Resource(in.ResourceKey)
	if !ok {
		return fmt.Errorf("engine: unknown resource_key %q", in.ResourceKey)
	}
	if res.CounterType != config.CounterGauge {
		return nil
	}
	org := partitionOrg(in.UserID, in.OrgID)
	g := e.Factory.Gauge(res.ResourceKey)
	orgLimit, userLimit, _ := e.gaugeLimit(ctx, org, in.UserID, res.ResourceKey)
	// Floor at zero atomically — a gauge must never go negative or it
	// manufactures free quota headroom (finding QG-06).
	newOrg, err := e.Factory.Floats.DecrByFloorZero(ctx, g.OrgKey(org), in.Cost)
	if err != nil {
		return err
	}
	// D-26 de-escalation: if this release brought the gauge back to/under
	// its limit, emit the paired over_limit_resolved so a self-healed
	// over-admission leaves a recovery trail (not an alert that never clears).
	if orgLimit != nil && crossedDown(newOrg, in.Cost, *orgLimit) {
		e.emit(ctx, QuotaEvent{Kind: EventOverLimitResolved, OrgID: org, ResourceKey: res.ResourceKey,
			Scope: "org", Level: newOrg, Limit: *orgLimit})
	}
	if in.UserID != "" {
		newUser, err := e.Factory.Floats.DecrByFloorZero(ctx, g.UserKey(org, in.UserID), in.Cost)
		if err != nil {
			return fmt.Errorf("engine: user partition decrement: %w", err)
		}
		if userLimit != nil && crossedDown(newUser, in.Cost, *userLimit) {
			e.emit(ctx, QuotaEvent{Kind: EventOverLimitResolved, OrgID: org, UserID: in.UserID, ResourceKey: res.ResourceKey,
				Scope: "user", Level: newUser, Limit: *userLimit})
		}
	}
	return nil
}

func (e *Engine) currentUsage(ctx context.Context, res config.ResourceDef, org string, now time.Time) (float64, error) {
	switch res.CounterType {
	case config.CounterAccumulator:
		a := e.Factory.Accumulator(res.ResourceKey, res.ResetPeriod)
		return a.GetOrg(ctx, org, now)
	case config.CounterGauge:
		g := e.Factory.Gauge(res.ResourceKey)
		v, _, err := e.Factory.Floats.GetFloat(ctx, g.OrgKey(org))
		return v, err
	case config.CounterRate:
		r := e.Factory.Rate(res)
		c, err := r.CountOrg(ctx, org, now)
		return float64(c), err
	}
	return 0, fmt.Errorf("engine: unsupported counter_type %q", res.CounterType)
}

func (e *Engine) now() time.Time {
	if e.Clock != nil {
		return e.Clock()
	}
	return time.Now()
}

// partitionOrg returns the value that fills the {org} slot of the Python
// key shape quota:{org}:{rk}:... (TASK P5.3). The org id is the partition;
// when it is empty (a userless/workspace call) the user id stands in so the
// partition stays stable. Per-user sub-partitioning is handled separately
// via Gauge.UserKey (TASK P5.4).
func partitionOrg(userID, orgID string) string {
	if orgID != "" {
		return orgID
	}
	return userID
}

// orgScope is the DEPRECATED pre-P5.3 scope helper (produced the
// non-parity "org:{org}" scope for the old counter builders). Retained only
// until D-13 retires the deprecated scope-based builders + their peer
// tests. Not used by production paths.
//
// Deprecated: use partitionOrg + the *Org key builders (finding QG-03).
func orgScope(userID, orgID string) string {
	if orgID != "" {
		return "org:" + orgID
	}
	return "user:" + userID
}

func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
