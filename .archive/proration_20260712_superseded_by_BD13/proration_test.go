package proration

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The FROZEN vector table. Its expectations were produced by EXECUTING billing's real
// `calculate_prorated_cost` in Python (which can import it; Go cannot) — they were not
// transcribed by hand and not invented by a Go author.
//
// ⭐ This is what holds BOTH runtimes to ONE money law (conformance ST-SETTLE-1). If Go's
// proration ever drifts from billing's, this goes RED against numbers the other house actually
// produced, rather than against a table that could repeat the very misunderstanding it exists
// to catch (DECISIONS B-D1: a double can prove what YOUR code does; only the real system can
// prove the CONTRACT holds).
//
// Regenerate (never hand-edit):
//
//	cd billing/output && PYTHONPATH=../../shared/ab0t-quota ./venv/bin/python -m pytest \
//	  ../../shared/ab0t-quota/tests/test_d12_proration_conformance_20260712.py
//	cp ../../shared/ab0t-quota/tests/data/proration_vectors_20260712.json \
//	   ../../shared/ab0t-quota-go/proration/testdata/
const vectorFile = "testdata/proration_vectors_20260712.json"

type vectorDoc struct {
	Law           string `json:"law"`
	SourceOfTruth string `json:"source_of_truth"`
	DerivedBy     string `json:"derived_by"`
	ConformanceID string `json:"conformance_id"`
	Vectors       []struct {
		ID             string `json:"id"`
		StartedAt      string `json:"started_at"`
		StoppedAt      string `json:"stopped_at"`
		ElapsedSeconds int64  `json:"elapsed_seconds"`
		HourlyRate     string `json:"hourly_rate"`
		AllocationFee  string `json:"allocation_fee"`
		ExpectedCost   string `json:"expected_cost"`
	} `json:"vectors"`
}

func loadVectors(t *testing.T) vectorDoc {
	t.Helper()
	raw, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatalf("FROZEN PRORATION VECTORS MISSING (%v).\n"+
			"Go's proration is therefore NOT VERIFIED against billing's real function — this "+
			"test refuses to certify a money law against nothing.", err)
	}
	var doc vectorDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("vector table unreadable: %v", err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatal("vector table is EMPTY — a green run here would prove nothing")
	}
	return doc
}

// TestGoProrationMatchesBillingsRealFunction is the cross-runtime money-law assertion.
func TestGoProrationMatchesBillingsRealFunction(t *testing.T) {
	doc := loadVectors(t)
	if doc.SourceOfTruth == "" || doc.DerivedBy == "" {
		t.Fatal("the vector table must declare its provenance — an undated, unsourced money " +
			"table is exactly the kind of double B-D1 forbids")
	}
	t.Logf("law: %s", doc.Law)
	t.Logf("source of truth: %s (%s)", doc.SourceOfTruth, doc.DerivedBy)

	for _, v := range doc.Vectors {
		v := v
		t.Run(v.ID, func(t *testing.T) {
			started, ok := ParseTime(v.StartedAt)
			if !ok {
				t.Fatalf("cannot parse started_at %q", v.StartedAt)
			}
			stopped, ok := ParseTime(v.StoppedAt)
			if !ok {
				t.Fatalf("cannot parse stopped_at %q", v.StoppedAt)
			}
			rate, err := decimal.NewFromString(v.HourlyRate)
			if err != nil {
				t.Fatalf("bad rate: %v", err)
			}
			fee, err := decimal.NewFromString(v.AllocationFee)
			if err != nil {
				t.Fatalf("bad fee: %v", err)
			}
			want, err := decimal.NewFromString(v.ExpectedCost)
			if err != nil {
				t.Fatalf("bad expected: %v", err)
			}

			got := Calculate(started, stopped, rate, fee)
			if !got.Equal(want) {
				t.Fatalf("PRORATION DRIFT on %q:\n"+
					"  go      = %s\n"+
					"  billing = %s   (billing's REAL function, executed)\n"+
					"The same usage would settle for a different amount in Go than billing would "+
					"charge on commit. Re-port proration.go from billing/output/app/core/proration.py.",
					v.ID, got, want)
			}
		})
	}
}

