package quota

// hardening_signup_20261016_test.go — the v0.1.10 sign-up credit path attacked
// end to end (receiver → default handler → BillingCreditGranter → fake billing),
// plus the settlement classification through the REAL billing client.
//
//	AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS honoured only when set     TestHardening_PreviousSecretEnvOnlyWhenSet
//	only trigger "signup" (or absent) grants                       TestHardening_OnlySignupTriggerGrantsEndToEnd
//	idempotency key from the grant, stable across a RESTART        TestHardening_GrantKeyStableAcrossProcessRestart
//	dedup policy decides who is granted                            TestHardening_DedupPolicyEndToEnd
//	settle classification on billing's real status codes           TestHardening_SettleClassificationThroughRealClient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/ab0t-com/ab0t-quota-go/observation"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
	"github.com/shopspring/decimal"
)

// creditLedger is a billing double that dedups on idempotency_key, as billing does.
type creditLedger struct {
	mu       sync.Mutex
	calls    []billingCall
	credited map[string]decimal.Decimal
}

func newCreditLedger(t *testing.T) (*httptest.Server, *creditLedger) {
	l := &creditLedger{credited: map[string]decimal.Decimal{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		l.mu.Lock()
		defer l.mu.Unlock()
		key, _ := body["idempotency_key"].(string)
		l.calls = append(l.calls, billingCall{path: r.URL.Path, key: key, body: body})
		if _, dup := l.credited[key]; !dup {
			s, _ := body["amount"].(string)
			l.credited[key] = decimal.RequireFromString(s)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"org_id": "o1", "old_balance": "0", "new_balance": "0",
			"amount": "0", "operation": "promotional_credit", "available_balance": "0"})
	}))
	t.Cleanup(srv.Close)
	return srv, l
}

func (l *creditLedger) total() (decimal.Decimal, int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sum := decimal.Zero
	for _, v := range l.credited {
		sum = sum.Add(v)
	}
	return sum, len(l.credited), len(l.calls)
}

func setupSignup(t *testing.T, cfg *config.Config) *Quota {
	t.Helper()
	paid := false
	q, err := Setup(context.Background(), Options{
		ConfigOverride: cfg, CreditGranter: BillingCreditGranter(nil), EnablePaid: &paid, SkipBackgroundLoops: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func signupEvent(typ, id, user, org string) []byte {
	return []byte(fmt.Sprintf(`{"event_type":%q,"event_id":%q,"data":{"user_id":%q,"org_id":%q}}`, typ, id, user, org))
}

func billingEnv(t *testing.T, url string) {
	t.Setenv("AB0T_QUOTA_BILLING_URL", url)
	t.Setenv("AB0T_QUOTA_BILLING_TOKEN", "ab0t_sk_e2e")
}

func TestHardening_PreviousSecretEnvOnlyWhenSet(t *testing.T) {
	srv, _ := newCreditLedger(t)
	billingEnv(t, srv.URL)
	for _, tc := range []struct {
		prev   string
		accept map[string]bool
	}{
		{"", map[string]bool{"cur": true, "old1": false, "old2": false}},
		{" old1 , ,old2,", map[string]bool{"cur": true, "old1": true, "old2": true, "old3": false, " old1 ": false}},
		{",,  ,", map[string]bool{"cur": true, "": false}},
	} {
		t.Run(fmt.Sprintf("prev=%q", tc.prev), func(t *testing.T) {
			authevents.ClearHandlers()
			defer authevents.ClearHandlers()
			t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "cur")
			t.Setenv(EnvWebhookSecretPrevious, tc.prev)
			q := setupSignup(t, signupCfg(config.CreditDestCreditBalance, config.CreditLifecyclePersistent))
			defer q.Close(context.Background())
			i := 0
			for sec, want := range tc.accept {
				i++
				rec := postSigned(t, q.WebhookHandler(), authevents.WebhookPath, signupEvent("org.created", fmt.Sprint("e", i), "u", "o1"), sec)
				if got := rec.Code == http.StatusOK; got != want {
					t.Errorf("signed with %q: HTTP %d, accepted=%v want %v", sec, rec.Code, got, want)
				}
			}
		})
	}
}

func TestHardening_OnlySignupTriggerGrantsEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		trig  config.CreditTrigger
		grant bool
	}{
		{config.CreditTriggerSignup, true},
		{"", true}, // documented: absent trigger treated as sign-up (with a warning)
		{config.CreditTriggerSubscriptionInvoicePaid, false},
		{config.CreditTriggerScheduledPeriodStart, false},
		{config.CreditTriggerManual, false},
		{config.CreditTriggerWebhookAdmin, false},
		{"SIGNUP", false},  // not the enum value
		{"signup ", false}, // not the enum value
		{"sign_up", false},
	} {
		t.Run(fmt.Sprintf("trigger=%q", tc.trig), func(t *testing.T) {
			authevents.ClearHandlers()
			defer authevents.ClearHandlers()
			srv, led := newCreditLedger(t)
			billingEnv(t, srv.URL)
			t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "s")
			cfg := signupCfg(config.CreditDestCreditBalance, config.CreditLifecyclePersistent)
			cfg.Tiers[0].CreditGrant.Trigger = tc.trig
			q := setupSignup(t, cfg)
			defer q.Close(context.Background())
			for i, typ := range []string{"org.created", "user.org_assigned"} {
				rec := postSigned(t, q.WebhookHandler(), authevents.WebhookPath, signupEvent(typ, fmt.Sprint("ev", i), "u1", "o1"), "s")
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: HTTP %d", typ, rec.Code)
				}
			}
			_, keys, calls := led.total()
			if tc.grant && (keys != 1 || calls != 1) {
				t.Fatalf("signup trigger: billing keys=%d calls=%d, want exactly one grant", keys, calls)
			}
			if !tc.grant && calls != 0 {
				t.Fatalf("trigger %q: billing was called %d time(s) — a non-signup grant was handed out on an auth event", tc.trig, calls)
			}
		})
	}
}

