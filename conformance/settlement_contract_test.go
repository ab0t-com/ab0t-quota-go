package conformance

// ST-SETTLE-1 / B-D13 — Go builds the settlement body billing REALLY accepts.
//
// Ticket: billing/output/tickets/20260712_revenue_chain_integrity (W-CHAIN).
//
// # WHAT THIS PINS, AND WHAT IT DELIBERATELY DOES NOT
//
// `/settle` no longer takes a computed `actual_cost` — it takes the INPUTS, and billing prices
// them with the one law it owns. So the Go library's proration is ARCHIVED (`.archive/`), and
// **Go can no longer get the money law wrong, because Go no longer has one.**
//
// What Go CAN still get wrong is the PAYLOAD: a renamed field, a mangled timestamp, a money value
// sent as a float — or, most sharply, **a value it does not have sent as `null` instead of being
// omitted** (B-D14: an always-present key whose value is sometimes null is a landmine for any
// `.get(k, default)` on the other side, and that is precisely the bug billing just fixed).
//
// So this replays a table whose `expected_request_body` was built by the PYTHON library, validated
// through **billing's REAL pydantic model**, and priced by **billing's REAL `price_usage`** —
// all by EXECUTING billing (Python can import it; Go cannot).
//
// ⚠️ IT IS A TRIPWIRE, NOT A CERTIFIER (B-D1). It does not prove Go can talk to the real billing
// service. **A Go<->real-billing test remains OWED.** Do not let this table quietly become the
// certifier it is not.
//
// Regenerate (never hand-edit):
//
//	cd billing/output && PYTHONPATH=../../shared/ab0t-quota ./venv/bin/python -m pytest \
//	  ../../shared/ab0t-quota/tests/test_d12_settlement_contract_20260712.py
//	cp ../../shared/ab0t-quota/tests/data/settlement_contract_vectors_20260712.json \
//	   ../../shared/ab0t-quota-go/conformance/testdata/

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/observation"
)

const contractVectorFile = "testdata/settlement_contract_vectors_20260712.json"

type contractDoc struct {
	Contract  string `json:"contract"`
	LawOwner  string `json:"law_owner"`
	DerivedBy string `json:"derived_by"`
	Caveat    string `json:"caveat"`
	Vectors   []struct {
		ID    string `json:"id"`
		Event struct {
			OrgID         string  `json:"org_id"`
			ResourceID    string  `json:"resource_id"`
			ReservationID string  `json:"reservation_id"`
			StartedAt     string  `json:"started_at"`
			StoppedAt     string  `json:"stopped_at"`
			HourlyRate    *string `json:"hourly_rate"`    // null == the event carries no rate
			AllocationFee *string `json:"allocation_fee"` // null == no fee
		} `json:"event"`
		ExpectedRequestBody map[string]string `json:"expected_request_body"`
		BillingPricedCost   string            `json:"billing_priced_cost"`
	} `json:"vectors"`
}

