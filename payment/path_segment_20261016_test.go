package payment

// v0.1.11 (ticket .private/tickets/20261016_test_hardening_findings, Q-3 — "same
// pattern in payment/client.go"): url.PathEscape left "." and ".." intact, so an
// org id ".." produced /portal/../session. A blank, "." or ".." id is now refused
// before any request is sent; any other id is still one escaped segment.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
)

func TestPaymentIDsRefuseDotSegments_20261016(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := New(mesh.URLs{Payment: srv.URL, PaymentToken: "ab0t_sk_p"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sent := func() int { mu.Lock(); defer mu.Unlock(); return len(paths) }

	for _, id := range []string{"", " ", ".", ".."} {
		calls := map[string]func() error{
			"CreatePortalSession":   func() error { _, err := c.CreatePortalSession(ctx, id, "https://app.example"); return err },
			"ListPaymentMethods":    func() error { _, err := c.ListPaymentMethods(ctx, id); return err },
			"VerifyCheckoutSession": func() error { _, err := c.VerifyCheckoutSession(ctx, id); return err },
		}
		for name, call := range calls {
			before := sent()
			err := call()
			if n := sent() - before; n != 0 || !errors.Is(err, httpx.ErrBadPathSegment) {
				t.Errorf("%s(%q): err=%v, %d request(s) sent — want ErrBadPathSegment and nothing sent", name, id, err, n)
			}
		}
	}

	// Ids that are not dot segments still go out as ONE escaped segment.
	for id, want := range map[string]string{
		"../admin": "/payment-methods/..%2Fadmin",
		"o1/x":     "/payment-methods/o1%2Fx",
		"...":      "/payment-methods/...",
		".a":       "/payment-methods/.a",
		"org-1":    "/payment-methods/org-1",
	} {
		before := sent()
		if _, err := c.ListPaymentMethods(ctx, id); err != nil {
			t.Fatalf("%q: %v", id, err)
		}
		mu.Lock()
		got := paths[before:]
		mu.Unlock()
		if len(got) != 1 || got[0] != want {
			t.Fatalf("%q sent as %v, want %s", id, got, want)
		}
	}
}
