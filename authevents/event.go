// Package authevents implements the auth-event webhook receiver, the
// pluggable handler registry, and the auto-subscribe machinery.
// See PRODUCT_SPEC.md §11.3-11.5 for the wire contract.
package authevents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Event is the parsed webhook payload.
//
// The auth service's two delivery paths have slightly different envelopes:
//   - v1: top-level event_id / event_type / occurred_at
//   - v2: top-level id / event_type / timestamp (+ X-Webhook-Signature)
//
// The dispatcher reads event_id OR id, event_type OR type, with `data.*`
// taking precedence over top-level for user_id/org_id.
type Event struct {
	EventType  string         `json:"event_type"`
	EventID    string         `json:"event_id"`
	Type       string         `json:"type,omitempty"`       // v2 alias
	ID         string         `json:"id,omitempty"`         // v2 alias
	OccurredAt time.Time      `json:"occurred_at"`
	Timestamp  time.Time      `json:"timestamp,omitempty"`  // v2 alias
	Data       map[string]any `json:"data,omitempty"`

	// Top-level envelope fields (also present in v1).
	TenantID      string         `json:"tenant_id,omitempty"`
	UserIDTop     string         `json:"user_id,omitempty"`
	ActorID       string         `json:"actor_id,omitempty"`
	ResourceID    string         `json:"resource_id,omitempty"`
	ResourceType  string         `json:"resource_type,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	CausationID   string         `json:"causation_id,omitempty"`

	// raw stores the exact bytes received for HMAC verification + replay.
	raw json.RawMessage
}

// Raw returns the exact JSON bytes the receiver got. Used for ledger
// snapshotting + HMAC verification (the verification happens before
// parsing, but Raw lets the dispatcher pass the snapshot to handlers).
func (e Event) Raw() json.RawMessage { return e.raw }

// SetRaw is for the receiver to populate after successful HMAC verify.
func (e *Event) SetRaw(b []byte) { e.raw = append(json.RawMessage{}, b...) }

// GetEventID returns the event_id (or id alias, or a content hash fallback).
func (e Event) GetEventID() string {
	if e.EventID != "" {
		return e.EventID
	}
	if e.ID != "" {
		return e.ID
	}
	return contentHash(e.raw)
}

// GetEventType returns event_type (or type alias).
func (e Event) GetEventType() string {
	if e.EventType != "" {
		return e.EventType
	}
	return e.Type
}

// GetUserID returns data.user_id, falling back to top-level user_id.
func (e Event) GetUserID() string {
	if v, ok := e.Data["user_id"].(string); ok {
		return v
	}
	return e.UserIDTop
}

// GetOrgID returns data.org_id, falling back to top-level tenant_id.
func (e Event) GetOrgID() string {
	if v, ok := e.Data["org_id"].(string); ok {
		return v
	}
	if v, ok := e.Data["organization_id"].(string); ok {
		return v
	}
	return e.TenantID
}

// GetEmail returns data.email if present.
func (e Event) GetEmail() string {
	if v, ok := e.Data["email"].(string); ok {
		return v
	}
	return ""
}

// contentHash is the event_id fallback for events with no id.
// Sha256 of the raw JSON, hex-encoded, first 32 chars.
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:32]
}
