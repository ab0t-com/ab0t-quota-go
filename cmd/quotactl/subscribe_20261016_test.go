package main

// Ticket 20261016_signup_credit_receiver_404_and_client_gaps, finding 3.
//
// v0.1.9 `quotactl subscribe-events`:
//   - exited 0 and printed an empty "subscription_id:" when registration FAILED
//     (SubscribeOnStartup is best-effort by design and returns nil on runtime
//     failure; the CLI reused it);
//   - re-running never changed anything: it found the subscription by endpoint and
//     returned it, so a new AB0T_AUTH_WEBHOOK_SECRET (or a new --events list) was
//     silently not applied, and every event then failed signature verification.
//
// The auth service's event-subscription contract: the secret is NOT
// patchable (PATCH accepts an allow-list of fields), a second
// subscription for the same endpoint is 409 (:55-60, api/events.py:160), the
// list is paginated {items,next_token,count} (api/events.py:169-232), DELETE is
// 204 (api/events.py:328). So rotation = delete + create, which is what
// --rotate-secret does. fakeAuth models exactly that.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeAuthSub struct {
	ID         string   `json:"subscription_id"`
	Endpoint   string   `json:"endpoint"`
	EventTypes []string `json:"event_types"`
	Secret     string   `json:"secret,omitempty"`
	IsActive   bool     `json:"is_active"`
}

type fakeAuth struct {
	mu       sync.Mutex
	token    string
	subs     []*fakeAuthSub
	pageSize int
	failPOST int // status to return on create (0 = succeed)
	next     int
	log      []string
	// hideSecret models an auth that (correctly) stops returning the secret.
	hideSecret bool
}

func (f *fakeAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Could not validate credentials"}`))
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/events/subscriptions":
		start := 0
		fmt.Sscanf(r.URL.Query().Get("next_token"), "p%d", &start)
		size := f.pageSize
		if size == 0 {
			size = 100
		}
		end := start + size
		if end > len(f.subs) {
			end = len(f.subs)
		}
		items := []fakeAuthSub{}
		for _, s := range f.subs[start:end] {
			c := *s
			if f.hideSecret {
				c.Secret = ""
			}
			items = append(items, c)
		}
		resp := map[string]any{"items": items, "count": len(items)}
		if end < len(f.subs) {
			resp["next_token"] = fmt.Sprintf("p%d", end)
		}
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodPost && r.URL.Path == "/events/subscriptions":
		if f.failPOST != 0 {
			w.WriteHeader(f.failPOST)
			_, _ = w.Write([]byte(`{"detail":"nope"}`))
			return
		}
		var in struct {
			Endpoint   string   `json:"endpoint"`
			EventTypes []string `json:"event_types"`
			Secret     string   `json:"secret"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, s := range f.subs {
			if strings.HasPrefix(s.Endpoint, in.Endpoint) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"detail":"Subscription already exists"}`))
				return
			}
		}
		f.next++
		s := &fakeAuthSub{ID: fmt.Sprintf("sub_%d", f.next), Endpoint: in.Endpoint, EventTypes: in.EventTypes,
			Secret: in.Secret, IsActive: true}
		f.subs = append(f.subs, s)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(s)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/events/subscriptions/"):
		id := strings.TrimPrefix(r.URL.Path, "/events/subscriptions/")
		for i, s := range f.subs {
			if s.ID == id {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

const testPublicURL = "https://svc.example.com"
const testEndpoint = testPublicURL + "/api/quotas/_webhooks/auth"

func runSubscribe(t *testing.T, auth *httptest.Server, token, secret string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("AB0T_AUTH_AUTH_URL", auth.URL)
	t.Setenv("AB0T_AUTH_ADMIN_TOKEN", token)
	t.Setenv("AB0T_AUTH_WEBHOOK_PUBLIC_URL", testPublicURL)
	t.Setenv("AB0T_AUTH_WEBHOOK_SECRET", secret)
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"subscribe-events"}, args...))
	err := root.Execute()
	return out.String(), err
}

