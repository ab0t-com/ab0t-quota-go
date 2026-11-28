package authevents

// v0.3.2 (lead, pre-cut): auto-subscribe found an EXISTING subscription and reported it as done even when it does
// not listen to the gift's events (an old-name subscription from v0.3.1 docs: org.created / user.org_assigned).
// Setup then said CreditGrant=true and "subscribed(<id>)" while no gift could arrive. SubscribeOnStartupReport
// names what the existing subscription lacks; SubscribeOnStartup keeps its (id, err) contract unchanged.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func existingSubServer(t *testing.T, eventTypes []string) (*httptest.Server, *int) {
	t.Helper()
	in := SubscribeInput{PublicURL: "https://svc.example.com"}
	in.resolve()
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
				"subscription_id": "sub_old", "endpoint": in.Endpoint(), "event_types": eventTypes,
			}}})
			return
		}
		posts++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"subscription_id":"sub_new"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &posts
}

func TestSubscribeOnStartupReport_ExistingOldNameSubscriptionLacksTheGiftEvent_20261128(t *testing.T) {
	srv, posts := existingSubServer(t, []string{"org.created", "user.org_assigned"})
	id, missing, err := SubscribeOnStartupReport(context.Background(), SubscribeInput{
		AuthURL: srv.URL, AdminToken: "t", PublicURL: "https://svc.example.com", Secret: "s",
		EventTypes: []string{EventUserRegistered},
	})
	if err != nil || id != "sub_old" {
		t.Fatalf("id=%q err=%v (the existing subscription is still reported by id)", id, err)
	}
	if strings.Join(missing, ",") != EventUserRegistered {
		t.Fatalf("missing=%v, want [%s]: the old subscription never delivers a sign-up", missing, EventUserRegistered)
	}
	if *posts != 0 {
		t.Fatalf("must not create a second subscription for the same endpoint (posts=%d)", *posts)
	}
}

func TestSubscribeOnStartupReport_CONTROL_ExistingSubscriptionWithTheGiftEvent_20261128(t *testing.T) {
	srv, _ := existingSubServer(t, []string{EventUserRegistered, "auth.user.login"})
	id, missing, err := SubscribeOnStartupReport(context.Background(), SubscribeInput{
		AuthURL: srv.URL, AdminToken: "t", PublicURL: "https://svc.example.com", Secret: "s",
		EventTypes: []string{EventUserRegistered},
	})
	if err != nil || id != "sub_old" || len(missing) != 0 {
		t.Fatalf("id=%q missing=%v err=%v, want sub_old and nothing missing", id, missing, err)
	}
}

func TestExistingSubscriptionMissingEventsText_IsTheSharedOne_20261128(t *testing.T) {
	raw, err := os.ReadFile("../testdata/money_safety_golden_20261128.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Tmpl string `json:"existing_subscription_missing_events_template"`
	}
	if err := json.Unmarshal(raw, &g); err != nil || g.Tmpl == "" {
		t.Fatalf("golden template missing: %v", err)
	}
	want := strings.NewReplacer("{id}", "sub_old", "{missing}", "auth.user.registered", "{fix}", "FIX").Replace(g.Tmpl)
	if got := ExistingSubscriptionMissingEventsText("sub_old", []string{"auth.user.registered"}, "FIX"); got != want {
		t.Fatalf("text drifted from the shared golden:\n got %q\nwant %q", got, want)
	}
}
