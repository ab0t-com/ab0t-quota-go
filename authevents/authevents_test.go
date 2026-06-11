package authevents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
	"github.com/shopspring/decimal"
)

// ---------- Event parsing ------------------------------------------------

func TestEventParsing_V1Envelope(t *testing.T) {
	body := []byte(`{
        "event_type":"org.created",
        "event_id":"evt-1",
        "occurred_at":"2026-06-11T10:00:00Z",
        "data":{"user_id":"u1","org_id":"o1","email":"u1@x.com"}
    }`)
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatal(err)
	}
	ev.SetRaw(body)
	if ev.GetEventID() != "evt-1" {
		t.Errorf("event_id = %q", ev.GetEventID())
	}
	if ev.GetEventType() != "org.created" {
		t.Errorf("event_type = %q", ev.GetEventType())
	}
	if ev.GetUserID() != "u1" {
		t.Errorf("user_id = %q", ev.GetUserID())
	}
	if ev.GetOrgID() != "o1" {
		t.Errorf("org_id = %q", ev.GetOrgID())
	}
}

func TestEventParsing_V2Aliases(t *testing.T) {
	body := []byte(`{
        "type":"user.org_assigned",
        "id":"evt-2",
        "timestamp":"2026-06-11T10:00:00Z",
        "data":{"user_id":"u2","org_id":"o2"}
    }`)
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.GetEventID() != "evt-2" {
		t.Errorf("got %q", ev.GetEventID())
	}
	if ev.GetEventType() != "user.org_assigned" {
		t.Errorf("got %q", ev.GetEventType())
	}
}

func TestEventID_FallsBackToContentHash(t *testing.T) {
	body := []byte(`{"event_type":"x"}`)
	var ev Event
	_ = json.Unmarshal(body, &ev)
	ev.SetRaw(body)
	id := ev.GetEventID()
	if id == "" {
		t.Fatal("empty id")
	}
	if len(id) != 32 {
		t.Errorf("expected 32-char hash, got %d", len(id))
	}
}

func TestEvent_OrgIDFallsBackToTenant(t *testing.T) {
	body := []byte(`{"event_type":"x","tenant_id":"o-top","data":{"user_id":"u"}}`)
	var ev Event
	_ = json.Unmarshal(body, &ev)
	if ev.GetOrgID() != "o-top" {
		t.Errorf("got %q", ev.GetOrgID())
	}
}

// ---------- HMAC ---------------------------------------------------------

func TestVerifyHMAC_PrefixedAndBare(t *testing.T) {
	body := []byte(`{"event_type":"x"}`)
	sig := SignBody(body, "secret")
	if !VerifyHMAC(body, sig, "secret") {
		t.Error("bare hex should verify")
	}
	if !VerifyHMAC(body, "sha256="+sig, "secret") {
		t.Error("sha256= prefix should verify")
	}
	if VerifyHMAC(body, sig, "wrong") {
		t.Error("wrong secret should not verify")
	}
	if VerifyHMAC(body, "", "secret") {
		t.Error("empty signature should not verify")
	}
}

// ---------- Registry -----------------------------------------------------

func TestRegistry_OnAuthEvent_Idempotent(t *testing.T) {
	r := NewRegistry()
	called := 0
	h := HandlerFunc(func(ctx context.Context, e Event) error {
		called++
		return nil
	})
	r.OnAuthEvent("x", h)
	r.OnAuthEvent("x", h) // dup, no-op
	if got := len(r.Handlers("x")); got != 1 {
		t.Errorf("expected 1 handler, got %d", got)
	}
}

func TestRegistry_EventTypes_Sorted(t *testing.T) {
	r := NewRegistry()
	r.OnAuthEvent("z", HandlerFunc(func(context.Context, Event) error { return nil }))
	r.OnAuthEvent("a", HandlerFunc(func(context.Context, Event) error { return nil }))
	got := r.EventTypes()
	if len(got) != 2 || got[0] != "a" || got[1] != "z" {
		t.Errorf("got %v", got)
	}
}

// ---------- ComposeCreditDedupKey ---------------------------------------

func TestComposeCreditDedupKey_Policies(t *testing.T) {
	tests := []struct {
		policy, want string
	}{
		{"per_user_per_tier", "credit_granted:user:u1:tier_pro"},
		{"per_org_per_tier", "credit_granted:org:o1:tier_pro"},
		{"per_user_global", "credit_granted:user:u1"},
		{"per_org_global", "credit_granted:org:o1"},
		{"", "credit_granted:user:u1:tier_pro"}, // default
	}
	for _, tc := range tests {
		got := ComposeCreditDedupKey(tc.policy, "u1", "o1", "tier_pro")
		if got != tc.want {
			t.Errorf("policy=%q want %q got %q", tc.policy, tc.want, got)
		}
	}
}

// ---------- MemoryPinStore operator-wins ---------------------------------

