package authevents

// v0.3.2 — behaviour pins that use ONLY the pre-v0.3.2 API, so they compile against v0.3.1 and go RED
// there by assertion (not by a build failure): the default sign-up handler is registered on
// auth.user.registered (the event auth delivers), and auto-subscribe asks auth only for delivered types.
// RED on v0.3.1: no handler on auth.user.registered; auto-subscribe posts org.created + user.org_assigned.
// Run: go test ./authevents -run NeverAstrayBehaviour -count=1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNeverAstrayBehaviour_DefaultHandlerIsOnTheSignupEvent_20261128(t *testing.T) {
	ClearHandlers()
	defer ClearHandlers()
	if _, err := RegisterDefaultCreditGrantHandler(CreditGrantDeps{
		Config: cfgWithTrigger("signup"), TierProvider: stubTierProvider{tier: "pro"}, Granter: &stubGranter{},
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(DefaultRegistry().Handlers("auth.user.registered")); n != 1 {
		t.Fatalf("handlers on auth.user.registered = %d, want 1 — auth delivers the sign-up on that event "+
			"only, so the gift would never arrive", n)
	}
	// CONTROL: the pre-v0.3.2 names stay handled (replays / backfills of old ledger rows).
	if len(DefaultRegistry().Handlers("org.created")) != 1 || len(DefaultRegistry().Handlers("user.org_assigned")) != 1 {
		t.Fatal("the old names must stay registered on the receiver")
	}
}

func TestNeverAstrayBehaviour_AutoSubscribeAsksOnlyForDeliveredTypes_20261128(t *testing.T) {
	ClearHandlers()
	defer ClearHandlers()
	if _, err := RegisterDefaultCreditGrantHandler(CreditGrantDeps{
		Config: cfgWithTrigger("signup"), TierProvider: stubTierProvider{tier: "pro"}, Granter: &stubGranter{},
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		var body struct {
			EventTypes []string `json:"event_types"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		posted = body.EventTypes
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"subscription_id":"sub_1"}`))
	}))
	defer srv.Close()
	id, err := SubscribeOnStartup(context.Background(), SubscribeInput{
		AuthURL: srv.URL, AdminToken: "t", PublicURL: "https://svc.example.com", Secret: "s",
	})
	if err != nil || id != "sub_1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(posted, ",") != "auth.user.registered" {
		t.Fatalf("subscribed to %v, want only auth.user.registered (auth refuses user.org_assigned and never "+
			"delivers org.created to a webhook)", posted)
	}
}
