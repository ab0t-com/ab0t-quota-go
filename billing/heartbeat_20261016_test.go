package billing

// Ticket 20261016 finding 5: billing serves no /billing/heartbeat route, so the
// loop Setup starts whenever billing is wired logged "billing heartbeat failed"
// every 60 s forever. A 404 is a permanent answer: the loop now stops after the
// first one. RED on v0.1.9 (it keeps calling).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/mesh"
)

func TestHeartbeatLoop_StopsOnMissingRoute_20261016(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"detail":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL})
	h := &HeartbeatLoop{Client: c, ServiceName: "svc", Interval: 5 * time.Millisecond}
	h.Start(context.Background())
	defer h.Stop()
	time.Sleep(120 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("heartbeat called %d times against a 404 route, want exactly 1", n)
	}
}

// A transient failure (5xx) keeps the loop alive — only "no such route" stops it.
func TestHeartbeatLoop_KeepsGoingOnTransientFailure_20261016(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c, _ := New(mesh.URLs{Billing: srv.URL})
	h := &HeartbeatLoop{Client: c, ServiceName: "svc", Interval: 5 * time.Millisecond}
	h.Start(context.Background())
	defer h.Stop()
	time.Sleep(120 * time.Millisecond)
	if n := calls.Load(); n < 3 {
		t.Fatalf("heartbeat called %d times against a 503, want it to keep beating", n)
	}
}
