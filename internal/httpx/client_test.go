package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_GET_DecodeJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc"})
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	var out map[string]string
	if err := c.GET(context.Background(), "/x", &out); err != nil {
		t.Fatal(err)
	}
	if out["id"] != "abc" {
		t.Errorf("got %v", out)
	}
}

func TestClient_POST_SendsBearer(t *testing.T) {
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c := New(srv.URL, "tok123")
	if err := c.POST(context.Background(), "/x", map[string]string{"k": "v"}, nil); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer tok123" {
		t.Errorf("auth = %q", got)
	}
}

func TestClient_4xxErrorTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"detail":"too many"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	err := c.GET(context.Background(), "/x", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsStatus(err, 429) {
		t.Errorf("not 429: %v", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Status != 429 {
		t.Errorf("untyped: %v", err)
	}
}
