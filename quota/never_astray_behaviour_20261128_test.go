package quota

// v0.3.2 — behaviour pins that use ONLY the pre-v0.3.2 API, so they compile against v0.3.1 and go RED
// there by assertion (not by a build failure):
//   - a real auth.user.registered delivery grants the default sign-up gift;
//   - tier_provider "mesh" reads billing's tier for the org (and serves default_tier when refused);
//   - "mesh" with no billing says so at startup; "jwt" warns that ab0t-issued tokens carry no tier;
//   - a price-list 403 is retried every 6 h, 28 times at most, saying what it means.
// RED on v0.3.1: no grant (no handler on the event); mesh returns default_tier; no warnings; a 403 is
// never retried.
// Run: go test ./quota -run NeverAstrayBehaviour -count=1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/shopspring/decimal"
)

func clearMoneyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"BILLING_SERVICE_URL", "BILLING_SERVICE_API_KEY", "AB0T_QUOTA_BILLING_URL",
		"AB0T_QUOTA_BILLING_TOKEN", "AB0T_QUOTA_SERVICE_TOKEN", "PAYMENT_SERVICE_URL", "AB0T_QUOTA_PAYMENT_URL",
		"AB0T_AUTH_WEBHOOK_SECRET", EnvWebhookSecretPrevious} {
		t.Setenv(k, "")
	}
}

func giftCfg() *config.Config {
	cfg := minimalConfig()
	cfg.Tiers[0].CreditGrant = &config.CreditGrant{
		Trigger: config.CreditTriggerSignup, AmountPerPeriod: config.Decimal{Decimal: decimal.NewFromInt(5)},
		Currency: "USD", Lifecycle: config.CreditLifecyclePersistent, Destination: config.CreditDestCreditBalance,
		Dedup: config.DedupPerUserPerTier,
	}
	cfg.TierProvider = config.TierProviderConfig{Type: "static", DefaultTier: "pro"}
	return cfg
}

