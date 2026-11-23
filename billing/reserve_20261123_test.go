package billing

// Reserve / refund / balance / zero-cost metering (v0.1.12, additive). Mirrors the Python
// ab0t-quota reserve-result and refund-outcome test contracts.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/shopspring/decimal"
)

func serve(t *testing.T, status int, body string, seen *[]map[string]any, path *string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path != nil {
			*path = r.Method + " " + r.URL.Path
		}
		if seen != nil {
			raw, _ := io.ReadAll(r.Body)
			m := map[string]any{}
			_ = json.Unmarshal(raw, &m)
			*seen = append(*seen, m)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c, err := New(mesh.URLs{Billing: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func req() ReserveRequest {
	return ReserveRequest{UserID: "u-1", EstimatedCost: decimal.RequireFromString("0.35"), RequestID: "launch-1"}
}

func TestReserveFunds_Reserved_20261123(t *testing.T) {
	var seen []map[string]any
	var path string
	c := serve(t, 200, `{"reservation_id":"reservation_abc","reserved_amount":"0.35","expires_at":"2026-11-24T10:00:00+00:00","available_balance":"4.65","session_id":""}`, &seen, &path)
	r, err := c.ReserveFunds(context.Background(), "org-1", req())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Reserved() || r.ReservationID != "reservation_abc" || r.ExpiresAt == "" {
		t.Fatalf("got %+v", r)
	}
	if !r.ReservedAmount.Equal(decimal.RequireFromString("0.35")) || !r.AvailableBalance.Equal(decimal.RequireFromString("4.65")) {
		t.Fatalf("money: %+v", r)
	}
	if path != "POST /billing/org-1/reserve" {
		t.Fatalf("path %q", path)
	}
	b := seen[0]
	if b["org_id"] != "org-1" || b["request_id"] != "launch-1" || b["estimated_cost"] != "0.35" ||
		b["tool_id"] != "default" || b["operation_type"] != "api_call" {
		t.Fatalf("body %v", b)
	}
	if md, ok := b["metadata"].(map[string]any); !ok || md == nil {
		t.Fatalf("metadata must be an object, never null (billing's model rejects null): %v", b["metadata"])
	}
}

func TestReserveFunds_402KeepsBillingNumbers_20261123(t *testing.T) {
	c := serve(t, 402, `{"detail":{"error":"insufficient_balance","message":"x","available_balance":"0.10","requested_amount":"0.35","required_topup":"0.25"}}`, nil, nil)
	r, err := c.ReserveFunds(context.Background(), "org-1", req())
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != ReserveInsufficientFunds || r.Reserved() || r.Reason != "insufficient_balance" {
		t.Fatalf("got %+v", r)
	}
	if !r.AvailableBalance.Equal(decimal.RequireFromString("0.10")) || !r.RequiredTopup.Equal(decimal.RequireFromString("0.25")) ||
		!r.RequestedAmount.Equal(decimal.RequireFromString("0.35")) {
		t.Fatalf("money: %+v", r)
	}
}

func TestReserveFunds_402WithoutBodyHasNilMoney_20261123(t *testing.T) {
	c := serve(t, 402, `Payment Required`, nil, nil)
	r, err := c.ReserveFunds(context.Background(), "org-1", req())
	if err != nil || r.Outcome != ReserveInsufficientFunds || r.AvailableBalance != nil {
		t.Fatalf("absent money must be nil, never 0: %+v %v", r, err)
	}
}

func TestReserveFunds_429IsLimited_20261123(t *testing.T) {
	cases := [][2]string{
		{`{"detail":{"error":"daily_limit_exceeded","message":"x"}}`, "daily_limit_exceeded"},
		{`{"detail":{"error":"monthly_limit_exceeded","message":"x"}}`, "monthly_limit_exceeded"},
		{`{"detail":"Reserve rate limit exceeded (60/min/org)."}`, "rate_limited"},
	}
	for _, tc := range cases {
		c := serve(t, 429, tc[0], nil, nil)
		r, err := c.ReserveFunds(context.Background(), "org-1", req())
		if err != nil || r.Outcome != ReserveLimited || r.Reason != tc[1] {
			t.Fatalf("%s: got %+v %v", tc[0], r, err)
		}
	}
}

func TestReserveFunds_OtherFailuresFailClosed_20261123(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 500, 503} {
		c := serve(t, status, `{"detail":"no"}`, nil, nil)
		if r, err := c.ReserveFunds(context.Background(), "org-1", req()); err == nil {
			t.Fatalf("%d: expected an error, got %+v", status, r)
		}
	}
	c := serve(t, 200, `{"reserved_amount":"1"}`, nil, nil)
	if _, err := c.ReserveFunds(context.Background(), "org-1", req()); err == nil {
		t.Fatal("2xx without reservation_id must be an error")
	}
}

func TestReserveFunds_RefusesBadInputBeforeSending_20261123(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL})
	bad := []struct {
		org string
		in  ReserveRequest
	}{
		{"", req()},
		{"..", req()},
		{"org-1", ReserveRequest{UserID: "u", EstimatedCost: decimal.Zero}},
		{"org-1", ReserveRequest{EstimatedCost: decimal.NewFromInt(1)}},
		{"org-1", ReserveRequest{OrgID: "org-2", UserID: "u", EstimatedCost: decimal.NewFromInt(1)}},
	}
	for i, b := range bad {
		_, err := c.ReserveFunds(context.Background(), b.org, b.in)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("case %d: want ErrInvalidRequest, got %v", i, err)
		}
		if IsTransient(err) {
			t.Fatalf("case %d: a refused request is never transient", i)
		}
	}
	if called {
		t.Fatal("a refused request reached billing")
	}
}

