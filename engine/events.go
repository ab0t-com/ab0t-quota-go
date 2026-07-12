package engine

// D-26 — the observability SINK for over-admission. An event with no
// consumer is a comment that costs CPU; D-24 B's entire premise is that
// over-admission becomes an OBSERVABLE fact, so the event needs a sink.
//
// Dependency inversion (no import cycle): the ENGINE owns this publish seam
// and calls OnEvent; `setup` (which may import alerts) SUBSCRIBES and
// forwards to alerts.Manager. The engine never imports alerts.

import (
	"context"
	"log/slog"
)

// EventKind enumerates the quota lifecycle events published to OnEvent.
type EventKind string

const (
	// EventOverLimitAdmitted: a legacy Spend counted a resource past its
	// limit (count_and_alert — D-24 B). Over-admission made observable.
	EventOverLimitAdmitted EventKind = "over_limit_admitted"
	// EventTierNotInConfig: an admission gate refused a resolved-but-unknown
	// tier (a mis-mapped org) — a config error made observable (D-14/QG-08).
	EventTierNotInConfig EventKind = "tier_not_in_config"
	// EventSettleConflict: a re-settle arrived with a cost that differs from
	// the first (idempotent) settle — first wins, but the caller's cost
	// mismatch is loud, never silently discarded (D-46/QG-11).
	EventSettleConflict EventKind = "settle_conflict"
	// EventOverLimitResolved: a gauge that WAS over its limit dropped back
	// to/under it (a release or a reconciler heal). The paired
	// de-escalation signal (FUTURE §5) so a self-healed drift leaves a
	// "resolved" trail and an alert can be seen to recover — not ignored.
	EventOverLimitResolved EventKind = "over_limit_resolved"
)

// QuotaEvent is one published observability event.
type QuotaEvent struct {
	Kind        EventKind
	OrgID       string
	UserID      string // "" for org-scope events
	ResourceKey string
	Scope       string  // "org" | "user"
	Level       float64 // the resulting counter level
	Limit       float64 // the limit that was crossed
}

// EventSink receives published QuotaEvents. Wired by setup to alerts.Manager.
type EventSink func(context.Context, QuotaEvent)

// emit publishes an event. It ALWAYS logs (so the signal exists even with no
// sink) and forwards to OnEvent when a subscriber is wired.
func (e *Engine) emit(ctx context.Context, ev QuotaEvent) {
	if ev.Kind == EventOverLimitResolved {
		slog.Info(string(ev.Kind), "org", ev.OrgID, "user", ev.UserID,
			"resource", ev.ResourceKey, "scope", ev.Scope, "level", ev.Level, "limit", ev.Limit)
	} else {
		slog.Warn(string(ev.Kind), "org", ev.OrgID, "user", ev.UserID,
			"resource", ev.ResourceKey, "scope", ev.Scope, "level", ev.Level, "limit", ev.Limit,
			"note", "over-admission is an observable fact (D-24 B); migrate to Acquire for enforcement")
	}
	if e.OnEvent != nil {
		e.OnEvent(ctx, ev)
	}
}

// crossedUp reports whether adding `delta` took the value from ≤limit to >limit.
func crossedUp(newVal, delta, limit float64) bool {
	return newVal > limit && newVal-delta <= limit
}

// crossedDown reports whether subtracting `delta` took the value from >limit to ≤limit.
func crossedDown(newVal, delta, limit float64) bool {
	return newVal <= limit && newVal+delta > limit
}