func TestHardening_GrantKeyStableAcrossProcessRestart(t *testing.T) {
	srv, led := newCreditLedger(t)
	billingEnv(t, srv.URL)
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "s")
	// Three "processes", each with a fresh in-memory handler ledger (the library's
	// own dedup has forgotten everything), each receiving the same user's sign-up
	// under a different event id. Only billing's key dedup stands between them and
	// a triple credit.
	for i := 0; i < 3; i++ {
		authevents.ClearHandlers()
		q := setupSignup(t, signupCfg(config.CreditDestCreditBalance, config.CreditLifecyclePersistent))
		rec := postSigned(t, q.WebhookHandler(), "/api/quotas"+authevents.WebhookPath,
			signupEvent("org.created", fmt.Sprint("restart-", i), "u1", "o1"), "s")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ran":1`) {
			t.Fatalf("process %d: %d %s", i, rec.Code, rec.Body)
		}
		_ = q.Close(context.Background())
	}
	authevents.ClearHandlers()
	sum, keys, calls := led.total()
	if calls != 3 {
		t.Fatalf("billing calls = %d, want 3 (each fresh process asks)", calls)
	}
	if keys != 1 || !sum.Equal(decimal.NewFromInt(5)) {
		t.Fatalf("distinct keys=%d credited=%s — the key must not depend on the event id or the process", keys, sum)
	}
}

