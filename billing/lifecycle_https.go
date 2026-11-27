package billing

// HTTPS lifecycle transport (v0.2.0, additive): deliver lifecycle events (resource.started /
// heartbeat / stopped / deleted) straight to billing when no SNS topic is configured. Same contract
// as the Python emitter's HTTPS transport.
//
//	POST {billing_url}/billing/{org_id}/lifecycle-events      (same API key as reserve)
//	200 {"accepted": true, "outcome": "..."}                   delivered
//	408 / 429 / 5xx / no answer                                retried here with backoff, then kept PENDING
//	                                                           in the outbox for the drain loop
//	any other 4xx, or 2xx without accepted: true, or no org_id refused: outbox.ErrPublishRefused — the
//	                                                           emitter settles a terminal event against
//	                                                           billing and voids + alerts any other
//
// LifecyclePublisher implements outbox.Publisher, so it rides the durable outbox exactly like the
// SNS publisher: the intent is written BEFORE Publish, and nothing is ever dropped silently. Every
// event carries an event_id fixed when it was built (meter.LifecycleEvent.EventID) and resent
// unchanged on every retry, so billing's exactly-once intake applies to retries; an intent written
// without one gets a stable id derived from its outbox key. quota.Setup wires this publisher
// automatically when billing is wired and outbox.sns_topic_arn is not set.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/ab0t-com/ab0t-quota-go/outbox"
)

// LifecycleEventResult is billing's answer to one delivered lifecycle event.
type LifecycleEventResult struct {
	Accepted bool   `json:"accepted"`
	Outcome  string `json:"outcome"`
}

// ErrLifecycleRejected marks a 2xx answer that did not accept the event (accepted != true). Only an
// affirmative answer retires an event.
var ErrLifecycleRejected = errors.New("billing: lifecycle event not accepted")

// PostLifecycleEvent sends ONE lifecycle event (its JSON, as the outbox stores it) to
// POST /billing/{org_id}/lifecycle-events. No retry here — see LifecyclePublisher. The error is the
// transport's (*httpx.Error carries the status), ErrLifecycleRejected for a 2xx that did not accept
// the event, or ErrInvalidRequest for a blank / unusable org id or an empty event.
func (c *Client) PostLifecycleEvent(ctx context.Context, orgID string, event json.RawMessage) (*LifecycleEventResult, error) {
	org, err := seg("PostLifecycleEvent", "org id", orgID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if len(bytes.TrimSpace(event)) == 0 {
		return nil, fmt.Errorf("%w: PostLifecycleEvent: empty event", ErrInvalidRequest)
	}
	var out LifecycleEventResult
	if err := c.http.POST(ctx, "/billing/"+org+"/lifecycle-events", event, &out); err != nil {
		return nil, err
	}
	if !out.Accepted {
		return &out, fmt.Errorf("%w (outcome %q)", ErrLifecycleRejected, out.Outcome)
	}
	return &out, nil
}

// LifecyclePublisherOptions tunes the in-call retry. Zero values = the defaults (the Python
// emitter's: 3 attempts, 0.5 s then 1 s).
type LifecyclePublisherOptions struct {
	// Attempts per Publish call (first try included); <= 0 → 3.
	Attempts int
	// Backoff before the second attempt, doubled each retry; <= 0 → 500ms.
	Backoff time.Duration
	// Sleep (tests). nil → a context-aware timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// LifecyclePublisher is the HTTPS outbox.Publisher (see the file comment).
type LifecyclePublisher struct {
	client   *Client
	attempts int
	backoff  time.Duration
	sleep    func(ctx context.Context, d time.Duration) error
}

// NewLifecyclePublisher builds the HTTPS publisher over a billing client (q.Billing).
func NewLifecyclePublisher(c *Client, o LifecyclePublisherOptions) *LifecyclePublisher {
	p := &LifecyclePublisher{client: c, attempts: o.Attempts, backoff: o.Backoff, sleep: o.Sleep}
	if p.attempts <= 0 {
		p.attempts = 3
	}
	if p.backoff <= 0 {
		p.backoff = 500 * time.Millisecond
	}
	if p.sleep == nil {
		p.sleep = sleepCtx
	}
	return p
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// lifecycleRetryable: no HTTP answer, or 408 / 429 / any 5xx (billing's 503 = retry).
func lifecycleRetryable(err error) bool {
	if errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrLifecycleRejected) {
		return false
	}
	var he *httpx.Error
	if errors.As(err, &he) {
		return he.Status == 408 || he.Status == 429 || he.Status >= 500
	}
	return true
}

// Publish implements outbox.Publisher. A retryable failure is retried up to Attempts with doubling
// backoff and then returned as a plain error (the event stays PENDING for the drain). A refusal is
// returned at once wrapping outbox.ErrPublishRefused.
func (p *LifecyclePublisher) Publish(ctx context.Context, r outbox.Record) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("%w: LifecyclePublisher has no billing client", ErrInvalidRequest)
	}
	body, orgID, eventID, err := lifecycleBody(r)
	if err != nil {
		slog.Error("lifecycle_https_REFUSED — the event cannot be delivered (no org_id / not JSON)",
			"reservation_id", r.ReservationID, "event_type", r.EventType, "err", err)
		return fmt.Errorf("%w: %v", outbox.ErrPublishRefused, err)
	}
	wait := p.backoff
	for attempt := 1; ; attempt++ {
		_, err = p.client.PostLifecycleEvent(ctx, orgID, body)
		if err == nil {
			return nil
		}
		if !lifecycleRetryable(err) {
			status := 0
			var he *httpx.Error
			if errors.As(err, &he) {
				status = he.Status
			}
			slog.Error("lifecycle_https_REFUSED — billing refused the event", "reservation_id", r.ReservationID,
				"event_type", r.EventType, "event_id", eventID, "status", status, "err", err)
			return fmt.Errorf("%w: %w", outbox.ErrPublishRefused, err)
		}
		if attempt >= p.attempts || ctx.Err() != nil {
			break
		}
		if serr := p.sleep(ctx, wait); serr != nil {
			break
		}
		wait *= 2
	}
	slog.Error("lifecycle_https_RETRY — billing's lifecycle intake did not answer; the event is RETAINED as "+
		"PENDING in the outbox and retried by the drain (same event_id); it is NOT lost",
		"reservation_id", r.ReservationID, "event_type", r.EventType, "event_id", eventID, "err", err)
	return err
}

