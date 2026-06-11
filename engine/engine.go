package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

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
	Cfg        *config.Config
	Reg        *registry.Registry
	Provider   providers.Provider
	Factory    *counters.Factory
	Messages   *messages.Builder
	Clock      func() time.Time
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
		return Result{
			Decision: UnknownTier,
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

	scope := orgScope(in.UserID, in.OrgID)
	used, err := e.currentUsage(ctx, res, scope, now)
	if err != nil {
		return Result{}, fmt.Errorf("engine: usage lookup: %w", err)
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
	res, ok := e.Reg.Resource(in.ResourceKey)
	if !ok {
		return 0, fmt.Errorf("engine: unknown resource_key %q", in.ResourceKey)
	}
	scope := orgScope(in.UserID, in.OrgID)
	now := e.now()
	switch res.CounterType {
	case config.CounterAccumulator:
		a := e.Factory.Accumulator(res.ResourceKey, res.ResetPeriod)
		return a.Add(ctx, scope, now, in.Cost)
	case config.CounterGauge:
		g := e.Factory.Gauge(res.ResourceKey)
		return e.Factory.Floats.IncrByFloat(ctx, g.Key(scope), in.Cost)
	case config.CounterRate:
		r := e.Factory.Rate(res)
		member := strconv.FormatInt(now.UnixNano(), 10)
		if err := r.Record(ctx, scope, now, member); err != nil {
			return 0, err
		}
		count, err := r.Count(ctx, scope, now)
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
	res, ok := e.Reg.Resource(in.ResourceKey)
	if !ok {
		return fmt.Errorf("engine: unknown resource_key %q", in.ResourceKey)
	}
	if res.CounterType != config.CounterGauge {
		return nil
	}
	g := e.Factory.Gauge(res.ResourceKey)
	_, err := e.Factory.Floats.IncrByFloat(ctx, g.Key(orgScope(in.UserID, in.OrgID)), -in.Cost)
	return err
}

func (e *Engine) currentUsage(ctx context.Context, res config.ResourceDef, scope string, now time.Time) (float64, error) {
	switch res.CounterType {
	case config.CounterAccumulator:
		a := e.Factory.Accumulator(res.ResourceKey, res.ResetPeriod)
		return a.Get(ctx, scope, now)
	case config.CounterGauge:
		g := e.Factory.Gauge(res.ResourceKey)
		v, _, err := e.Factory.Floats.GetFloat(ctx, g.Key(scope))
		return v, err
	case config.CounterRate:
		r := e.Factory.Rate(res)
		c, err := r.Count(ctx, scope, now)
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

// orgScope is the canonical scope string. Per-user metering uses the
// user-scoped variant; for now we use org-level.
func orgScope(userID, orgID string) string {
	if orgID != "" {
		return "org:" + orgID
	}
	return "user:" + userID
}

func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
