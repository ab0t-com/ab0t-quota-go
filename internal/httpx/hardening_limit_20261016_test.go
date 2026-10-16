package httpx

// Ticket 20261016 review B-23: response bodies were read with an unbounded
// io.ReadAll. RED on v0.1.9 by compile (MaxResponseBytes is new).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_BoundsTheResponseBody_20261016(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"a":"` + strings.Repeat("x", MaxResponseBytes+10) + `"}`))
	}))
	defer srv.Close()
	var out map[string]any
	if err := New(srv.URL, "").GET(context.Background(), "/big", &out); err == nil {
		t.Fatal("a response over MaxResponseBytes must be an error, not buffered whole")
	}
}
