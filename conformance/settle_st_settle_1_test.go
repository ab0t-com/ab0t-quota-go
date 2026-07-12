package conformance

// ST-SETTLE-1 — the settlement contract, asserted against the Go IMPLEMENTATION.
//
// Ticket: billing/output/tickets/20260712_revenue_chain_integrity (the CALLER leg).
//
// A structural-conformance entry that nothing checks is prose. This test binds the declaration
// in `scenarios.json` to the actual Go constants, so the two cannot drift: change the code and
// forget the spec (or vice versa) and this goes RED.
//
// The Python runtime asserts the SAME entry against ITS constants
// (`tests/test_d12_settlement_conformance_20260712.py`). One spec, two runtimes.

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/outbox"
)

type stSettle struct {
	ID                              string   `json:"id"`
	Runtimes                        []string `json:"runtimes"`
	Law                             string   `json:"law"`
	Contract                        []string `json:"contract"`
	TerminalCostEvents              []string `json:"terminal_cost_events"`
	SettlementKey                   string   `json:"settlement_key"`
	SettlementEndpoint              string   `json:"settlement_endpoint"`
	TransientStatusesRetryNeverVoid []int    `json:"transient_statuses_retry_never_void"`
	PermanentStatusesVoidAndAlert   []int    `json:"permanent_statuses_void_and_alert"`
	Ambiguous409MustRetryNeverAck   bool     `json:"ambiguous_409_must_RETRY_never_ack"`
	Status409IsNotASuccess          bool     `json:"409_is_not_a_success"`
	OnlyAffirmative200MayRetire     bool     `json:"only_an_affirmative_200_may_retire_a_money_event"`
	DedupIsDurableNoTTL             bool     `json:"dedup_is_durable_no_ttl"`
	UnsettleableStillVoidsAndAlerts bool     `json:"unsettleable_still_voids_and_alerts"`
}

