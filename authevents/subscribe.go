package authevents

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
//
//	GET  /events/subscriptions          → idempotency check
//	POST /events/subscriptions          → create
type SubscribeInput struct {
	AuthURL      string // default $AB0T_AUTH_AUTH_URL (namespaced only — GO-09)
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
	// RotateSecret (v0.1.10, Subscribe only): if a subscription for this
	// endpoint exists, DELETE it and create it again with the current Secret and
	// EventTypes. Auth cannot edit a subscription's secret, so this is the only
	// way to rotate it. SubscribeOnStartup ignores it (boot never deletes).
	RotateSecret bool
}

// SubscribeResult is what Subscribe did (v0.1.10).
type SubscribeResult struct {
	SubscriptionID string
	// Action is "created", "existing" (already subscribed, unchanged) or
	// "rotated" (the old subscription was deleted and a new one created).
	Action     string
	Endpoint   string
	EventTypes []string
	// ReplacedID is the deleted subscription's id when Action is "rotated".
	ReplacedID string
	// SecretVerified reports whether auth disclosed the stored secret so it
	// could be compared with Secret (constant time; never printed). False means
	// "could not compare", not "differs".
	SecretVerified bool
}

// SubscribeError is a Subscribe failure with an operator-facing remedy.
type SubscribeError struct {
	Op     string // "config" | "list" | "existing" | "delete" | "create"
	Status int    // HTTP status from auth, 0 when not an HTTP failure
	Msg    string
}

func (e *SubscribeError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("subscribe-events: %s: HTTP %d: %s", e.Op, e.Status, e.Msg)
	}
	return fmt.Sprintf("subscribe-events: %s: %s", e.Op, e.Msg)
}

