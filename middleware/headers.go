// Package middleware exposes HTTP-shaped helpers: header writers, denial
// responders, and the Guard wrapper that runs Engine.Check on each request.
//
// Wire-level parity:
//
//	X-Quota-Limit          — the limit for this tier+resource
//	X-Quota-Remaining       — remaining (limit - used)
//	X-Quota-Used            — used so far in the current period
//	X-Quota-Period           — the period bucket label
//	X-Quota-Tier             — the resolved tier_id
//	X-Quota-Resource         — the resource_key being checked
//	X-Quota-Reason           — the engine decision reason
//	X-Quota-Retry-After      — when to retry (seconds), only on Deny
//	Retry-After              — standard HTTP header, on Deny
//
// Match Python lib's set so dashboards continue to work cross-language.
package middleware

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/ab0t-com/ab0t-quota-go/engine"
)

// WriteHeaders sets the X-Quota-* headers from a Result. Safe to call
// before WriteDenial or before letting the request through.
func WriteHeaders(w http.ResponseWriter, r engine.Result) {
	if r.Resource != "" {
		w.Header().Set("X-Quota-Resource", r.Resource)
	}
	if r.TierID != "" {
		w.Header().Set("X-Quota-Tier", r.TierID)
	}
	if r.Reason != "" {
		w.Header().Set("X-Quota-Reason", r.Reason)
	}
	if r.Limit != nil {
		w.Header().Set("X-Quota-Limit", strconv.FormatFloat(*r.Limit, 'f', -1, 64))
		rem := *r.Limit - r.Used
		if rem < 0 {
			rem = 0
		}
		w.Header().Set("X-Quota-Remaining", strconv.FormatFloat(rem, 'f', -1, 64))
	}
	w.Header().Set("X-Quota-Used", strconv.FormatFloat(r.Used, 'f', -1, 64))
	if r.RetryAfter > 0 {
		secs := int(r.RetryAfter.Seconds())
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		w.Header().Set("X-Quota-Retry-After", strconv.Itoa(secs))
	}
}

// WriteDenial serializes a 429 response from a denied Result. Body shape:
//
//	{
//	  "detail":     "...",          // the rendered message
//	  "reason":     "exceeded",
//	  "resource":   "sandbox.concurrent",
//	  "tier":       "pro",
//	  "used":       7,
//	  "limit":      5,
//	  "upgrade_url": "https://...",
//	}
//
// Matches the Python lib's denial body so any UI/dashboard that already
// parses it continues to work.
func WriteDenial(w http.ResponseWriter, r engine.Result) {
	WriteHeaders(w, r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	body := map[string]any{
		"detail":   r.Message,
		"reason":   r.Reason,
		"resource": r.Resource,
		"tier":     r.TierID,
		"used":     r.Used,
	}
	if r.Limit != nil {
		body["limit"] = *r.Limit
	}
	if r.UpgradeURL != "" {
		body["upgrade_url"] = r.UpgradeURL
	}
	_ = json.NewEncoder(w).Encode(body)
}

// WriteWarn injects the warning copy into a response header without
// affecting the status code. The caller decides whether to expose it in
// the body too.
func WriteWarn(w http.ResponseWriter, r engine.Result) {
	WriteHeaders(w, r)
	if r.Message != "" {
		w.Header().Set("X-Quota-Warning", r.Message)
	}
}