func TestRefundReservationOutcome_ClassifiesFromCode_20261123(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   RefundOutcome
	}{
		{200, `{"status":"refunded","reservation_id":"r1","refunded_amount":"0.07","message":"ok"}`, RefundRefunded},
		{409, `{"detail":"Reservation is not active","code":"RESERVATION_NOT_ACTIVE"}`, RefundAlreadyTerminal},
		{409, `{"detail":"Reservation has already been settled","code":"RESERVATION_ALREADY_SETTLED"}`, RefundAlreadySettled},
		{404, `{"detail":"Reservation not found or expired","code":"RESERVATION_NOT_FOUND"}`, RefundNotFound},
		{409, `{"detail":"Reservation is not active"}`, RefundFailed},
		{404, `{"detail":"Reservation not found"}`, RefundFailed},
		{500, `{"detail":"boom"}`, RefundFailed},
		{403, `{"detail":"Forbidden"}`, RefundFailed},
	}
	for _, tc := range cases {
		var path string
		c := serve(t, tc.status, tc.body, nil, &path)
		got := c.RefundReservationOutcome(context.Background(), "org-1", "r1", "launch_failed")
		if got != tc.want {
			t.Fatalf("%d %s: got %s want %s", tc.status, tc.body, got, tc.want)
		}
		if path != "POST /billing/org-1/refund" {
			t.Fatalf("path %q", path)
		}
	}
	if !RefundFailed.HoldMayRemain() || RefundRefunded.HoldMayRemain() || RefundNotFound.HoldMayRemain() {
		t.Fatal("HoldMayRemain must be true only for RefundFailed")
	}
	c := serve(t, 200, `{}`, nil, nil)
	if got := c.RefundReservationOutcome(context.Background(), "org-1", " ", ""); got != RefundFailed {
		t.Fatalf("blank reservation id: %s", got)
	}
}

func TestGetBalance_20261123(t *testing.T) {
	var path string
	c := serve(t, 200, `{"id":"a","org_id":"org-1","balance":"5.00","credit_balance":"1.00","subscription_credit":"0","reserved_balance":"0.35","available_balance":"5.65","alert_threshold":"0","currency":"usd"}`, nil, &path)
	b, err := c.GetBalance(context.Background(), "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if path != "GET /billing/org-1/balance" || !b.AvailableBalance.Equal(decimal.RequireFromString("5.65")) ||
		!b.ReservedBalance.Equal(decimal.RequireFromString("0.35")) {
		t.Fatalf("%s %+v", path, b)
	}
}

func TestRecordResourceUsage_NeverPriced_20261123(t *testing.T) {
	var seen []map[string]any
	c := serve(t, 200, `{}`, &seen, nil)
	if err := c.RecordResourceUsage(context.Background(), RecordUsageRequest{OrgID: "org-1", UserID: "u", ToolID: "my-svc", ReservationID: "r1"}); err != nil {
		t.Fatal(err)
	}
	b := seen[0]
	if b["cost"] != "0" || b["platform_fee"] != "0" || b["resource_type"] != "compute" || b["request_id"] == nil {
		t.Fatalf("a metering row must be explicitly zero-cost: %v", b)
	}
	if err := c.RecordResourceUsage(context.Background(), RecordUsageRequest{OrgID: "org-1"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing ToolID must be refused, got %v", err)
	}
	if len(seen) != 1 {
		t.Fatal("refused row reached billing")
	}
}

func TestIsTransient_20261123(t *testing.T) {
	c := serve(t, 503, `{"detail":"down"}`, nil, nil)
	_, err := c.GetBalance(context.Background(), "org-1")
	if !IsTransient(err) {
		t.Fatalf("503 must be transient: %v", err)
	}
	c = serve(t, 403, `{"detail":"no"}`, nil, nil)
	_, err = c.GetBalance(context.Background(), "org-1")
	if IsTransient(err) {
		t.Fatal("403 is not transient")
	}
	if IsTransient(nil) {
		t.Fatal("nil is not transient")
	}
	if !IsTransient(errors.New("dial tcp: connection refused")) {
		t.Fatal("a transport failure (no HTTP answer) is transient")
	}
}
