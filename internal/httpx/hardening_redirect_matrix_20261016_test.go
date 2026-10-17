package httpx

// hardening_redirect_matrix_20261016_test.go — the v0.1.10 claim "a redirect to
// another host carried the X-API-Key (now refused)", attacked on every 3xx code,
// both methods, userinfo / scheme-relative / port tricks and multi-hop chains.
// The scheme-downgrade case (same host, https → http) is in
// hardening_knownbugs_20261016_test.go.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

const testKey = "ab0t_sk_live_redirect_probe"

func evilServer(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var hits atomic.Int32
	var leaked atomic.Value
	leaked.Store("")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if k := r.Header.Get("X-API-Key"); k != "" {
			leaked.Store(k)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(s.Close)
	return s, &hits, &leaked
}

func TestHardening_CrossHostRedirectRefusedForEvery3xx(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			t.Run(fmt.Sprintf("%d/%s", code, method), func(t *testing.T) {
				evil, hits, leaked := evilServer(t)
				front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, evil.URL+"/steal", code)
				}))
				defer front.Close()
				c := New(front.URL, testKey)
				var err error
				switch method {
				case "GET":
					err = c.GET(context.Background(), "/billing/o1/x", nil)
				case "POST":
					err = c.POST(context.Background(), "/billing/o1/settle", map[string]string{"settlement_key": "r1"}, nil)
				default:
					err = c.DELETE(context.Background(), "/subscriptions/o1/s1", nil)
				}
				if err == nil {
					t.Fatal("a refused cross-host redirect returned success")
				}
				if hits.Load() != 0 {
					t.Fatalf("the other host received %d request(s) (leaked key %q)", hits.Load(), leaked.Load())
				}
			})
		}
	}
}

func TestHardening_RedirectTricks(t *testing.T) {
	evil, hits, _ := evilServer(t)
	eu, _ := url.Parse(evil.URL)
	var frontURL string
	tricks := map[string]func() string{
		"userinfo front@evil":    func() string { return "http://" + strings.TrimPrefix(frontURL, "http://") + "@" + eu.Host + "/x" },
		"scheme-relative //evil": func() string { return "//" + eu.Host + "/x" },
		"same IP other port":     func() string { return evil.URL + "/x" },
		"localhost alias":        func() string { return "http://localhost:" + eu.Port() + "/x" },
	}
	for name, loc := range tricks {
		t.Run(name, func(t *testing.T) {
			before := hits.Load()
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", loc())
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer front.Close()
			frontURL = front.URL
			err := New(front.URL, testKey).POST(context.Background(), "/billing/o1/settle", map[string]string{}, nil)
			if err == nil || hits.Load() != before {
				t.Fatalf("err=%v other-host hits %d→%d", err, before, hits.Load())
			}
		})
	}
}

func TestHardening_MultiHopChainToAnotherHostRefused(t *testing.T) {
	evil, hits, _ := evilServer(t)
	var front *httptest.Server
	var frontHits atomic.Int32
	front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frontHits.Add(1)
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, front.URL+"/b", http.StatusFound)
		case "/b":
			http.Redirect(w, r, front.URL+"/c", http.StatusTemporaryRedirect)
		default:
			http.Redirect(w, r, evil.URL+"/steal", http.StatusPermanentRedirect)
		}
	}))
	defer front.Close()
	err := New(front.URL, testKey).GET(context.Background(), "/a", nil)
	if err == nil || hits.Load() != 0 {
		t.Fatalf("err=%v evil hits=%d — a same-host hop must not launder a later cross-host hop", err, hits.Load())
	}
	if frontHits.Load() != 3 {
		t.Fatalf("front hits = %d, want 3 (the same-host hops were followed)", frontHits.Load())
	}
}

func TestHardening_SameHostRedirectKeepsKeyAndBody(t *testing.T) {
	var got atomic.Value
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, srv.URL+"/new", http.StatusPermanentRedirect)
			return
		}
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		got.Store(r.Method + " " + r.Header.Get("X-API-Key") + " " + string(b[:n]))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := New(srv.URL, testKey).POST(context.Background(), "/old", map[string]string{"a": "b"}, nil); err != nil {
		t.Fatal(err)
	}
	if got.Load() != `POST `+testKey+` {"a":"b"}` {
		t.Fatalf("308 same host: target saw %q, want method, key and body preserved", got.Load())
	}
}

func TestHardening_TooManySameHostRedirectsIsAnError(t *testing.T) {
	var srv *httptest.Server
	var n atomic.Int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Redirect(w, r, srv.URL+fmt.Sprintf("/loop%d", n.Load()), http.StatusFound)
	}))
	defer srv.Close()
	if err := New(srv.URL, testKey).GET(context.Background(), "/loop", nil); err == nil {
		t.Fatal("an infinite redirect loop returned success")
	}
	if n.Load() > 11 {
		t.Fatalf("followed %d redirects, want ≤ 10", n.Load())
	}
}
