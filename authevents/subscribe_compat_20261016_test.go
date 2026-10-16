package authevents

// Regression guard (ticket 20261016): SubscribeOnStartup is best-effort and must
// keep v0.1.9's behaviour for a least-privilege token that may SUBSCRIBE but not
// LIST (no events.read): the list 403s, the create still happens. GREEN on both
// versions by design — it pins compatibility, not a bug.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubscribeOnStartup_CreatesWhenListIsForbidden_20261016(t *testing.T) {
	created := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusForbidden)
		case http.MethodPost:
			created = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"subscription_id":"sub_new"}`))
		}
	}))
	defer srv.Close()
	id, err := SubscribeOnStartup(context.Background(), SubscribeInput{
		AuthURL: srv.URL, AdminToken: "t", PublicURL: "https://svc.example.com", Secret: "s",
		EventTypes: []string{"org.created"},
	})
	if err != nil || id != "sub_new" || !created {
		t.Fatalf("id=%q err=%v created=%v — a subscribe-only token must still subscribe at boot", id, err, created)
	}
}