// resolve fills SubscribeInput defaults from env (shared by both entry points).
func (in *SubscribeInput) resolve() {
	if in.AuthURL == "" {
		// GO-09: only OUR namespaced name. The generic AUTH_SERVICE_URL is
		// another service's convention — a co-deployed service's variable must
		// never decide which auth service we register a webhook receiver with.
		in.AuthURL = os.Getenv("AB0T_AUTH_AUTH_URL")
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
}

// Endpoint returns the receiver URL this input registers with auth:
// PublicURL + MountPrefix + "/quotas" + WebhookPath.
func (in SubscribeInput) Endpoint() string {
	return strings.TrimRight(in.PublicURL, "/") + in.MountPrefix + "/quotas" + WebhookPath
}

// Subscribe (v0.1.10) registers this service's receiver with auth and REPORTS
// every failure — the strict sibling of SubscribeOnStartup, for operators and
// CI (`quotactl subscribe-events`). It never "succeeds" with nothing
// registered:
//
//   - missing env / no event types / an org slug that does not resolve → error;
//   - any non-2xx from auth (list, delete, create) → *SubscribeError with status;
//   - a subscription for the endpoint already exists:
//   - same event types and (where auth discloses it) the same secret →
//     Action "existing", no change;
//   - different event types, a different secret, or disabled → error naming
//     RotateSecret, because auth would keep delivering with the old settings
//     (and a changed secret means every event fails signature verification);
//   - with RotateSecret → DELETE it, then create it again (Action "rotated").
//     Deliveries between the two calls, and anything auth had queued for the
//     old subscription, are not delivered: run quotactl backfill afterwards
//     for sign-ups in that window. If the create fails after the delete, the
//     error says so — nothing is subscribed until you re-run.
func Subscribe(ctx context.Context, in SubscribeInput) (*SubscribeResult, error) {
	in.resolve()
	var missing []string
	for _, kv := range [][2]string{
		{"AB0T_AUTH_AUTH_URL", in.AuthURL}, {"AB0T_AUTH_ADMIN_TOKEN", in.AdminToken},
		{"AB0T_AUTH_WEBHOOK_PUBLIC_URL", in.PublicURL}, {"AB0T_AUTH_WEBHOOK_SECRET", in.Secret},
	} {
		if kv[1] == "" {
			missing = append(missing, kv[0])
		}
	}
	if len(missing) > 0 {
		return nil, &SubscribeError{Op: "config", Msg: "not set: " + strings.Join(missing, ", ")}
	}
	if len(in.EventTypes) == 0 {
		return nil, &SubscribeError{Op: "config", Msg: "no event types (pass --events, or register a handler first)"}
	}
	endpoint := in.Endpoint()
	orgID := in.WatchOrgID
	if orgID == "" && in.WatchOrgSlug != "" {
		v, err := resolveOrgIDFromSlug(ctx, in.Client, in.AuthURL, in.WatchOrgSlug)
		if err != nil {
			// Strict: never silently widen the subscription to every org.
			return nil, &SubscribeError{Op: "config", Msg: fmt.Sprintf(
				"org slug %q did not resolve (%v); pass --org-id instead", in.WatchOrgSlug, err)}
		}
		orgID = v
	}

	existing, err := listByEndpoint(ctx, in, endpoint)
	if err != nil {
		return nil, err
	}
	res := &SubscribeResult{Endpoint: endpoint, EventTypes: in.EventTypes}
	if existing != nil {
		res.SubscriptionID = existing.id
		if existing.secret != "" {
			res.SecretVerified = true
		}
		if !in.RotateSecret {
			var problems []string
			if !sameSet(existing.eventTypes, in.EventTypes) {
				problems = append(problems, fmt.Sprintf("it listens to %v, you asked for %v",
					existing.eventTypes, in.EventTypes))
			}
			if existing.secret != "" && !hmac.Equal([]byte(existing.secret), []byte(in.Secret)) {
				problems = append(problems, "it signs with a DIFFERENT secret than AB0T_AUTH_WEBHOOK_SECRET "+
					"(every event would fail with 401 invalid signature)")
			}
			if existing.inactive {
				problems = append(problems, "it is disabled in auth")
			}
			if len(problems) > 0 {
				return nil, &SubscribeError{Op: "existing", Msg: fmt.Sprintf(
					"subscription %s for %s exists but %s. Auth cannot edit a subscription's secret: "+
						"re-run with --rotate-secret to delete it and create it again with the current secret "+
						"and events (then backfill sign-ups from the gap)",
					existing.id, endpoint, strings.Join(problems, "; "))}
			}
			res.Action = "existing"
			return res, nil
		}
		if err := deleteSubscription(ctx, in, existing.id); err != nil {
			return nil, err
		}
		res.ReplacedID = existing.id
	}

	body := map[string]any{
		"name":        in.Name,
		"event_types": in.EventTypes,
		"endpoint":    endpoint,
		"secret":      in.Secret,
	}
	if orgID != "" {
		body["filters"] = []map[string]string{{"field": "org_id", "value": orgID}}
	}
	id, status, err := createSubscriptionStatus(ctx, in, body)
	if err != nil || id == "" {
		msg := "auth returned no subscription id"
		if err != nil {
			msg = err.Error()
		}
		if res.ReplacedID != "" {
			msg += fmt.Sprintf(" — the old subscription %s WAS deleted, so nothing is subscribed now: "+
				"re-run the same command", res.ReplacedID)
		}
		return nil, &SubscribeError{Op: "create", Status: status, Msg: msg}
	}
	res.SubscriptionID = id
	res.Action = "created"
	if res.ReplacedID != "" {
		res.Action = "rotated"
	}
	res.SecretVerified = true
	return res, nil
}

type existingSub struct {
	id         string
	eventTypes []string
	secret     string
	inactive   bool
}

// listByEndpoint walks every page of GET /events/subscriptions for an exact
// endpoint match. Any non-200 is an error (the strict path never guesses).
func listByEndpoint(ctx context.Context, in SubscribeInput, endpoint string) (*existingSub, error) {
	next := ""
	for page := 0; page < 1000; page++ {
		u := strings.TrimRight(in.AuthURL, "/") + "/events/subscriptions?limit=100"
		if next != "" {
			u += "&next_token=" + url.QueryEscape(next)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, &SubscribeError{Op: "list", Msg: err.Error()}
		}
		req.Header.Set("Authorization", "Bearer "+in.AdminToken)
		resp, err := in.Client.Do(req)
		if err != nil {
			return nil, &SubscribeError{Op: "list", Msg: err.Error()}
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, &SubscribeError{Op: "list", Status: resp.StatusCode, Msg: authHint(resp.StatusCode, "events.read") +
				truncate(string(b), 200)}
		}
		var payload struct {
			Items     []map[string]any `json:"items"`
			NextToken *string          `json:"next_token"`
		}
		if err := json.Unmarshal(b, &payload); err != nil {
			return nil, &SubscribeError{Op: "list", Msg: "unreadable response: " + err.Error()}
		}
		for _, item := range payload.Items {
			if ep, _ := item["endpoint"].(string); ep != endpoint {
				continue
			}
			e := &existingSub{}
			if id, _ := item["subscription_id"].(string); id != "" {
				e.id = id
			} else if id, _ := item["id"].(string); id != "" {
				e.id = id
			}
			if raw, ok := item["event_types"].([]any); ok {
				for _, v := range raw {
					if s, ok := v.(string); ok {
						e.eventTypes = append(e.eventTypes, s)
					}
				}
			}
			e.secret, _ = item["secret"].(string)
			if active, ok := item["is_active"].(bool); ok && !active {
				e.inactive = true
			}
			if e.id == "" {
				return nil, &SubscribeError{Op: "list", Msg: "a subscription for " + endpoint + " has no id"}
			}
			return e, nil
		}
		if payload.NextToken == nil || *payload.NextToken == "" {
			return nil, nil
		}
		next = *payload.NextToken
	}
	return nil, &SubscribeError{Op: "list", Msg: "more than 1000 pages of subscriptions; refusing to guess"}
}

func deleteSubscription(ctx context.Context, in SubscribeInput, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		strings.TrimRight(in.AuthURL, "/")+"/events/subscriptions/"+url.PathEscape(id), nil)
	if err != nil {
		return &SubscribeError{Op: "delete", Msg: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+in.AdminToken)
	resp, err := in.Client.Do(req)
	if err != nil {
		return &SubscribeError{Op: "delete", Msg: err.Error()}
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound: // 404: already gone
		return nil
	}
	return &SubscribeError{Op: "delete", Status: resp.StatusCode, Msg: authHint(resp.StatusCode, "events.delete") +
		"subscription " + id + " was NOT deleted (nothing changed): " + truncate(string(b), 200)}
}

func authHint(status int, perm string) string {
	switch status {
	case http.StatusUnauthorized:
		return "the admin token was rejected (expired? auth tokens last 15 minutes) — "
	case http.StatusForbidden:
		return "the admin token lacks " + perm + " — "
	}
	return ""
}

func sameSet(a, b []string) bool {
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

// createSubscriptionStatus is createSubscription that also reports the status.
func createSubscriptionStatus(ctx context.Context, in SubscribeInput, body map[string]any) (string, int, error) {
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(in.AuthURL, "/")+"/events/subscriptions", bytes.NewReader(buf))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+in.AdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := in.Client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		hint := authHint(resp.StatusCode, "events.subscribe")
		if resp.StatusCode == http.StatusConflict {
			hint = "auth already has a subscription whose endpoint starts with this one — "
		}
		return "", resp.StatusCode, fmt.Errorf("%s%s", hint, truncate(string(b), 200))
	}
	var sub map[string]any
	_ = json.Unmarshal(b, &sub)
	if id, _ := sub["subscription_id"].(string); id != "" {
		return id, resp.StatusCode, nil
	}
	if id, _ := sub["id"].(string); id != "" {
		return id, resp.StatusCode, nil
	}
	return "", resp.StatusCode, nil
}