// lifecycleBody returns the event to post (with an event_id), its org and its event id. An intent
// without an event_id (written before v0.2.0) gets a STABLE one derived from its outbox key and
// emitted_at — the Python emitter's derivation — so every retry of it carries the same id.
func lifecycleBody(r outbox.Record) (json.RawMessage, string, string, error) {
	dec := json.NewDecoder(bytes.NewReader(r.Event))
	dec.UseNumber()
	var ev map[string]any
	if err := dec.Decode(&ev); err != nil || ev == nil {
		return nil, "", "", fmt.Errorf("lifecycle event %s for %s is not a JSON object", r.EventType, r.ReservationID)
	}
	org, _ := ev["org_id"].(string)
	if strings.TrimSpace(org) == "" {
		return nil, "", "", fmt.Errorf("lifecycle event %s for %s has no org_id", r.EventType, r.ReservationID)
	}
	if id, _ := ev["event_id"].(string); id != "" {
		return r.Event, org, id, nil
	}
	emitted, _ := ev["emitted_at"].(string)
	id := legacyEventID(r.Key, emitted)
	ev["event_id"] = id
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil, "", "", err
	}
	return raw, org, id, nil
}

// uuid5URL is RFC 4122 UUID v5 in the URL namespace, as 32 hex digits (Python's
// uuid.uuid5(uuid.NAMESPACE_URL, name).hex).
// uuid5EventID: RFC 4122 v5 under the library's own namespace, uuid5(NAMESPACE_URL,
// "https://github.com/ab0t-com/ab0t-quota/lifecycle-event-id") = 832691a0-b72c-5ebd-98e3-d39f381d2c7d — the SAME
// namespace the Python emitter uses, so both derive identical ids. Ids are opaque dedup keys, never storage keys.
func uuid5EventID(name string) string {
	ns := []byte{0x83, 0x26, 0x91, 0xa0, 0xb7, 0x2c, 0x5e, 0xbd, 0x98, 0xe3, 0xd3, 0x9f, 0x38, 0x1d, 0x2c, 0x7d}
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(name))
	sum := h.Sum(nil)[:16]
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return hex.EncodeToString(sum)
}

var _ outbox.Publisher = (*LifecyclePublisher)(nil)
