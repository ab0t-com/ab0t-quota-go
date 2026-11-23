// Package meter runs a metered resource against billing with one helper (v0.1.12, additive).
//
// Same contract as the Python ab0t_quota.billing.meter.ResourceMeter. The money path for anything
// that runs for a while is
//
//	reserve → resource.started → resource.heartbeat (every few minutes) → resource.stopped
//	                                                                    ↘ refund (never started)
//
// and every step used to be hand-wired, each with a silent way to lose money. Here the right thing
// happens without the client remembering it:
//
//   - every heartbeat carries the hourly rate, the allocation fee and the start time, so billing
//     keeps the hold funded to the running cost and emits billing.funds_low / billing.funds_exhausted
//     (billing's funded hold engages only for heartbeats that carry both);
//   - heartbeats are scheduled (default every 300 s);
//   - reserve retries are idempotent: a fresh billing request_id per launch attempt, one same-id
//     retry on 502/503/504 or a transport failure;
//   - Session(...) stops the run when fn returns (billing commits) or refunds it if it never
//     started; Close does the same for long-lived resources (`defer run.Close(ctx, "closed")`);
//   - funds events reach OnFundsLow / OnFundsExhausted through HandleBillingEvent with the matching
//     run; with no hook they are logged loudly and recorded on run.FundsState() — the library never
//     stops anything itself (policy is the client's);
//   - a missing dependency fails in New (no biller, no sink, no transport), never as a silent drop.
//
// The meter never prices anything: the only arithmetic is the hold ESTIMATE
// allocation_fee + hourly_rate × HoldHours. Billing prices the lifetime with its own law when the
// stop event arrives, and records an unaffordable overrun as debt instead of refusing it. HTTP
// /commit is never used.
//
// Heartbeat goroutines die with the process: persist run.State() on your own resource row and
// either Resume it at boot or call Meter.Heartbeat(state) from a sweeper over your table.
package meter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
	"github.com/shopspring/decimal"
)

// Lifecycle event types billing's lifecycle consumer reads.
const (
	EventStarted   = "resource.started"
	EventHeartbeat = "resource.heartbeat"
	EventStopped   = "resource.stopped"
	EventDeleted   = "resource.deleted"
)

// Run statuses.
const (
	StatusReserved = "reserved"
	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusAborted  = "aborted"
)

// Funds states (from billing's events).
const (
	FundsFunded    = "funded"
	FundsLow       = "low"
	FundsExhausted = "exhausted"
)

// DefaultHeartbeatInterval matches the cadence billing's funded hold is designed for.
const DefaultHeartbeatInterval = 300 * time.Second

// BillingLowWater is billing's default HOLD_TOPUP_LOW_WATER_SECONDS; slower beats can let a hold
// drain between top-ups (allowed, logged).
const BillingLowWater = 1800 * time.Second

// StateVersion is the RunState schema version.
const StateVersion = 1

// wireTime is RFC 3339 with microseconds — what billing's Python parser reads everywhere.
const wireTime = "2006-01-02T15:04:05.000000Z07:00"

var (
	// ErrConfig: the meter cannot meter (missing dependency or bad input). Returned before any money moves.
	ErrConfig = errors.New("meter: config")
	// ErrState: an operation that does not fit the run's state (e.g. Abort on a running resource —
	// that would refund real runtime; use Stop).
	ErrState = errors.New("meter: invalid state")
)

// InsufficientFundsError: billing refused the hold (402). Do not launch.
type InsufficientFundsError struct {
	OrgID            string
	EstimatedCost    decimal.Decimal
	AvailableBalance *decimal.Decimal
	RequiredTopup    *decimal.Decimal
}

func (e *InsufficientFundsError) Error() string {
	avail := "unknown"
	if e.AvailableBalance != nil {
		avail = e.AvailableBalance.String()
	}
	return fmt.Sprintf("meter: insufficient funds for org %s: hold %s, available %s", e.OrgID, e.EstimatedCost, avail)
}

// APIError is the response body the Python BudgetChecker returns for a 402 (`budget_exceeded`),
// plus billing's required_topup when it sent one.
func (e *InsufficientFundsError) APIError() map[string]any {
	out := map[string]any{
		"error":          "budget_exceeded",
		"message":        "Insufficient budget (estimated $" + e.EstimatedCost.String() + ")",
		"estimated_cost": e.EstimatedCost.String(),
	}
	if e.AvailableBalance != nil {
		out["available_balance"] = e.AvailableBalance.String()
	} else {
		out["available_balance"] = nil
	}
	if e.RequiredTopup != nil {
		out["required_topup"] = e.RequiredTopup.String()
	}
	return out
}

