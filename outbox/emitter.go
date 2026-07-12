package outbox

// The emitter + drain. Money-bearing lifecycle events go through the durable
// store: write intent FIRST, publish, mark delivered — else leave PENDING for
// drain (D-29). Drain reads FROM THE STORE, voids past-horizon events + alerts
// (D-12), and is bounded per pass (a delivery storm after an outage must not
// become a self-inflicted throughput incident).

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Publisher delivers one event to the transport (SNS in prod). The library
// core is transport-agnostic; the consumer wires this. A nil publisher means
// "no transport configured" → the event is fundamentally undeliverable and is
// voided+alerted (never silently dropped).
type Publisher interface {
	Publish(ctx context.Context, r Record) error
}

// VoidEntry is an alert-mirror row for an explicitly voided (unsettleable) event.
type VoidEntry struct {
	ReservationID string
	EventType     string
	Reason        string
}

// Emitter owns the durable outbox lifecycle.
type Emitter struct {
	store           Store
	pub             Publisher
	maxRetryHorizon float64 // seconds (D-12)
	pastHorizon     string  // "void_and_alert" (default) | "drop" (never in prod)

	mu         sync.Mutex
	voidLedger []VoidEntry
	killed     bool

	// D-12 (the CALLER leg): the settlement fallback. An undeliverable money event is
	// SETTLED against billing and only voided if it genuinely CANNOT settle. A nil settler
	// preserves the old void-and-alert behaviour exactly. See settle.go.
	settler        Settler
	settledLedger  []SettleEntry
	settleFailures int

	// OnVoid is an optional sink (wire to alerts) — a void is a money incident.
	OnVoid func(VoidEntry)
}

// NewEmitter builds an emitter. maxRetryHorizon<=0 → 900s. pastHorizon "" →
// "void_and_alert".
func NewEmitter(store Store, pub Publisher, maxRetryHorizon float64, pastHorizon string) *Emitter {
	if maxRetryHorizon <= 0 {
		maxRetryHorizon = 900
	}
	if pastHorizon == "" {
		pastHorizon = "void_and_alert"
	}
	return &Emitter{store: store, pub: pub, maxRetryHorizon: maxRetryHorizon, pastHorizon: pastHorizon}
}

// EmitViaOutbox writes a durable intent, publishes, and marks delivered — or
// leaves it PENDING for drain on publish failure. Returns (delivered, err).
// key should be reservation_id:event_type (or activation_id:event_type).
func (e *Emitter) EmitViaOutbox(ctx context.Context, key string, r Record) (bool, error) {
	if r.Key == "" {
		r.Key = key
	}
	if r.FirstTS == 0 {
		r.FirstTS = nowEpoch()
	}
	if _, err := e.store.PutIntent(ctx, r); err != nil {
		return false, err // durable intent failed — surface it (never proceed as if written)
	}
	if e.pub == nil {
		// Undeliverable over the transport. That is NOT the same as unsettleable: the
		// settlement path is a direct HTTP call to billing and needs no publisher at all.
		// SETTLE it; void only if it genuinely cannot settle (D-12).
		e.settleOrVoid(ctx, r, "no_publisher_configured")
		return false, nil
	}
	if err := e.pub.Publish(ctx, r); err != nil {
		_ = e.store.BumpAttempt(ctx, r.Key)
		slog.Warn("lifecycle_outbox_pending", "reservation_id", r.ReservationID, "event_type", r.EventType,
			"err", err, "note", "publish failed; durable intent retained for drain")
		return false, nil
	}
	return true, e.store.MarkDelivered(ctx, r.Key)
}

// void records an explicit, ALERTED void (D-12) — never silent, never dropped.
func (e *Emitter) void(ctx context.Context, r Record, reason string) {
	if e.pastHorizon == "drop" { // never allowed in prod
		slog.Error("lifecycle_event_DROPPED", "reservation_id", r.ReservationID, "event_type", r.EventType,
			"reason", reason, "note", "outbox.past_horizon=drop — money event lost")
		_ = e.store.MarkDelivered(ctx, r.Key)
		return
	}
	_ = e.store.MarkVoided(ctx, r.Key, reason)
	entry := VoidEntry{ReservationID: r.ReservationID, EventType: r.EventType, Reason: reason}
	e.mu.Lock()
	e.voidLedger = append(e.voidLedger, entry)
	e.mu.Unlock()
	slog.Error("lifecycle_event_VOIDED", "reservation_id", r.ReservationID, "event_type", r.EventType,
		"reason", reason, "note", "ALERT: activation will not settle; usage may be unbilled — manual reconciliation required")
	if e.OnVoid != nil {
		e.OnVoid(entry)
	}
}

