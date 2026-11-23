package billing

// Typed billing funds events + a handler with client-supplied hooks (v0.1.12, additive).
// Same contract as the Python ab0t_quota.billing.funds_events.
//
// Billing publishes two CLIENT events:
//
//	billing.funds_low        trigger=heartbeat        — still funded, less than the low-water runway
//	billing.funds_exhausted  trigger=heartbeat        — the resource is STILL RUNNING; further runtime is debt
//	                         trigger=commit_overrun   — a stop cost more than hold + funds (already stopped)
//	                         trigger=settlement_debt  — a late settlement drove the balance negative (already stopped)
//
// They are facts; what to do is the client's policy. Delivery is at-least-once and billing notifies
// once per transition, so the handler dedups on billing's key (reservation_id, event_type, trigger)
// inside a bounded window, releases the claim when a hook fails (the transport redelivers), and logs
// LOUDLY when a funds event arrives with no hook.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// BillingEventNamespace is billing's event namespace; FundsEventKind values are the short names.
const BillingEventNamespace = "billing"

// FundsEventKind is the kind of funds event.
type FundsEventKind string

const (
	FundsLow       FundsEventKind = "funds_low"
	FundsExhausted FundsEventKind = "funds_exhausted"
)

// EventType is the wire event_type billing publishes for this kind ("billing.funds_low", …).
func (k FundsEventKind) EventType() string { return BillingEventNamespace + "." + string(k) }

// Triggers.
const (
	TriggerHeartbeat      = "heartbeat"
	TriggerCommitOverrun  = "commit_overrun"
	TriggerSettlementDebt = "settlement_debt"
)

// DefaultFundsDedupTTL is the default dedup window (a re-armed transition after it is delivered again).
const DefaultFundsDedupTTL = time.Hour

// ErrMalformedFundsEvent is returned by ParseFundsEvent for undecodable input or a funds event
// without an org_id.
var ErrMalformedFundsEvent = errors.New("billing: malformed funds event")

// FundsEvent is one billing funds event. A nil money pointer / zero time means "billing did not
// say" — never read as 0.
type FundsEvent struct {
	Kind          FundsEventKind
	EventType     string
	Trigger       string
	OrgID         string
	ReservationID string
	ResourceID    string
	State         string
	SettlementKey string
	Timestamp     time.Time
	StartedAt     time.Time
	RunwaySeconds *float64

	Accrued         *decimal.Decimal
	HoldAmount      *decimal.Decimal
	Available       *decimal.Decimal
	HourlyRate      *decimal.Decimal
	ActualCost      *decimal.Decimal
	ReservedAmount  *decimal.Decimal
	UnfundedOverrun *decimal.Decimal
	NewBalance      *decimal.Decimal

	Raw map[string]any
}

// IsRunningResource is true only for heartbeat-triggered events: the resource is running NOW.
func (e FundsEvent) IsRunningResource() bool { return e.Trigger == TriggerHeartbeat }

// IsDebt is true when usage is (or will be) recorded beyond the org's funds.
func (e FundsEvent) IsDebt() bool { return e.Kind == FundsExhausted }

// DedupKey is billing's documented idempotency key: (reservation_id, event_type, trigger).
func (e FundsEvent) DedupKey() string {
	subject := e.ReservationID
	if subject == "" {
		subject = e.SettlementKey
	}
	if subject == "" {
		subject = "org:" + e.OrgID
	}
	return subject + "|" + e.EventType + "|" + e.Trigger
}

// ParseFundsEvent parses an SQS message body, an SNS envelope ({"Message": "<json>"}), an SQS entry
// ({"Body": "<json>"}) or the raw event JSON. It returns (nil, nil) for any event that is not a
// billing funds event, so it is safe on a shared queue; ErrMalformedFundsEvent for garbage or a
// funds event without org_id.
func ParseFundsEvent(msg []byte) (*FundsEvent, error) {
	raw, err := unwrapEvent(msg)
	if err != nil || raw == nil {
		return nil, err
	}
	et, _ := raw["event_type"].(string)
	ns, name, _ := strings.Cut(et, ".")
	if ns != BillingEventNamespace {
		return nil, nil
	}
	kind := FundsEventKind(name)
	if kind != FundsLow && kind != FundsExhausted {
		return nil, nil
	}
	org := text(raw["org_id"])
	if org == "" {
		return nil, fmt.Errorf("%w: %s without org_id", ErrMalformedFundsEvent, kind.EventType())
	}
	ev := &FundsEvent{
		Kind:            kind,
		EventType:       kind.EventType(),
		Trigger:         text(raw["trigger"]),
		OrgID:           org,
		ReservationID:   text(raw["reservation_id"]),
		ResourceID:      text(raw["resource_id"]),
		State:           text(raw["state"]),
		SettlementKey:   text(raw["settlement_key"]),
		Timestamp:       when(raw["timestamp"]),
		StartedAt:       when(raw["started_at"]),
		Accrued:         decimalField(raw, "accrued"),
		HoldAmount:      decimalField(raw, "hold_amount"),
		Available:       decimalField(raw, "available"),
		HourlyRate:      decimalField(raw, "hourly_rate"),
		ActualCost:      decimalField(raw, "actual_cost"),
		ReservedAmount:  decimalField(raw, "reserved_amount"),
		UnfundedOverrun: decimalField(raw, "unfunded_overrun"),
		NewBalance:      decimalField(raw, "new_balance"),
		Raw:             raw,
	}
	if f, ok := raw["runway_seconds"].(float64); ok {
		ev.RunwaySeconds = &f
	}
	return ev, nil
}