// ReservationLimitedError: billing refused with 429 (daily/monthly cap or the reserve rate limit).
type ReservationLimitedError struct {
	OrgID  string
	Reason string
}

func (e *ReservationLimitedError) Error() string {
	return "meter: reservation limited for org " + e.OrgID + ": " + e.Reason
}

// APIError is a static, client-safe body.
func (e *ReservationLimitedError) APIError() map[string]any {
	return map[string]any{"error": e.Reason, "message": "Usage limit reached for this organization"}
}

// Biller is the billing side the meter needs (*billing.Client satisfies it; so does metertest.Fake).
type Biller interface {
	ReserveFunds(ctx context.Context, orgID string, in billing.ReserveRequest) (*billing.ReserveResult, error)
	RefundReservationOutcome(ctx context.Context, orgID, reservationID, reason string) billing.RefundOutcome
}

// EventSink delivers one lifecycle event. published=false with a nil error means "retained for a
// later delivery" (e.g. a durable outbox waiting for drain).
type EventSink interface {
	Emit(ctx context.Context, ev LifecycleEvent) (published bool, err error)
}

// TransportChecker is optionally implemented by a sink that can tell whether events can leave the
// process at all. New refuses a sink that answers false (unless AllowMissingTransport).
type TransportChecker interface {
	TransportConfigured() bool
}

// LifecycleEvent is the lifecycle event wire shape — the Python LifecycleEmitter's fields
// (ab0t_quota/billing/lifecycle.py emit()). Absent values are omitted (billing reads an absent key
// and a null the same way); stopped_at is always set.
type LifecycleEvent struct {
	EventType     string         `json:"event_type"`
	OrgID         string         `json:"org_id"`
	UserID        string         `json:"user_id"`
	ResourceID    string         `json:"resource_id"`
	ResourceType  string         `json:"resource_type"`
	ReservationID string         `json:"reservation_id,omitempty"`
	InstanceType  string         `json:"instance_type,omitempty"`
	HourlyRate    string         `json:"hourly_rate,omitempty"`
	AllocationFee string         `json:"allocation_fee"`
	StartedAt     string         `json:"started_at,omitempty"`
	StoppedAt     string         `json:"stopped_at"`
	Reason        string         `json:"reason"`
	Metadata      map[string]any `json:"metadata"`
	EmittedAt     string         `json:"emitted_at"`
}

// OutboxSink adapts the library's durable outbox.Emitter (q.Outbox from quota.Setup): every event
// is written as a durable intent keyed reservation_id:event_type (the Python key) before publish,
// so a crash resumes delivery and a terminal event that cannot be published is SETTLED.
type OutboxSink struct {
	Emitter *outbox.Emitter
}

// Emit implements EventSink.
func (s OutboxSink) Emit(ctx context.Context, ev LifecycleEvent) (bool, error) {
	if s.Emitter == nil {
		return false, fmt.Errorf("%w: OutboxSink has no Emitter (quota.Setup wires q.Outbox only when billing is on)", ErrConfig)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return false, err
	}
	key := ev.ReservationID + ":" + ev.EventType
	return s.Emitter.EmitViaOutbox(ctx, key, outbox.Record{
		Event: raw, EventType: ev.EventType, ResourceType: ev.ResourceType, ReservationID: ev.ReservationID,
	})
}

// TransportConfigured implements TransportChecker.
func (s OutboxSink) TransportConfigured() bool { return s.Emitter.HasPublisher() }

// RunHook is a client policy hook; run is nil when the reservation is not running in this process.
type RunHook func(ctx context.Context, ev billing.FundsEvent, run *Run) error

