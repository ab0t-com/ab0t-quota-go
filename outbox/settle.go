package outbox

// D-12, the CALLER leg — an undeliverable money event is SETTLED, not lost.
//
// Ticket: billing/output/tickets/20260712_revenue_chain_integrity.
//
// THE DEFECT
// ----------
// A money-bearing lifecycle event that could not be delivered before the retry horizon was
// VOIDED and ALERTED, on the premise — stated in the code — that "a late commit would 404 at
// billing anyway". That premise was TRUE and is now FALSE: billing has a durable,
// activation-scoped settlement path (POST /billing/{org_id}/settle) that needs no live
// reservation hash. "Commit can't take it" no longer implies "nothing can take it".
//
// Until this file existed, NOTHING CALLED THAT ENDPOINT. The mechanism shipped and the money
// was still lost — the parent ticket's D-64 class ("a mechanism is not a guarantee").
//
// THE FAIL DIRECTION (D-31), stated plainly:
//
//	It fails toward RETRYING, never toward DISCARDING;
//	and toward NOT DEBITING, never toward DEBITING TWICE.
//
// The no-double-debit guarantee is NOT a branch of this code: it is billing's DynamoDB
// conditional write on `settlement_key`, which has NO TTL. That is why a timeout is safe to
// retry — if the settlement landed and only the response was lost, the retry returns the
// original result and moves no money.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ab0t-com/ab0t-quota-go/observation"
)

// TerminalCostEvents are the ONLY event types that may be settled.
//
// ⚠️ THIS SET IS LOAD-BEARING. `resource.started` and `resource.heartbeat` ride the SAME
// outbox and reach the SAME void path, and they carry a reservation_id — so they LOOK
// settleable. They are not:
//
//   - a settlement is keyed on `reservation_id`, and billing's dedup is DURABLE AND ETERNAL.
//     Settling a `resource.started` would BURN that key on a partial, wrong (start→now)
//     amount — and the REAL `resource.stopped` settlement that follows would then be REFUSED
//     as a duplicate.
//   - net effect: the customer is charged the WRONG amount AND the true settlement is lost.
//     That is strictly worse than the defect being fixed.
//
// Only terminal events carry a final lifetime, and they are the only ones billing's own
// consumer commits on. A lost heartbeat is not lost revenue (it is a missed extension); a lost
// `started` is not lost revenue either. Both are correctly VOIDED, exactly as before.
var TerminalCostEvents = map[string]bool{
	"resource.stopped": true,
	"resource.deleted": true,
}

// Settlement outcomes, as sentinel errors. A Settler translates its transport's failures into
// these so this package stays transport-agnostic (exactly as Publisher already is) and so no
// money-path control flow ever branches on an error STRING — the defect class that left
// billing's own revenue-loss alarm as dead code for months (DECISIONS B-D11).
var (
	// ErrSettleRefused409 — billing answered 409, which is AMBIGUOUS BY DESIGN.
	//
	// ⚠️ IT DOES NOT MEAN THE MONEY WAS TAKEN. This sentinel used to be called
	// `ErrAlreadyAccounted` and was ACKED AS SUCCESS. That was a revenue-loss bug: billing
	// returns ONE OPAQUE 409 (distinct codes would build a cross-tenant enumeration oracle,
	// because its precheck reads Redis BEFORE checking tenancy), and that single code covers:
	//
	//   * "reservation_still_live:use_commit"    — THE MONEY IS NOT TAKEN.
	//   * "org_mismatch"                         — not ours; nothing settled.
	//   * "already_committed:ledger_row_exists"  — the money IS booked.
	//
	// Two of the three mean the settlement did NOT land. Acking it retired the outbox row and
	// DISCARDED the money — D-12's loss, re-entering through the ERROR CONTRACT.
	//
	// So it RETRIES. Ambiguity is not success (D-49). Retrying a genuinely-settled event is
	// free: billing's dedup is a durable conditional write, so the money moves once and the
	// retry returns 200/replayed — an AFFIRMATIVE answer, which is the only thing allowed to
	// retire a money event.
	ErrSettleRefused409 = errors.New("settle: refused with an ambiguous 409 (NOT a success)")

	// ErrSettleTransient — 5xx, timeout, connection refused. The settlement did NOT land, or
	// it landed and the response was lost. RETRY; never void, never assume.
	ErrSettleTransient = errors.New("settle: transient failure")

	// ErrSettlePermanent — a 4xx that is not 409 (400 negative cost, 403, 404 unknown org).
	// A retry will never change it → void + alert.
	ErrSettlePermanent = errors.New("settle: permanent failure")
)

