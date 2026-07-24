package authevents

// T-G2(c) RED (GO-09/E-73, pack 20260721_shared_lib_declared_not_discovered).
// SubscribeOnStartup fell back to the GENERIC AUTH_SERVICE_URL — another
// service's convention — to decide which auth service this library registers
// a webhook RECEIVER with. Contract: only the namespaced AB0T_AUTH_AUTH_URL
// is consulted; with it unset, auto-subscribe is an explicitly-logged no-op,
// never aimed by a variable nobody addressed to us.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGenericAuthServiceURLIsIgnored(t *testing.T) {
	var contacted int32
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&contacted, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer decoy.Close()

	t.Setenv("AUTH_SERVICE_URL", decoy.URL) // the decoy: another service's variable
	t.Setenv("AB0T_AUTH_AUTH_URL", "")      // OUR name deliberately unset

	id, err := SubscribeOnStartup(context.Background(), SubscribeInput{
		AdminToken: "tok",
		PublicURL:  "https://svc.example.com",
		Secret:     "s",
		EventTypes: []string{"org.created"},
	})
	if err != nil {
		t.Fatalf("unset AB0T_AUTH_AUTH_URL must be a logged no-op, not an error: %v", err)
	}
	if id != "" {
		t.Errorf("no subscription may be created without a declared auth URL, got id %q", id)
	}
	if n := atomic.LoadInt32(&contacted); n != 0 {
		t.Errorf("GO-09: the generic AUTH_SERVICE_URL was used to aim a webhook registration — "+
			"%d request(s) reached the decoy server", n)
	}
}