// Config builds a Meter. Billing and Sink are required.
type Config struct {
	Billing Biller
	Sink    EventSink
	// Pricing (optional): ParsePricing(cfg.Pricing). With it, ReserveInput names a Product (and
	// Variant) and never a rate — prices are declared once in quota-config.json.
	Pricing *PriceBook
	// HeartbeatInterval: 0 → DefaultHeartbeatInterval; >= BillingLowWater is allowed but logged.
	HeartbeatInterval time.Duration
	// HoldHours: hours of runtime the initial hold covers; zero → 1.
	HoldHours decimal.Decimal
	// OnFundsLow / OnFundsExhausted: your policy. Nil → loud log + run.FundsState() only.
	OnFundsLow       RunHook
	OnFundsExhausted RunHook
	// Dedup for funds events (nil → per-process MemoryDedup); DisableDedup turns it off.
	Dedup        billing.EventDedup
	DisableDedup bool
	// ToolID on reserves; "" → "default".
	ToolID string
	// AllowMissingTransport accepts a sink whose TransportConfigured() is false (settle-only; no
	// heartbeat reaches billing). Default false — a construction error.
	AllowMissingTransport bool
	// Now (tests). nil → time.Now.
	Now func() time.Time
}

// Meter is the metered-resource helper. Safe for concurrent use.
type Meter struct {
	billing  Biller
	sink     EventSink
	prices   *PriceBook
	interval time.Duration
	hold     decimal.Decimal
	onLow    RunHook
	onExh    RunHook
	toolID   string
	now      func() time.Time
	handler  *billing.FundsEventHandler

	mu   sync.Mutex
	runs map[string]*Run
}