// Settler settles usage that can no longer be committed.
//
// ⚠️ `settlementKey` is the ONLY thing standing between a retry and a double-charge. Billing
// dedups it with a DynamoDB conditional write that has NO TTL — durable and eternal.
// Implementations MUST pass the RESERVATION ID: it is the same key billing's own SQS lifecycle
// consumer settles under, which is what makes the two settlement paths dedup AGAINST EACH
// OTHER. A different key here (e.g. an activation id) would be TWO keys for ONE usage — a
// double charge.
type Settler interface {
	// Returns what BILLING charged (actualCost) and whether this was an idempotent replay.
	// The cost is billing's answer; the caller does not compute it.
	SettleActivation(ctx context.Context, in SettleRequest) (actualCost string, replayed bool, err error)
}

// SettleRequest is the wire shape of POST /billing/{org_id}/settle.
//
// ⚠️ IT CARRIES THE INPUTS, NOT A COST (B-D13). We send what we OBSERVED — when it started, when
// it stopped, the rate we were quoted — and BILLING computes what it costs, with the one law it
// owns. This struct used to carry `ActualCost decimal.Decimal`, and that single field forced this
// library to carry a PORT OF BILLING'S PRORATION (now archived).
//
//	A caller that cannot compute a cost cannot compute it wrong.
//
// DO NOT ADD A COST FIELD BACK.
type SettleRequest struct {
	OrgID         string
	SettlementKey string
	Observation   observation.Observation
	ReservationID string
	UsageRecordID string
}

// SettleEntry is the mirror of a settlement that LANDED — money this used to lose.
//
// ActualCost is BILLING'S ANSWER, read back off the response — not something we computed. We no
// longer know what the usage costs, and that is the point (B-D13).
type SettleEntry struct {
	ReservationID string
	OrgID         string
	ActualCost    string // what BILLING charged
	Replayed      bool
}

// SetSettler wires the settlement fallback. A nil Settler preserves the pre-existing
// void-and-alert behaviour exactly, so a consumer with no billing wired is unaffected.
func (e *Emitter) SetSettler(s Settler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.settler = s
}

// Settled returns a copy of the settled mirror.
func (e *Emitter) Settled() []SettleEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]SettleEntry(nil), e.settledLedger...)
}

// SettleFailures is the count of TRANSIENT settlement failures. The events are still PENDING —
// they are retained, not lost. A permanently rising count IS a money incident.
func (e *Emitter) SettleFailures() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settleFailures
}

// settleOrVoid: SETTLE an undeliverable money event; void only if it genuinely cannot settle.
//
// Three outcomes:
//
//   - settled / already-accounted → mark the intent DELIVERED. Not a loss.
//   - transient failure           → leave the intent PENDING and bump its attempt count. Do NOT
//     void: a network blip must never consume real revenue. The next
//     drain pass retries — forever, if need be.
//   - permanently unsettleable    → void + alert, exactly as before. The loud path is KEPT; it
//     is now the fallback it was always meant to be.
func (e *Emitter) settleOrVoid(ctx context.Context, r Record, reason string) {
	outcome, detail := e.trySettle(ctx, r)

	switch outcome {
	case "settled":
		// ⚠️ THE ONLY THING THAT MAY RETIRE A MONEY EVENT IS AN AFFIRMATIVE ANSWER.
		// `settled` means billing returned 200 — it took the money, or told us (replayed=true)
		// that it already had. `already_accounted` used to be a second way in here, INFERRED
		// from an opaque 409. It is gone: see ErrSettleRefused409.
		_ = e.store.MarkDelivered(ctx, r.Key)
	case "not_applicable":
		// No settlement was ATTEMPTED (no settler wired, or a non-terminal event). Nothing
		// about this event's situation has changed, so it voids with its ORIGINAL reason,
		// byte-for-byte as it did before this ticket. Enriching the reason here would break
		// peer tests for no behavioural gain.
		e.void(ctx, r, reason)
	case "retry":
		e.mu.Lock()
		e.settleFailures++
		e.mu.Unlock()
		_ = e.store.BumpAttempt(ctx, r.Key)
		slog.Error("lifecycle_settle_RETRY",
			"reservation_id", r.ReservationID, "event_type", r.EventType, "detail", detail,
			"note", "settlement did not land (transient). The event is RETAINED as PENDING and "+
				"will be retried; it is NOT voided and NOT lost. A permanently rising "+
				"pending_count IS a money incident.")
	default: // unsettleable — a settlement WAS attempted and PERMANENTLY refused. That is new
		// information the void reason did not previously carry, so it is recorded.
		e.void(ctx, r, fmt.Sprintf("%s:unsettleable:%s", reason, detail))
	}
}