// A rejected admin token is a FAILURE: non-zero exit (Execute returns an error),
// never an empty "subscription_id:" and exit 0. RED on v0.1.9.
func TestSubscribeEvents_FailsLoudlyWhenAuthRejects_20261016(t *testing.T) {
	f := &fakeAuth{token: "good"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	out, err := runSubscribe(t, srv, "expired", "s1")
	if err == nil {
		t.Fatalf("subscribe-events with a rejected token returned success (exit 0); output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should name the HTTP status: %v", err)
	}
}

func TestSubscribeEvents_FailsLoudlyWhenCreateFails_20261016(t *testing.T) {
	f := &fakeAuth{token: "good", failPOST: http.StatusForbidden}
	srv := httptest.NewServer(f)
	defer srv.Close()
	if out, err := runSubscribe(t, srv, "good", "s1"); err == nil {
		t.Fatalf("create returned 403 but subscribe-events succeeded; output:\n%s", out)
	}
}

// Missing env is a usage error, not a silent skip. RED on v0.1.9 (exit 0).
func TestSubscribeEvents_FailsWhenEnvMissing_20261016(t *testing.T) {
	f := &fakeAuth{token: "good"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, err := runSubscribe(t, srv, "good", "") // no secret
	if err == nil || !strings.Contains(err.Error(), "AB0T_AUTH_WEBHOOK_SECRET") {
		t.Fatalf("err = %v, want a refusal naming AB0T_AUTH_WEBHOOK_SECRET", err)
	}
}

// Re-running with a NEW secret and no --rotate-secret must not report success:
// the existing subscription still signs with the old secret, so every event would
// be rejected with 401. RED on v0.1.9 (exit 0, nothing changed).
func TestSubscribeEvents_SecretDriftIsAnError_20261016(t *testing.T) {
	f := &fakeAuth{token: "good"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	if _, err := runSubscribe(t, srv, "good", "old-secret"); err != nil {
		t.Fatal(err)
	}
	out, err := runSubscribe(t, srv, "good", "new-secret")
	if err == nil || !strings.Contains(err.Error(), "--rotate-secret") {
		t.Fatalf("secret changed without --rotate-secret: err=%v out=%s", err, out)
	}
	if strings.Contains(out, "new-secret") || strings.Contains(err.Error(), "old-secret") {
		t.Fatal("a secret value was printed")
	}
}

// --rotate-secret replaces the subscription: the old one is deleted, a new one is
// created with the CURRENT secret and --events. RED on v0.1.9 (unknown flag).
func TestSubscribeEvents_RotateSecretReplacesSubscription_20261016(t *testing.T) {
	f := &fakeAuth{token: "good"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	if _, err := runSubscribe(t, srv, "good", "old-secret", "--events", "org.created"); err != nil {
		t.Fatal(err)
	}
	out, err := runSubscribe(t, srv, "good", "new-secret", "--rotate-secret",
		"--events", "org.created,user.org_assigned")
	if err != nil {
		t.Fatalf("rotate: %v\n%s", err, out)
	}
	if len(f.subs) != 1 || f.subs[0].Secret != "new-secret" || f.subs[0].ID == "sub_1" ||
		len(f.subs[0].EventTypes) != 2 || f.subs[0].Endpoint != testEndpoint {
		t.Fatalf("after rotate: %+v", f.subs)
	}
	if !strings.Contains(out, "subscription_id: "+f.subs[0].ID) || !strings.Contains(out, "replaced: sub_1") {
		t.Fatalf("output should name the new and the replaced id:\n%s", out)
	}
	// Idempotent afterwards: same secret, same events → success, nothing changes.
	if _, err := runSubscribe(t, srv, "good", "new-secret", "--events", "org.created,user.org_assigned"); err != nil {
		t.Fatalf("re-run after rotate: %v", err)
	}
	if len(f.subs) != 1 {
		t.Fatalf("re-run created another subscription: %+v", f.subs)
	}
}

// The existing subscription is found even when it is not on the first page of
// auth's paginated list (v0.1.9 read page 1 only, then hit 409 on create and
// exited 0 with an empty id).
func TestSubscribeEvents_FindsExistingOnLaterPage_20261016(t *testing.T) {
	f := &fakeAuth{token: "good", pageSize: 2}
	for i := 0; i < 5; i++ {
		f.subs = append(f.subs, &fakeAuthSub{ID: fmt.Sprintf("sub_other_%d", i),
			Endpoint: fmt.Sprintf("https://other-%d.example.com/hook", i), EventTypes: []string{"user.login"}, IsActive: true})
	}
	f.subs = append(f.subs, &fakeAuthSub{ID: "sub_mine", Endpoint: testEndpoint,
		EventTypes: []string{"org.created", "user.org_assigned"}, Secret: "s1", IsActive: true})
	srv := httptest.NewServer(f)
	defer srv.Close()
	out, err := runSubscribe(t, srv, "good", "s1")
	if err != nil || !strings.Contains(out, "subscription_id: sub_mine") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}

// When auth does not disclose the stored secret, an existing subscription is
// reported as existing, and the output says the secret could not be compared.
func TestSubscribeEvents_ExistingWithoutDisclosedSecret_20261016(t *testing.T) {
	f := &fakeAuth{token: "good", hideSecret: true}
	srv := httptest.NewServer(f)
	defer srv.Close()
	if _, err := runSubscribe(t, srv, "good", "s1"); err != nil {
		t.Fatal(err)
	}
	out, err := runSubscribe(t, srv, "good", "s1")
	if err != nil || !strings.Contains(out, "subscription_id: sub_1") || !strings.Contains(out, "not verified") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}
