package quota

// Ticket 20261016_signup_credit_receiver_404_and_client_gaps — the whole sign-up
// credit path, wired the documented way, with nothing hand-written:
//
//	quota.Setup(CreditGranter: quota.BillingCreditGranter(nil))
//	mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())
//	auth → signed org.created → default handler → billing promotional-credit
//
// On v0.1.9 this path failed twice over: the mount answered 404 and the typed
// billing client posted to a route billing does not serve.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/shopspring/decimal"
)

type billingCall struct {
	path string
	key  string
	body map[string]any
}

func fakeBillingCredit(t *testing.T) (*httptest.Server, func() []billingCall) {
	var mu sync.Mutex
	var calls []billingCall
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || !(strings.HasSuffix(r.URL.Path, "/promotional-credit") ||
			strings.HasSuffix(r.URL.Path, "/apply-credit-grant")) {
			http.Error(w, `{"detail":"Not Found"}`, http.StatusNotFound)
			return
		}
		if r.Header.Get("X-API-Key") != "ab0t_sk_e2e" {
			http.Error(w, `{"detail":"Not authenticated"}`, http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		key, _ := body["idempotency_key"].(string)
		calls = append(calls, billingCall{path: r.URL.Path, key: key, body: body})
		amt := "0"
		if !seen[r.URL.Path+key] {
			seen[r.URL.Path+key] = true
			amt, _ = body["amount"].(string)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"org_id": "o1", "old_balance": "0", "new_balance": amt, "amount": amt,
			"operation": "promotional_credit", "available_balance": amt, "payment_id": "promo:" + key,
		})
	}))
	return srv, func() []billingCall { mu.Lock(); defer mu.Unlock(); return append([]billingCall(nil), calls...) }
}

func signupCfg(dest config.CreditDestination, life config.CreditLifecycle) *config.Config {
	cfg := minimalConfig()
	cfg.TierProvider = config.TierProviderConfig{Type: "static", DefaultTier: "pro", Mapping: map[string]string{}}
	cfg.Tiers[0].CreditGrant = &config.CreditGrant{
		Trigger: config.CreditTriggerSignup, AmountPerPeriod: config.Decimal{Decimal: decimal.RequireFromString("5.00")},
		Currency: "USD", Lifecycle: life, Destination: dest, Dedup: config.DedupPerUserPerTier,
	}
	return cfg
}

func postSigned(t *testing.T, h http.Handler, path string, body []byte, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("X-Event-Signature", "sha256="+authevents.SignBody(body, secret))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSignupCredit_EndToEnd_DocumentedWiring_20261016(t *testing.T) {
	authevents.ClearHandlers()
	defer authevents.ClearHandlers()
	srv, calls := fakeBillingCredit(t)
	defer srv.Close()
	t.Setenv("AB0T_QUOTA_BILLING_URL", srv.URL)
	t.Setenv("AB0T_QUOTA_BILLING_TOKEN", "ab0t_sk_e2e")
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "e2e-secret")

	paid := false // credit grants only — no metering yet, so no outbox required
	q, err := Setup(context.Background(), Options{
		ConfigOverride:      signupCfg(config.CreditDestCreditBalance, config.CreditLifecyclePersistent),
		CreditGranter:       BillingCreditGranter(nil),
		EnablePaid:          &paid,
		SkipBackgroundLoops: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if !q.Capabilities().CreditGrant {
		t.Fatalf("credit_grant capability off: %v", q.Capabilities().WhyOff)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/quotas"+authevents.WebhookPath, q.WebhookHandler())

	ev := []byte(`{"event_type":"org.created","event_id":"evt-1","data":{"user_id":"u1","org_id":"o1"}}`)
	rec := postSigned(t, mux, "/api/quotas/_webhooks/auth", ev, "e2e-secret")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ran":1`) {
		t.Fatalf("receiver: HTTP %d %s", rec.Code, rec.Body.String())
	}
	got := calls()
	if len(got) != 1 || got[0].path != "/billing/o1/promotional-credit" ||
		got[0].key != "user:u1:initial_credit:pro" || got[0].body["amount"] != "5" {
		t.Fatalf("billing calls = %+v", got)
	}

	// A redelivery under a NEW event id (auth retry after a lost ack, or a
	// backfill) is deduped by the library, and would be by billing too: the key
	// does not depend on the event id.
	ev2 := []byte(`{"event_type":"user.org_assigned","event_id":"evt-2","data":{"user_id":"u1","org_id":"o1"}}`)
	if rec := postSigned(t, mux, "/api/quotas/_webhooks/auth", ev2, "e2e-secret"); rec.Code != http.StatusOK {
		t.Fatalf("second event: %d", rec.Code)
	}
	if n := len(calls()); n != 1 {
		t.Fatalf("billing called %d times for one user's sign-up grant, want 1", n)
	}
}

// A grant declared into subscription_credit / use_it_or_lose_it goes to billing's
// apply-credit-grant (the declared bucket), never silently into credit_balance.
func TestBillingCreditGranter_RoutesByDeclaredBucket_20261016(t *testing.T) {
	srv, calls := fakeBillingCredit(t)
	defer srv.Close()
	t.Setenv("AB0T_QUOTA_BILLING_URL", srv.URL)
	t.Setenv("AB0T_QUOTA_BILLING_TOKEN", "ab0t_sk_e2e")
	bc, err := newBillingFromEnvForTest()
	if err != nil {
		t.Fatal(err)
	}
	g := BillingCreditGranter(bc)
	err = g.GrantCredit(context.Background(), authevents.CreditGrantRequest{
		UserID: "u1", OrgID: "o1", TierID: "pro", Amount: decimal.NewFromInt(10),
		IdempotencyKey: "org:o1:initial_credit:pro", Destination: "subscription_credit", Lifecycle: "use_it_or_lose_it",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := calls()
	if len(c) != 1 || c[0].path != "/billing/o1/apply-credit-grant" || c[0].body["destination"] != "subscription_credit" ||
		c[0].body["lifecycle"] != "use_it_or_lose_it" || c[0].key != "org:o1:initial_credit:pro" {
		t.Fatalf("calls = %+v", c)
	}
}

// BillingCreditGranter(nil) without a billing URL refuses at Setup — a granter
// that can never grant must not report credit_grant=on.
func TestBillingCreditGranter_NilWithoutBillingRefuses_20261016(t *testing.T) {
	authevents.ClearHandlers()
	defer authevents.ClearHandlers()
	t.Setenv("AB0T_QUOTA_BILLING_URL", "")
	_, err := Setup(context.Background(), Options{
		ConfigOverride: signupCfg(config.CreditDestCreditBalance, config.CreditLifecyclePersistent),
		CreditGranter:  BillingCreditGranter(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "AB0T_QUOTA_BILLING_URL") {
		t.Fatalf("err = %v, want an actionable refusal naming AB0T_QUOTA_BILLING_URL", err)
	}
}

func newBillingFromEnvForTest() (*billing.Client, error) { return billing.New(mesh.Resolve()) }
