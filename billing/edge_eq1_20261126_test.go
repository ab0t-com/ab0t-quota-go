package billing

// Edge cases (v0.2.0) — lifecycle event-id stability across runtimes and decimal → Money
// boundaries. The event-id table is testdata/event_id_golden_20261126.json, an identical copy of the
// Python library's (tests/event_id_golden_20261126.json): the same input must give the same id in both.
// Rows marked RED were wrong before this change (see the changelog); every gate has a control.

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/outbox"
	"github.com/shopspring/decimal"
)

type eventIDGolden struct {
	NamespaceUUID string `json:"namespace_uuid"`
	UUID5         []struct {
		Name string `json:"name"`
		Hex  string `json:"hex"`
	} `json:"uuid5"`
	OneShot []struct {
		EventType     string `json:"event_type"`
		ReservationID string `json:"reservation_id"`
		EventID       string `json:"event_id"`
	} `json:"one_shot"`
	Fresh []struct {
		EventType     string `json:"event_type"`
		ReservationID string `json:"reservation_id"`
	} `json:"fresh"`
	Legacy []struct {
		Key       string  `json:"key"`
		EmittedAt *string `json:"emitted_at"`
		EventID   string  `json:"event_id"`
	} `json:"legacy"`
}

func loadEventIDGolden(t *testing.T) eventIDGolden {
	t.Helper()
	raw, err := os.ReadFile("../testdata/event_id_golden_20261126.json")
	if err != nil {
		t.Fatal(err)
	}
	var g eventIDGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.UUID5) == 0 || len(g.OneShot) == 0 || len(g.Fresh) == 0 || len(g.Legacy) == 0 {
		t.Fatal("event-id golden is missing sections")
	}
	return g
}

var evtRE = regexp.MustCompile(`^evt_[0-9a-f]{32}$`)

func TestEventIDGolden_Namespace_EQ1(t *testing.T) {
	// The namespace is uuid5(NAMESPACE_URL, namespace_url): recompute it and compare with the golden AND
	// with the bytes uuid5EventID hard-codes (through uuid5 of the empty name).
	var g struct {
		URL  string `json:"namespace_url"`
		UUID string `json:"namespace_uuid"`
	}
	raw, _ := os.ReadFile("../testdata/event_id_golden_20261126.json")
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	nsURL, _ := hex.DecodeString("6ba7b8119dad11d180b400c04fd430c8") // RFC 4122 NAMESPACE_URL
	h := sha1.New()
	h.Write(nsURL)
	h.Write([]byte(g.URL))
	sum := h.Sum(nil)[:16]
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	if got := hex.EncodeToString(sum); got != strings.ReplaceAll(g.UUID, "-", "") {
		t.Fatalf("uuid5(NAMESPACE_URL, %q) = %s; golden %s", g.URL, got, g.UUID)
	}
	h2 := sha1.New()
	h2.Write(sum)
	empty := h2.Sum(nil)[:16]
	empty[6] = (empty[6] & 0x0f) | 0x50
	empty[8] = (empty[8] & 0x3f) | 0x80
	if uuid5EventID("") != hex.EncodeToString(empty) {
		t.Fatal("uuid5EventID's hard-coded namespace differs from the golden namespace")
	}
}

func TestEventIDGolden_UUID5_EQ1(t *testing.T) {
	for _, r := range loadEventIDGolden(t).UUID5 {
		if got := uuid5EventID(r.Name); got != r.Hex {
			t.Errorf("uuid5(%q) = %s; golden %s", r.Name, got, r.Hex)
		}
	}
}

// RED before: the Go meter gave every event a RANDOM id (meter.go randomHex), so a re-emitted stop
// was a new event to billing, while Python's was a replay.
func TestEventIDGolden_OneShotDeterministic_EQ1(t *testing.T) {
	for _, r := range loadEventIDGolden(t).OneShot {
		a, b := LifecycleEventID(r.EventType, r.ReservationID), LifecycleEventID(r.EventType, r.ReservationID)
		if a != r.EventID || b != r.EventID {
			t.Errorf("LifecycleEventID(%s, %q) = %s / %s; golden %s", r.EventType, r.ReservationID, a, b, r.EventID)
		}
	}
}

func TestEventIDGolden_FreshIsRandom_EQ1(t *testing.T) {
	for _, r := range loadEventIDGolden(t).Fresh {
		a, b := LifecycleEventID(r.EventType, r.ReservationID), LifecycleEventID(r.EventType, r.ReservationID)
		if !evtRE.MatchString(a) || !evtRE.MatchString(b) || a == b {
			t.Errorf("%s/%q: %s, %s — want two different evt_+32hex ids", r.EventType, r.ReservationID, a, b)
		}
	}
}

