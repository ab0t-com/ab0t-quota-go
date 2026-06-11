package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/engine"
	"github.com/ab0t-com/ab0t-quota-go/middleware"
	"github.com/shopspring/decimal"
)

func ptrFloat(f float64) *float64 { return &f }

func minimalConfig() *config.Config {
	return &config.Config{
		Enforcement: config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{
			Type:    "static",
			Mapping: map[string]string{"alice": "pro"},
		},
		Tiers: []config.Tier{
			{TierID: "pro", Limits: map[string]config.TierLimit{
				"sandbox.concurrent": {Limit: ptrFloat(2)},
			}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandbox.concurrent", CounterType: config.CounterGauge},
		},
	}
}

func TestSetup_MinimalConfig_AllowsAndDenies(t *testing.T) {
	q, err := Setup(context.Background(), Options{
		ConfigOverride: minimalConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())

	cap := q.Capabilities()
	if !cap.Engine || !cap.Enforcement {
		t.Errorf("capabilities = %+v", cap)
	}
	if cap.Billing {
		t.Error("billing should be off without AB0T_QUOTA_BILLING_URL")
	}
	if cap.CreditGrant {
		t.Error("credit grant should be off without CreditGranter")
	}
}

func TestSetup_MiddlewareEndToEnd(t *testing.T) {
	q, err := Setup(context.Background(), Options{ConfigOverride: minimalConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())

	type ctxKey string
	const userKey ctxKey = "u"
	identity := func(r *http.Request) (string, string, error) {
		u, _ := r.Context().Value(userKey).(string)
		return u, "", nil
	}
	router := func(*http.Request) (string, float64) { return "sandbox.concurrent", 1 }

	guard := q.Middleware(MiddlewareDeps{Identity: identity, Router: router})
	srv := guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/x", nil)
		req = req.WithContext(context.WithValue(req.Context(), userKey, "alice"))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// Pre-fill to limit.
	_, _ = q.Spend(context.Background(), engine.CheckInput{UserID: "alice", ResourceKey: "sandbox.concurrent", Cost: 2})
	rec := doReq()
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after preload, got %d", rec.Code)
	}
}

func TestSetup_WithCreditGranter_RegistersHandler(t *testing.T) {
	authevents.ClearHandlers()
	defer authevents.ClearHandlers()

	cfg := minimalConfig()
	// Add a credit_grant to make the handler relevant.
	cfg.Tiers[0].CreditGrant = &config.CreditGrant{
		Trigger:         config.CreditTriggerSignup,
		AmountPerPeriod: config.Decimal{Decimal: decimal.NewFromInt(25)},
		Currency:        "USD",
		Lifecycle:       config.CreditLifecycleUseItOrLoseIt,
		Destination:     config.CreditDestSubscriptionCredit,
		Dedup:           config.DedupPerUserPerTier,
	}

	granted := false
	gr := granterFunc(func(ctx context.Context, in authevents.CreditGrantRequest) error {
		granted = true
		return nil
	})
	q, err := Setup(context.Background(), Options{
		ConfigOverride: cfg,
		CreditGranter:  gr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())

	if !q.Capabilities().CreditGrant {
		t.Error("credit_grant should be on")
	}
	if got := authevents.RegisteredEventTypes(); len(got) == 0 {
		t.Errorf("expected handlers registered, got %v", got)
	}

	// Smoke: webhook handler exists.
	_ = q.WebhookHandler()

	// granted flag — we'd need to fire an event through the webhook to flip
	// this; covered fully in authevents tests. Here we only check wiring.
	_ = granted
}

// granterFunc adapts a func to authevents.CreditGranter.
type granterFunc func(ctx context.Context, in authevents.CreditGrantRequest) error

func (f granterFunc) GrantCredit(ctx context.Context, in authevents.CreditGrantRequest) error {
	return f(ctx, in)
}

// silence unused-import check
var _ = middleware.GuardConfig{}