// New validates cfg and builds a Meter. Every missing dependency is an ErrConfig here.
func New(cfg Config) (*Meter, error) {
	if cfg.Billing == nil {
		return nil, fmt.Errorf("%w: Billing is required (e.g. billing.New(mesh.FromEnv()) or q.Billing)", ErrConfig)
	}
	if cfg.Sink == nil {
		return nil, fmt.Errorf("%w: Sink is required (e.g. meter.OutboxSink{Emitter: q.Outbox})", ErrConfig)
	}
	if tc, ok := cfg.Sink.(TransportChecker); ok && !tc.TransportConfigured() && !cfg.AllowMissingTransport {
		return nil, fmt.Errorf("%w: the sink has no transport (outbox.sns_topic_arn unset / no publisher): no heartbeat "+
			"would reach billing, so the funded hold and funds events would never engage — configure the topic, or set "+
			"AllowMissingTransport to accept settle-only metering knowingly", ErrConfig)
	}
	interval := cfg.HeartbeatInterval
	switch {
	case interval < 0:
		return nil, fmt.Errorf("%w: HeartbeatInterval must be > 0", ErrConfig)
	case interval == 0:
		interval = DefaultHeartbeatInterval
	case interval >= BillingLowWater:
		slog.Warn("meter heartbeat interval >= billing's low-water mark — a hold can drain between beats; prefer <= 300s",
			"interval", interval.String(), "low_water", BillingLowWater.String())
	}
	hold := cfg.HoldHours
	if hold.IsZero() {
		hold = decimal.NewFromInt(1)
	}
	if hold.Sign() < 0 {
		return nil, fmt.Errorf("%w: HoldHours must be > 0", ErrConfig)
	}
	tool := cfg.ToolID
	if strings.TrimSpace(tool) == "" {
		tool = "default"
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	m := &Meter{
		billing:  cfg.Billing,
		sink:     cfg.Sink,
		prices:   cfg.Pricing,
		interval: interval,
		hold:     hold,
		onLow:    cfg.OnFundsLow,
		onExh:    cfg.OnFundsExhausted,
		toolID:   tool,
		now:      now,
		runs:     map[string]*Run{},
	}
	m.handler = &billing.FundsEventHandler{
		OnLow: func(ctx context.Context, ev billing.FundsEvent) error { return m.route(ctx, ev, FundsLow, m.onLow) },
		OnExhausted: func(ctx context.Context, ev billing.FundsEvent) error {
			return m.route(ctx, ev, FundsExhausted, m.onExh)
		},
		Dedup:        cfg.Dedup,
		DisableDedup: cfg.DisableDedup,
	}
	return m, nil
}

// ReserveInput describes one lifetime to reserve. Price: Product (+ Variant) from Config.Pricing, OR
// an explicit HourlyRate (+ AllocationFee) — never both, never neither. ResourceType "" → Product.
type ReserveInput struct {
	OrgID         string
	UserID        string
	ResourceID    string
	ResourceType  string
	Product       string
	Variant       string
	HourlyRate    decimal.Decimal
	AllocationFee decimal.Decimal
	// EstimatedCost: zero → AllocationFee + HourlyRate × HoldHours.
	EstimatedCost decimal.Decimal
	// OperationType: "" → ResourceType.
	OperationType string
	// RequestID: "" → a fresh id per Reserve call (never derive it from the resource: a restarted
	// resource is a new lifetime and must get a new reservation).
	RequestID    string
	InstanceType string
	Metadata     map[string]any
}

// RunState is the JSON-safe snapshot to store on your resource row (Resume / Heartbeat take it back).
type RunState struct {
	Version       int            `json:"version"`
	OrgID         string         `json:"org_id"`
	UserID        string         `json:"user_id"`
	ResourceID    string         `json:"resource_id"`
	ResourceType  string         `json:"resource_type"`
	ReservationID string         `json:"reservation_id"`
	HourlyRate    string         `json:"hourly_rate"`
	AllocationFee string         `json:"allocation_fee"`
	Status        string         `json:"status"`
	StartedAt     string         `json:"started_at,omitempty"`
	InstanceType  string         `json:"instance_type,omitempty"`
	Metadata      map[string]any `json:"metadata"`
	RequestID     string         `json:"request_id,omitempty"`
}

// Run is one metered lifetime of one resource.
type Run struct {
	m             *Meter
	OrgID         string
	UserID        string
	ResourceID    string
	ResourceType  string
	ReservationID string
	HourlyRate    decimal.Decimal
	AllocationFee decimal.Decimal
	InstanceType  string
	Metadata      map[string]any
	RequestID     string

	// emitMu serialises heartbeat emits with Stop, so no heartbeat can leave after the stop event.
	emitMu sync.Mutex

	mu                sync.Mutex
	status            string
	startedAt         time.Time
	fundsState        string
	lastFunds         *billing.FundsEvent
	heartbeatsSent    int
	heartbeatFailures int
	stopLoop          chan struct{}
}

func (m *Meter) resolvePrice(in ReserveInput) (rate, fee decimal.Decimal, meta map[string]any, err error) {
	meta = map[string]any{}
	if in.Product != "" {
		if !in.HourlyRate.IsZero() || !in.AllocationFee.IsZero() {
			return rate, fee, nil, fmt.Errorf("%w: set Product OR HourlyRate/AllocationFee, not both — prices are "+
				"declared once in pricing.products", ErrConfig)
		}
		if m.prices == nil {
			return rate, fee, nil, fmt.Errorf("%w: Product needs Config.Pricing (meter.ParsePricing(cfg.Pricing))", ErrConfig)
		}
		p, err := m.prices.Lookup(strings.TrimSpace(in.Product), in.Variant)
		if err != nil {
			return rate, fee, nil, err
		}
		meta["product"] = strings.TrimSpace(in.Product)
		if in.Variant != "" {
			meta["variant"] = in.Variant
		}
		return p.HourlyRate, p.AllocationFee, meta, nil
	}
	if in.HourlyRate.IsZero() {
		return rate, fee, nil, fmt.Errorf("%w: a price is required: Product (declared in pricing.products) or HourlyRate", ErrConfig)
	}
	return in.HourlyRate, in.AllocationFee, meta, nil
}

func required(name, v string) (string, error) {
	t := strings.TrimSpace(v)
	if t == "" {
		return "", fmt.Errorf("%w: %s is required (blank given)", ErrConfig, name)
	}
	return t, nil
}

// Reserve holds funds for one lifetime. Errors: *InsufficientFundsError (402),
// *ReservationLimitedError (429), ErrConfig (bad input), anything else from billing (fail closed —
// do not launch).
func (m *Meter) Reserve(ctx context.Context, in ReserveInput) (*Run, error) {
	org, err := required("OrgID", in.OrgID)
	if err != nil {
		return nil, err
	}
	user, err := required("UserID", in.UserID)
	if err != nil {
		return nil, err
	}
	res, err := required("ResourceID", in.ResourceID)
	if err != nil {
		return nil, err
	}
	rate, fee, priceMeta, err := m.resolvePrice(in)
	if err != nil {
		return nil, err
	}
	rtype := in.ResourceType
	if rtype == "" {
		rtype = in.Product
	}
	if rtype, err = required("ResourceType", rtype); err != nil {
		return nil, err
	}
	if rate.Sign() <= 0 {
		return nil, fmt.Errorf("%w: HourlyRate must be > 0: an unpriced heartbeat disables billing's funded hold and an "+
			"unpriced stop bills the runtime at $0", ErrConfig)
	}
	if fee.Sign() < 0 {
		return nil, fmt.Errorf("%w: AllocationFee must be >= 0", ErrConfig)
	}
	est := in.EstimatedCost
	if est.IsZero() {
		est = fee.Add(rate.Mul(m.hold))
	}
	if est.Sign() <= 0 {
		return nil, fmt.Errorf("%w: EstimatedCost must be > 0", ErrConfig)
	}
	meta := map[string]any{}
	for k, v := range in.Metadata {
		meta[k] = v
	}
	for k, v := range priceMeta {
		meta[k] = v
	}
	reqMeta := map[string]any{"resource_id": res}
	for k, v := range meta {
		reqMeta[k] = v
	}
	rid := strings.TrimSpace(in.RequestID)
	if rid == "" {
		rid = "meter:" + randomHex(16)
	}
	op := in.OperationType
	if op == "" {
		op = rtype
	}
	req := billing.ReserveRequest{
		OrgID:         org,
		UserID:        user,
		ToolID:        m.toolID,
		EstimatedCost: est,
		SessionID:     res,
		RequestID:     rid,
		OperationType: op,
		Metadata:      reqMeta,
	}
	result, err := m.billing.ReserveFunds(ctx, org, req)
	if err != nil && ctx.Err() == nil && billing.IsTransient(err) {
		// Same request_id: if the first attempt landed, billing returns THAT reservation.
		slog.Warn("meter reserve transient — one retry with the same request_id", "org", org, "resource", res, "err", err)
		result, err = m.billing.ReserveFunds(ctx, org, req)
	}
	if err != nil {
		return nil, err
	}
	switch result.Outcome {
	case billing.ReserveInsufficientFunds:
		return nil, &InsufficientFundsError{
			OrgID:            org,
			EstimatedCost:    est,
			AvailableBalance: result.AvailableBalance,
			RequiredTopup:    result.RequiredTopup,
		}
	case billing.ReserveLimited:
		reason := result.Reason
		if reason == "" {
			reason = "limited"
		}
		return nil, &ReservationLimitedError{OrgID: org, Reason: reason}
	case billing.ReserveReserved:
	default:
		return nil, fmt.Errorf("meter: unknown reserve outcome %q (failing closed)", result.Outcome)
	}
	return &Run{
		m:             m,
		OrgID:         org,
		UserID:        user,
		ResourceID:    res,
		ResourceType:  rtype,
		ReservationID: result.ReservationID,
		HourlyRate:    rate,
		AllocationFee: fee,
		InstanceType:  in.InstanceType,
		Metadata:      meta,
		RequestID:     rid,
		status:        StatusReserved,
		fundsState:    FundsFunded,
	}, nil
}

// Start is Reserve + Started for a resource that is already up (startedAt zero → now).
func (m *Meter) Start(ctx context.Context, in ReserveInput, startedAt time.Time) (*Run, error) {
	run, err := m.Reserve(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := run.Started(ctx, startedAt); err != nil {
		return run, err
	}
	return run, nil
}

// Session reserves, starts (the lifetime is fn's), runs fn, then stops the run ("completed", or
// "failed" when fn returns an error or panics) — billing commits what ran. fn's error is returned.
func (m *Meter) Session(ctx context.Context, in ReserveInput, fn func(ctx context.Context, run *Run) error) (err error) {
	run, err := m.Start(ctx, in, time.Time{})
	if err != nil {
		if run != nil {
			_ = run.Close(ctx, "failed")
		}
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			if cerr := run.Close(ctx, "failed"); cerr != nil {
				slog.Error("metered session close failed", "reservation_id", run.ReservationID, "err", cerr)
			}
			panic(p)
		}
	}()
	if ferr := fn(ctx, run); ferr != nil {
		if cerr := run.Close(ctx, "failed"); cerr != nil {
			slog.Error("metered session close failed", "reservation_id", run.ReservationID, "err", cerr)
		}
		return ferr
	}
	return run.Close(ctx, "completed")
}

// Started marks the resource up: emits resource.started and starts priced heartbeats. Idempotent.
// startedAt zero → now.
func (r *Run) Started(ctx context.Context, startedAt time.Time) error {
	r.mu.Lock()
	switch r.status {
	case StatusRunning:
		r.mu.Unlock()
		return nil
	case StatusReserved:
	default:
		st := r.status
		r.mu.Unlock()
		return fmt.Errorf("%w: cannot start a run that is %s", ErrState, st)
	}
	if startedAt.IsZero() {
		startedAt = r.m.now()
	}
	r.startedAt = startedAt.UTC()
	r.status = StatusRunning
	r.mu.Unlock()
	r.m.register(r)
	r.startLoop()
	if ok, err := r.m.sink.Emit(ctx, r.event(EventStarted, "provisioned", time.Time{})); err != nil {
		slog.Warn("resource.started emit failed (billing does not act on it; heartbeats are running)",
			"reservation_id", r.ReservationID, "err", err)
	} else if !ok {
		slog.Warn("resource.started not published now (retained for drain)", "reservation_id", r.ReservationID)
	}
	return nil
}

func (r *Run) event(eventType, reason string, stoppedAt time.Time) LifecycleEvent {
	r.mu.Lock()
	started := r.startedAt
	r.mu.Unlock()
	now := r.m.now().UTC()
	if stoppedAt.IsZero() {
		stoppedAt = now
	}
	meta := map[string]any{}
	for k, v := range r.Metadata {
		meta[k] = v
	}
	ev := LifecycleEvent{
		EventType:     eventType,
		OrgID:         r.OrgID,
		UserID:        r.UserID,
		ResourceID:    r.ResourceID,
		ResourceType:  r.ResourceType,
		ReservationID: r.ReservationID,
		InstanceType:  r.InstanceType,
		HourlyRate:    r.HourlyRate.String(),
		AllocationFee: r.AllocationFee.String(),
		StoppedAt:     stoppedAt.UTC().Format(wireTime),
		Reason:        reason,
		Metadata:      meta,
		EmittedAt:     now.Format(wireTime),
	}
	if !started.IsZero() {
		ev.StartedAt = started.Format(wireTime)
	}
	return ev
}

func (r *Run) startLoop() {
	r.mu.Lock()
	if r.stopLoop != nil {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	r.stopLoop = stop
	interval := r.m.interval
	r.mu.Unlock()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if r.Status() != StatusRunning {
					return
				}
				// Detached from any request context: the loop lives as long as the run.
				_, _ = r.HeartbeatNow(context.Background())
			}
		}
	}()
}

