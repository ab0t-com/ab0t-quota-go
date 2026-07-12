// Package alerts dispatches threshold-crossing notifications.
//
// Two backends:
//
//	log     — slog.Warn / slog.Error
//	webhook — POST to a consumer-configured URL (SSRF-guarded)
//
// The default is log-only. Webhook is enabled when AlertsConfig.WebhookURL
// is set and passes the SSRF guard.
package alerts

import (
	"context"
	"sync"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/engine"
)

// Manager is the dispatch point. Engine code calls Notify on every Result
// where Decision is Warn or Critical; Manager applies cooldowns + fanout.
type Manager struct {
	Cfg        config.AlertsConfig
	Dispatcher Dispatcher

	mu       sync.Mutex
	lastSent map[string]time.Time
}

// Dispatcher writes an alert somewhere.
type Dispatcher interface {
	Send(ctx context.Context, level Level, r engine.Result) error
}

// Level is the alert severity.
type Level string

const (
	LevelWarning  Level = "warning"
	LevelCritical Level = "critical"
)

// NewManager constructs a Manager. Use LogDispatcher{} as the dispatcher
// for plain slog; chain dispatchers via Multi.
func NewManager(cfg config.AlertsConfig, d Dispatcher) *Manager {
	if cfg.CooldownSeconds == 0 {
		cfg.CooldownSeconds = 3600
	}
	if cfg.WarningThreshold == 0 {
		cfg.WarningThreshold = 0.80
	}
	if cfg.CriticalThreshold == 0 {
		cfg.CriticalThreshold = 0.95
	}
	return &Manager{Cfg: cfg, Dispatcher: d, lastSent: map[string]time.Time{}}
}

// Notify dispatches an alert if cooldown allows. Idempotent under bursts.
func (m *Manager) Notify(ctx context.Context, r engine.Result) {
	if !m.Cfg.Enabled || m.Dispatcher == nil {
		return
	}
	var level Level
	switch r.Decision {
	case engine.Critical:
		level = LevelCritical
	case engine.Warn:
		level = LevelWarning
	default:
		return
	}
	key := string(level) + ":" + r.Resource + ":" + r.TierID
	m.mu.Lock()
	last, seen := m.lastSent[key]
	cool := time.Duration(m.Cfg.CooldownSeconds) * time.Second
	if seen && time.Since(last) < cool {
		m.mu.Unlock()
		return
	}
	m.lastSent[key] = time.Now()
	m.mu.Unlock()
	_ = m.Dispatcher.Send(ctx, level, r)
}

// NotifyQuotaEvent is the SINK for engine over-admission events (D-26). The
// engine publishes QuotaEvents to a callback it owns; setup subscribes and
// forwards here — so the "over_limit_admitted" event that D-24 B relies on
// actually reaches operators instead of being a log line nothing reads.
//
// over_limit_admitted → critical; over_limit_resolved → warning (the paired
// de-escalation, so a healed drift clears rather than alarming forever).
// Cooldown is keyed per (kind, resource, scope, org) so a burst is deduped
// but a resolve is NOT suppressed by a prior admit (distinct kind).
func (m *Manager) NotifyQuotaEvent(ctx context.Context, ev engine.QuotaEvent) {
	if !m.Cfg.Enabled || m.Dispatcher == nil {
		return
	}
	level := LevelCritical
	if ev.Kind == engine.EventOverLimitResolved {
		level = LevelWarning
	}
	key := string(ev.Kind) + ":" + ev.ResourceKey + ":" + ev.Scope + ":" + ev.OrgID + ":" + ev.UserID
	m.mu.Lock()
	last, seen := m.lastSent[key]
	cool := time.Duration(m.Cfg.CooldownSeconds) * time.Second
	if seen && time.Since(last) < cool {
		m.mu.Unlock()
		return
	}
	m.lastSent[key] = time.Now()
	m.mu.Unlock()
	_ = m.Dispatcher.Send(ctx, level, engine.Result{
		Resource: ev.ResourceKey,
		Used:     ev.Level,
		Reason:   string(ev.Kind),
		Message:  string(ev.Kind) + " scope=" + ev.Scope,
	})
}

