package authevents

import "sync"

// MemoryPinStore is an in-process pin store. Tests + degraded mode.
type MemoryPinStore struct {
	mu     sync.RWMutex
	values map[string]string // user_id -> org_id
	source map[string]string // user_id -> source ("auto" | "operator")
}

// NewMemoryPinStore returns an empty in-memory pin store.
func NewMemoryPinStore() *MemoryPinStore {
	return &MemoryPinStore{
		values: map[string]string{},
		source: map[string]string{},
	}
}

// Get returns the pinned org_id for user, or "" if absent.
func (s *MemoryPinStore) Get(userID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.values[userID], nil
}

// Set pins user → org. Operator-set values are not overwritten by auto.
func (s *MemoryPinStore) Set(userID, orgID, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existingSource := s.source[userID]
	if existingSource == "operator" && source == "auto" {
		return nil // operator wins
	}
	s.values[userID] = orgID
	s.source[userID] = source
	return nil
}

// Stubs for the persistent backends. Wire-level details in PRODUCT_SPEC.

// NewRedisPinStore returns a Redis-backed pin store. v0.1.0 falls back to
// in-memory; v0.2 wires real Redis.
func NewRedisPinStore(_ any) PinStore { return NewMemoryPinStore() }

// NewDDBPinStore returns a DynamoDB-backed pin store. v0.1.0 falls back to
// in-memory; v0.2 wires real DDB.
//
// Future schema:
//   PK: USER#{user_id}
//   SK: BILLING_ORG
//   attrs: org_id, set_at, source ("auto" | "operator")
// Conditional write on Set: auto never overwrites operator.
func NewDDBPinStore(_ any, _ string) PinStore { return NewMemoryPinStore() }