func (r *Run) cancelLoop() {
	r.mu.Lock()
	if r.stopLoop != nil {
		close(r.stopLoop)
		r.stopLoop = nil
	}
	r.mu.Unlock()
}

// HeartbeatNow emits one priced heartbeat (rate, fee and start always ride it). A failure is
// counted and logged; the next beat heals (billing's hold target is absolute).
func (r *Run) HeartbeatNow(ctx context.Context) (bool, error) {
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.Status() != StatusRunning {
		return false, nil
	}
	ok, err := r.m.sink.Emit(ctx, r.event(EventHeartbeat, "heartbeat", time.Time{}))
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.heartbeatFailures++
		slog.Warn("heartbeat emit failed", "reservation_id", r.ReservationID, "failures", r.heartbeatFailures, "err", err)
		return false, err
	}
	r.heartbeatsSent++
	return ok, nil
}

// StopInput: Reason "" → "user_stopped"; StoppedAt zero → now; Deleted emits resource.deleted.
type StopInput struct {
	Reason    string
	StoppedAt time.Time
	Deleted   bool
}

// Stop emits resource.stopped (or .deleted) with start, stop, rate and fee; billing prices the
// lifetime and commits. published=false means retained by the durable outbox. Idempotent: a finished
// run returns (true, nil) without emitting. A never-started run is refunded instead (false, nil).
// On an emit error the run stays running (heartbeats off): retry Stop — billing commits once.
func (r *Run) Stop(ctx context.Context, in StopInput) (bool, error) {
	switch r.Status() {
	case StatusStopped, StatusAborted:
		return true, nil
	case StatusReserved:
		slog.Warn("Stop on a run that never started — refunding instead (nothing ran)", "reservation_id", r.ReservationID)
		reason := in.Reason
		if reason == "" {
			reason = "user_stopped"
		}
		_, err := r.Abort(ctx, "never_started:"+reason)
		return false, err
	}
	reason := in.Reason
	if reason == "" {
		reason = "user_stopped"
	}
	stoppedAt := in.StoppedAt
	if stoppedAt.IsZero() {
		stoppedAt = r.m.now()
	}
	r.cancelLoop()
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if st := r.Status(); st == StatusStopped || st == StatusAborted { // a concurrent Stop won
		return true, nil
	}
	eventType := EventStopped
	if in.Deleted {
		eventType = EventDeleted
	}
	ok, err := r.m.sink.Emit(ctx, r.event(eventType, reason, stoppedAt))
	if err != nil {
		slog.Error("stop emit FAILED — retry Stop; until then billing still holds the reservation",
			"reservation_id", r.ReservationID, "err", err)
		return false, err
	}
	r.mu.Lock()
	r.status = StatusStopped
	r.mu.Unlock()
	r.m.unregister(r.ReservationID)
	return ok, nil
}

