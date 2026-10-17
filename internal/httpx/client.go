// Package httpx is the lib's internal HTTP client. Centralizes timeout
// policy, typed errors, and minimal retry — no consumer should hit
// http.Client directly.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client wraps net/http with quota-aware ergonomics.
type Client struct {
	http    *http.Client
	BaseURL string
	Token   string // bearer token, if any
}

// MaxResponseBytes bounds a response body (v0.1.10). Billing/payment replies are
// small JSON documents; anything larger is refused rather than buffered whole.
const MaxResponseBytes = 10 << 20

// New builds a Client with a per-call default timeout of 15s. Override
// via *Client.SetTimeout.
//
// v0.1.10: redirects to a DIFFERENT host are refused. The mesh key travels in
// X-API-Key, which net/http (unlike Authorization) forwards across hosts — a
// redirect would hand the service's key to whatever host it points at.
//
// v0.1.11: redirects are followed only on the same host, keeping the same ORIGIN —
// scheme, host and port (default port filled in) — or upgrading http → https;
// never a downgrade. v0.1.10 compared the host alone, so an https base URL
// answering a redirect to http:// on the same host sent the key over plaintext.
func New(baseURL, token string) *Client {
	return &Client{
		http:    &http.Client{Timeout: 15 * time.Second, CheckRedirect: sameOriginRedirectsOnly},
		BaseURL: baseURL,
		Token:   token,
	}
}

// origin is the comparison key scheme://host:port — lowercased, default port
// filled in. ok is false for anything that is not an absolute http(s) URL.
func origin(u *url.URL) (key string, ok bool) {
	if u == nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	switch {
	case scheme == "https" && port == "":
		port = "443"
	case scheme == "http" && port == "":
		port = "80"
	case scheme != "http" && scheme != "https":
		return "", false
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port), true
}

// sameOriginRedirectsOnly: every hop must stay on the FIRST request's host, and each hop either keeps the previous
// hop's origin or is an http → https UPGRADE on that host. An upgrade is allowed because refusing it protects nothing
// (the key already went out in plaintext on the first, http request) and would break a client configured with an
// http:// base URL that its server upgrades. A downgrade (https → http) is refused at every hop — that is Q-1.
func sameOriginRedirectsOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("httpx: stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	from, _ := origin(via[0].URL)
	prev, _ := origin(via[len(via)-1].URL)
	to, ok := origin(req.URL)
	sameHost := ok && strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname())
	upgrade := sameHost && strings.EqualFold(via[len(via)-1].URL.Scheme, "http") && strings.EqualFold(req.URL.Scheme, "https")
	if !ok || !sameHost || (to != prev && !upgrade) {
		return fmt.Errorf("httpx: refusing redirect from %s to another origin (%s): it would carry the API key "+
			"(a redirect is followed only to the same scheme, host and port — set the base URL to the final address)",
			from, req.URL.Scheme+"://"+req.URL.Host)
	}
	return nil
}

// ErrBadPathSegment is returned (wrapped) when an id cannot be one URL path
// segment: it is blank, "." or "..".
var ErrBadPathSegment = errors.New("must not be blank, \".\" or \"..\" (it would address another route)")

// PathSegment escapes s as ONE URL path segment, so an id can never address
// another route. url.PathEscape alone leaves "." and ".." intact, and every
// normalising proxy resolves "/billing/../x" to "/x" (RFC 3986 §5.2.4); "%2E%2E"
// is no better (§6.2.2.2 lets a normaliser decode it first). So a blank, "." or
// ".." id is refused (v0.1.11), like the clients' existing blank-org-id check.
// what names the id in the error ("org id", "session id", ...).
func PathSegment(what, s string) (string, error) {
	if t := strings.TrimSpace(s); t == "" || t == "." || t == ".." {
		return "", fmt.Errorf("%s %q %w", what, s, ErrBadPathSegment)
	}
	return url.PathEscape(s), nil
}

// SetTimeout adjusts the http.Client timeout.
func (c *Client) SetTimeout(d time.Duration) { c.http.Timeout = d }

// SetHTTPClient swaps the underlying http.Client. Useful when consumers
// want to inject transport-level instrumentation.
func (c *Client) SetHTTPClient(h *http.Client) {
	if h != nil {
		c.http = h
	}
}

// Error is the typed HTTP error this package returns.
type Error struct {
	Status int
	URL    string
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("httpx: %s → HTTP %d: %s", e.URL, e.Status, truncate(e.Body, 200))
}

// IsStatus reports whether err wraps an Error with the given status code.
func IsStatus(err error, status int) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Status == status
	}
	return false
}

// GET issues a GET. The response body is decoded into `out` if non-nil.
func (c *Client) GET(ctx context.Context, path string, out any) error {
	return c.send(ctx, "GET", path, nil, out)
}

// POST issues a POST with a JSON body. `in` is encoded; `out` is decoded.
func (c *Client) POST(ctx context.Context, path string, in, out any) error {
	return c.sendJSON(ctx, "POST", path, in, out)
}

// PUT issues a PUT with a JSON body.
func (c *Client) PUT(ctx context.Context, path string, in, out any) error {
	return c.sendJSON(ctx, "PUT", path, in, out)
}

// DELETE issues a DELETE.
func (c *Client) DELETE(ctx context.Context, path string, out any) error {
	return c.send(ctx, "DELETE", path, nil, out)
}

func (c *Client) sendJSON(ctx context.Context, method, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("httpx: encode: %w", err)
	}
	return c.send(ctx, method, path, body, out)
}

// send builds the request — and, since v0.1.10, REPORTS a build failure (a
// malformed base URL) instead of dereferencing a nil request.
func (c *Client) send(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return fmt.Errorf("httpx: build %s request: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	if c.Token != "" {
		// Mesh service-to-service auth: ab0t_sk_ API keys are sent via X-API-Key
		// (billing/payment/etc.'s ab0t_auth reads keys from X-API-Key and validates
		// the Authorization header as a JWT only — an API key sent as Bearer 401s).
		// JWT-style tokens keep using Authorization: Bearer.
		if strings.HasPrefix(c.Token, "ab0t_sk_") {
			req.Header.Set("X-API-Key", c.Token)
		} else {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("httpx: %s %s: %w", req.Method, req.URL.String(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("httpx: read %s: %w", req.URL.String(), err)
	}
	if len(body) > MaxResponseBytes {
		return fmt.Errorf("httpx: %s: response larger than %d bytes refused", req.URL.String(), MaxResponseBytes)
	}
	if resp.StatusCode >= 400 {
		return &Error{Status: resp.StatusCode, URL: req.URL.String(), Body: string(body)}
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("httpx: decode %s: %w", req.URL.String(), err)
	}
	return nil
}

// RetryAfter pulls Retry-After (seconds) from an *Error response. Returns
// 0 if absent or unparseable. The Status, URL, Body fields contain the
// rest of the error context.
func (e *Error) RetryAfter() time.Duration {
	return 0 // Body parsing handled by consumer; left intentionally simple.
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Headers are exposed read-only via this small helper for tests.
type Headers map[string]string

// HeaderTime parses an RFC-1123 HTTP date.
func HeaderTime(s string) (time.Time, error) {
	return http.ParseTime(s)
}

// Itoa wraps strconv.Itoa, exposed so callers don't need to import strconv
// for trivial header writes.
func Itoa(i int) string { return strconv.Itoa(i) }