// Drain delivers PENDING intents from the durable store. Past-horizon events
// are voided+alerted. Bounded per pass. Returns count delivered.
func (e *Emitter) Drain(ctx context.Context, maxPerPass int) (int, error) {
	if maxPerPass <= 0 {
		maxPerPass = 100
	}
	pending, err := e.store.ListPending(ctx, maxPerPass)
	if err != nil {
		return 0, err
	}
	if len(pending) >= maxPerPass {
		slog.Warn("outbox_drain_budget_reached", "max_per_pass", maxPerPass, "note", "more may remain; deferred to next pass")
	}
	delivered := 0
	now := nowEpoch()
	for _, r := range pending {
		if now-r.FirstTS > e.maxRetryHorizon {
			// ============================================================
			// D-12 — THIS IS WHERE THE REVENUE USED TO DIE.
			//
			// This branch used to void the event outright, on the premise
			// that "a late commit would 404 at billing anyway". True then;
			// FALSE now — billing has a durable, activation-scoped
			// SETTLEMENT path that needs no live reservation hash.
			//
			// The money is SETTLED. void() survives untouched as the
			// FALLBACK for events that genuinely cannot settle.
			// Ticket: 20260712_revenue_chain_integrity
			// ============================================================
			e.settleOrVoid(ctx, r, "past_retry_horizon")
			continue
		}
		if e.pub == nil {
			continue // nowhere to deliver yet; try next pass
		}
		if err := e.pub.Publish(ctx, r); err != nil {
			_ = e.store.BumpAttempt(ctx, r.Key)
			continue
		}
		if err := e.store.MarkDelivered(ctx, r.Key); err == nil {
			delivered++
		}
	}
	return delivered, nil
}

// PendingCount is the number of undelivered intents in the durable store.
func (e *Emitter) PendingCount(ctx context.Context) (int, error) {
	p, err := e.store.ListPending(ctx, 10000)
	return len(p), err
}

// Voids returns a copy of the void alert-mirror.
func (e *Emitter) Voids() []VoidEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]VoidEntry(nil), e.voidLedger...)
}

// Kill / Unkill toggle the drain worker (emergency stop).
func (e *Emitter) Kill() { e.mu.Lock(); e.killed = true; e.mu.Unlock() }
func (e *Emitter) killedNow() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.killed
}

// RunDrainLoop drains on an interval until ctx is cancelled. Start it as a
// goroutine from Setup. Honors the kill switch.
func (e *Emitter) RunDrainLoop(ctx context.Context, interval time.Duration, maxPerPass int) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	slog.Info("outbox_drain_worker_started", "interval", interval.String(), "max_per_pass", maxPerPass)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if e.killedNow() {
				continue
			}
			e.drainOnceLoud(ctx, maxPerPass)
		}
	}
}

// drainOnceLoud runs one drain pass with a recover so an exception inside the
// periodic loop is LOUD and the worker keeps going — never a swallowed error
// backing off forever, a dead worker inside a healthy process (D-50). Python's
// _drain_loop referenced a deleted attribute, raised every pass, and the outbox
// never drained behind 700 passing tests because every test called drain()
// directly. This is why the worker must be driven, not the function it calls.
func (e *Emitter) drainOnceLoud(ctx context.Context, maxPerPass int) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("outbox_drain_PANIC", "panic", r,
				"note", "drain worker recovered and will retry; a recurring panic is a DEAD money worker — investigate")
		}
	}()
	if n, err := e.Drain(ctx, maxPerPass); err != nil {
		slog.Error("outbox_drain_error", "err", err,
			"note", "a persistent drain error means undelivered billing events are accumulating")
	} else if n > 0 {
		slog.Info("outbox_drain_pass", "delivered", n)
	}
}