// Abort refunds a run that never started. It refuses a running run (ErrState): refunding would
// erase runtime that happened — use Stop. A RefundFailed outcome leaves the run retryable.
func (r *Run) Abort(ctx context.Context, reason string) (billing.RefundOutcome, error) {
	switch r.Status() {
	case StatusAborted:
		return billing.RefundAlreadyTerminal, nil
	case StatusReserved:
	default:
		return billing.RefundFailed, fmt.Errorf("%w: Abort on a %s run would refund real runtime — use Stop", ErrState, r.Status())
	}
	if reason == "" {
		reason = "launch_failed"
	}
	out := r.m.billing.RefundReservationOutcome(ctx, r.OrgID, r.ReservationID, reason)
	if out.HoldMayRemain() {
		slog.Error("refund FAILED (a hold may remain) — retry Abort/Close", "reservation_id", r.ReservationID)
		return out, nil
	}
	r.mu.Lock()
	r.status = StatusAborted
	r.mu.Unlock()
	r.m.unregister(r.ReservationID)
	return out, nil
}

// Close does the right thing for wherever the run is: never started → refund; running → stop;
// finished → nothing. Made for `defer run.Close(ctx, "closed")`.
func (r *Run) Close(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "closed"
	}
	switch r.Status() {
	case StatusStopped, StatusAborted:
		return nil
	case StatusReserved:
		slog.Warn("run closed without Started — refunded", "reservation_id", r.ReservationID)
		out, err := r.Abort(ctx, "never_started:"+reason)
		if err == nil && out.HoldMayRemain() {
			return fmt.Errorf("meter: refund failed for %s (a hold may remain)", r.ReservationID)
		}
		return err
	}
	_, err := r.Stop(ctx, StopInput{Reason: reason})
	return err
}

