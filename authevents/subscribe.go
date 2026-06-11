package authevents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// SubscribeOnStartup registers THIS service's webhook receiver with auth,
// idempotently. Returns the subscription_id on success or no-op match.
// Returns an error only on usage errors; runtime failures log a warning
// and return nil so callers can fire-and-forget without checking.
//
// Wire shape per `auth/output/appv2/api/events.py`:
//   GET  /events/subscriptions          → idempotency check
//   POST /events/subscriptions          → create
type SubscribeInput struct {
	AuthURL      string // default $AB0T_AUTH_AUTH_URL / $AUTH_SERVICE_URL
	AdminToken   string // default $AB0T_AUTH_ADMIN_TOKEN
	PublicURL    string // default $AB0T_AUTH_WEBHOOK_PUBLIC_URL
	Secret       string // default $AB0T_AUTH_WEBHOOK_SECRET
	EventTypes   []string
	WatchOrgSlug string
	WatchOrgID   string
	Name         string // default "ab0t-quota-credit-grant"
	// MountPrefix is the prefix used by your Mount call. The endpoint is
	// composed as PublicURL + MountPrefix + "/quotas" + WebhookPath.
	// Default "/api".
	MountPrefix string
	// Client is optional; falls back to a 15s http.Client.
	Client *http.Client
}

// SubscribeOnStartup is idempotent: GET first, then POST only on no match.
// Best-effort — never blocks startup.
func SubscribeOnStartup(ctx context.Context, in SubscribeInput) (string, error) {
	if in.AuthURL == "" {
		in.AuthURL = firstNonEmpty(os.Getenv("AB0T_AUTH_AUTH_URL"), os.Getenv("AUTH_SERVICE_URL"))
	}
	if in.AdminToken == "" {
		in.AdminToken = os.Getenv("AB0T_AUTH_ADMIN_TOKEN")
	}
	if in.PublicURL == "" {
		in.PublicURL = os.Getenv("AB0T_AUTH_WEBHOOK_PUBLIC_URL")
	}
	if in.Secret == "" {
		in.Secret = os.Getenv("AB0T_AUTH_WEBHOOK_SECRET")
	}
	if in.MountPrefix == "" {
		in.MountPrefix = "/api"
	}
	if in.Name == "" {
		in.Name = "ab0t-quota-credit-grant"
	}
	if in.Client == nil {
		in.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if len(in.EventTypes) == 0 {
		in.EventTypes = RegisteredEventTypes()
	}
	if len(in.EventTypes) == 0 {
		slog.Info("auth-event auto-subscribe skipped: no handlers registered")
		return "", nil
	}
	if in.AuthURL == "" || in.AdminToken == "" || in.PublicURL == "" || in.Secret == "" {
		slog.Info("auth-event auto-subscribe skipped: missing one of " +
			"AB0T_AUTH_AUTH_URL, AB0T_AUTH_ADMIN_TOKEN, AB0T_AUTH_WEBHOOK_PUBLIC_URL, AB0T_AUTH_WEBHOOK_SECRET")
		return "", nil
	}

	endpoint := strings.TrimRight(in.PublicURL, "/") + in.MountPrefix + "/quotas" + WebhookPath

	// Resolve slug → org_id if needed.
	orgID := in.WatchOrgID
	if orgID == "" && in.WatchOrgSlug != "" {
		if v, err := resolveOrgIDFromSlug(ctx, in.Client, in.AuthURL, in.WatchOrgSlug); err == nil {
			orgID = v
		} else {
			slog.Warn("auto-subscribe: org slug resolution failed; subscribing without org filter",
				"slug", in.WatchOrgSlug, "err", err)
		}
	}

	// Idempotency — GET first.
	if id, err := findExisting(ctx, in, endpoint); err == nil && id != "" {
		slog.Info("auth-event auto-subscribe: already subscribed", "id", id)
		return id, nil
	}

	// Create.
	body := map[string]any{
		"name":        in.Name,
		"event_types": in.EventTypes,
		"endpoint":    endpoint,
		"secret":      in.Secret,
	}
	if orgID != "" {
		body["filters"] = []map[string]string{{"field": "org_id", "value": orgID}}
	}
	id, err := createSubscription(ctx, in, body)
	if err != nil {
		slog.Warn("auth-event auto-subscribe: create failed", "err", err)
		return "", nil
	}
	slog.Info("auth-event auto-subscribe: created",
		"id", id, "events", in.EventTypes, "endpoint", endpoint)
	return id, nil
}

func findExisting(ctx context.Context, in SubscribeInput, endpoint string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		strings.TrimRight(in.AuthURL, "/")+"/events/subscriptions", nil)
	req.Header.Set("Authorization", "Bearer "+in.AdminToken)
	resp, err := in.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("admin token rejected (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil
	}
	b, _ := io.ReadAll(resp.Body)
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		// Try as bare array.
		var arr []map[string]any
		_ = json.Unmarshal(b, &arr)
		payload.Items = arr
	}
	for _, item := range payload.Items {
		if ep, _ := item["endpoint"].(string); ep == endpoint {
			if id, _ := item["subscription_id"].(string); id != "" {
				return id, nil
			}
			if id, _ := item["id"].(string); id != "" {
				return id, nil
			}
		}
	}
	return "", nil
}

func createSubscription(ctx context.Context, in SubscribeInput, body map[string]any) (string, error) {
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(in.AuthURL, "/")+"/events/subscriptions", bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+in.AdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := in.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	b, _ := io.ReadAll(resp.Body)
	var sub map[string]any
	_ = json.Unmarshal(b, &sub)
	if id, _ := sub["subscription_id"].(string); id != "" {
		return id, nil
	}
	if id, _ := sub["id"].(string); id != "" {
		return id, nil
	}
	return "", nil
}

// resolveOrgIDFromSlug scrapes the public hosted-login HTML for window.__AUTH_CONFIG__.orgId.
func resolveOrgIDFromSlug(ctx context.Context, client *http.Client, authURL, slug string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		strings.TrimRight(authURL, "/")+"/login/"+slug, nil)
	client2 := *client
	client2.Timeout = 10 * time.Second
	resp, err := client2.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	const needle = `"orgId":"`
	i := bytes.Index(b, []byte(needle))
	if i < 0 {
		return "", fmt.Errorf("orgId not found in HTML")
	}
	j := bytes.IndexByte(b[i+len(needle):], '"')
	if j < 0 {
		return "", fmt.Errorf("malformed orgId")
	}
	return string(b[i+len(needle) : i+len(needle)+j]), nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
