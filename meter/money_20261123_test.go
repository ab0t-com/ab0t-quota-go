package meter_test

// Exact money in the meter (v0.2.0). Same contract as the Python ResourceMeter tests in
// ab0t-quota 0.7.0 (tests/test_exact_money_20261123.py):
//   - pricing.products prices with more than 6 decimal places are refused at load (ParsePricing);
//   - an explicit HourlyRate / AllocationFee / EstimatedCost with more than 6 places is refused
//     before any reserve;
//   - the hold estimate (AllocationFee + HourlyRate × HoldHours) rounds ROUND_UP (away from zero,
//     billing's rule) to 6 places;
//   - the running cost over many heartbeats is exact when derived from elapsed time.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/meter"
	"github.com/ab0t-com/ab0t-quota-go/meter/metertest"
	"github.com/shopspring/decimal"
)

func newMoneyFake() *metertest.Fake { return metertest.New(map[string]string{"org-1": "5"}) }

func TestParsePricing_RefusesMoreThanSixDecimalPlaces_20261123(t *testing.T) {
	for _, price := range []string{`"0.1234567"`, `0.1234567`, `0.30000000000000004`, `"9007199254.740992"`} {
		raw := `{"products":{"vm":{"variants":{"s":{"price_per_hour":` + price + `,"allocation_price":"0"}}}}}`
		_, err := meter.ParsePricing([]byte(raw))
		if !errors.Is(err, meter.ErrConfig) ||
			!(strings.Contains(err.Error(), "decimal places") || strings.Contains(err.Error(), "out of range")) {
			t.Errorf("price %s: err = %v; want a load refusal naming the precision/range", price, err)
		}
	}
}

func TestParsePricing_Control_ExactPricesLoad_20261123(t *testing.T) {
	for _, price := range []string{`"0.123456"`, `0.123456`, `"0.1234560"`, `2`, `"9007199254.740991"`} {
		raw := `{"products":{"vm":{"variants":{"s":{"price_per_hour":` + price + `,"allocation_price":"0"}}}}}`
		book, err := meter.ParsePricing([]byte(raw))
		if err != nil {
			t.Errorf("price %s refused: %v", price, err)
			continue
		}
		p, err := book.Lookup("vm", "")
		want := decimal.RequireFromString(strings.Trim(price, `"`))
		if err != nil || !p.HourlyRate.Equal(want) {
			t.Errorf("price %s loaded as %v, %v", price, p.HourlyRate, err)
		}
	}
}

func TestReserve_HoldEstimateRoundsUp_20261123(t *testing.T) {
	for _, c := range []struct{ rate, hold, fee, want, halfEven string }{
		{"0.000001", "1.5", "0", "0.000002", "0.000002"},    // 0.0000015 tie -> up (2); modes agree
		{"0.000001", "2.5", "0", "0.000003", "0.000002"},    // 0.0000025 tie -> up (3)
		{"0.000005", "0.5", "0", "0.000003", "0.000002"},    // 0.0000025 tie -> up (3)
		{"0.000001", "1.01", "0", "0.000002", "0.000001"},   // 0.00000101 -> up (2); HALF_UP 0.000001 too
		{"0.000061", "0.0166", "0", "0.000002", "0.000001"}, // 0.0000010126 -> up (2); HALF_UP 0.000001 too
		{"0.25", "1", "0.01", "0.26", "0.26"},               // control: nothing to round
	} {
		f := newMoneyFake()
		m := newMeter(t, f, func(cfg *meter.Config) { cfg.HoldHours = d(c.hold) })
		_, err := m.Reserve(context.Background(), in(func(r *meter.ReserveInput) {
			r.HourlyRate, r.AllocationFee = d(c.rate), d(c.fee)
		}))
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		calls := f.ReserveCalls()
		if len(calls) != 1 || calls[0].EstimatedCost.String() != c.want {
			t.Errorf("%+v: estimate sent = %v; want %s", c, calls, c.want)
		}
		// Control: the unrounded product is not what was sent when something was rounded, and the
		// reference values — what ROUND_UP and HALF_EVEN give for the same product.
		raw := d(c.fee).Add(d(c.rate).Mul(d(c.hold)))
		if c.want != "0.26" && raw.String() == c.want {
			t.Errorf("%+v: control — nothing was rounded", c)
		}
		if up, he := raw.RoundUp(6).String(), raw.RoundBank(6).String(); up != c.want || he != c.halfEven {
			t.Errorf("%+v: control — reference ROUND_UP %s / HALF_EVEN %s", c, up, he)
		}
	}
}

func TestReserve_RefusesInexactExplicitPricesBeforeMoneyMoves_20261123(t *testing.T) {
	for name, over := range map[string]func(*meter.ReserveInput){
		"rate":     func(r *meter.ReserveInput) { r.HourlyRate = d("0.1234567") },
		"fee":      func(r *meter.ReserveInput) { r.AllocationFee = d("0.0000001") },
		"estimate": func(r *meter.ReserveInput) { r.EstimatedCost = d("1.0000001") },
	} {
		f := newMoneyFake()
		m := newMeter(t, f, nil)
		if _, err := m.Reserve(context.Background(), in(over)); !errors.Is(err, meter.ErrConfig) {
			t.Errorf("%s: err = %v; want ErrConfig", name, err)
		}
		if n := len(f.ReserveCalls()); n != 0 {
			t.Errorf("%s: %d reserve(s) reached billing", name, n)
		}
	}
}

func TestHeartbeats_RunningCostIsExactWhenDerived_20261123(t *testing.T) {
	// 1000 beats of 300 s at $0.36/h: each beat costs exactly $0.03; the lifetime cost is exactly $30
	// whether summed or derived.
	rate := billing.MustParseMoney("0.36")
	beat, err := billing.CostForDuration(rate, 300*time.Second)
	if err != nil || beat.String() != "0.03" {
		t.Fatalf("beat = %v, %v", beat, err)
	}
	var sum billing.Money
	for i := 0; i < 1000; i++ {
		if sum, err = sum.Add(beat); err != nil {
			t.Fatal(err)
		}
	}
	derived, _ := billing.CostForDuration(rate, 1000*300*time.Second)
	if sum.String() != "30" || derived.String() != "30" {
		t.Fatalf("sum %s, derived %s; want 30", sum, derived)
	}
}
