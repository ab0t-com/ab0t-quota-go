package handlerledger

import (
	"context"
	"sort"
	"sync"
	"time"
)

// InMemoryLedgerStore is process-local. Tests and degraded mode only.
type InMemoryLedgerStore struct {
	mu       sync.Mutex
	rows     map[ledgerKey]*LedgerRow // (handler, event_id) -> row
	bizdedup map[string]struct{}      // sha256(key) -> ok
}

type ledgerKey struct{ handler, eventID string }

// NewInMemoryLedgerStore returns an empty in-memory store.
func NewInMemoryLedgerStore() *InMemoryLedgerStore {
	return &InMemoryLedgerStore{
		rows:     map[ledgerKey]*LedgerRow{},
		bizdedup: map[string]struct{}{},
	}
}

func (s *InMemoryLedgerStore) RecordAttempt(ctx context.Context, in AttemptInput) (*AttemptResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := ledgerKey{in.HandlerName, in.EventID}
	existing := s.rows[k]
	if existing != nil {
		if IsTerminal(existing.Status) {
			cp := *existing
			return &AttemptResult{Proceed: false, CachedRow: &cp}, nil
		}
		if existing.Status == StatusInProgress && !existing.LeaseExpiresAt.IsZero() &&
			existing.LeaseExpiresAt.After(time.Now()) {
			cp := *existing
			return &AttemptResult{Proceed: false, CachedRow: &cp}, nil
		}
	}
	attempts := 1
	if existing != nil {
		attempts = existing.Attempts + 1
	}
	lease := in.LeaseSeconds
	if lease == 0 {
		lease = 60
	}
	row := &LedgerRow{
		HandlerName:    in.HandlerName,
		EventID:        in.EventID,
		EventType:      in.EventType,
		Status:         StatusInProgress,
		UserID:         in.UserID,
		OrgID:          in.OrgID,
		Attempts:       attempts,
		AttemptedAt:    time.Now().UTC(),
		LeaseExpiresAt: time.Now().Add(time.Duration(lease) * time.Second).UTC(),
		EventPayload:   in.EventPayload,
	}
	s.rows[k] = row
	return &AttemptResult{Proceed: true}, nil
}

func (s *InMemoryLedgerStore) RecordOutcome(ctx context.Context, in OutcomeInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.rows[ledgerKey{in.HandlerName, in.EventID}]
	if row == nil {
		return nil
	}
	row.Status = in.Status
	row.Reason = in.Reason
	row.SideEffectID = in.SideEffectID
	row.Error = in.Error
	if in.Attempts > 0 {
		row.Attempts = in.Attempts
	}
	row.CompletedAt = time.Now().UTC()
	row.LeaseExpiresAt = time.Time{}
	return nil
}

func (s *InMemoryLedgerStore) GetRow(ctx context.Context, handler, eventID string) (*LedgerRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[ledgerKey{handler, eventID}]
	if r == nil {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (s *InMemoryLedgerStore) AlreadyDone(ctx context.Context, dedupKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.bizdedup[HashKey(dedupKey)]
	return ok, nil
}

func (s *InMemoryLedgerStore) MarkDone(ctx context.Context, in MarkDoneInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bizdedup[HashKey(in.DedupKey)] = struct{}{}
	return nil
}

func (s *InMemoryLedgerStore) QueryByUser(ctx context.Context, userID string, opt QueryOptions) ([]*LedgerRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*LedgerRow
	for _, r := range s.rows {
		if r.UserID != userID {
			continue
		}
		if !opt.Since.IsZero() && r.AttemptedAt.Before(opt.Since) {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptedAt.After(out[j].AttemptedAt) })
	if opt.Limit > 0 && len(out) > opt.Limit {
		out = out[:opt.Limit]
	}
	return out, nil
}

func (s *InMemoryLedgerStore) QueryByStatus(ctx context.Context, status LedgerStatus, opt QueryOptions) ([]*LedgerRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*LedgerRow
	for _, r := range s.rows {
		if r.Status != status {
			continue
		}
		if !opt.Since.IsZero() && r.AttemptedAt.Before(opt.Since) {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptedAt.After(out[j].AttemptedAt) })
	if opt.Limit > 0 && len(out) > opt.Limit {
		out = out[:opt.Limit]
	}
	return out, nil
}

func (s *InMemoryLedgerStore) DeleteUser(ctx context.Context, userID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, r := range s.rows {
		if r.UserID == userID {
			delete(s.rows, k)
			n++
		}
	}
	return n, nil
}
