// Package handlerledger provides observability + idempotency for auth-event
// handlers. See PRODUCT_SPEC.md §10 (handler signature) and §13 (multi-replica
// semantics) for the design.
package handlerledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// LedgerStatus is the persisted handler outcome.
type LedgerStatus string

const (
	StatusInProgress      LedgerStatus = "in_progress"
	StatusSuccess         LedgerStatus = "success"
	StatusSkipped         LedgerStatus = "skipped"
	StatusFailed          LedgerStatus = "failed"
	StatusFailedPermanent LedgerStatus = "failed_permanent"
)

// LedgerRow is one ledger entry.
type LedgerRow struct {
	HandlerName    string          `json:"handler_name"`
	EventID        string          `json:"event_id"`
	EventType      string          `json:"event_type"`
	Status         LedgerStatus    `json:"status"`
	UserID         string          `json:"user_id,omitempty"`
	OrgID          string          `json:"org_id,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	SideEffectID   string          `json:"side_effect_id,omitempty"`
	Attempts       int             `json:"attempts"`
	AttemptedAt    time.Time       `json:"attempted_at"`
	CompletedAt    time.Time       `json:"completed_at,omitempty"`
	LeaseExpiresAt time.Time       `json:"lease_expires_at,omitempty"`
	Error          string          `json:"error,omitempty"`
	EventPayload   json.RawMessage `json:"event_payload,omitempty"`
}

// AttemptResult tells the dispatcher whether to run the handler body.
type AttemptResult struct {
	Proceed   bool
	CachedRow *LedgerRow
}

// AttemptInput is the argument to LedgerStore.RecordAttempt.
type AttemptInput struct {
	HandlerName  string
	EventID      string
	EventType    string
	EventPayload json.RawMessage
	UserID       string
	OrgID        string
	LeaseSeconds int // default 60
}

// OutcomeInput is the argument to LedgerStore.RecordOutcome.
type OutcomeInput struct {
	HandlerName  string
	EventID      string
	Status       LedgerStatus
	Reason       string
	SideEffectID string
	Error        string
	Attempts     int
}

// MarkDoneInput is the argument to LedgerStore.MarkDone.
type MarkDoneInput struct {
	DedupKey       string
	SourceHandler  string
	SourceEventID  string
	SideEffectID   string
}

// QueryOptions narrows query results.
type QueryOptions struct {
	Limit int
	Since time.Time // zero = no filter
}

// LedgerStore is the storage contract. Three implementations:
// InMemoryLedgerStore (tests + fail-safe), RedisLedgerStore (72h),
// DDBLedgerStore (90d, via TTL attr). Conformance suite in
// conformance_test.go runs the same tests against all three.
type LedgerStore interface {
	RecordAttempt(ctx context.Context, in AttemptInput) (*AttemptResult, error)
	RecordOutcome(ctx context.Context, in OutcomeInput) error
	GetRow(ctx context.Context, handlerName, eventID string) (*LedgerRow, error)
	AlreadyDone(ctx context.Context, dedupKey string) (bool, error)
	MarkDone(ctx context.Context, in MarkDoneInput) error
	QueryByUser(ctx context.Context, userID string, opt QueryOptions) ([]*LedgerRow, error)
	QueryByStatus(ctx context.Context, status LedgerStatus, opt QueryOptions) ([]*LedgerRow, error)
	DeleteUser(ctx context.Context, userID string) (int, error)
}

// HashKey returns a stable sha256 hex of the dedup key.
func HashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// IsTerminal reports whether the status blocks further attempts.
func IsTerminal(s LedgerStatus) bool {
	return s == StatusSuccess || s == StatusSkipped || s == StatusFailedPermanent
}