func TestEventIDGolden_LegacyIntent_EQ1(t *testing.T) {
	for _, r := range loadEventIDGolden(t).Legacy {
		emitted := ""
		if r.EmittedAt != nil {
			emitted = *r.EmittedAt
		}
		if got := legacyEventID(r.Key, emitted); got != r.EventID {
			t.Errorf("legacyEventID(%q, %q) = %s; golden %s", r.Key, emitted, got, r.EventID)
		}
		// through the publisher's real body builder (an intent without event_id)
		ev := map[string]any{"event_type": "resource.stopped", "org_id": "org-1"}
		if r.EmittedAt != nil {
			ev["emitted_at"] = *r.EmittedAt
		}
		raw, _ := json.Marshal(ev)
		_, _, id, err := lifecycleBody(outbox.Record{Key: r.Key, Event: raw, EventType: "resource.stopped"})
		if err != nil || id != r.EventID {
			t.Errorf("lifecycleBody(%q): id %s err %v; golden %s", r.Key, id, err, r.EventID)
		}
	}
}

// ----------------------------------------------------------------------------- decimal → Money

// within fails the test if fn does not return inside d (an absurd exponent used to make shopspring
// rescale by 10^exponent — effectively a hang).
func within(t *testing.T, d time.Duration, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", name, d)
	}
}

func TestMoneyFromDecimal_Edges_EQ1(t *testing.T) {
	for _, c := range []struct {
		name string
		d    decimal.Decimal
		want string
		kind MoneyErrorKind
	}{
		{"sub-micro", decimal.New(5, -7), "", MoneyPrecision},
		{"one negative micro", decimal.New(-1, -6), "-0.000001", ""},
		{"exact with trailing zeros", decimal.New(15000000, -7), "1.5", ""},
		{"10^406", decimal.New(1, 400), "", MoneyRange},
		{"absurd positive exponent", decimal.New(1, 1<<30), "", MoneyRange},          // RED: hang
		{"absurd negative exponent", decimal.New(1, -(1 << 30)), "", MoneyPrecision}, // RED: hang
		{"a zero with an absurd exponent", decimal.New(0, 1<<30), "0", ""},           // RED: hang
		{"just above the max", decimal.New(9007199254740992, -6), "", MoneyRange},
		{"the max", decimal.New(9007199254740991, -6), "9007199254.740991", ""},
		{"1e17 micro-units", decimal.New(1, 11), "", MoneyRange},
	} {
		var m Money
		var err error
		within(t, 2*time.Second, c.name, func() { m, err = MoneyFromDecimal(c.d) })
		if c.kind != "" {
			var me *MoneyError
			if !errors.As(err, &me) || me.Kind != c.kind || !errors.Is(err, ErrInvalidMoney) {
				t.Errorf("%s: %v, %v; want a %s refusal", c.name, m, err, c.kind)
			}
			continue
		}
		if err != nil || m.String() != c.want {
			t.Errorf("%s: %v, %v; want %s", c.name, m, err, c.want)
		}
	}
}

func TestMoneyJSON_Edges_EQ1(t *testing.T) {
	for in, want := range map[string]string{`-0`: "0", `"-0"`: "0", `0.0000000`: "0", `"+0.5"`: "0.5", `" 5 "`: "5"} {
		var m Money
		if err := json.Unmarshal([]byte(in), &m); err != nil || m.String() != want {
			t.Errorf("%s → %v, %v; want %s", in, m, err, want)
		}
	}
	for _, in := range []string{`1e3`, `"1E3"`, `"NaN"`, `"Infinity"`, `0.0000001`, `true`, `"١٢"`, `"1_000"`} {
		var m Money
		if err := json.Unmarshal([]byte(in), &m); err == nil {
			t.Errorf("%s accepted as %v", in, m)
		}
	}
	// a huge JSON number is judged from its text, quickly
	big := `1` + strings.Repeat("0", 1_000_000)
	within(t, 2*time.Second, "1e1000000", func() {
		var m Money
		if err := json.Unmarshal([]byte(big), &m); errKind(err) != string(MoneyRange) {
			t.Errorf("huge number: %v", err)
		}
	})
}

func TestCostForDuration_Edges_EQ1(t *testing.T) {
	if _, err := CostForDuration(MustParseMoney("9007199254.740991"), 1<<62); errKind(err) != string(MoneyRange) {
		t.Fatalf("max rate × ~146 years: %v (want range)", err)
	}
	if got, err := CostForDuration(MustParseMoney("-0"), time.Hour); err != nil || got.String() != "0" {
		t.Fatalf("-0 rate: %v %v", got, err)
	}
	if got, err := CostForDuration(MustParseMoney("1"), time.Nanosecond); err != nil || got.String() != "0.000001" {
		t.Fatalf("1 ns at $1/h rounds UP to one micro-unit: %v %v", got, err)
	}
}
