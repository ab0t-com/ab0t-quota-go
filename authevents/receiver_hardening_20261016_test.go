package authevents

// Ticket 20261016 review: receiver hardening (additive in v0.1.10).
//   - PreviousSecrets: secret-rotation overlap (auth cannot edit a subscription's
//     secret, so rotation = new subscription; the receiver must accept both secrets
//     while deliveries signed with either are in flight).
//   - MaxBodyBytes: the receiver is a public endpoint and v0.1.9 buffered the body
//     with an unbounded io.ReadAll before verifying the signature.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Secret-rotation overlap: during a rotation the receiver accepts the current
// secret AND the previous one, so a subscription re-created with the new secret
// and in-flight deliveries signed with the old one both verify. An unknown secret
// is still a 401. (RED on v0.1.9: no PreviousSecrets field.)
func TestReceiver_AcceptsPreviousSecretDuringRotation_20261016(t *testing.T) {
	reg := NewRegistry()
	var calls atomic.Int32
	reg.OnAuthEvent("org.created", HandlerFunc(func(context.Context, Event) error {
		calls.Add(1)
		return nil
	}))
	srv := MakeRouter(ReceiverConfig{Secret: "new-secret", PreviousSecrets: []string{"", "old-secret"}, Registry: reg})
	body := []byte(`{"event_type":"org.created","event_id":"e1","data":{"user_id":"u1"}}`)
	for _, sec := range []string{"new-secret", "old-secret"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, signedPost(t, WebhookPath, body, sec))
		if rec.Code != http.StatusOK {
			t.Fatalf("signed with %q: got %d, want 200", sec, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, signedPost(t, WebhookPath, body, "attacker"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown secret: got %d, want 401", rec.Code)
	}
	// An empty previous secret must never make an unsigned request verify.
	req := httptest.NewRequest(http.MethodPost, WebhookPath, bytes.NewReader(body))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: got %d, want 401", rec.Code)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

// An oversized body is refused before it is buffered without bound (the receiver
// is a public, unauthenticated-until-verified endpoint). RED on v0.1.9 (io.ReadAll
// with no limit).
func TestReceiver_RefusesOversizedBody_20261016(t *testing.T) {
	srv := MakeRouter(ReceiverConfig{Secret: "shh", Registry: NewRegistry(), MaxBodyBytes: 1024})
	big := bytes.Repeat([]byte("a"), 4096)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, signedPost(t, WebhookPath, big, "shh"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: got %d, want 413", rec.Code)
	}
}