// TestTheSubMinuteFloorIsReal pins the vector that a whole-hours-only suite cannot see.
// Python's negative control NC-5 swapped the proration for a naive float version and ALL of
// its (then whole-hour) tests stayed green. The floor is what separates the two.
func TestTheSubMinuteFloorIsReal(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	got := Calculate(t0, t0.Add(30*time.Second), decimal.RequireFromString("1.00"), decimal.Zero)

	want := decimal.RequireFromString("0.016667") // 60s at $1.00/h, ROUND_UP to 1e-6
	if !got.Equal(want) {
		t.Fatalf("a 30s lifetime must bill the 60-SECOND FLOOR: got %s, want %s", got, want)
	}
	naive := decimal.RequireFromString("0.008333") // what 30 literal seconds would cost
	if got.Equal(naive) {
		t.Fatal("the floor did not bite — Go is billing literal elapsed time and UNDERCHARGING")
	}
}

// TestRoundingIsUpNeverDown — the platform never rounds a charge down.
func TestRoundingIsUpNeverDown(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	rate := decimal.RequireFromString("0.0208")
	got := Calculate(t0, t0.Add(3601*time.Second), rate, decimal.Zero)

	exact := rate.Mul(decimal.NewFromInt(3601)).Div(decimal.NewFromInt(3600))
	if got.LessThan(exact) {
		t.Fatalf("ROUND_UP violated: %s < exact %s — the platform rounded a charge DOWN", got, exact)
	}
}

// TestSettlementCostRefusesWhatItCannotPrice — the permanent-failure direction. We never
// invent a number for money.
func TestSettlementCostRefusesWhatItCannotPrice(t *testing.T) {
	base := Event{
		OrgID: "org-1", ResourceID: "sbx-1", ReservationID: "res-1",
		HourlyRate: "1.00", AllocationFee: "0",
		StartedAt: "2026-07-01T12:00:00+00:00",
		StoppedAt: "2026-07-01T15:00:00+00:00",
	}
	for _, tc := range []struct {
		name   string
		mutate func(e *Event)
	}{
		{"no org_id", func(e *Event) { e.OrgID = "" }},
		{"no started_at", func(e *Event) { e.StartedAt = "" }},
		{"unparseable started_at", func(e *Event) { e.StartedAt = "not-a-date" }},
		{"unparseable rate", func(e *Event) { e.HourlyRate = "abc" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := base
			tc.mutate(&ev)
			if _, err := SettlementCost(ev); err == nil {
				t.Fatalf("expected ErrUnsettleable for %q — a money path must never guess", tc.name)
			}
		})
	}

	// The happy path still prices correctly.
	got, err := SettlementCost(base)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !got.Equal(decimal.RequireFromString("3")) {
		t.Fatalf("3h at $1.00/h = 3, got %s", got)
	}
}

// TestARateLessEventSettlesZeroRatherThanInventingARate — see SettlementCost's doc comment:
// billing CRASHES on a rate-less event (Decimal(str(None))), so there is no billing amount to
// agree with. $0 is a positive, auditable record (QM-02); an invented rate is not.
func TestARateLessEventSettlesZeroRatherThanInventingARate(t *testing.T) {
	ev := Event{
		OrgID: "org-1", ResourceID: "sbx-1", ReservationID: "res-1",
		HourlyRate: "", AllocationFee: "",
		StartedAt: "2026-07-01T12:00:00+00:00",
		StoppedAt: "2026-07-01T13:00:00+00:00",
	}
	got, err := SettlementCost(ev)
	if err != nil {
		t.Fatalf("a rate-less event must still settle (at $0), not fail: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("expected a $0 settlement, got %s — we must never invent a rate", got)
	}
}