func loadContract(t *testing.T) contractDoc {
	t.Helper()
	raw, err := os.ReadFile(contractVectorFile)
	if err != nil {
		t.Fatalf("FROZEN SETTLEMENT CONTRACT VECTORS MISSING (%v).\n"+
			"Go's settlement payload is therefore NOT VERIFIED against billing's real request "+
			"model — this test refuses to certify a money contract against nothing.", err)
	}
	var doc contractDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("contract vectors unreadable: %v", err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatal("the contract table is EMPTY — a green run here would prove nothing")
	}
	if doc.DerivedBy == "" || doc.LawOwner == "" {
		t.Fatal("the table must declare its provenance — an unsourced money contract is exactly " +
			"the kind of double B-D1 forbids")
	}
	return doc
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TestBD13_GoBuildsTheBodyBillingAccepts is the cross-runtime contract assertion.
func TestBD13_GoBuildsTheBodyBillingAccepts(t *testing.T) {
	doc := loadContract(t)
	t.Logf("contract: %s", doc.Contract)
	t.Logf("law owner: %s (%s)", doc.LawOwner, doc.DerivedBy)
	t.Logf("CAVEAT: %s", doc.Caveat)

	for _, v := range doc.Vectors {
		v := v
		t.Run(v.ID, func(t *testing.T) {
			obs, err := observation.Observe(observation.Event{
				OrgID:         v.Event.OrgID,
				ResourceID:    v.Event.ResourceID,
				ReservationID: v.Event.ReservationID,
				StartedAt:     v.Event.StartedAt,
				StoppedAt:     v.Event.StoppedAt,
				HourlyRate:    deref(v.Event.HourlyRate),
				AllocationFee: deref(v.Event.AllocationFee),
			})
			if err != nil {
				t.Fatalf("observe: %v", err)
			}

			// Marshal exactly as the client does, then compare the WIRE, key by key.
			raw, err := json.Marshal(obs.Payload())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			// 1. Every key billing expects is present and identical.
			for k, want := range v.ExpectedRequestBody {
				gv, ok := got[k]
				if !ok {
					t.Fatalf("MISSING %q — billing cannot price what it is not told.\n"+
						"  go   = %v\n  want = %v", k, got, v.ExpectedRequestBody)
				}
				if k == "started_at" || k == "stopped_at" {
					// Timestamps: compare as INSTANTS, not as bytes. Go renders RFC3339 with a
					// "Z"; Python renders "+00:00". Same moment, different spelling — and it is
					// the moment billing prices, not the spelling (D-68).
					a, ok1 := observation.ParseTime(gv.(string))
					b, ok2 := observation.ParseTime(want)
					if !ok1 || !ok2 || !a.Equal(b) {
						t.Fatalf("%s differs as an INSTANT: go=%v want=%v", k, gv, want)
					}
					continue
				}
				if gv != any(want) {
					t.Fatalf("%s: go=%v want=%v — the value billing prices must be forwarded "+
						"verbatim", k, gv, want)
				}
			}

			// 2. ⚠️ Go must send NOTHING billing did not ask for — and in particular, a key whose
			//    value we do not have must be ABSENT, not null (B-D14).
			for k := range got {
				if _, expected := v.ExpectedRequestBody[k]; !expected {
					t.Fatalf("Go sent %q, which billing's contract does NOT include.\n"+
						"If this is a null money field: send absence as ABSENCE. An "+
						"always-present key whose value is sometimes null is exactly the B-D14 "+
						"landmine.\n  go   = %v\n  want = %v", k, got, v.ExpectedRequestBody)
				}
			}

			// 3. NO COST, EVER. The whole of B-D13 in one assertion.
			for _, forbidden := range []string{"actual_cost", "cost", "amount", "total"} {
				if _, leaked := got[forbidden]; leaked {
					t.Fatalf("Go sent %q. B-D13 is regressed: billing prices usage, the caller "+
						"reports it. A caller that can compute a cost can compute it WRONG.",
						forbidden)
				}
			}
		})
	}
}

// TestBD13_TheRateLessVectorIsFrozenAsAnABSENCE is the B-D14 guard, called out on its own because
// it is the one that bit the program last time.
func TestBD13_TheRateLessVectorIsFrozenAsAnABSENCE(t *testing.T) {
	doc := loadContract(t)

	var found bool
	for _, v := range doc.Vectors {
		if v.Event.HourlyRate != nil {
			continue
		}
		found = true
		if _, present := v.ExpectedRequestBody["hourly_rate"]; present {
			t.Fatalf("vector %q has no rate, yet the frozen body still carries an `hourly_rate` "+
				"key. Absence must be frozen as ABSENCE — a null here is the B-D14 landmine.", v.ID)
		}

		// And Go must reproduce that absence.
		obs, err := observation.Observe(observation.Event{
			OrgID: v.Event.OrgID, ResourceID: v.Event.ResourceID,
			ReservationID: v.Event.ReservationID,
			StartedAt:     v.Event.StartedAt, StoppedAt: v.Event.StoppedAt,
			HourlyRate: "", AllocationFee: deref(v.Event.AllocationFee),
		})
		if err != nil {
			t.Fatalf("a rate-less event must still be OBSERVABLE (it settles at zero runtime "+
				"and billing alerts) — it must not be refused: %v", err)
		}
		raw, _ := json.Marshal(obs.Payload())
		var got map[string]any
		_ = json.Unmarshal(raw, &got)
		if _, present := got["hourly_rate"]; present {
			t.Fatal("Go emitted an `hourly_rate` key for a rate-less event. Send absence as " +
				"ABSENCE — and never invent a rate: a fabricated price is worse than no price.")
		}
	}
	if !found {
		t.Fatal("the frozen table has NO rate-less vector — the B-D14 direction is untested")
	}
}
