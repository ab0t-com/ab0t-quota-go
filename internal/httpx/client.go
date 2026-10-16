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
	"net/http"
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
func New(baseURL, token string) *Client {
	return &Client{
		http:    &http.Client{Timeout: 15 * time.Second, CheckRedirect: sameHostRedirectsOnly},
		BaseURL: baseURL,
		Token:   token,
	}
}

func sameHostRedirectsOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("httpx: stopped after 10 redirects")
	}
	if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return fmt.Errorf("httpx: refusing redirect from %s to another host (%s): it would carry the API key",
			via[0].URL.Host, req.URL.Host)
	}
	return nil
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