// SubscribeOnStartup is idempotent: GET first, then POST only on no match.
// Best-effort — never blocks startup.
func SubscribeOnStartup(ctx context.Context, in SubscribeInput) (string, error) {
	in.resolve()
	if len(in.EventTypes) == 0 {
		slog.Info("auth-event auto-subscribe skipped: no handlers registered")
		return "", nil
	}
	if in.AuthURL == "" || in.AdminToken == "" || in.PublicURL == "" || in.Secret == "" {
		slog.Info("auth-event auto-subscribe skipped: missing one of " +
			"AB0T_AUTH_AUTH_URL, AB0T_AUTH_ADMIN_TOKEN, AB0T_AUTH_WEBHOOK_PUBLIC_URL, AB0T_AUTH_WEBHOOK_SECRET")
		return "", nil
	}

	endpoint := in.Endpoint()

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
	// v0.1.10: walk every page (v0.1.9 read page 1 only, then hit 409 on create
	// when the subscription sat on a later page). Best-effort contract kept: a
	// failure is logged and boot continues.
	existing, lerr := listByEndpoint(ctx, in, endpoint)
	if lerr != nil {
		// Same as v0.1.9: a token that may subscribe but not list (no events.read)
		// still gets to create; a duplicate is refused by auth (409) and logged.
		slog.Warn("auth-event auto-subscribe: could not list subscriptions — trying to create anyway; "+
			"run `quotactl subscribe-events` to see the cause", "err", lerr)
	}
	if existing != nil {
		if existing.secret != "" && !hmac.Equal([]byte(existing.secret), []byte(in.Secret)) {
			slog.Error("auth-event auto-subscribe: the existing subscription signs with a DIFFERENT secret "+
				"than AB0T_AUTH_WEBHOOK_SECRET — every event will be rejected with 401. Rotate with "+
				"`quotactl subscribe-events --rotate-secret`", "id", existing.id)
		}
		slog.Info("auth-event auto-subscribe: already subscribed", "id", existing.id)
		return existing.id, nil
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
	id, _, err := createSubscriptionStatus(ctx, in, body)
	if err == nil && id == "" {
		err = fmt.Errorf("auth returned no subscription id")
	}
	if err != nil {
		slog.Warn("auth-event auto-subscribe: create failed", "err", err)
		return "", nil
	}
	slog.Info("auth-event auto-subscribe: created",
		"id", id, "events", in.EventTypes, "endpoint", endpoint)
	return id, nil
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