// NotifyDrift is the sink for reconciler money-incidents (D-40/D-62). A
// positive `unsettleableLive` (resources live with no activation record) is a
// CRITICAL money incident — usage that cannot be settled (QB-01's signature).
// Primitive args so `alerts` need not import `reconcile`.
func (m *Manager) NotifyDrift(ctx context.Context, orgID, resourceKey, source string, unsettleableLive int) {
	if !m.Cfg.Enabled || m.Dispatcher == nil {
		return
	}
	level := LevelWarning
	if unsettleableLive > 0 {
		level = LevelCritical
	}
	key := "drift:" + resourceKey + ":" + source + ":" + orgID
	m.mu.Lock()
	last, seen := m.lastSent[key]
	if seen && time.Since(last) < time.Duration(m.Cfg.CooldownSeconds)*time.Second {
		m.mu.Unlock()
		return
	}
	m.lastSent[key] = time.Now()
	m.mu.Unlock()
	_ = m.Dispatcher.Send(ctx, level, engine.Result{Resource: resourceKey, Reason: "drift_" + source,
		Message: "reconciler converged " + resourceKey + " (source=" + source + ")"})
}

// NotifyVoid is the sink for outbox past-horizon/undeliverable settlement voids
// (D-12/D-40): an activation that will not settle — un-billable usage.
// NotifyInvariantViolated (D-75) — an infrastructure invariant the library VERIFIED AT
// STARTUP has changed underneath us (someone flipped maxmemory-policy to allkeys-lru; a
// managed failover landed on a clustered endpoint). The counter is now unsafe, and the
// re-check that caught it needs a HUMAN at the other end (D-40: an event with no sink is not
// observability). CRITICAL — this is the over-admission path. Rate-limited by the same
// cooldown as every other alert; the paired restore below is NOT (a heal must never be
// swallowed by a prior violation — D-26's resolve trail).
func (m *Manager) NotifyInvariantViolated(ctx context.Context, capability, detail string) {
	if !m.Cfg.Enabled || m.Dispatcher == nil {
		return
	}
	key := "invariant:" + capability
	m.mu.Lock()
	last, seen := m.lastSent[key]
	if seen && time.Since(last) < time.Duration(m.Cfg.CooldownSeconds)*time.Second {
		m.mu.Unlock()
		return
	}
	m.lastSent[key] = time.Now()
	m.mu.Unlock()
	_ = m.Dispatcher.Send(ctx, LevelCritical, engine.Result{
		Resource: capability,
		Reason:   "infrastructure_invariant_violated",
		Message: "infrastructure_invariant_violated capability=" + capability + " detail=" + detail +
			" — the library verified this at STARTUP and it has CHANGED at runtime (D-75). Health is " +
			"degraded; the service keeps serving. Drain or fix.",
	})
}

// NotifyInvariantRestored (D-75) — the paired all-clear. An on-call engineer who saw the
// violation must also see the heal, or the next one gets ignored. Clears the cooldown so a
// re-violation alerts immediately.
func (m *Manager) NotifyInvariantRestored(ctx context.Context, capability string) {
	if !m.Cfg.Enabled || m.Dispatcher == nil {
		return
	}
	m.mu.Lock()
	delete(m.lastSent, "invariant:"+capability)
	m.mu.Unlock()
	_ = m.Dispatcher.Send(ctx, LevelWarning, engine.Result{
		Resource: capability,
		Reason:   "infrastructure_invariant_restored",
		Message:  "infrastructure_invariant_restored capability=" + capability + " (D-75)",
	})
}

func (m *Manager) NotifyVoid(ctx context.Context, reservationID, eventType, reason string) {
	if !m.Cfg.Enabled || m.Dispatcher == nil {
		return
	}
	key := "void:" + reservationID + ":" + eventType
	m.mu.Lock()
	last, seen := m.lastSent[key]
	if seen && time.Since(last) < time.Duration(m.Cfg.CooldownSeconds)*time.Second {
		m.mu.Unlock()
		return
	}
	m.lastSent[key] = time.Now()
	m.mu.Unlock()
	_ = m.Dispatcher.Send(ctx, LevelCritical, engine.Result{Resource: eventType, Reason: "settlement_voided_" + reason,
		Message: "settlement VOIDED reservation=" + reservationID + " reason=" + reason + " — usage may be unbilled"})
}

// Multi chains dispatchers; the first error stops fanout. Use this when
// you want log + webhook.
type Multi []Dispatcher

// Send fans out to each child Dispatcher.
func (m Multi) Send(ctx context.Context, level Level, r engine.Result) error {
	for _, d := range m {
		if err := d.Send(ctx, level, r); err != nil {
			return err
		}
	}
	return nil
}