func fakeBillingTier(t *testing.T, status int, tier string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/tier") {
			http.Error(w, `{"detail":"Not Found"}`, http.StatusNotFound)
			return
		}
		calls.Add(1)
		if r.Header.Get("X-API-Key") != "ab0t_sk_tier" {
			http.Error(w, `{"detail":"Not authenticated"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"org_id": strings.Split(r.URL.Path, "/")[2], "tier_id": tier})
	}))
	return srv, &calls
}

func meshCfg() *config.Config {
	cfg := minimalConfig()
	cfg.TierProvider = config.TierProviderConfig{Type: "mesh", DefaultTier: "free"}
	cfg.Tiers = append(cfg.Tiers, config.Tier{TierID: "free", Limits: map[string]config.TierLimit{
		"sandbox.concurrent": {Limit: ptrFloat(1)}}})
	return cfg
}

func TestNeverAstrayBehaviour_ARealSignupEventGrantsTheGift_20261128(t *testing.T) {
	authevents.ClearHandlers()
	defer authevents.ClearHandlers()
	clearMoneyEnv(t)
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "s")
	var grants atomic.Int32
	q, err := Setup(context.Background(), Options{ConfigOverride: giftCfg(), SkipBackgroundLoops: true,
		CreditGranter: granterFunc(func(context.Context, authevents.CreditGrantRequest) error { grants.Add(1); return nil })})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	// The shape auth delivers (appv2/events_v2/webhook_outbox.py user_registered; goauth UserRegistered).
	ev := []byte(`{"event_type":"auth.user.registered","event_id":"evt_1","data":{"user_id":"u1","email":null,` +
		`"org_id":"o1","role":"member","registration_method":"password"}}`)
	rec := postSigned(t, q.WebhookHandler(), authevents.WebhookPath, ev, "s")
	if rec.Code != http.StatusOK || grants.Load() != 1 {
		t.Fatalf("HTTP %d %s, grants %d — a sign-up auth delivers must give the gift", rec.Code, rec.Body.String(), grants.Load())
	}
}

func TestNeverAstrayBehaviour_MeshReadsBillingsTier_20261128(t *testing.T) {
	clearMoneyEnv(t)
	srv, calls := fakeBillingTier(t, http.StatusOK, "pro")
	defer srv.Close()
	t.Setenv("BILLING_SERVICE_URL", srv.URL)
	t.Setenv("BILLING_SERVICE_API_KEY", "ab0t_sk_tier")
	paid := false
	q, err := Setup(context.Background(), Options{ConfigOverride: meshCfg(), EnablePaid: &paid, SkipBackgroundLoops: true})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	tier, err := q.Provider.GetTier(context.Background(), "u1", "org_paying")
	if err != nil || tier != "pro" || calls.Load() != 1 {
		t.Fatalf("tier=%q err=%v billing calls=%d — a paying org must get the tier billing holds", tier, err, calls.Load())
	}
	// Cached (the Python default 60 s): no second lookup.
	if tier, _ := q.Provider.GetTier(context.Background(), "u1", "org_paying"); tier != "pro" || calls.Load() != 1 {
		t.Fatalf("second read tier=%q calls=%d", tier, calls.Load())
	}
}

// CONTROL: a refused lookup serves default_tier — never a 503, never a cached guess.
func TestNeverAstrayBehaviour_MeshLookupRefusedServesTheDefault_20261128(t *testing.T) {
	clearMoneyEnv(t)
	srv, calls := fakeBillingTier(t, http.StatusOK, "pro")
	defer srv.Close()
	t.Setenv("BILLING_SERVICE_URL", srv.URL)
	t.Setenv("BILLING_SERVICE_API_KEY", "ab0t_sk_wrong") // billing answers 401
	paid := false
	q, err := Setup(context.Background(), Options{ConfigOverride: meshCfg(), EnablePaid: &paid, SkipBackgroundLoops: true})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if tier, err := q.Provider.GetTier(context.Background(), "u1", "org_paying"); err != nil || tier != "free" || calls.Load() != 1 {
		t.Fatalf("tier=%q err=%v calls=%d", tier, err, calls.Load())
	}
}

func TestNeverAstrayBehaviour_MeshWithoutBillingSaysSo_20261128(t *testing.T) {
	clearMoneyEnv(t)
	q, err := Setup(context.Background(), Options{ConfigOverride: meshCfg(), SkipBackgroundLoops: true})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	if why := q.Capabilities().WhyOff["tier_provider"]; !strings.Contains(why, "cannot reach billing") ||
		!strings.Contains(why, "https://billing.service.ab0t.com") {
		t.Fatalf("WhyOff[tier_provider] = %q", why)
	}
}

func TestNeverAstrayBehaviour_JWTSaysAuthTokensCarryNoTier_20261128(t *testing.T) {
	clearMoneyEnv(t)
	cfg := minimalConfig()
	cfg.TierProvider = config.TierProviderConfig{Type: "jwt", DefaultTier: "pro"}
	q, err := Setup(context.Background(), Options{ConfigOverride: cfg, SkipBackgroundLoops: true})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close(context.Background())
	for _, w := range q.Capabilities().ConfigWarnings {
		if strings.Contains(w, "tokens issued by the ab0t auth service") && strings.Contains(w, `"org_tier"`) {
			return
		}
	}
	t.Fatalf("no jwt startup warning in %v", q.Capabilities().ConfigWarnings)
}

func TestNeverAstrayBehaviour_A403IsRetriedRarelyAndLandsOnceBound_20261128(t *testing.T) {
	p, rec, sl := &fakePutter{answers: []int{403, 403, 200}}, &recorded{}, &sleeps{}
	if out := runPriceListPublisher(context.Background(), p, "s", []byte(`{}`), "v", rec.report, sl.sleep); out != "published" {
		t.Fatalf("outcome %q — a 403 clears on billing's side; the publisher must try again", out)
	}
	if p.calls != 3 || len(sl.calls) != 2 || sl.calls[0] != 6*time.Hour || sl.calls[1] != 6*time.Hour {
		t.Fatalf("calls %d sleeps %v", p.calls, sl.calls)
	}
	if !strings.HasPrefix(rec.states[0], "refused(403: billing has not bound this service name") ||
		rec.states[len(rec.states)-1] != "published(v, after 2 retries)" {
		t.Fatalf("states %v", rec.states)
	}
}

func TestNeverAstrayBehaviour_A403GivesUpAfterTheCap_20261128(t *testing.T) {
	answers := make([]int, 0, 30)
	for i := 0; i < 29; i++ {
		answers = append(answers, 403)
	}
	p, rec, sl := &fakePutter{answers: append(answers, 200)}, &recorded{}, &sleeps{}
	if out := runPriceListPublisher(context.Background(), p, "s", []byte(`{}`), "v", rec.report, sl.sleep); out != "refused" {
		t.Fatalf("outcome %q", out)
	}
	last := rec.states[len(rec.states)-1]
	if p.calls != 29 || len(sl.calls) != 28 || !strings.Contains(last, "gave up after 28 retries") {
		t.Fatalf("calls %d sleeps %d last %q", p.calls, len(sl.calls), last)
	}
}