func loadSTSettle1(t *testing.T) stSettle {
	t.Helper()
	raw, err := os.ReadFile("scenarios.json")
	if err != nil {
		t.Fatalf("scenarios.json unreadable: %v", err)
	}
	var doc struct {
		Structural []json.RawMessage `json:"structural_conformance"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("scenarios.json unparseable: %v", err)
	}
	for _, r := range doc.Structural {
		var s stSettle
		if err := json.Unmarshal(r, &s); err != nil {
			continue
		}
		if s.ID == "ST-SETTLE-1" {
			return s
		}
	}
	t.Fatal("ST-SETTLE-1 is MISSING from scenarios.json — the settlement contract is undeclared, " +
		"so the two runtimes are no longer held to one spec")
	return stSettle{}
}

// TestSTSettle1_TerminalEventGateMatchesTheSpec is the one that matters most: the gate is what
// stops a `resource.started` from burning the settlement key and refusing the REAL settlement.
func TestSTSettle1_TerminalEventGateMatchesTheSpec(t *testing.T) {
	spec := loadSTSettle1(t)

	if len(spec.TerminalCostEvents) == 0 {
		t.Fatal("the spec declares no terminal events — a vacuous gate")
	}
	for _, ev := range spec.TerminalCostEvents {
		if !outbox.TerminalCostEvents[ev] {
			t.Fatalf("ST-SETTLE-1 declares %q settleable, but Go's TerminalCostEvents does not "+
				"contain it — that event's revenue is being VOIDED instead of settled", ev)
		}
	}
	if len(outbox.TerminalCostEvents) != len(spec.TerminalCostEvents) {
		t.Fatalf("Go settles %d event types but the spec declares %d. An event type that settles "+
			"WITHOUT being in the spec is the dangerous direction: settling a non-terminal event "+
			"burns the settlement key and REFUSES the real settlement. go=%v spec=%v",
			len(outbox.TerminalCostEvents), len(spec.TerminalCostEvents),
			outbox.TerminalCostEvents, spec.TerminalCostEvents)
	}

	// The forbidden ones, named explicitly.
	for _, forbidden := range []string{"resource.started", "resource.heartbeat"} {
		if outbox.TerminalCostEvents[forbidden] {
			t.Fatalf("%q MUST NOT be settleable — it has no final cost, and settling it burns "+
				"the settlement key for the terminal event that follows", forbidden)
		}
	}
}

// TestSTSettle1_TheContractIsDeclaredForBothRuntimes guards the declaration itself.
func TestSTSettle1_TheContractIsDeclaredForBothRuntimes(t *testing.T) {
	spec := loadSTSettle1(t)

	if spec.SettlementKey != "reservation_id" {
		t.Fatalf("the settlement key MUST be reservation_id (the key billing's own SQS consumer "+
			"settles under, which is what makes the two paths dedup against each other). "+
			"spec says %q", spec.SettlementKey)
	}
	if !spec.DedupIsDurableNoTTL {
		t.Fatal("the spec must record that billing's dedup is DURABLE with NO TTL — that is the " +
			"whole reason a client-side retry is safe, and why we do not invent client dedup")
	}
	if !spec.UnsettleableStillVoidsAndAlerts {
		t.Fatal("the void/alert path must survive as the fallback — an event that genuinely " +
			"cannot settle must still reach a human")
	}
	// ⚠️ INVERTED 2026-07-12. This used to require that 409 be declared a SUCCESS. That was a
	// REVENUE-LOSS BUG — billing's 409 is opaque by design and also covers "reservation still
	// live" (the money is NOT taken) and "org mismatch". Acking it DISCARDED the settlement.
	if !spec.Status409IsNotASuccess || !spec.Ambiguous409MustRetryNeverAck {
		t.Fatal("the spec must declare that an ambiguous 409 is NOT a success and must RETRY. " +
			"Acking a 409 retires the durable outbox row and discards revenue — D-12's loss, " +
			"re-entering through the error contract.")
	}
	if !spec.OnlyAffirmative200MayRetire {
		t.Fatal("only an AFFIRMATIVE answer (a 200 saying the usage is accounted for) may retire " +
			"a money event. No outcome may be INFERRED from an ambiguous refusal.")
	}

	var hasPy, hasGo bool
	for _, r := range spec.Runtimes {
		hasPy = hasPy || r == "python"
		hasGo = hasGo || r == "go"
	}
	if !hasPy || !hasGo {
		t.Fatalf("ST-SETTLE-1 must bind BOTH runtimes, got %v", spec.Runtimes)
	}
	if len(spec.Contract) < 5 {
		t.Fatalf("the contract is too thin to be a spec (%d clauses)", len(spec.Contract))
	}
}

// TestSTSettle1_TransientAndPermanentStatusesAreDisjoint — the fail-direction table. If a status
// were in both lists, the same failure could both void and retry: undefined money behaviour.
func TestSTSettle1_TransientAndPermanentStatusesAreDisjoint(t *testing.T) {
	spec := loadSTSettle1(t)

	perm := map[int]bool{}
	for _, s := range spec.PermanentStatusesVoidAndAlert {
		perm[s] = true
	}
	for _, s := range spec.TransientStatusesRetryNeverVoid {
		if perm[s] {
			t.Fatalf("status %d is declared BOTH transient and permanent — the same failure would "+
				"both retry and void", s)
		}
		if s < 500 && s != 408 && s != 429 {
			t.Fatalf("status %d is declared transient but is not a 5xx/408/429", s)
		}
	}
	if perm[409] {
		t.Fatal("409 must not be a permanent VOID either — the money may yet be owed; we simply " +
			"cannot confirm it either way, so we RETRY")
	}
	if !perm[404] || !perm[400] {
		t.Fatal("400 (negative cost) and 404 (unknown org) must be PERMANENT — retrying them " +
			"forever helps nobody")
	}
}
