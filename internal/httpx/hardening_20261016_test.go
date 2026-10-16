package httpx

// Ticket 20261016 review (lane B, re-verified). RED on v0.1.9.
//
// B-13: the billing/payment key travels in X-API-Key. net/http strips
//       Authorization/Cookie on a cross-host redirect but NOT custom headers, so
//       a redirect from billing (or anything in front of it) to another host
//       handed that host the service's mesh key.
// B-11: the request-build error was discarded (req, _ := ...), so a malformed
//       base URL made every call PANIC (nil request) — inside the outbox drain
//       loop that aborts the pass, every pass.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_DoesNotLeakAPIKeyOnCrossHostRedirect_20261016(t *testing.T) {
	var leaked string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("X-API-Key")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer evil.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer front.Close()

	c := New(front.URL, "ab0t_sk_live_secret")
	err := c.POST(context.Background(), "/billing/o1/settle", map[string]string{"a": "b"}, nil)
	if leaked != "" {
		t.Fatalf("the mesh API key was sent to the redirect target %s", evil.URL)
	}
	if err == nil {
		t.Fatal("a refused cross-host redirect must be an error, not a success")
	}
}

func TestClient_SameHostRedirectStillWorks_20261016(t *testing.T) {
	var got string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, srv.URL+"/new", http.StatusTemporaryRedirect)
			return
		}
		got = r.Header.Get("X-API-Key")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := New(srv.URL, "ab0t_sk_k").GET(context.Background(), "/old", nil); err != nil || got != "ab0t_sk_k" {
		t.Fatalf("same-host redirect: err=%v key=%q", err, got)
	}
}

func TestClient_BadBaseURLIsAnErrorNotAPanic_20261016(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked on a malformed base URL: %v", r)
		}
	}()
	c := New("http://bad\x7fhost", "")
	if err := c.GET(context.Background(), "/x", nil); err == nil {
		t.Fatal("want an error")
	}
	if err := c.POST(context.Background(), "/x", map[string]int{}, nil); err == nil {
		t.Fatal("want an error")
	}
}
