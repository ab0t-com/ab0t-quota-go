package middleware

// v0.2.0: the guard fails CLOSED on an unresolved tier — 503 with a static body, the handler is not
// called — unless the config sets enforcement.allow_unknown_tier. GuardConfig.FailOpen keeps its
// meaning (engine ERRORS only) and does not admit an unresolved tier.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/engine"
)

type failingProvider struct{}

func (failingProvider) GetTier(context.Context, string, string) (string, error) {
	return "", errors.New("tier lookup failed")
}

func guardUnknownTier(t *testing.T, allowUnknown, failOpen bool) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	e := newTestEngine(t)
	e.Provider = failingProvider{}
	e.Cfg.Enforcement.AllowUnknownTier = allowUnknown
	called := false
	var decided []engine.Result
	g := Guard(GuardConfig{
		Engine:     e,
		Identity:   identityFromCtx,
		Router:     func(*http.Request) (string, float64) { return "sandbox.concurrent", 1 },
		FailOpen:   failOpen,
		OnDecision: func(_ *http.Request, r engine.Result) { decided = append(decided, r) },
	})
	srv := g(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), userKey, "alice"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if len(decided) != 1 || decided[0].Decision != engine.UnknownTier {
		t.Fatalf("OnDecision must see the UnknownTier decision: %+v", decided)
	}
	return rec, called
}

func TestGuard_UnknownTierFailsClosed503_20261123(t *testing.T) {
	rec, called := guardUnknownTier(t, false, false)
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("got %d (handler called: %v) — an unresolved tier must not reach the handler", rec.Code, called)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "quota_service_unavailable" || body["detail"] != "Quota enforcement is temporarily unavailable." ||
		body["reason"] != "tier_unresolved" || body["resource"] != "sandbox.concurrent" {
		t.Fatalf("body %v", body)
	}
	if rec.Header().Get("X-Quota-Reason") != "tier_unresolved" {
		t.Errorf("X-Quota-Reason = %q", rec.Header().Get("X-Quota-Reason"))
	}
}

// FailOpen is about engine errors; it is not the unresolved-tier opt-out.
func TestGuard_UnknownTierNotAdmittedByFailOpen_20261123(t *testing.T) {
	rec, called := guardUnknownTier(t, false, true)
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("FailOpen=true admitted an unresolved tier: %d (called %v)", rec.Code, called)
	}
}

func TestGuard_UnknownTierOptOutAdmits_20261123(t *testing.T) {
	rec, called := guardUnknownTier(t, true, false)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("enforcement.allow_unknown_tier=true must admit as before v0.2.0: %d (called %v)", rec.Code, called)
	}
}

// Control: the same guard with a resolvable tier still admits (the 503 above is the tier, not the setup).
func TestGuard_ResolvedTierStillAdmitted_20261123(t *testing.T) {
	e := newTestEngine(t)
	g := Guard(GuardConfig{Engine: e, Identity: identityFromCtx,
		Router: func(*http.Request) (string, float64) { return "sandbox.concurrent", 1 }})
	srv := g(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), userKey, "alice"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}
