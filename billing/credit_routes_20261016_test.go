package billing

// Ticket 20261016_signup_credit_receiver_404_and_client_gaps, finding 2.
//
// v0.1.9's Client.GrantCredit POSTed /billing/credits/grant — a route billing has
// never served (routers: billing/output/app/main.py:219-231; route list
// app/api/billing.py). Every call 404'd and no credit moved.
//
// fakeBilling below is modelled on billing's REAL handlers, read 2026-10-16:
//
//	POST /billing/{org_id}/promotional-credit   app/api/billing.py:1831
//	  body  PromotionalCreditRequest  app/models/billing.py:833
//	        {amount: Decimal >0, reason: str="initial_credit", idempotency_key: str (non-blank)}
//	  caps  $500/grant (400), 10/h/org (429)  :1854-1880
//	  dedup same idempotency_key → the ORIGINAL response, no money moves  :1882-1897, :1995-2010
//	POST /billing/{org_id}/apply-credit-grant   app/api/billing.py:2051
//	  body  CreditGrantRequest  app/models/billing.py:849
//	        {amount, destination: balance|credit_balance|subscription_credit,
//	         lifecycle: persistent|use_it_or_lose_it|rollover_unlimited|rollover_capped,
//	         rollover_max (required iff rollover_capped), idempotency_key, reason, source?, source_tier?}
//	  reply UpdateBalanceResponse  app/models/billing.py:1025 (Decimals serialised as JSON strings)
//	auth: a mesh API key in X-API-Key (BillingCreditIssuer, app/auth.py:417)
//	anything else: FastAPI's 404 {"detail":"Not Found"}

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/shopspring/decimal"
)

type fakeBilling struct {
	t       *testing.T
	mu      sync.Mutex
	credit  map[string]decimal.Decimal // org → credit_balance
	seen    map[string]map[string]any  // scope|org|key → original response
	calls   []string                   // "METHOD escaped-path"
	lastKey string
}

func newFakeBilling(t *testing.T) (*fakeBilling, *httptest.Server) {
	f := &fakeBilling{t: t, credit: map[string]decimal.Decimal{}, seen: map[string]map[string]any{}}
	return f, httptest.NewServer(f)
}

func (f *fakeBilling) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.EscapedPath())
	notFound := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	if r.Method != http.MethodPost || len(parts) != 3 || parts[0] != "billing" {
		notFound()
		return
	}
	org, route := parts[1], parts[2]
	if route != "promotional-credit" && route != "apply-credit-grant" {
		notFound()
		return
	}
	if r.Header.Get("X-API-Key") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Not authenticated"}`))
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	amount, err := decimal.NewFromString(toString(body["amount"]))
	key, _ := body["idempotency_key"].(string)
	if err != nil || amount.Sign() <= 0 || strings.TrimSpace(key) == "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"validation error"}`))
		return
	}
	f.lastKey = key
	op := "promotional_credit"
	if route == "apply-credit-grant" {
		op = "credit_grant"
		dest, _ := body["destination"].(string)
		life, _ := body["lifecycle"].(string)
		_, hasMax := body["rollover_max"]
		if dest == "" || life == "" || (life == "rollover_capped") != hasMax {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"detail":"validation error"}`))
			return
		}
	} else if amount.GreaterThan(decimal.NewFromInt(500)) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"Promotional credit exceeds single-credit maximum"}`))
		return
	}
	dk := route + "|" + org + "|" + key
	if orig, ok := f.seen[dk]; ok { // durable dedup: the ORIGINAL result, nothing moves
		_ = json.NewEncoder(w).Encode(orig)
		return
	}
	old := f.credit[org]
	f.credit[org] = old.Add(amount)
	resp := map[string]any{
		"org_id": org, "old_balance": old.String(), "new_balance": f.credit[org].String(),
		"amount": amount.String(), "operation": op, "available_balance": f.credit[org].String(),
		"payment_id": "promo:" + key, "description": "Promotional credit: " + toString(body["reason"]),
	}
	f.seen[dk] = resp
	_ = json.NewEncoder(w).Encode(resp)
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return decimal.NewFromFloat(x).String()
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// The pre-existing typed method — same signature as v0.1.9 — now reaches billing's
// real promotional-credit route and moves the money exactly once. RED on v0.1.9
// (HTTP 404 from /billing/credits/grant).
func TestGrantCredit_UsesRealPromotionalCreditRoute_20261016(t *testing.T) {
	f, srv := newFakeBilling(t)
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_test"})

	in := CreditGrantRequest{UserID: "u1", OrgID: "org-1", TierID: "free", Amount: decimal.RequireFromString("5.00")}
	resp, err := c.GrantCredit(context.Background(), in)
	if err != nil {
		t.Fatalf("GrantCredit: %v (calls: %v)", err, f.calls)
	}
	if !resp.Balance.Equal(decimal.RequireFromString("5")) || resp.GrantID == "" {
		t.Fatalf("response = %+v", resp)
	}
	if f.calls[0] != "POST /billing/org-1/promotional-credit" {
		t.Fatalf("called %q", f.calls[0])
	}
	// A retry (lost response, redelivered event) must not move money twice: the
	// default idempotency key is derived from the grant, not from the attempt.
	if _, err := c.GrantCredit(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := f.credit["org-1"]; !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("credit after retry = %s, want 5 (double grant)", got)
	}
	if f.lastKey != "user:u1:initial_credit:free" {
		t.Fatalf("idempotency key = %q, want the Python-compatible sign-up key", f.lastKey)
	}
}

