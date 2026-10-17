package billing

// hardening_money_20261016_test.go — money exactness and grant idempotency of the
// v0.1.10 credit routes, asserted on the bytes the fake billing RECEIVED.
//
//	amounts travel as exact decimal strings, never via float      TestHardening_AmountWireIsExactDecimal_Property
//	billing's numbers (string OR JSON number) decode exactly       TestHardening_ResponseDecimalsDecodeExactly
//	non-positive amounts refused before any HTTP call              TestHardening_NonPositiveAmountNeverSent
//	same grant twice → one key → one credit                        TestHardening_SameGrantTwiceIsOneCredit
//	org id can never address another route                         TestHardening_OrgIDIsOnePathSegment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/shopspring/decimal"
)

type moneyFake struct {
	mu       sync.Mutex
	raw      []string // raw request bodies
	paths    []string
	credited map[string]decimal.Decimal // idempotency key → amount (first wins)
	respBody string                     // when set, returned verbatim
}

func (m *moneyFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raw = append(m.raw, string(b))
	m.paths = append(m.paths, r.URL.EscapedPath())
	var body struct {
		Amount         json.RawMessage `json:"amount"`
		IdempotencyKey string          `json:"idempotency_key"`
	}
	_ = json.Unmarshal(b, &body)
	if m.credited == nil {
		m.credited = map[string]decimal.Decimal{}
	}
	if _, dup := m.credited[body.IdempotencyKey]; !dup {
		var s string
		_ = json.Unmarshal(body.Amount, &s)
		d, _ := decimal.NewFromString(s)
		m.credited[body.IdempotencyKey] = d
	}
	w.Header().Set("Content-Type", "application/json")
	if m.respBody != "" {
		_, _ = w.Write([]byte(m.respBody))
		return
	}
	_, _ = w.Write([]byte(`{"org_id":"o1","old_balance":"0","new_balance":"0","amount":"0","operation":"x","available_balance":"0"}`))
}