// Status returns reserved | running | stopped | aborted.
func (r *Run) Status() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// StartedAt is the lifetime's start (zero before Started).
func (r *Run) StartedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startedAt
}

// FundsState is the last state billing told this process about: funded | low | exhausted.
func (r *Run) FundsState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fundsState
}

// LastFundsEvent is the last funds event routed to this run (nil if none).
func (r *Run) LastFundsEvent() *billing.FundsEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastFunds
}

// HeartbeatsSent / HeartbeatFailures are counters for this process.
func (r *Run) HeartbeatsSent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.heartbeatsSent
}

func (r *Run) HeartbeatFailures() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.heartbeatFailures
}

// State is the JSON-safe snapshot to persist.
func (r *Run) State() RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	meta := map[string]any{}
	for k, v := range r.Metadata {
		meta[k] = v
	}
	st := RunState{
		Version:       StateVersion,
		OrgID:         r.OrgID,
		UserID:        r.UserID,
		ResourceID:    r.ResourceID,
		ResourceType:  r.ResourceType,
		ReservationID: r.ReservationID,
		HourlyRate:    r.HourlyRate.String(),
		AllocationFee: r.AllocationFee.String(),
		Status:        r.status,
		InstanceType:  r.InstanceType,
		Metadata:      meta,
		RequestID:     r.RequestID,
	}
	if !r.startedAt.IsZero() {
		st.StartedAt = r.startedAt.Format(wireTime)
	}
	return st
}

