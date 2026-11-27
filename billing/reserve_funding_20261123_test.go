package billing

// funding_sources on reserve (v0.2.0, optional): absent = every bucket and the body is exactly as
// before; declared = sent as billing's bucket names; a bad list is refused before sending.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/shopspring/decimal"
)

func fundingClient(t *testing.T, hits *int32, bodies *[]map[string]any) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		raw, _ := io.ReadAll(r.Body)
		m := map[string]any{}
		_ = json.Unmarshal(raw, &m)
		*bodies = append(*bodies, m)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"reservation_id":"r-1","reserved_amount":"1","available_balance":"4"}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(mesh.URLs{Billing: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func fundingReq(sources []string) ReserveRequest {
	return ReserveRequest{UserID: "u-1", EstimatedCost: decimal.RequireFromString("1"), FundingSources: sources}
}

func TestReserveFunds_FundingSources_20261123(t *testing.T) {
	var hits int32
	var bodies []map[string]any
	c := fundingClient(t, &hits, &bodies)
	ctx := context.Background()

	// absent → the key is not sent at all (billing's default: every bucket; body as before v0.2.0)
	if _, err := c.ReserveFunds(ctx, "org-1", fundingReq(nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := bodies[0]["funding_sources"]; ok {
		t.Fatalf("absent funding_sources must not be sent: %v", bodies[0])
	}
	// declared → sent verbatim, in order
	if _, err := c.ReserveFunds(ctx, "org-1", fundingReq([]string{FundingSubscriptionCredit, FundingBalance})); err != nil {
		t.Fatal(err)
	}
	got, _ := bodies[1]["funding_sources"].([]any)
	if len(got) != 2 || got[0] != "subscription_credit" || got[1] != "balance" {
		t.Fatalf("funding_sources sent as %v", bodies[1]["funding_sources"])
	}

	// refused before sending
	for name, bad := range map[string][]string{
		"empty list":     {},
		"unknown bucket": {"cash"},
		"repeated":       {FundingBalance, FundingBalance},
		"wrong case":     {"Balance"},
	} {
		if _, err := c.ReserveFunds(ctx, "org-1", fundingReq(bad)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: got %v, want ErrInvalidRequest", name, err)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("a refused funding_sources reached billing (hits=%d, want 2)", n)
	}
}

// billing's echo of the applied funding_sources is read leniently (the Python ReserveResult's).
func TestReserveFunds_FundingSourcesEcho_20261123(t *testing.T) {
	for reply, want := range map[string]int{
		`{"reservation_id":"r-1","reserved_amount":"1","funding_sources":["balance"]}`: 1,
		`{"reservation_id":"r-1","reserved_amount":"1"}`:                               -1,
		`{"reservation_id":"r-1","reserved_amount":"1","funding_sources":"balance"}`:   -1, // not a list → nil, never an error
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, reply)
		}))
		c, _ := New(mesh.URLs{Billing: srv.URL})
		r, err := c.ReserveFunds(context.Background(), "org-1", fundingReq([]string{FundingBalance}))
		srv.Close()
		if err != nil || !r.Reserved() {
			t.Fatalf("%s: %v %v", reply, r, err)
		}
		if (want == -1 && r.FundingSources != nil) || (want >= 0 && len(r.FundingSources) != want) {
			t.Fatalf("%s: echo %v", reply, r.FundingSources)
		}
	}
}

func TestValidateFundingSources_20261123(t *testing.T) {
	if err := ValidateFundingSources(nil); err != nil {
		t.Fatalf("nil = every bucket: %v", err)
	}
	if err := ValidateFundingSources([]string{FundingBalance, FundingSubscriptionCredit, FundingCreditBalance}); err != nil {
		t.Fatalf("all three buckets: %v", err)
	}
	if ValidateFundingSources([]string{}) == nil || ValidateFundingSources([]string{"credit"}) == nil {
		t.Fatal("an empty list and an unknown name must be refused")
	}
	// the same literal list config pins (config cannot import billing)
	if FundingBalance != "balance" || FundingSubscriptionCredit != "subscription_credit" || FundingCreditBalance != "credit_balance" {
		t.Fatal("billing's bucket names changed — update config.fundingBuckets too")
	}
}
