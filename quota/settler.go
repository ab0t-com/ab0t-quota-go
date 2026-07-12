package quota

// The composition root for D-12's settlement fallback.
//
// Ticket: billing/output/tickets/20260712_revenue_chain_integrity (the CALLER leg).
//
// `outbox` must not know about HTTP and `billing` must not know about the outbox — so the
// mapping from billing's real status codes onto the outbox's outcome sentinels lives HERE,
// at the one place that already imports both. This is the same dependency inversion the
// Publisher interface already uses.
//
// ⚠️ THE MAPPING IS THE CONTRACT. It is derived from billing's REAL handler
// (app/api/billing.py:720 -> app/core/reservation.py), not from a guess:
//
//	409 -> ErrSettleRefused409  AMBIGUOUS BY DESIGN, and NOT a success. Billing returns ONE
//	                            opaque 409 (distinct codes would build a cross-tenant
//	                            enumeration oracle), covering "still live / use commit",
//	                            "org mismatch" AND "already committed". Two of the three mean
//	                            the money was NOT taken. RETRY — never ack, never void.
//	400 -> ErrSettlePermanent   negative cost. A retry cannot fix it.
//	403 -> ErrSettlePermanent   authz. A retry cannot fix it.
//	404 -> ErrSettlePermanent   no billing account for this org.
//	5xx -> ErrSettleTransient   billing is broken/restarting. RETRY.
//	408 -> ErrSettleTransient   timeout.
//	429 -> ErrSettleTransient   rate-limited.
//	 no response (connection refused, timeout) -> ErrSettleTransient
//
// Dispatch is on the STATUS CODE, never on the error's text. Billing's own consumer once
// dispatched on `str(e)` — the EMPTY STRING for an HTTPException on its pinned starlette — and
// its entire revenue-loss alarm was dead code for months as a result (DECISIONS B-D11). In a
// money path, branching on an error message is a defect class, not a style choice.

import (
	"context"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
)

// billingSettler adapts *billing.Client to outbox.Settler.
type billingSettler struct {
	c *billing.Client
}

// SettleActivation implements outbox.Settler.
//
// B-D13: it sends the OBSERVATION (the inputs) and reads the cost back off billing's response.
// It computes nothing. There is no arithmetic in this file, and there must not be.
func (b billingSettler) SettleActivation(ctx context.Context, in outbox.SettleRequest) (string, bool, error) {
	pl := in.Observation.Payload()
	resp, err := b.c.SettleActivation(ctx, in.OrgID, billing.SettleActivationRequest{
		SettlementKey: in.SettlementKey,
		StartedAt:     pl.StartedAt,
		StoppedAt:     pl.StoppedAt,
		// Empty == absent == omitted on the wire (omitempty). NEVER a default rate.
		HourlyRate:    pl.HourlyRate,
		AllocationFee: pl.AllocationFee,
		ReservationID: in.ReservationID,
		UsageRecordID: in.UsageRecordID,
	})
	if err != nil {
		return "", false, classifySettleError(err)
	}
	// What BILLING charged. Not what we think it costs — we do not know, by design.
	return resp.ActualCost, resp.Replayed, nil
}

// classifySettleError maps billing's real HTTP contract onto the outbox's outcome sentinels.
//
// The DEFAULT is TRANSIENT (retry), and that default is deliberate: an error we do not
// recognise is NOT evidence that the settlement failed to land. It may have landed and the
// response been lost. Retrying is safe — billing's dedup is a durable conditional write — and
// voiding is not: voiding money we may already have charged corrupts the books in the one
// direction this ticket exists to prevent.
func classifySettleError(err error) error {
	switch {
	case httpx.IsStatus(err, 409):
		return outbox.ErrSettleRefused409
	case httpx.IsStatus(err, 400), httpx.IsStatus(err, 403), httpx.IsStatus(err, 404),
		httpx.IsStatus(err, 401), httpx.IsStatus(err, 422):
		return outbox.ErrSettlePermanent
	default:
		// 5xx, 408, 429, connection refused, timeout, anything unrecognised.
		return outbox.ErrSettleTransient
	}
}
