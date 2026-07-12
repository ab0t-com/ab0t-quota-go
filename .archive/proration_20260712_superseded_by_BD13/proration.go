// Package proration is the proration law — the arithmetic that turns a resource
// lifetime into money.
//
// Ticket: billing/output/tickets/20260712_revenue_chain_integrity (D-12, the caller leg).
//
// # WHY THIS IS A DUPLICATE, AND WHY THAT IS NOT AN ACCIDENT
//
// This is a port of billing's `app/core/proration.py::calculate_prorated_cost`, and of the
// Python library's `ab0t_quota/proration.py`. Three implementations of one money law is
// exactly the multi-source-of-truth hazard the parent ticket exists to kill (D-35). It is
// duplicated anyway, because the contract forces it:
//
//   - `POST /billing/{org_id}/settle` accepts a COMPUTED `actual_cost` — a number, not the
//     proration inputs. The caller must do the arithmetic.
//   - Go cannot import billing's Python, and a shared library must not depend on a service.
//
// So the duplication is FORCED, not chosen — and it is made SAFE the only way a forced
// duplicate can be: **both runtimes are held to ONE frozen vector table**
// (`testdata/proration_vectors_20260712.json`), whose expectations were derived by
// EXECUTING billing's real function in Python — not transcribed from it by hand, and not
// invented by a Go author who might repeat the same misunderstanding.
//
// If billing changes its proration, `proration_test.go` goes RED. That is the whole point.
// Do not "improve" this arithmetic: it is not ours. It mirrors billing. If it looks wrong, it
// is wrong THERE, and must change there first.
//
// # THE THREE PLACES A NAIVE PRORATION GETS THIS WRONG
//
//  1. the 60-SECOND FLOOR — a 30s sandbox bills a full minute;
//  2. ROUND_UP at 1e-6 — the platform never rounds a charge DOWN;
//  3. the allocation fee is added AFTER the runtime is quantized, so the fee is never rounded.
//
// A negative control in the Python suite (NC-5) proved that a test suite using only whole-hour
// lifetimes CANNOT SEE any of these — it stayed green against arithmetic that was flatly wrong.
package proration

import (
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// MinBillingSeconds is the minimum billable unit. Mirrors billing's MIN_BILLING_SECONDS.
const MinBillingSeconds = 60

// costScale is the quantization used by billing: 6 decimal places, rounded UP.
const costScale int32 = 6

// ErrUnsettleable means the event cannot be turned into a settlement at all (no org, no start
// time, a negative cost). This is the PERMANENT failure direction: not retried — voided and
// alerted, which is the pre-existing behaviour, kept as the fallback it was always meant to be.
var ErrUnsettleable = errors.New("unsettleable")

// Calculate returns allocation_fee + prorated runtime.
//
// A port of billing `app/core/proration.py:20-44`. The ORDER OF OPERATIONS is load-bearing and
// is deliberately not tidied:
//
//	elapsed  = max(stopped - started, 60s)      // floor BEFORE the division
//	runtime  = ROUND_UP(rate * elapsed/3600, 1e-6)
//	total    = allocation_fee + runtime         // the fee is never rounded
//
// Reproducing the RESULT is not enough; reproducing the ARITHMETIC is — that is what keeps a
// settled amount identical to a committed amount for the same usage.
func Calculate(started, stopped time.Time, hourlyRate, allocationFee decimal.Decimal) decimal.Decimal {
	elapsed := stopped.Sub(started).Seconds()
	if elapsed < MinBillingSeconds {
		elapsed = MinBillingSeconds
	}
	// Python computes `Decimal(str(elapsed)) / Decimal("3600")` under a 28-digit context, then
	// multiplies. DivRound at 24 places reproduces that to far beyond the 1e-6 we quantize to;
	// the frozen vectors are the arbiter, not this comment.
	hours := decimal.NewFromFloat(elapsed).DivRound(decimal.NewFromInt(3600), 24)
	runtime := hourlyRate.Mul(hours)
	// RoundUp = away from zero. Costs are non-negative here (a negative one is refused in
	// SettlementCost), so this is billing's ROUND_UP exactly.
	return allocationFee.Add(runtime.RoundUp(costScale))
}

// ParseTime parses a lifecycle-event timestamp. Port of billing's `_parse_dt`
// (`app/workers/lifecycle_consumer.py:478-487`) — same tolerance, same failure mode (a zero
// time, never a panic), so both houses agree on what an unparseable timestamp IS.
func ParseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// Event is the money-bearing subset of a lifecycle event, as billing's own consumer reads it.
type Event struct {
	OrgID         string `json:"org_id"`
	ResourceID    string `json:"resource_id"`
	ReservationID string `json:"reservation_id"`
	HourlyRate    string `json:"hourly_rate"`
	AllocationFee string `json:"allocation_fee"`
	StartedAt     string `json:"started_at"`
	StoppedAt     string `json:"stopped_at"`
}

// SettlementCost derives the settlement amount from a lifecycle event, the way BILLING would.
//
// Returns ErrUnsettleable (wrapped, with a reason) when the event can never yield a cost — a
// genuine "this can never settle", i.e. the void+alert path.
//
// ⚠️ `hourly_rate` deliberately does NOT mirror billing byte-for-byte, and here is why (FOUND;
// reported for billing's owner):
//
//	billing: Decimal(str(event.get("hourly_rate", "0.10")))
//
// Python's `dict.get(k, default)` returns the default only when the key is ABSENT. The Python
// library ALWAYS SETS the key (to null when there is no price), so billing computes
// `Decimal(str(None))` == `Decimal('None')` and raises `decimal.InvalidOperation`. Billing's
// "0.10" fallback is UNREACHABLE from a library-emitted event, and a rate-less money event
// CRASHES billing's consumer rather than being charged $0.10/h.
//
// So there is no billing AMOUNT to agree with in that case — only a crash. A missing rate is
// treated as ZERO and still settled: under QM-02 a $0 settlement is a positive, auditable
// record ("metered, charged nothing"), which beats both a crash and an invented rate. Where a
// rate IS present — the entire normal path — the arithmetic is identical to billing's.
func SettlementCost(ev Event) (decimal.Decimal, error) {
	zero := decimal.Zero
	if ev.OrgID == "" {
		return zero, fmt.Errorf("%w: no org_id; the usage is unattributable", ErrUnsettleable)
	}
	started, ok := ParseTime(ev.StartedAt)
	if !ok {
		return zero, fmt.Errorf("%w: no parseable started_at; a lifetime cannot be prorated", ErrUnsettleable)
	}
	stopped, ok := ParseTime(ev.StoppedAt)
	if !ok {
		stopped = time.Now().UTC()
	}

	rate := zero
	if ev.HourlyRate != "" && ev.HourlyRate != "null" {
		d, err := decimal.NewFromString(ev.HourlyRate)
		if err != nil {
			return zero, fmt.Errorf("%w: unparseable hourly_rate", ErrUnsettleable)
		}
		rate = d
	}
	fee := zero
	if ev.AllocationFee != "" && ev.AllocationFee != "null" {
		d, err := decimal.NewFromString(ev.AllocationFee)
		if err != nil {
			return zero, fmt.Errorf("%w: unparseable allocation_fee", ErrUnsettleable)
		}
		fee = d
	}

	cost := Calculate(started, stopped, rate, fee)
	if cost.IsNegative() {
		// Billing REFUSES a negative settlement (400): it would CREDIT the customer through a
		// path with no credit authorisation whatsoever. Fail toward void+alert.
		return zero, fmt.Errorf("%w: negative settlement cost", ErrUnsettleable)
	}
	return cost, nil
}