// A grant that cannot name an org or a stable idempotency key is refused locally,
// never sent with a blank key (billing would 422 it; a blank key false-dedups).
func TestGrantCredit_RefusesWithoutOrgOrKey_20261016(t *testing.T) {
	f, srv := newFakeBilling(t)
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_test"})
	if _, err := c.GrantCredit(context.Background(), CreditGrantRequest{UserID: "u1", TierID: "free",
		Amount: decimal.NewFromInt(1)}); err == nil {
		t.Error("no OrgID: want an error")
	}
	if _, err := c.GrantCredit(context.Background(), CreditGrantRequest{OrgID: "o1",
		Amount: decimal.NewFromInt(1)}); err == nil {
		t.Error("no user/tier/event/key: want an error")
	}
	if _, err := c.GrantCredit(context.Background(), CreditGrantRequest{OrgID: "o1", UserID: "u", TierID: "t",
		Amount: decimal.Zero}); err == nil {
		t.Error("zero amount: want an error")
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing should reach billing, got %v", f.calls)
	}
}

func TestGrantPromotionalCredit_TypedErrorsAndEscaping_20261016(t *testing.T) {
	f, srv := newFakeBilling(t)
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_test"})
	ctx := context.Background()

	resp, err := c.GrantPromotionalCredit(ctx, "org-2", PromotionalCreditRequest{
		Amount: decimal.RequireFromString("10.00"), Reason: "goodwill", IdempotencyKey: "case:42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Operation != "promotional_credit" || !resp.NewBalance.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("resp = %+v", resp)
	}
	// Billing's caps surface as a typed status the caller can branch on.
	_, err = c.GrantPromotionalCredit(ctx, "org-2", PromotionalCreditRequest{
		Amount: decimal.NewFromInt(501), IdempotencyKey: "case:43"})
	if !httpx.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("over the single-grant cap: err = %v, want HTTP 400", err)
	}
	// An org id is a path SEGMENT: it can never address another route.
	_, _ = c.GrantPromotionalCredit(ctx, "../admin", PromotionalCreditRequest{
		Amount: decimal.NewFromInt(1), IdempotencyKey: "k"})
	if last := f.calls[len(f.calls)-1]; last != "POST /billing/..%2Fadmin/promotional-credit" {
		t.Fatalf("org id not escaped: %q", last)
	}
}

func TestApplyCreditGrant_RealRouteAndRolloverRule_20261016(t *testing.T) {
	f, srv := newFakeBilling(t)
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_test"})
	ctx := context.Background()
	max := decimal.NewFromInt(30)
	resp, err := c.ApplyCreditGrant(ctx, "org-3", ApplyCreditGrantRequest{
		Amount: decimal.NewFromInt(10), Destination: "subscription_credit", Lifecycle: "rollover_capped",
		RolloverMax: &max, IdempotencyKey: "user:u:initial_credit:pro", Reason: "signup_grant_pro",
	})
	if err != nil {
		t.Fatalf("ApplyCreditGrant: %v (calls %v)", err, f.calls)
	}
	if resp.Operation != "credit_grant" || f.calls[0] != "POST /billing/org-3/apply-credit-grant" {
		t.Fatalf("resp=%+v calls=%v", resp, f.calls)
	}
	// rollover_max outside rollover_capped is refused locally (billing would 422).
	if _, err := c.ApplyCreditGrant(ctx, "org-3", ApplyCreditGrantRequest{
		Amount: decimal.NewFromInt(10), Destination: "credit_balance", Lifecycle: "persistent",
		RolloverMax: &max, IdempotencyKey: "k2"}); err == nil {
		t.Fatal("rollover_max with lifecycle persistent: want an error")
	}
}
