package billing

// Money is never a binary float (v0.2.0). Same contract as the Python ab0t-quota 0.7.0 tests
// (tests/test_exact_money_20261123.py): both suites read the SAME golden file
// (testdata/money_golden_20261123.json, an identical copy of the Python one).
//
// Pinned: ParseMoney / String / micro-units / Add / Sub / CostForDuration (ROUND_UP — away from
// zero, the billing service's rule; controls show HALF_EVEN and HALF_UP differ on golden rows) on
// every golden case; heartbeat accumulation (derive, don't accumulate); Money marshals as a JSON
// string; QuotaCheckRequest.CostExact sends a string while the deprecated float field is unchanged;
// money fields of billing answers and funds events are read from the exact number text, never
// float64. Each assertion has a control that fails on the old behaviour.

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/shopspring/decimal"
)

type moneyGolden struct {
	Parse []struct {
		In     string `json:"in"`
		Out    string `json:"out"`
		Micros int64  `json:"micros"`
		Error  string `json:"error"`
	} `json:"parse"`
	Add        []goldenBinary `json:"add"`
	Sub        []goldenBinary `json:"sub"`
	FromMicros []struct {
		Micros int64  `json:"micros"`
		Out    string `json:"out"`
		Error  string `json:"error"`
	} `json:"from_micros"`
	Cost []struct {
		Rate    string `json:"rate"`
		Seconds string `json:"seconds"`
		Out     string `json:"out"`
		Note    string `json:"note"`
	} `json:"cost"`
	Heartbeats []struct {
		Rate            string `json:"rate"`
		IntervalSeconds string `json:"interval_seconds"`
		Beats           int    `json:"beats"`
		SummedOut       string `json:"summed_out"`
		DerivedOut      string `json:"derived_out"`
	} `json:"heartbeats"`

	Rounding string `json:"rounding"`
}

type goldenBinary struct {
	A, B, Out, Error string
}

func loadMoneyGolden(t *testing.T) moneyGolden {
	t.Helper()
	raw, err := os.ReadFile("../testdata/money_golden_20261123.json")
	if err != nil {
		t.Fatal(err)
	}
	var g moneyGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Parse) == 0 || len(g.Cost) == 0 || len(g.Heartbeats) == 0 {
		t.Fatal("golden file is missing sections")
	}
	return g
}

func errKind(err error) string {
	var me *MoneyError
	if errors.As(err, &me) {
		return string(me.Kind)
	}
	return ""
}

// seconds parses a golden "seconds" string (<= 6 decimal places) into an exact Duration.
func seconds(t *testing.T, s string) time.Duration {
	t.Helper()
	m := MustParseMoney(s) // same grammar: micro-units of a second
	return time.Duration(m.Micros()) * time.Microsecond
}