func moneyClient(t *testing.T) (*Client, *moneyFake) {
	t.Helper()
	f := &moneyFake{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_money"})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func randDecimalString(r *rand.Rand) string {
	intDigits := 1 + r.Intn(22)
	frac := r.Intn(25)
	var b strings.Builder
	b.WriteByte(byte('1' + r.Intn(9)))
	for i := 1; i < intDigits; i++ {
		b.WriteByte(byte('0' + r.Intn(10)))
	}
	if frac > 0 {
		b.WriteByte('.')
		for i := 0; i < frac-1; i++ {
			b.WriteByte(byte('0' + r.Intn(10)))
		}
		b.WriteByte(byte('1' + r.Intn(9))) // no trailing zero, so String() round-trips verbatim
	}
	if r.Intn(4) == 0 { // tiny amounts too
		return "0." + strings.Repeat("0", r.Intn(20)) + "1"
	}
	return b.String()
}

func TestHardening_AmountWireIsExactDecimal_Property(t *testing.T) {
	c, f := moneyClient(t)
	r := rand.New(rand.NewSource(20261016))
	ctx := context.Background()
	for i := 0; i < 400; i++ {
		s := randDecimalString(r)
		d := decimal.RequireFromString(s)
		var err error
		switch i % 3 {
		case 0:
			_, err = c.GrantPromotionalCredit(ctx, "o1", PromotionalCreditRequest{Amount: d, IdempotencyKey: fmt.Sprint("k", i)})
		case 1:
			_, err = c.ApplyCreditGrant(ctx, "o1", ApplyCreditGrantRequest{Amount: d, Destination: "credit_balance",
				Lifecycle: "persistent", IdempotencyKey: fmt.Sprint("k", i)})
		default:
			_, err = c.GrantCredit(ctx, CreditGrantRequest{OrgID: "o1", UserID: "u", TierID: fmt.Sprint("t", i), Amount: d})
		}
		if err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		raw := f.raw[len(f.raw)-1]
		f.mu.Unlock()
		want := `"amount":"` + s + `"`
		if !strings.Contains(raw, want) {
			t.Fatalf("amount %s: billing received %s — want the exact decimal string %s (a float would round it)", s, raw, want)
		}
	}
}

func TestHardening_ResponseDecimalsDecodeExactly(t *testing.T) {
	for _, v := range []string{
		`"1234567890123456789.123456789"`, `1234567890123456789.123456789`, // JSON number, beyond float64
		`0.30000000000000004441`, `"0.1"`, `0.1`, `9007199254740993`, `"-0.000000000000000000001"`,
	} {
		c, f := moneyClient(t)
		f.respBody = fmt.Sprintf(`{"org_id":"o1","old_balance":%s,"new_balance":%s,"amount":%s,"available_balance":%s}`, v, v, v, v)
		res, err := c.GrantPromotionalCredit(context.Background(), "o1", PromotionalCreditRequest{Amount: decimal.NewFromInt(1), IdempotencyKey: "k"})
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		want := decimal.RequireFromString(strings.Trim(v, `"`))
		for name, got := range map[string]decimal.Decimal{"old": res.OldBalance, "new": res.NewBalance, "amount": res.Amount, "available": res.AvailableBalance} {
			if !got.Equal(want) {
				t.Fatalf("billing sent %s: %s decoded as %s — precision lost", v, name, got)
			}
		}
	}
}

func TestHardening_NonPositiveAmountNeverSent(t *testing.T) {
	c, f := moneyClient(t)
	ctx := context.Background()
	for _, s := range []string{"0", "0.000", "-1", "-0.0000000000001"} {
		d := decimal.RequireFromString(s)
		if _, err := c.GrantPromotionalCredit(ctx, "o1", PromotionalCreditRequest{Amount: d, IdempotencyKey: "k"}); err == nil {
			t.Errorf("GrantPromotionalCredit(%s) accepted", s)
		}
		if _, err := c.ApplyCreditGrant(ctx, "o1", ApplyCreditGrantRequest{Amount: d, Destination: "balance", Lifecycle: "persistent", IdempotencyKey: "k"}); err == nil {
			t.Errorf("ApplyCreditGrant(%s) accepted", s)
		}
		if _, err := c.GrantCredit(ctx, CreditGrantRequest{OrgID: "o1", UserID: "u", TierID: "t", Amount: d}); err == nil {
			t.Errorf("GrantCredit(%s) accepted", s)
		}
	}
	for _, key := range []string{"", " ", "\t\n"} {
		if _, err := c.GrantPromotionalCredit(ctx, "o1", PromotionalCreditRequest{Amount: decimal.NewFromInt(1), IdempotencyKey: key}); err == nil {
			t.Errorf("blank idempotency key %q accepted", key)
		}
	}
	for _, org := range []string{"", " ", "\t"} {
		if _, err := c.GrantCredit(ctx, CreditGrantRequest{OrgID: org, UserID: "u", TierID: "t", Amount: decimal.NewFromInt(1)}); err == nil {
			t.Errorf("blank org %q accepted", org)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.raw) != 0 {
		t.Fatalf("billing received %d request(s) for refused grants", len(f.raw))
	}
}

func TestHardening_SameGrantTwiceIsOneCredit(t *testing.T) {
	c, f := moneyClient(t)
	ctx := context.Background()
	// The same user's sign-up grant arrives under different event ids (redelivery,
	// replay, backfill) and with a whitespace-only explicit key (= unset).
	for i, in := range []CreditGrantRequest{
		{OrgID: "o1", UserID: "u1", TierID: "pro", EventID: "evt-1", Amount: decimal.RequireFromString("5.00")},
		{OrgID: "o1", UserID: "u1", TierID: "pro", EventID: "evt-2", Amount: decimal.RequireFromString("5.00")},
		{OrgID: "o1", UserID: "u1", TierID: "pro", EventID: "evt-3", Amount: decimal.RequireFromString("5.00"), IdempotencyKey: "  "},
	} {
		if _, err := c.GrantCredit(ctx, in); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.credited) != 1 {
		t.Fatalf("billing saw %d distinct idempotency keys for one user's one grant: %v", len(f.credited), f.credited)
	}
	for k, amt := range f.credited {
		if k != "user:u1:initial_credit:pro" || !amt.Equal(decimal.NewFromInt(5)) {
			t.Fatalf("key/amount = %q/%s", k, amt)
		}
	}
	// A different tier or user is a different grant.
	f.mu.Unlock()
	_, _ = c.GrantCredit(ctx, CreditGrantRequest{OrgID: "o1", UserID: "u1", TierID: "team", Amount: decimal.NewFromInt(1)})
	_, _ = c.GrantCredit(ctx, CreditGrantRequest{OrgID: "o1", UserID: "u2", TierID: "pro", Amount: decimal.NewFromInt(1)})
	f.mu.Lock()
	if len(f.credited) != 3 {
		t.Fatalf("distinct keys = %d, want 3", len(f.credited))
	}
}

func TestHardening_OrgIDIsOnePathSegment(t *testing.T) {
	c, f := moneyClient(t)
	ctx := context.Background()
	for _, org := range []string{"../admin", "o1/settle", "o1?x=1", "o1#frag", "o%2F1", "o 1", "ö1"} {
		_, err := c.GrantPromotionalCredit(ctx, org, PromotionalCreditRequest{Amount: decimal.NewFromInt(1), IdempotencyKey: "k-" + org})
		if err != nil {
			t.Fatalf("%q: %v", org, err)
		}
		f.mu.Lock()
		p := f.paths[len(f.paths)-1]
		f.mu.Unlock()
		segs := strings.Split(strings.Trim(p, "/"), "/")
		if len(segs) != 3 || segs[0] != "billing" || segs[2] != "promotional-credit" {
			t.Fatalf("org %q produced path %q — it escaped its segment", org, p)
		}
	}
}