func TestHardening_DedupPolicyEndToEnd(t *testing.T) {
	users := []struct{ u, o string }{{"u1", "o1"}, {"u2", "o1"}, {"u3", "o2"}, {"u1", "o1"}}
	for _, tc := range []struct {
		policy config.DedupPolicy
		want   []string
	}{
		{config.DedupPerUserPerTier, []string{"user:u1:initial_credit:pro", "user:u2:initial_credit:pro", "user:u3:initial_credit:pro"}},
		{config.DedupPerOrgPerTier, []string{"org:o1:initial_credit:pro", "org:o2:initial_credit:pro"}},
		{config.DedupPerUserGlobal, []string{"user:u1:initial_credit", "user:u2:initial_credit", "user:u3:initial_credit"}},
		{config.DedupPerOrgGlobal, []string{"org:o1:initial_credit", "org:o2:initial_credit"}},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			authevents.ClearHandlers()
			defer authevents.ClearHandlers()
			srv, led := newCreditLedger(t)
			billingEnv(t, srv.URL)
			t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "s")
			cfg := signupCfg(config.CreditDestCreditBalance, config.CreditLifecyclePersistent)
			cfg.Tiers[0].CreditGrant.Dedup = tc.policy
			q := setupSignup(t, cfg)
			defer q.Close(context.Background())
			for i, p := range users {
				_ = postSigned(t, q.WebhookHandler(), authevents.WebhookPath, signupEvent("org.created", fmt.Sprint("d", i), p.u, p.o), "s")
			}
			led.mu.Lock()
			defer led.mu.Unlock()
			if len(led.credited) != len(tc.want) {
				t.Fatalf("credited keys = %v, want %v", led.credited, tc.want)
			}
			for _, k := range tc.want {
				if _, ok := led.credited[k]; !ok {
					t.Fatalf("missing grant %q; got %v", k, led.credited)
				}
			}
		})
	}
}

// ---- settlement classification through the real billing client ----

func TestHardening_SettleClassificationThroughRealClient(t *testing.T) {
	type row struct {
		status int
		body   string
		want   error // nil = success
	}
	var rows []row
	for s := 400; s <= 599; s++ {
		if http.StatusText(s) == "" && s != 499 {
			continue
		}
		var want error = outbox.ErrSettleTransient
		switch s {
		case 400, 422:
			want = outbox.ErrSettlePermanent
		case 409:
			want = outbox.ErrSettleRefused409
		}
		rows = append(rows, row{s, `{"detail":"x"}`, want})
	}
	rows = append(rows,
		row{404, `{"detail":"Billing account not found"}`, outbox.ErrSettlePermanent},
		row{404, `{"detail":"Not Found"}`, outbox.ErrSettleTransient},
		row{404, ``, outbox.ErrSettleTransient},
		row{404, `{"detail":"billing account not found"}`, outbox.ErrSettleTransient}, // case differs: retry (safe)
		row{401, `{"detail":"Billing account not found"}`, outbox.ErrSettleTransient}, // the phrase alone never voids
		row{403, `{"detail":"Billing account not found"}`, outbox.ErrSettleTransient},
	)
	for _, r := range rows {
		t.Run(fmt.Sprintf("%d %s", r.status, r.body), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(r.status)
				_, _ = w.Write([]byte(r.body))
			}))
			defer srv.Close()
			bc, err := billing.New(mesh.URLs{Billing: srv.URL, BillingToken: "ab0t_sk_x"})
			if err != nil {
				t.Fatal(err)
			}
			obs, err := observation.Observe(observation.Event{OrgID: "o1", ResourceID: "r",
				ReservationID: "res-1", StartedAt: "2026-10-16T00:00:00Z", StoppedAt: "2026-10-16T01:00:00Z", HourlyRate: "1"})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = billingSettler{c: bc}.SettleActivation(context.Background(), outbox.SettleRequest{
				OrgID: "o1", SettlementKey: "res-1", Observation: obs, ReservationID: "res-1"})
			if !errors.Is(err, r.want) {
				t.Fatalf("HTTP %d %q → %v, want %v", r.status, r.body, err, r.want)
			}
		})
	}
	// No response at all (connection refused) → transient.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	bc, _ := billing.New(mesh.URLs{Billing: url, BillingToken: "ab0t_sk_x"})
	obs, _ := observation.Observe(observation.Event{OrgID: "o1", ResourceID: "r",
		ReservationID: "res-1", StartedAt: "2026-10-16T00:00:00Z", StoppedAt: "2026-10-16T01:00:00Z"})
	if _, _, err := (billingSettler{c: bc}).SettleActivation(context.Background(), outbox.SettleRequest{OrgID: "o1", SettlementKey: "res-1", Observation: obs}); !errors.Is(err, outbox.ErrSettleTransient) {
		t.Fatalf("connection refused → %v, want transient", err)
	}
}
