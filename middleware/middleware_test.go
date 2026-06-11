package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/counters"
	"github.com/ab0t-com/ab0t-quota-go/engine"
	"github.com/ab0t-com/ab0t-quota-go/messages"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/registry"
)

func ptrFloat(f float64) *float64 { return &f }

func newTestEngine(t *testing.T) *engine.Engine {
	t.Helper()
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "static", Mapping: map[string]string{"alice": "pro"}},
		Tiers: []config.Tier{
			{TierID: "pro", UpgradeURL: "https://billing.example.com/upgrade",
				Limits: map[string]config.TierLimit{
					"sandbox.concurrent": {Limit: ptrFloat(2)},
				}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "sandbox.concurrent", CounterType: config.CounterGauge},
		},
	}
	prov, _ := providers.New(cfg.TierProvider)
	return &engine.Engine{
		Cfg:      cfg,
		Reg:      registry.New(cfg),
		Provider: prov,
		Factory:  counters.NewMemoryFactory("quota"),
		Messages: messages.New(messages.Templates{}),
		Clock:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
}

type ctxKey string

const userKey ctxKey = "user"
const orgKey ctxKey = "org"

func identityFromCtx(r *http.Request) (string, string, error) {
	u, _ := r.Context().Value(userKey).(string)
	o, _ := r.Context().Value(orgKey).(string)
	return u, o, nil
}

func TestGuard_AllowsUnderLimit(t *testing.T) {
	e := newTestEngine(t)
	g := Guard(GuardConfig{
		Engine:   e,
		Identity: identityFromCtx,
		Router: func(r *http.Request) (string, float64) {
			return "sandbox.concurrent", 1
		},
	})
	srv := g(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), userKey, "alice"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Quota-Resource"); got != "sandbox.concurrent" {
		t.Errorf("X-Quota-Resource = %q", got)
	}
	if got := rec.Header().Get("X-Quota-Tier"); got != "pro" {
		t.Errorf("X-Quota-Tier = %q", got)
	}
	if got := rec.Header().Get("X-Quota-Limit"); got != "2" {
		t.Errorf("X-Quota-Limit = %q", got)
	}
}

func TestGuard_DeniesOverLimit(t *testing.T) {
	e := newTestEngine(t)
	g := Guard(GuardConfig{
		Engine:   e,
		Identity: identityFromCtx,
		Router: func(r *http.Request) (string, float64) {
			return "sandbox.concurrent", 1
		},
	})
	srv := g(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Pre-fill to the limit.
	_, _ = e.Spend(context.Background(), engine.CheckInput{UserID: "alice", OrgID: "", ResourceKey: "sandbox.concurrent", Cost: 2})

	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), userKey, "alice"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["resource"] != "sandbox.concurrent" {
		t.Errorf("body = %+v", body)
	}
	if body["upgrade_url"] != "https://billing.example.com/upgrade" {
		t.Errorf("upgrade_url missing: %+v", body)
	}
}

func TestGuard_ExemptPathsSkip(t *testing.T) {
	e := newTestEngine(t)
	g := Guard(GuardConfig{
		Engine:   e,
		Identity: identityFromCtx,
		Router: func(r *http.Request) (string, float64) {
			return "sandbox.concurrent", 1
		},
		Exempt: []string{"/healthz"},
	})
	srv := g(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Quota-Resource"); got != "" {
		t.Error("exempt path should not write quota headers")
	}
}

func TestGuard_RouterEmptyResource_Skips(t *testing.T) {
	e := newTestEngine(t)
	g := Guard(GuardConfig{
		Engine:   e,
		Identity: identityFromCtx,
		Router:   func(*http.Request) (string, float64) { return "", 0 },
	})
	srv := g(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("got %d", rec.Code)
	}
}

func TestGuard_FailOpenOnEngineError(t *testing.T) {
	e := newTestEngine(t)
	g := Guard(GuardConfig{
		Engine:   e,
		Identity: identityFromCtx,
		Router:   func(*http.Request) (string, float64) { return "missing.resource", 1 }, // unknown → engine errors
		FailOpen: true,
	})
	srv := g(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), userKey, "alice"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected fail-open allow, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGuard_FailClosedReturns503(t *testing.T) {
	e := newTestEngine(t)
	g := Guard(GuardConfig{
		Engine:   e,
		Identity: identityFromCtx,
		Router:   func(*http.Request) (string, float64) { return "missing.resource", 1 },
		FailOpen: false,
	})
	srv := g(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), userKey, "alice"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestGuard_BearerTokenPropagatesToProviderContext(t *testing.T) {
	cfg := &config.Config{
		Enforcement:  config.EnforcementConfig{Enabled: true},
		TierProvider: config.TierProviderConfig{Type: "jwt", DefaultTier: "free"},
		Tiers: []config.Tier{
			{TierID: "free", Limits: map[string]config.TierLimit{"x": {Limit: ptrFloat(1)}}},
		},
		Resources: []config.ResourceDef{
			{ResourceKey: "x", CounterType: config.CounterGauge},
		},
	}
	prov, _ := providers.New(cfg.TierProvider)
	e := &engine.Engine{
		Cfg: cfg, Reg: registry.New(cfg), Provider: prov,
		Factory: counters.NewMemoryFactory("quota"), Messages: messages.New(messages.Templates{}),
	}
	g := Guard(GuardConfig{
		Engine:   e,
		Identity: func(*http.Request) (string, string, error) { return "alice", "org", nil },
		Router:   func(*http.Request) (string, float64) { return "x", 1 },
	})
	srv := g(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJub25lIn0.eyJ0aWVyIjoiZnJlZSJ9.x")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Quota-Tier"); !strings.HasPrefix(got, "free") {
		t.Errorf("tier header = %q", got)
	}
}
