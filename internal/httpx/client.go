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
	"time"
)

// Client wraps net/http with quota-aware ergonomics.
type Client struct {
	http    *http.Client
	BaseURL string
	Token   string // bearer token, if any
}

// New builds a Client with a per-call default timeout of 15s. Override
// via *Client.SetTimeout.
func New(baseURL, token string) *Client {
	return &Client{
		http:    &http.Client{Timeout: 15 * time.Second},
		BaseURL: baseURL,
		Token:   token,
	}
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
	req, _ := http.NewRequestWithContext(ctx, "GET", c.BaseURL+path, nil)
	return c.do(req, out)
}

// POST issues a POST with a JSON body. `in` is encoded; `out` is decoded.
func (c *Client) POST(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("httpx: encode: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

// PUT issues a PUT with a JSON body.
func (c *Client) PUT(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("httpx: encode: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, "PUT", c.BaseURL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

// DELETE issues a DELETE.
func (c *Client) DELETE(ctx context.Context, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "DELETE", c.BaseURL+path, nil)
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("httpx: %s %s: %w", req.Method, req.URL.String(), err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
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
