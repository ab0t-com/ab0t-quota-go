package counters

// Claim 2 (finding QG-05) — accumulator period-TTL parity. The accumulator
// key name matches Python after P5.3, but the TTL did not: only DAILY
// agreed. A shorter Go TTL expires a live spend/usage accumulator EARLY,
// mid-period, silently zeroing it → under-billing (accumulators feed
// billing). Python's TTL = period length + a 1-day dashboard buffer
// (accumulator.py:39-50). This asserts Go's PeriodTTL equals Python's for
// every period.
//
// RED before the fix (Go had 2h / 48h / 14d / 45d).

import (
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

func TestPeriodTTL_MatchesPython_QG05(t *testing.T) {
	// Python _period_ttl_seconds: period_len + 86400 buffer.
	cases := []struct {
		reset config.ResetPeriod
		want  time.Duration
	}{
		{config.ResetHourly, (3600 + 86400) * time.Second},     // 25h
		{config.ResetDaily, (86400 + 86400) * time.Second},     // 48h
		{config.ResetWeekly, (604800 + 86400) * time.Second},   // 8d
		{config.ResetMonthly, (2678400 + 86400) * time.Second}, // 31d + 1d = 32d
	}
	for _, tc := range cases {
		if got := PeriodTTL(tc.reset); got != tc.want {
			t.Errorf("QG-05: PeriodTTL(%v)=%v, want Python's %v", tc.reset, got, tc.want)
		}
	}
}