// trySettle attempts the durable settlement. Returns (outcome, detail).
func (e *Emitter) trySettle(ctx context.Context, r Record) (string, string) {
	e.mu.Lock()
	s := e.settler
	e.mu.Unlock()
	if s == nil {
		// No billing wired: nothing has changed for this consumer. Void exactly as before.
		return "not_applicable", "no_settler"
	}

	// ⚠️ THE TERMINAL-EVENT GATE. See TerminalCostEvents — settling a non-terminal event
	// would burn the settlement key and REFUSE the real settlement that follows.
	if !TerminalCostEvents[r.EventType] {
		return "not_applicable", "not_a_terminal_cost_event"
	}

	var ev observation.Event
	if err := json.Unmarshal(r.Event, &ev); err != nil {
		return "unsettleable", "event_unparseable"
	}
	// The Record is the authority on which reservation this is; the payload may disagree.
	if ev.ReservationID == "" {
		ev.ReservationID = r.ReservationID
	}

	// B-D13: we report what we OBSERVED. We do not price it.
	//
	// ⚠️ DO NOT REINTRODUCE A COST HERE — not a rate default, not a minimum, not a "fallback"
	// price. A fabricated price is worse than no price (D-36). A missing rate is reported as
	// MISSING; billing prices its runtime at ZERO and ALERTS, in exactly one place.
	obs, err := observation.Observe(ev)
	if err != nil {
		// No org / no start time / a lifetime that runs backwards. It can never settle, at any
		// horizon, on any retry. Void + alert is the honest outcome.
		return "unsettleable", err.Error()
	}

	cost, replayed, err := s.SettleActivation(ctx, SettleRequest{
		OrgID: obs.OrgID,
		// THE KEY — the reservation id, the same key billing's own SQS consumer settles under.
		SettlementKey: r.ReservationID,
		Observation:   obs,
		ReservationID: r.ReservationID,
		UsageRecordID: "lifecycle:" + obs.ResourceID,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrSettleRefused409):
			// NOT a success. See ErrSettleRefused409 — a 409 also means "still live" and "org
			// mismatch", in which case the money was never taken. Retry; never ack.
			slog.Error("lifecycle_settle_REFUSED_409",
				"reservation_id", r.ReservationID, "org_id", obs.OrgID,
				"note", "billing refused with an OPAQUE 409. This does NOT mean the money was "+
					"taken. The event is RETAINED as PENDING and will be retried; it is NOT "+
					"acked and NOT lost. Retrying is safe (billing's dedup is durable).")
			return "retry", "http_409_ambiguous_refusal"
		case errors.Is(err, ErrSettleTransient):
			return "retry", err.Error()
		case errors.Is(err, ErrSettlePermanent):
			return "unsettleable", err.Error()
		default:
			// An unclassified error is NOT evidence that the settlement failed to land — it may
			// have landed and the response been lost. Retry (safe: the durable key dedups)
			// rather than voiding money we may already have charged.
			return "retry", err.Error()
		}
	}

	entry := SettleEntry{
		ReservationID: r.ReservationID,
		OrgID:         obs.OrgID,
		ActualCost:    cost, // BILLING'S answer, not ours
		Replayed:      replayed,
	}
	e.mu.Lock()
	e.settledLedger = append(e.settledLedger, entry)
	e.mu.Unlock()

	slog.Info("lifecycle_SETTLED_past_window",
		"reservation_id", r.ReservationID, "org_id", obs.OrgID,
		"actual_cost", cost, "replayed", replayed,
		"note", "revenue that would previously have been VOIDED AND LOST has been settled "+
			"durably against billing")
	return "settled", "ok"
}
