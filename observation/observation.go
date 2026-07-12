// Package observation reports what the library OBSERVED about a resource lifetime.
// It does NOT price it — that is billing's business.
//
// Ticket: billing/output/tickets/20260712_revenue_chain_integrity (B-D13, the caller's half).
//
// # THIS PACKAGE REPLACES A MONEY LAW WITH A REPORT
//
// `POST /billing/{org_id}/settle` used to take a pre-computed `actual_cost`. That one field
// pushed the cost law across the mesh boundary: every caller had to reimplement billing's
// proration, and THREE implementations of one money law existed — billing's, ab0t-quota's, and
// this library's `proration/` package — guarded by a frozen cross-house vector table.
//
// Billing now accepts the INPUTS and prices them with the one law it owns
// (`app/core/proration.py::price_usage`). So `proration/` is ARCHIVED, not synchronised.
//
//	A copy kept in sync is still a copy.
//	A caller that cannot compute a cost cannot compute it wrong.
//
// # THE TRAP THIS PACKAGE MUST NOT FALL INTO
//
// Deleting the local proration must not become the library INFERRING a cost some other way, or
// shipping a "fallback" price. A fabricated price is worse than no price: nothing is honest,
// recoverable and alertable; a fabricated number is an overcharge we cannot defend (D-36).
//
// So there is NO ARITHMETIC IN THIS PACKAGE AT ALL — no rate default, no minimum, no rounding.
// A missing rate is reported as MISSING. Billing prices a rate-less runtime at ZERO and ALERTS
// (`settle_missing_hourly_rate`). That decision is billing's, it is made in exactly one place,
// and this library neither duplicates it nor competes with it.
//
// # B-D14, AND NOT RECREATING IT
//
// The last landmine was an ALWAYS-PRESENT KEY WHOSE VALUE WAS SOMETIMES NULL: the Python library
// always set `hourly_rate` (to null when unpriced), and billing's `.get(k, "0.10")` only defaults
// on an ABSENT key — so it evaluated Decimal("None") -> InvalidOperation -> the money event
// DLQ'd, and the fallback was unreachable.
//
// SettlementPayload therefore OMITS a money key it has no value for (`omitempty` + a pointer-free
// string), rather than emitting an explicit `null`. Send absence as ABSENCE.
package observation

import (
	"errors"
	"fmt"
	"time"
)

// ErrUnsettleable means the event can never yield a settlement (no org, no start time, a lifetime
// that runs backwards). This is the PERMANENT failure direction: not retried — voided and
// alerted, the pre-existing behaviour, kept as the fallback it was always meant to be.
var ErrUnsettleable = errors.New("unsettleable")

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

// Observation is what happened, as this library saw it. It contains NO COST, by design.
type Observation struct {
	OrgID      string
	StartedAt  time.Time
	StoppedAt  time.Time
	ResourceID string
	// Empty string == "we were never quoted a rate". NOT zero, NOT a default — absent.
	HourlyRate    string
	AllocationFee string
}

// SettlementPayload is the body of POST /billing/{org_id}/settle, minus the keys the caller
// supplies (settlement_key, reservation_id, usage_record_id).
//
// Money is a STRING on the wire (the house Decimal-as-string convention — a float would put
// binary-fraction error into a number about to be debited from a customer). `omitempty` is
// load-bearing: a value we do not have is OMITTED, never sent as null (B-D14).
type SettlementPayload struct {
	StartedAt     string `json:"started_at"`
	StoppedAt     string `json:"stopped_at"`
	HourlyRate    string `json:"hourly_rate,omitempty"`
	AllocationFee string `json:"allocation_fee,omitempty"`
}

// ParseTime parses a lifecycle-event timestamp. Mirrors billing's `_parse_dt` — same tolerance,
// same failure mode, so both houses agree on what an unparseable timestamp IS.
// This is parsing, not pricing. It stays.
func ParseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04:05.999999", "2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// Observe extracts the settlement observation from a lifecycle event.
//
// Returns ErrUnsettleable (wrapped, with a reason) when the event can never settle — at any
// horizon, on any retry.
//
// Note what is NOT a reason to refuse: A MISSING HOURLY RATE. That is a pricing-config gap, not
// an unsettleable event — the allocation fee may still be owed, and billing prices the runtime at
// zero and ALERTS. Refusing here would re-create a revenue-loss path (B-D9) and would hide the
// very gap billing's alert exists to surface.
func Observe(ev Event) (Observation, error) {
	var zero Observation
	if ev.OrgID == "" {
		return zero, fmt.Errorf("%w: no org_id; the usage is unattributable", ErrUnsettleable)
	}
	started, ok := ParseTime(ev.StartedAt)
	if !ok {
		return zero, fmt.Errorf("%w: no parseable started_at; a lifetime cannot be priced", ErrUnsettleable)
	}
	// Billing's consumer defaults a missing stop to "now" — the resource stopped, we were just
	// not told precisely when. Mirrored, so the two houses price the same lifetime.
	stopped, ok := ParseTime(ev.StoppedAt)
	if !ok {
		stopped = time.Now().UTC()
	}
	if stopped.Before(started) {
		// Billing 400s this ("Settlement lifetime is invalid"). Catch it here so a broken event
		// is voided + alerted rather than burning a round-trip to be refused.
		return zero, fmt.Errorf("%w: lifetime runs backwards (stopped_at < started_at)", ErrUnsettleable)
	}

	resourceID := ev.ResourceID
	if resourceID == "" {
		resourceID = "unknown"
	}

	return Observation{
		OrgID:         ev.OrgID,
		StartedAt:     started,
		StoppedAt:     stopped,
		ResourceID:    resourceID,
		HourlyRate:    ev.HourlyRate, // "" == absent. We do not default it. We never default it.
		AllocationFee: ev.AllocationFee,
	}, nil
}

// Payload renders the observation as billing's request body.
func (o Observation) Payload() SettlementPayload {
	return SettlementPayload{
		StartedAt:     o.StartedAt.Format(time.RFC3339Nano),
		StoppedAt:     o.StoppedAt.Format(time.RFC3339Nano),
		HourlyRate:    o.HourlyRate,
		AllocationFee: o.AllocationFee,
	}
}
