package authevents

// v0.3.2 — the sign-up gift listens on the events auth DELIVERS. Before, the default handler listened
// only on "org.created" / "user.org_assigned": auth sends org.created to its audit trail only and has
// no user.org_assigned type, so the documented gift never arrived. Shared vectors:
// testdata/money_safety_golden_20261128.json (byte-identical to the Python library's tests/ copy).
// RED before v0.3.2: DeliveredEventTypes / SignupGiftEventTypes do not exist, and the default handler is
// not registered on auth.user.registered.
// Run: go test ./authevents -run NeverAstray -count=1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type safetyGolden struct {
	Delivered       []string `json:"auth_delivered_event_types"`
	SignupGiftEvent struct {
		Default      []string `json:"default"`
		MemberJoined []string `json:"grant_on_member_joined"`
	} `json:"signup_gift_events"`
	Legacy      []string `json:"never_delivered_legacy_gift_events"`
	Undelivered string   `json:"undelivered_subscription_warning_template"`
}

func loadSafetyGolden(t *testing.T) safetyGolden {
	t.Helper()
	raw, err := os.ReadFile("../testdata/money_safety_golden_20261128.json")
	if err != nil {
		t.Fatal(err)
	}
	var g safetyGolden
	if err := json.Unmarshal(raw, &g); err != nil || len(g.Delivered) == 0 {
		t.Fatalf("golden: %v", err)
	}
	return g
}

func TestNeverAstray_DeliveredSetIsTheSharedOne_20261128(t *testing.T) {
	g := loadSafetyGolden(t)
	if strings.Join(DeliveredEventTypes, ",") != strings.Join(g.Delivered, ",") {
		t.Fatalf("delivered %v, golden %v", DeliveredEventTypes, g.Delivered)
	}
	if strings.Join(NeverDeliveredLegacyGiftEvents, ",") != strings.Join(g.Legacy, ",") {
		t.Fatalf("legacy %v, golden %v", NeverDeliveredLegacyGiftEvents, g.Legacy)
	}
	if got := SignupGiftEventTypes(false); strings.Join(got, ",") != strings.Join(g.SignupGiftEvent.Default, ",") {
		t.Fatalf("gift events %v", got)
	}
	if got := SignupGiftEventTypes(true); strings.Join(got, ",") != strings.Join(g.SignupGiftEvent.MemberJoined, ",") {
		t.Fatalf("gift events (member joined) %v", got)
	}
	want := strings.ReplaceAll(g.Undelivered, "{event_types}", "org.created, user.org_assigned")
	if got := UndeliveredSubscriptionWarning([]string{"user.org_assigned", "org.created"}); got != want {
		t.Fatalf("text\n go: %s\npy: %s", got, want)
	}
	for _, e := range SignupGiftEventTypes(true) {
		if !IsDelivered(e) {
			t.Fatalf("the gift listens on %q, which auth does not deliver", e)
		}
	}
}

// The default handler grants on a real auth.user.registered delivery (the shape auth sends).
func TestNeverAstray_ARealSignupEventGrants_20261128(t *testing.T) {
	gr := fireAuthEvent(t, cfgWithTrigger("signup"), EventUserRegistered)
	if gr.calls.Load() != 1 {
		t.Fatalf("grants = %d, want 1", gr.calls.Load())
	}
	for _, e := range DefaultCreditGrantEventTypes[1:] {
		if IsDelivered(e) {
			t.Fatalf("%s is kept for replays only; it must not be a delivered type", e)
		}
	}
}

// CONTROL: an explicit list is sent as given (with Subscribe reporting what auth never delivers).
func TestNeverAstray_ExplicitListIsSentAsGiven_20261128(t *testing.T) {
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
		posted = body.EventTypes
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"subscription_id":"sub_2"}`))
	}))
	defer srv.Close()
	res, err := Subscribe(context.Background(), SubscribeInput{
		AuthURL: srv.URL, AdminToken: "t", PublicURL: "https://svc.example.com", Secret: "s",
		EventTypes: []string{"org.created", EventUserRegistered},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(posted, ",") != "org.created,"+EventUserRegistered || strings.Join(res.Undelivered, ",") != "org.created" {
		t.Fatalf("posted %v undelivered %v", posted, res.Undelivered)
	}
}

func TestNeverAstray_LookupSubscriptionReadsTheEvents_20261128(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"subscription_id":"sub_old","endpoint":"https://svc.example.com/api/quotas/_webhooks/auth",` +
			`"event_types":["org.created","user.org_assigned"],"is_active":true}]}`))
	}))
	defer srv.Close()
	events, found, err := LookupSubscription(context.Background(), SubscribeInput{
		AuthURL: srv.URL, AdminToken: "t", PublicURL: "https://svc.example.com"})
	if err != nil || !found || strings.Join(events, ",") != "org.created,user.org_assigned" {
		t.Fatalf("events=%v found=%v err=%v", events, found, err)
	}
}