func (m *Meter) fromState(st RunState) (*Run, error) {
	if st.Version != StateVersion {
		return nil, fmt.Errorf("%w: unsupported meter state version %d", ErrConfig, st.Version)
	}
	for _, f := range [][2]string{
		{"org_id", st.OrgID},
		{"user_id", st.UserID},
		{"resource_id", st.ResourceID},
		{"resource_type", st.ResourceType},
		{"reservation_id", st.ReservationID},
	} {
		if _, err := required(f[0], f[1]); err != nil {
			return nil, err
		}
	}
	rate, err := decimal.NewFromString(st.HourlyRate)
	if err != nil {
		return nil, fmt.Errorf("%w: state hourly_rate %q: %v", ErrConfig, st.HourlyRate, err)
	}
	fee := decimal.Zero
	if st.AllocationFee != "" {
		if fee, err = decimal.NewFromString(st.AllocationFee); err != nil {
			return nil, fmt.Errorf("%w: state allocation_fee %q: %v", ErrConfig, st.AllocationFee, err)
		}
	}
	var started time.Time
	if st.StartedAt != "" {
		if started, err = time.Parse(time.RFC3339Nano, st.StartedAt); err != nil {
			return nil, fmt.Errorf("%w: state started_at %q: %v", ErrConfig, st.StartedAt, err)
		}
	}
	status := st.Status
	if status == "" {
		status = StatusReserved
	}
	meta := map[string]any{}
	for k, v := range st.Metadata {
		meta[k] = v
	}
	return &Run{
		m:             m,
		OrgID:         st.OrgID,
		UserID:        st.UserID,
		ResourceID:    st.ResourceID,
		ResourceType:  st.ResourceType,
		ReservationID: st.ReservationID,
		HourlyRate:    rate,
		AllocationFee: fee,
		InstanceType:  st.InstanceType,
		Metadata:      meta,
		RequestID:     st.RequestID,
		status:        status,
		startedAt:     started.UTC(),
		fundsState:    FundsFunded,
	}, nil
}

// Resume rebuilds a run from State() after a restart; a running run is registered for funds events
// and (when heartbeats) its heartbeat loop restarts.
func (m *Meter) Resume(st RunState, heartbeats bool) (*Run, error) {
	run, err := m.fromState(st)
	if err != nil {
		return nil, err
	}
	if run.Status() == StatusRunning {
		m.register(run)
		if heartbeats {
			run.startLoop()
		}
	}
	return run, nil
}

// Heartbeat is a stateless heartbeat for a sweeper over YOUR resource table. A state that is not
// running is skipped (false, nil).
func (m *Meter) Heartbeat(ctx context.Context, st RunState) (bool, error) {
	run, err := m.fromState(st)
	if err != nil {
		return false, err
	}
	return run.HeartbeatNow(ctx)
}

// ActiveRun returns the running run for reservationID in this process, or nil.
func (m *Meter) ActiveRun(reservationID string) *Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[reservationID]
}

// Shutdown stops this process's heartbeat loops. It does NOT stop the resources (they may outlive
// the process) — Resume their persisted state at the next boot.
func (m *Meter) Shutdown() {
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	for _, r := range runs {
		r.cancelLoop()
	}
}

func (m *Meter) register(r *Run) {
	m.mu.Lock()
	m.runs[r.ReservationID] = r
	m.mu.Unlock()
}

func (m *Meter) unregister(id string) {
	m.mu.Lock()
	delete(m.runs, id)
	m.mu.Unlock()
}

// FundsEvents is the underlying handler (for callers that already parsed an event).
func (m *Meter) FundsEvents() *billing.FundsEventHandler { return m.handler }

// HandleBillingEvent: plug your queue consumer in here (SQS body, SNS envelope or raw JSON). Billing
// funds events reach your hooks with the run when it runs in this process; anything else is
// DispatchIgnored, so the queue can be shared. A hook error is returned — do not ack the message.
func (m *Meter) HandleBillingEvent(ctx context.Context, msg []byte) (billing.DispatchResult, error) {
	return m.handler.Handle(ctx, msg)
}

func (m *Meter) route(ctx context.Context, ev billing.FundsEvent, state string, hook RunHook) error {
	run := m.ActiveRun(ev.ReservationID)
	if run != nil {
		run.mu.Lock()
		run.fundsState = state
		evCopy := ev
		run.lastFunds = &evCopy
		run.mu.Unlock()
	}
	if hook != nil {
		return hook(ctx, ev, run)
	}
	log := slog.Warn
	if state == FundsExhausted {
		log = slog.Error
	}
	log("billing funds event with no hook — nothing in this service acts on it (recorded on run.FundsState())",
		"event_type", ev.EventType, "org", ev.OrgID, "reservation_id", ev.ReservationID, "trigger", ev.Trigger,
		"running_here", run != nil, "missing_hook", "OnFunds"+strings.ToUpper(state[:1])+state[1:])
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