func unwrapEvent(msg []byte) (map[string]any, error) {
	cur := msg
	for i := 0; i < 4; i++ {
		var v any
		if err := json.Unmarshal(cur, &v); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformedFundsEvent, err)
		}
		if s, ok := v.(string); ok { // a JSON-encoded string holding JSON
			cur = []byte(s)
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil
		}
		if _, ok := m["event_type"]; ok {
			return m, nil
		}
		next, ok := m["Body"].(string)
		if !ok {
			next, ok = m["Message"].(string)
		}
		if !ok {
			return nil, nil
		}
		cur = []byte(next)
	}
	return nil, nil
}

func text(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func when(v any) time.Time {
	s, _ := v.(string)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// DispatchResult is what Handle did with a message.
type DispatchResult string

const (
	DispatchHandled   DispatchResult = "handled"   // a hook ran and returned nil
	DispatchDuplicate DispatchResult = "duplicate" // handled inside the dedup window; hook not called
	DispatchIgnored   DispatchResult = "ignored"   // not a billing funds event
	DispatchNoHook    DispatchResult = "no_hook"   // a funds event nobody registered a hook for (logged loudly)
	DispatchMalformed DispatchResult = "malformed" // looked like a funds event but could not be read
)

// EventDedup is the claim store. Several replicas on one queue should share one (e.g. Redis SET NX EX).
type EventDedup interface {
	Claim(ctx context.Context, key string) (bool, error)
	Release(ctx context.Context, key string) error
}

// MemoryDedup is a bounded, time-windowed per-process claim set.
type MemoryDedup struct {
	TTL time.Duration    // <= 0 → DefaultFundsDedupTTL
	Max int              // <= 0 → 10000
	Now func() time.Time // nil → time.Now

	mu    sync.Mutex
	seen  map[string]time.Time
	queue []dedupEntry
}

type dedupEntry struct {
	key string
	exp time.Time
}

// NewMemoryDedup builds a MemoryDedup with the given window.
func NewMemoryDedup(ttl time.Duration) *MemoryDedup { return &MemoryDedup{TTL: ttl} }

// Claim reports whether key was free (and takes it for the window).
func (d *MemoryDedup) Claim(_ context.Context, key string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = map[string]time.Time{}
	}
	ttl, limit, now := d.TTL, d.Max, time.Now()
	if ttl <= 0 {
		ttl = DefaultFundsDedupTTL
	}
	if limit <= 0 {
		limit = 10000
	}
	if d.Now != nil {
		now = d.Now()
	}
	for len(d.queue) > 0 {
		f := d.queue[0]
		if f.exp.After(now) && len(d.seen) < limit {
			break
		}
		d.queue = d.queue[1:]
		if exp, ok := d.seen[f.key]; ok && exp.Equal(f.exp) {
			delete(d.seen, f.key)
		}
	}
	if exp, ok := d.seen[key]; ok && exp.After(now) {
		return false, nil
	}
	exp := now.Add(ttl)
	d.seen[key] = exp
	d.queue = append(d.queue, dedupEntry{key: key, exp: exp})
	return true, nil
}

// Release frees key (a hook failed; let the redelivery through).
func (d *MemoryDedup) Release(_ context.Context, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen, key)
	return nil
}

// Len is the number of live claims (tests / metrics).
func (d *MemoryDedup) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// FundsHook is a client policy hook.
type FundsHook func(ctx context.Context, ev FundsEvent) error

// FundsEventHandler routes billing funds events to client hooks at most once per dedup window.
// Zero value is usable; Dedup nil → a per-process MemoryDedup (DisableDedup turns dedup off — your
// hooks must then be idempotent on ev.DedupKey()).
type FundsEventHandler struct {
	OnLow        FundsHook
	OnExhausted  FundsHook
	Dedup        EventDedup
	DisableDedup bool

	once sync.Once
}

func (h *FundsEventHandler) dedup() EventDedup {
	if h.DisableDedup {
		return nil
	}
	h.once.Do(func() {
		if h.Dedup == nil {
			h.Dedup = NewMemoryDedup(DefaultFundsDedupTTL)
		}
	})
	return h.Dedup
}

// Handle parses msg and dispatches it. A hook error is returned (after releasing the claim) so the
// caller does not ack the message.
func (h *FundsEventHandler) Handle(ctx context.Context, msg []byte) (DispatchResult, error) {
	ev, err := ParseFundsEvent(msg)
	if err != nil {
		slog.Error("funds_event_MALFORMED — the hook was NOT called; if this was a billing.funds_exhausted "+
			"the client did not act on it", "err", err)
		return DispatchMalformed, nil
	}
	if ev == nil {
		return DispatchIgnored, nil
	}
	return h.Dispatch(ctx, *ev)
}

// Dispatch routes an already-parsed event.
func (h *FundsEventHandler) Dispatch(ctx context.Context, ev FundsEvent) (DispatchResult, error) {
	hook := h.OnLow
	if ev.Kind == FundsExhausted {
		hook = h.OnExhausted
	}
	if hook == nil {
		log := slog.Warn
		if ev.Kind == FundsExhausted {
			log = slog.Error
		}
		log("billing funds event received but NO hook is registered — nothing in this service acts on it "+
			"(policy is yours: set OnLow / OnExhausted)",
			"event_type", ev.EventType, "org", ev.OrgID, "reservation_id", ev.ReservationID, "trigger", ev.Trigger)
		return DispatchNoHook, nil
	}
	key := ev.DedupKey()
	dd := h.dedup()
	if dd != nil {
		ok, err := dd.Claim(ctx, key)
		if err != nil {
			return "", fmt.Errorf("billing: funds event dedup claim: %w", err)
		}
		if !ok {
			return DispatchDuplicate, nil
		}
	}
	if err := hook(ctx, ev); err != nil {
		if dd != nil {
			_ = dd.Release(ctx, key)
		}
		return "", err
	}
	return DispatchHandled, nil
}
