package quota

// v0.3.2 (lead, pre-cut): with AutoSubscribeAuthEvents, an EXISTING subscription that does not listen to the gift's
// event (an old-name one from the v0.3.1 docs) was reported as "subscribed(<id>)" with CreditGrant=true, while no
// gift could arrive. The capability must say OFF and name the fix; a subscription that does listen stays ON.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
)

func autoSubscribeAgainst(t *testing.T, existingTypes []string) Capabilities {
	t.Helper()
	authevents.ClearHandlers()
	t.Cleanup(authevents.ClearHandlers)
	authevents.OnAuthEvent(authevents.EventUserRegistered, authevents.HandlerFunc(
		func(context.Context, authevents.Event) error { return nil }))
	public := "https://svc.example.com"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("auto-subscribe must not create a second subscription (%s)", r.Method)
			w.WriteHeader(http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
			"subscription_id": "sub_old", "endpoint": public + "/api/quotas" + authevents.WebhookPath,
			"event_types": existingTypes,
		}}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AB0T_AUTH_AUTH_URL", srv.URL)
	t.Setenv("AB0T_AUTH_ADMIN_TOKEN", "t")
	t.Setenv("AB0T_AUTH_WEBHOOK_PUBLIC_URL", public)
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", "s")
	q := &Quota{capability: Capabilities{
		CreditGrant: true, CreditGrantEvents: []string{authevents.EventUserRegistered},
		CreditGrantSubscription: "pending (auto-subscribe running)", WhyOff: map[string]string{},
	}}
	q.autoSubscribe()
	q.capMu.RLock()
	defer q.capMu.RUnlock()
	return q.capability
}

func TestAutoSubscribe_ExistingOldNameSubscriptionTurnsTheGiftOff_20261128(t *testing.T) {
	c := autoSubscribeAgainst(t, []string{"org.created", "user.org_assigned"})
	if c.CreditGrant {
		t.Fatalf("CreditGrant=true with a subscription that never delivers a sign-up: %+v", c)
	}
	if !strings.Contains(c.CreditGrantSubscription, "does not listen to auth.user.registered") ||
		!strings.Contains(c.WhyOff["credit_grant"], "--rotate-secret") {
		t.Fatalf("the capability must name what is missing and the fix: sub=%q why=%q",
			c.CreditGrantSubscription, c.WhyOff["credit_grant"])
	}
}

func TestAutoSubscribe_CONTROL_ExistingSubscriptionWithTheGiftEventStaysOn_20261128(t *testing.T) {
	c := autoSubscribeAgainst(t, []string{authevents.EventUserRegistered, "auth.user.login"})
	if !c.CreditGrant || c.CreditGrantSubscription != "subscribed(sub_old)" {
		t.Fatalf("CreditGrant=%v sub=%q, want true and subscribed(sub_old)", c.CreditGrant, c.CreditGrantSubscription)
	}
}