func TestMemoryPinStore_OperatorWinsOverAuto(t *testing.T) {
	s := NewMemoryPinStore()
	_ = s.Set("u1", "auto-org", "auto")
	_ = s.Set("u1", "op-org", "operator")
	_ = s.Set("u1", "auto-org-2", "auto") // should be ignored
	v, _ := s.Get("u1")
	if v != "op-org" {
		t.Errorf("operator should win: got %q", v)
	}
	// Operator can overwrite operator.
	_ = s.Set("u1", "op-org-2", "operator")
	v, _ = s.Get("u1")
	if v != "op-org-2" {
		t.Errorf("operator can overwrite operator: got %q", v)
	}
}

// ---------- Receiver wire contract --------------------------------------

func newReceiver(t *testing.T, secret string) (*Registry, http.Handler, handlerledger.LedgerStore) {
	t.Helper()
	r := NewRegistry()
	store := handlerledger.NewInMemoryLedgerStore()
	h := MakeRouter(ReceiverConfig{Secret: secret, Registry: r, LedgerStore: store})
	return r, h, store
}

func TestReceiver_RejectsInvalidSignature(t *testing.T) {
	_, srv, _ := newReceiver(t, "shh")
	req := httptest.NewRequest("POST", WebhookPath,
		strings.NewReader(`{"event_type":"x"}`))
	req.Header.Set("X-Event-Signature", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid signature") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestReceiver_RejectsInvalidJSON(t *testing.T) {
	_, srv, _ := newReceiver(t, "shh")
	body := []byte(`not json`)
	req := httptest.NewRequest("POST", WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-Event-Signature", SignBody(body, "shh"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid json") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestReceiver_IgnoredWhenNoHandlers(t *testing.T) {
	_, srv, _ := newReceiver(t, "shh")
	body := []byte(`{"event_type":"unknown","event_id":"e1"}`)
	req := httptest.NewRequest("POST", WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-Event-Signature", "sha256="+SignBody(body, "shh"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ignored"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestReceiver_DispatchesPlainHandler(t *testing.T) {
	r, srv, _ := newReceiver(t, "shh")
	calls := atomic.Int32{}
	r.OnAuthEvent("org.created", HandlerFunc(func(ctx context.Context, e Event) error {
		calls.Add(1)
		return nil
	}))
	body := []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1","org_id":"o1"}}`)
	req := httptest.NewRequest("POST", WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-Event-Signature", SignBody(body, "shh"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", calls.Load())
	}
}

func TestReceiver_LegacyXWebhookSignatureHeader(t *testing.T) {
	r, srv, _ := newReceiver(t, "shh")
	calls := atomic.Int32{}
	r.OnAuthEvent("org.created", HandlerFunc(func(ctx context.Context, e Event) error {
		calls.Add(1)
		return nil
	}))
	body := []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1"}}`)
	req := httptest.NewRequest("POST", WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-Webhook-Signature", SignBody(body, "shh"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("legacy header should be honored")
	}
}

// ---------- Idempotent integration --------------------------------------

func TestReceiver_DispatchesIdempotentHandler_RunsOnce(t *testing.T) {
	r, srv, _ := newReceiver(t, "shh")
	calls := atomic.Int32{}
	wrapped := Idempotent(handlerledger.IdempotentConfig{
		Handler: "test_handler",
		Retry:   handlerledger.NoRetry,
	}, func(ctx context.Context, e handlerledger.Event, hctx *handlerledger.Context) error {
		calls.Add(1)
		return nil
	})
	r.OnAuthEvent("org.created", wrapped)

	body := []byte(`{"event_type":"org.created","event_id":"e-dup","data":{"user_id":"u1"}}`)
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", WebhookPath, strings.NewReader(string(body)))
		req.Header.Set("X-Event-Signature", SignBody(body, "shh"))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("first send: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("second send: %d %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("idempotent handler should run once, got %d", calls.Load())
	}
}

func TestReceiver_IdempotentHandler_RetriesOnTransientFailure(t *testing.T) {
	r, srv, _ := newReceiver(t, "shh")
	attempts := atomic.Int32{}
	wrapped := Idempotent(handlerledger.IdempotentConfig{
		Handler: "flaky",
		Retry: &handlerledger.RetryConfig{
			Attempts: 3,
			Backoff:  handlerledger.BackoffConstant,
			Initial:  1 * time.Millisecond,
			Max:      1 * time.Millisecond,
		},
	}, func(ctx context.Context, e handlerledger.Event, hctx *handlerledger.Context) error {
		n := attempts.Add(1)
		if n < 2 {
			return errors.New("transient")
		}
		return nil
	})
	r.OnAuthEvent("org.created", wrapped)

	body := []byte(`{"event_type":"org.created","event_id":"e-retry","data":{"user_id":"u1"}}`)
	req := httptest.NewRequest("POST", WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-Event-Signature", SignBody(body, "shh"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
}

// ---------- Default credit-grant handler --------------------------------

type stubTierProvider struct {
	tier string
	err  error
}

func (s stubTierProvider) GetTier(ctx context.Context, userID, orgID string) (string, error) {
	return s.tier, s.err
}

type stubGranter struct {
	calls atomic.Int32
	last  CreditGrantRequest
	err   error
}

func (g *stubGranter) GrantCredit(ctx context.Context, in CreditGrantRequest) error {
	if g.err != nil {
		return g.err
	}
	g.calls.Add(1)
	g.last = in
	return nil
}

func newCreditCfg(t *testing.T) *config.Config {
	t.Helper()
	one := decimal.NewFromInt(25)
	return &config.Config{
		Tiers: []config.Tier{
			{
				TierID:      "pro",
				DisplayName: "Pro",
				CreditGrant: &config.CreditGrant{
					Trigger:         config.CreditTriggerSignup,
					AmountPerPeriod: config.Decimal{Decimal: one},
					Currency:        "USD",
					Lifecycle:       config.CreditLifecycleUseItOrLoseIt,
					Destination:     config.CreditDestSubscriptionCredit,
					Dedup:           config.DedupPerUserPerTier,
				},
			},
		},
	}
}

func TestBuildDefaultCreditGrantHandler_Errors(t *testing.T) {
	if _, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{}); err == nil {
		t.Error("expected error without Config")
	}
	if _, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{Config: &config.Config{}}); err == nil {
		t.Error("expected error without TierProvider")
	}
	if _, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{
		Config:       &config.Config{},
		TierProvider: stubTierProvider{},
	}); err == nil {
		t.Error("expected error without Granter")
	}
}

func TestDefaultCreditGrant_GrantsAndDedups(t *testing.T) {
	ClearHandlers()
	defer ClearHandlers()

	cfg := newCreditCfg(t)
	gr := &stubGranter{}
	store := handlerledger.NewInMemoryLedgerStore()
	h, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{
		Config:       cfg,
		TierProvider: stubTierProvider{tier: "pro"},
		Granter:      gr,
		Ledger:       store,
	})
	if err != nil {
		t.Fatal(err)
	}

	registry := NewRegistry()
	registry.OnAuthEvent("org.created", h)
	srv := MakeRouter(ReceiverConfig{Secret: "shh", Registry: registry, LedgerStore: store})

	send := func(eventID string) int {
		body := []byte(`{"event_type":"org.created","event_id":"` + eventID +
			`","data":{"user_id":"u1","org_id":"o1"}}`)
		req := httptest.NewRequest("POST", WebhookPath, strings.NewReader(string(body)))
		req.Header.Set("X-Event-Signature", SignBody(body, "shh"))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := send("evt-1"); code != 200 {
		t.Fatalf("first event: %d", code)
	}
	// Different event_id, same business dedup key → still only one grant.
	if code := send("evt-2"); code != 200 {
		t.Fatalf("second event: %d", code)
	}
	if got := gr.calls.Load(); got != 1 {
		t.Errorf("expected 1 grant via business dedup, got %d", got)
	}
	if !gr.last.Amount.Equal(decimal.NewFromInt(25)) {
		t.Errorf("amount = %s", gr.last.Amount)
	}
}

func TestDefaultCreditGrant_SkipsWhenNoTier(t *testing.T) {
	cfg := newCreditCfg(t)
	gr := &stubGranter{}
	store := handlerledger.NewInMemoryLedgerStore()
	h, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{
		Config:       cfg,
		TierProvider: stubTierProvider{tier: ""},
		Granter:      gr,
		Ledger:       store,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	r.OnAuthEvent("org.created", h)
	srv := MakeRouter(ReceiverConfig{Secret: "shh", Registry: r, LedgerStore: store})
	body := []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1"}}`)
	req := httptest.NewRequest("POST", WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-Event-Signature", SignBody(body, "shh"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if gr.calls.Load() != 0 {
		t.Errorf("should not grant when no tier")
	}
}

func TestDefaultCreditGrant_PinStoreInversePins(t *testing.T) {
	cfg := newCreditCfg(t)
	gr := &stubGranter{}
	store := handlerledger.NewInMemoryLedgerStore()
	pin := NewMemoryPinStore()
	h, err := BuildDefaultCreditGrantHandler(CreditGrantDeps{
		Config:       cfg,
		TierProvider: stubTierProvider{tier: "pro"},
		PinStore:     pin,
		Granter:      gr,
		Ledger:       store,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	r.OnAuthEvent("org.created", h)
	srv := MakeRouter(ReceiverConfig{Secret: "shh", Registry: r, LedgerStore: store})

	body := []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1","org_id":"o-from-event"}}`)
	req := httptest.NewRequest("POST", WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-Event-Signature", SignBody(body, "shh"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := pin.Get("u1")
	if got != "o-from-event" {
		t.Errorf("inverse pin = %q", got)
	}
}