func TestMoneyGolden_Parse_20261123(t *testing.T) {
	for _, c := range loadMoneyGolden(t).Parse {
		m, err := ParseMoney(c.In)
		if c.Error != "" {
			if err == nil || errKind(err) != c.Error || !errors.Is(err, ErrInvalidMoney) {
				t.Errorf("ParseMoney(%q) = %v, %v; want a %s refusal", c.In, m, err, c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMoney(%q): %v", c.In, err)
			continue
		}
		if m.String() != c.Out || m.Micros() != c.Micros {
			t.Errorf("ParseMoney(%q) = %s (%d µ); want %s (%d µ)", c.In, m, m.Micros(), c.Out, c.Micros)
		}
		if back, err := MoneyFromMicros(c.Micros); err != nil || back != m {
			t.Errorf("MoneyFromMicros(%d) = %v, %v; want %s", c.Micros, back, err, m)
		}
		if viaDec, err := MoneyFromDecimal(m.Decimal()); err != nil || viaDec != m {
			t.Errorf("decimal round trip of %s = %v, %v", m, viaDec, err)
		}
	}
}

func TestMoneyGolden_AddSub_20261123(t *testing.T) {
	g := loadMoneyGolden(t)
	for op, cases := range map[string][]goldenBinary{"add": g.Add, "sub": g.Sub} {
		for _, c := range cases {
			a, b := MustParseMoney(c.A), MustParseMoney(c.B)
			var got Money
			var err error
			if op == "add" {
				got, err = a.Add(b)
			} else {
				got, err = a.Sub(b)
			}
			if c.Error != "" {
				if errKind(err) != c.Error {
					t.Errorf("%s %s %s = %v, %v; want %s refusal", c.A, op, c.B, got, err, c.Error)
				}
				continue
			}
			if err != nil || got.String() != c.Out {
				t.Errorf("%s %s %s = %v, %v; want %s", c.A, op, c.B, got, err, c.Out)
			}
		}
	}
}

func TestMoneyGolden_FromMicros_20261123(t *testing.T) {
	for _, c := range loadMoneyGolden(t).FromMicros {
		m, err := MoneyFromMicros(c.Micros)
		if c.Error != "" {
			if errKind(err) != c.Error {
				t.Errorf("MoneyFromMicros(%d) = %v, %v; want %s", c.Micros, m, err, c.Error)
			}
			continue
		}
		if err != nil || m.String() != c.Out {
			t.Errorf("MoneyFromMicros(%d) = %v, %v; want %s", c.Micros, m, err, c.Out)
		}
	}
}

func TestMoneyGolden_CostRoundUp_20261123(t *testing.T) {
	g := loadMoneyGolden(t)
	if g.Rounding != "ROUND_UP" {
		t.Fatalf("golden rounding = %q; want ROUND_UP (billing's rule)", g.Rounding)
	}
	for _, c := range g.Cost {
		got, err := CostForDuration(MustParseMoney(c.Rate), seconds(t, c.Seconds))
		if err != nil || got.String() != c.Out {
			t.Errorf("CostForDuration(%s, %ss) = %v, %v; want %s (%s)", c.Rate, c.Seconds, got, err, c.Out, c.Note)
		}
		// and the golden value IS the exact product rounded ROUND_UP (an independent reference)
		if ref := roundUpCost(c.Rate, c.Seconds); ref != c.Out {
			t.Errorf("reference ROUND_UP(%s × %ss / 3600) = %s; golden says %s", c.Rate, c.Seconds, ref, c.Out)
		}
	}
}

// exactCost is rate × secs / 3600 (multiply, then divide) to 40 places — far beyond 6, so the
// rounding below sees the true remainder. A shopspring reference, independent of divRoundUp.
func exactCost(rate, secs string) decimal.Decimal {
	return decimal.RequireFromString(rate).Mul(decimal.RequireFromString(secs)).DivRound(decimal.NewFromInt(3600), 40)
}

// roundUpCost is billing's rule: ROUND_UP (away from zero) to 6 places.
func roundUpCost(rate, secs string) string { return exactCost(rate, secs).RoundUp(6).String() }

// halfUpCost is a NOT-chosen rounding mode, used only as a control.
func halfUpCost(rate, secs string) string {
	return exactCost(rate, secs).Round(6).String() // shopspring Round = half away from zero
}

// halfEvenCost is a NOT-chosen rounding mode (the pre-ruling choice), used only as a control.
func halfEvenCost(rate, secs string) string { return exactCost(rate, secs).RoundBank(6).String() }

func TestMoneyGolden_Control_RowsDiscriminateTheMode_20261123(t *testing.T) {
	g := loadMoneyGolden(t)
	ties, differHalfUp, differHalfEven := 0, 0, 0
	for _, c := range g.Cost {
		if strings.Contains(c.Note, "tie") {
			ties++
		}
		if halfUpCost(c.Rate, c.Seconds) != c.Out {
			differHalfUp++
		}
		if halfEvenCost(c.Rate, c.Seconds) != c.Out {
			differHalfEven++
		}
	}
	if ties == 0 || differHalfUp == 0 || differHalfEven == 0 {
		t.Fatalf("ties=%d differHalfUp=%d differHalfEven=%d: the golden file cannot tell ROUND_UP from the other modes",
			ties, differHalfUp, differHalfEven)
	}
	// The named control row: $0.000061/h for 60 s = 1.0166... micro-units.
	got, err := CostForDuration(MustParseMoney("0.000061"), 60*time.Second)
	if err != nil || got.String() != "0.000002" {
		t.Fatalf("CostForDuration(0.000061, 60s) = %v, %v; want 0.000002", got, err)
	}
	if he, hu := halfEvenCost("0.000061", "60"), halfUpCost("0.000061", "60"); he != "0.000001" || hu != "0.000001" {
		t.Fatalf("control: HALF_EVEN %s / HALF_UP %s; want 0.000001 for both", he, hu)
	}
}

func TestMoney_CostRoundsNegativeRatesAwayFromZero_20261123(t *testing.T) {
	for _, c := range []struct{ rate, secs, want string }{
		{"-1", "0.0018", "-0.000001"},    // -0.0000005 tie
		{"-0.000061", "60", "-0.000002"}, // -1.0166... micro-units
	} {
		got, err := CostForDuration(MustParseMoney(c.rate), seconds(t, c.secs))
		if err != nil || got.String() != c.want {
			t.Errorf("CostForDuration(%s, %ss) = %v, %v; want %s (away from zero)", c.rate, c.secs, got, err, c.want)
		}
	}
	// Control: HALF_EVEN gives the smaller magnitude.
	if he := halfEvenCost("-0.000061", "60"); he != "-0.000001" {
		t.Fatalf("control: HALF_EVEN = %s", he)
	}
}

func TestDivRoundUp_20261123(t *testing.T) {
	for _, c := range []struct{ num, den, want int64 }{
		{7, 2, 4}, {-7, 2, -4}, {6, 2, 3}, {-6, 2, -3}, {0, 5, 0}, {1, 3600, 1}, {-1, 3600, -1}, {3599, 3600, 1},
	} {
		if got := divRoundUp(big.NewInt(c.num), big.NewInt(c.den)); got.Int64() != c.want {
			t.Errorf("divRoundUp(%d, %d) = %s; want %d", c.num, c.den, got, c.want)
		}
	}
}

func TestMoneyGolden_HeartbeatsSumAndDeriveExactly_20261123(t *testing.T) {
	for _, c := range loadMoneyGolden(t).Heartbeats {
		rate, interval := MustParseMoney(c.Rate), seconds(t, c.IntervalSeconds)
		beat, err := CostForDuration(rate, interval)
		if err != nil {
			t.Fatal(err)
		}
		var sum Money
		for i := 0; i < c.Beats; i++ {
			if sum, err = sum.Add(beat); err != nil {
				t.Fatal(err)
			}
		}
		if sum.String() != c.SummedOut {
			t.Errorf("%s/h × %d beats of %ss summed = %s; want %s", c.Rate, c.Beats, c.IntervalSeconds, sum, c.SummedOut)
		}
		derived, err := CostForDuration(rate, interval*time.Duration(c.Beats))
		if err != nil || derived.String() != c.DerivedOut {
			t.Errorf("derived lifetime cost = %v, %v; want %s", derived, err, c.DerivedOut)
		}
	}
}

func TestMoney_Control_FloatAccumulationDrifts_20261123(t *testing.T) {
	f := 0.0
	for i := 0; i < 10; i++ {
		f += 0.1
	}
	if f == 1.0 {
		t.Fatal("control: ten float 0.1s summed to exactly 1 — the drift this type exists to avoid did not show")
	}
	var m Money
	tenth := MustParseMoney("0.1")
	for i := 0; i < 10; i++ {
		m, _ = m.Add(tenth)
	}
	if m.String() != "1" {
		t.Fatalf("ten Money 0.1s = %s; want 1", m)
	}
}

func TestMoney_CostRefusesNegativeDuration_20261123(t *testing.T) {
	if _, err := CostForDuration(MustParseMoney("1"), -time.Second); errKind(err) != string(MoneyRange) {
		t.Fatalf("negative duration: %v", err)
	}
}

func TestMoney_UnicodeDigitsAreSyntaxErrors_20261123(t *testing.T) {
	if _, err := ParseMoney("٣"); errKind(err) != string(MoneySyntax) { // ARABIC-INDIC DIGIT THREE
		t.Fatalf("unicode digit accepted: %v", err)
	}
}

func TestMoney_JSONIsAStringAndReadsNumbersExactly_20261123(t *testing.T) {
	b, err := json.Marshal(struct {
		A Money  `json:"a"`
		B *Money `json:"b,omitempty"`
	}{A: MustParseMoney("0.30")})
	if err != nil || string(b) != `{"a":"0.3"}` {
		t.Fatalf("marshal = %s, %v", b, err)
	}
	var got struct{ A, B, C Money }
	if err := json.Unmarshal([]byte(`{"A":"100000000.000001","B":123456789.123457,"C":null}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.A.String() != "100000000.000001" || got.B.String() != "123456789.123457" || !got.C.IsZero() {
		t.Fatalf("unmarshal = %+v", got)
	}
	if err := json.Unmarshal([]byte(`{"A":0.1234567}`), &got); !errors.Is(err, ErrInvalidMoney) {
		t.Fatalf("7 decimal places accepted: %v", err)
	}
}

func TestQuotaCheckRequest_CostExactIsAStringAndTheOldBodyIsUnchanged_20261123(t *testing.T) {
	old, err := json.Marshal(QuotaCheckRequest{UserID: "u", OrgID: "o", ResourceKey: "r", Cost: 1.5})
	if err != nil || string(old) != `{"user_id":"u","org_id":"o","resource_key":"r","cost":1.5}` {
		t.Fatalf("deprecated float body changed: %s, %v", old, err)
	}
	none, _ := json.Marshal(QuotaCheckRequest{UserID: "u", OrgID: "o", ResourceKey: "r"})
	if string(none) != `{"user_id":"u","org_id":"o","resource_key":"r"}` {
		t.Fatalf("zero-cost body changed: %s", none)
	}
	a, b := MustParseMoney("0.1"), MustParseMoney("0.2")
	sum, _ := a.Add(b)
	exact, err := json.Marshal(QuotaCheckRequest{UserID: "u", OrgID: "o", ResourceKey: "r", Cost: 9, CostExact: &sum})
	if err != nil || string(exact) != `{"user_id":"u","org_id":"o","resource_key":"r","cost":"0.3"}` {
		t.Fatalf("exact body = %s, %v", exact, err)
	}
	// Control: the deprecated float field carries the binary noise.
	// Runtime values: `0.1 + 0.2` written as constants is evaluated EXACTLY by the Go compiler (= 0.3), which would make
	// this control vacuous; the float64 noise only appears when the sum is computed at run time.
	x, y := 0.1, 0.2
	noisy, _ := json.Marshal(QuotaCheckRequest{Cost: x + y})
	if !strings.Contains(string(noisy), `"cost":0.30000000000000004`) {
		t.Fatalf("control: float body = %s", noisy)
	}
}

// beyondFloat is a JSON number float64 cannot carry: through float64 it becomes 1234567890123456800.
const beyondFloat = `1234567890123456789.123456789`

func TestDecodeExact_MoneyFromNumberTextNotFloat_20261123(t *testing.T) {
	raw := []byte(`{"actual_cost":` + beyondFloat + `,"detail":{"required_topup":` + beyondFloat + `}}`)
	exact := decodeExact(raw)
	want := decimal.RequireFromString(beyondFloat)
	if got := decimalField(exact, "actual_cost"); got == nil || !got.Equal(want) {
		t.Fatalf("decodeExact actual_cost = %v; want %s", got, want)
	}
	// Control: the old decoding (json.Unmarshal → float64) loses the digits.
	var viaFloat map[string]any
	_ = json.Unmarshal(raw, &viaFloat)
	if got := decimalField(viaFloat, "actual_cost"); got != nil && got.Equal(want) {
		t.Fatal("control: float64 decoding kept every digit — the test cannot discriminate")
	}
	if len(decodeExact([]byte(`{"a":1} trailing`))) != 0 || len(decodeExact([]byte(`[1]`))) != 0 {
		t.Fatal("decodeExact accepted trailing data or a non-object")
	}
}

func TestBillingAnswers_NumericMoneyIsExact_20261123(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/reserve"):
			w.WriteHeader(402)
			_, _ = w.Write([]byte(`{"detail":{"error":"insufficient_balance","available_balance":` + beyondFloat +
				`,"requested_amount":1,"required_topup":` + beyondFloat + `}}`))
		case strings.HasSuffix(r.URL.Path, "/commit"):
			_, _ = w.Write([]byte(`{"reservation_id":"r1","actual_cost":` + beyondFloat + `,"new_balance":0.000001}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_money"})
	if err != nil {
		t.Fatal(err)
	}
	want := decimal.RequireFromString(beyondFloat)
	rr, err := c.ReserveFunds(context.Background(), "org-1", ReserveRequest{UserID: "u", EstimatedCost: decimal.NewFromInt(1)})
	if err != nil || rr.Outcome != ReserveInsufficientFunds {
		t.Fatalf("reserve = %+v, %v", rr, err)
	}
	if rr.RequiredTopup == nil || rr.AvailableBalance == nil || !rr.RequiredTopup.Equal(want) || !rr.AvailableBalance.Equal(want) {
		t.Fatalf("402 money = %v / %v; want %s", rr.RequiredTopup, rr.AvailableBalance, want)
	}
	cr, err := c.CommitReservationResult(context.Background(), "org-1", "r1",
		CommitRequest{StartedAt: time.Unix(0, 0), StoppedAt: time.Unix(3600, 0)})
	if err != nil || cr.ActualCost == nil || cr.NewBalance == nil || !cr.ActualCost.Equal(want) || cr.NewBalance.String() != "0.000001" {
		t.Fatalf("commit = %+v, %v", cr, err)
	}
	if _, isFloat := cr.Raw["actual_cost"].(float64); !isFloat {
		t.Fatal("CommitResult.Raw decoding changed (it must stay as before)")
	}
}

func TestFundsEvent_NumericMoneyIsExactAndRawUnchanged_20261123(t *testing.T) {
	msg := []byte(`{"event_type":"billing.funds_low","trigger":"heartbeat","org_id":"o","reservation_id":"r",` +
		`"accrued":` + beyondFloat + `,"runway_seconds":900}`)
	ev, err := ParseFundsEvent(msg)
	if err != nil || ev == nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Accrued == nil || !ev.Accrued.Equal(decimal.RequireFromString(beyondFloat)) {
		t.Fatalf("accrued = %v", ev.Accrued)
	}
	if ev.RunwaySeconds == nil || *ev.RunwaySeconds != 900 {
		t.Fatalf("runway_seconds = %v", ev.RunwaySeconds)
	}
	if _, isFloat := ev.Raw["accrued"].(float64); !isFloat {
		t.Fatal("FundsEvent.Raw decoding changed (it must stay as before)")
	}
}
