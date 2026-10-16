package main

// Ticket 20261016 finding 5: the example must boot and actually limit. v0.1.9's
// example told users to set AB0T_QUOTA_CONFIG_PATH (nothing reads it), returned
// an empty org, and never counted usage, so its "100 requests/hour" never denied.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/quota"
)

func TestBasicExample_BootsAndLimits_20261016(t *testing.T) {
	t.Setenv("QUOTA_CONFIG_PATH", "quota-config.json")
	q, err := quota.Setup(context.Background(), quota.Options{SkipBackgroundLoops: true})
	if err != nil {
		t.Fatalf("the shipped example config does not boot: %v", err)
	}
	defer q.Close(context.Background())
	h := newMux(q)

	call := func(user string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/echo", nil)
		req.Header.Set("X-Demo-User", user)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 1; i <= 100; i++ {
		if code := call("alice"); code != http.StatusOK {
			t.Fatalf("request %d: %d, want 200 (limit is 100/hour)", i, code)
		}
	}
	if code := call("alice"); code != http.StatusTooManyRequests {
		t.Fatalf("request 101: %d, want 429", code)
	}
	if code := call("bob"); code != http.StatusOK {
		t.Fatalf("another org must not share alice's counter: %d", code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("X-Quota-Resource") != "" {
		t.Fatalf("/healthz: %d headers=%v", rec.Code, rec.Header())
	}
}

// The README's "Minimal example" is this file verbatim — so the README example
// is exactly the code that the test above boots (v0.1.9's README showed an API
// that does not exist: quota.Config, Mount, CheckBundle, WriteDenial).
func TestReadmeMinimalExampleIsThisFile_20261016(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "```go\n"+string(src)+"```") {
		t.Fatal("README.md's minimal example is not examples/basic/main.go verbatim — update the README block")
	}
}
