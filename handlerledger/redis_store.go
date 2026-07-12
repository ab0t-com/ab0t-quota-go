package handlerledger

// TASK P5.1 — real go-redis handler-ledger backend. Ticket
// 20260709_ab0t_quota_systemic_integrity_redesign (findings QG-01/QG-02).
//
// Before P5.1 newRedisLedgerStore returned NewInMemoryLedgerStore() while
// AutoSelectStore logged "backend: Redis" (QG-02). It now builds this
// durable store when a real *redis.Client is supplied; a non-redis client
// (e.g. the honesty test's struct{}{}) returns an error so AutoSelectStore
// degrades LOUDLY to memory — the QG-02 contract survives P5.1 by design.
//
// Schema (PRODUCT_SPEC §7):
//   ledger:row:{handler}:{event_id}   JSON LedgerRow (72h TTL)
//   ledger:by_user:{user_id}          ZSET (score=attempted_at epoch, member=rowKey)
//   ledger:by_status:{status}         ZSET (score=attempted_at epoch, member=rowKey)
//   ledger:bizdedup:{sha256(key)}     JSON dedup row (NO TTL)
//
// Concurrency: RecordAttempt/RecordOutcome use optimistic WATCH/MULTI so two
// replicas racing the same (handler,event_id) cannot both proceed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisRowTTL       = 72 * time.Hour
	redisWatchTries   = 20
	redisDefaultLease = 60
)

type redisLedgerStore struct {
	c redis.Cmdable
}

func rowKey(handler, eventID string) string { return "ledger:row:" + handler + ":" + eventID }
func byUserKey(userID string) string        { return "ledger:by_user:" + userID }
func byStatusKey(s LedgerStatus) string     { return "ledger:by_status:" + string(s) }
func bizDedupKey(hash string) string        { return "ledger:bizdedup:" + hash }

func (s *redisLedgerStore) loadRow(ctx context.Context, g redis.Cmdable, handler, eventID string) (*LedgerRow, error) {
	raw, err := g.Get(ctx, rowKey(handler, eventID)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var row LedgerRow
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *redisLedgerStore) RecordAttempt(ctx context.Context, in AttemptInput) (*AttemptResult, error) {
	rk := rowKey(in.HandlerName, in.EventID)
	var result *AttemptResult
	txf := func(tx *redis.Tx) error {
		existing, err := s.loadRow(ctx, tx, in.HandlerName, in.EventID)
		if err != nil {
			return err
		}
		if existing != nil {
			if IsTerminal(existing.Status) {
				result = &AttemptResult{Proceed: false, CachedRow: existing}
				return nil
			}
			if existing.Status == StatusInProgress && !existing.LeaseExpiresAt.IsZero() &&
				existing.LeaseExpiresAt.After(time.Now()) {
				result = &AttemptResult{Proceed: false, CachedRow: existing}
				return nil
			}
		}
		attempts := 1
		if existing != nil {
			attempts = existing.Attempts + 1
		}
		lease := in.LeaseSeconds
		if lease == 0 {
			lease = redisDefaultLease
		}
		now := time.Now().UTC()
		row := &LedgerRow{
			HandlerName:    in.HandlerName,
			EventID:        in.EventID,
			EventType:      in.EventType,
			Status:         StatusInProgress,
			UserID:         in.UserID,
			OrgID:          in.OrgID,
			Attempts:       attempts,
			AttemptedAt:    now,
			LeaseExpiresAt: now.Add(time.Duration(lease) * time.Second),
			EventPayload:   in.EventPayload,
		}
		blob, err := json.Marshal(row)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, rk, blob, redisRowTTL)
			score := float64(now.Unix())
			if in.UserID != "" {
				pipe.ZAdd(ctx, byUserKey(in.UserID), redis.Z{Score: score, Member: rk})
				pipe.Expire(ctx, byUserKey(in.UserID), redisRowTTL)
			}
			pipe.ZAdd(ctx, byStatusKey(StatusInProgress), redis.Z{Score: score, Member: rk})
			pipe.Expire(ctx, byStatusKey(StatusInProgress), redisRowTTL)
			return nil
		})
		if err != nil {
			return err
		}
		result = &AttemptResult{Proceed: true}
		return nil
	}
	if err := s.watchRetry(ctx, txf, rk); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *redisLedgerStore) RecordOutcome(ctx context.Context, in OutcomeInput) error {
	rk := rowKey(in.HandlerName, in.EventID)
	txf := func(tx *redis.Tx) error {
		row, err := s.loadRow(ctx, tx, in.HandlerName, in.EventID)
		if err != nil {
			return err
		}
		if row == nil {
			return nil // nothing to update (mirrors in-memory store)
		}
		oldStatus := row.Status
		row.Status = in.Status
		row.Reason = in.Reason
		row.SideEffectID = in.SideEffectID
		row.Error = in.Error
		if in.Attempts > 0 {
			row.Attempts = in.Attempts
		}
		row.CompletedAt = time.Now().UTC()
		row.LeaseExpiresAt = time.Time{}
		blob, err := json.Marshal(row)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, rk, blob, redisRowTTL)
			if oldStatus != in.Status {
				pipe.ZRem(ctx, byStatusKey(oldStatus), rk)
			}
			pipe.ZAdd(ctx, byStatusKey(in.Status), redis.Z{Score: float64(row.AttemptedAt.Unix()), Member: rk})
			pipe.Expire(ctx, byStatusKey(in.Status), redisRowTTL)
			return nil
		})
		return err
	}
	return s.watchRetry(ctx, txf, rk)
}

func (s *redisLedgerStore) GetRow(ctx context.Context, handler, eventID string) (*LedgerRow, error) {
	return s.loadRow(ctx, s.c, handler, eventID)
}

func (s *redisLedgerStore) AlreadyDone(ctx context.Context, dedupKey string) (bool, error) {
	n, err := s.c.Exists(ctx, bizDedupKey(HashKey(dedupKey))).Result()
	return n > 0, err
}

func (s *redisLedgerStore) MarkDone(ctx context.Context, in MarkDoneInput) error {
	blob, err := json.Marshal(in)
	if err != nil {
		return err
	}
	// No TTL — promotional/credit dedup rows must never expire.
	return s.c.Set(ctx, bizDedupKey(HashKey(in.DedupKey)), blob, 0).Err()
}

func (s *redisLedgerStore) queryIndex(ctx context.Context, indexKey string, opt QueryOptions) ([]*LedgerRow, error) {
	members, err := s.c.ZRevRange(ctx, indexKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var out []*LedgerRow
	for _, m := range members {
		raw, err := s.c.Get(ctx, m).Result()
		if err == redis.Nil {
			continue // row TTL'd out; index is best-effort
		}
		if err != nil {
			return nil, err
		}
		var row LedgerRow
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, err
		}
		if !opt.Since.IsZero() && row.AttemptedAt.Before(opt.Since) {
			continue
		}
		cp := row
		out = append(out, &cp)
		if opt.Limit > 0 && len(out) >= opt.Limit {
			break
		}
	}
	return out, nil
}

func (s *redisLedgerStore) QueryByUser(ctx context.Context, userID string, opt QueryOptions) ([]*LedgerRow, error) {
	return s.queryIndex(ctx, byUserKey(userID), opt)
}

func (s *redisLedgerStore) QueryByStatus(ctx context.Context, status LedgerStatus, opt QueryOptions) ([]*LedgerRow, error) {
	return s.queryIndex(ctx, byStatusKey(status), opt)
}

func (s *redisLedgerStore) DeleteUser(ctx context.Context, userID string) (int, error) {
	members, err := s.c.ZRange(ctx, byUserKey(userID), 0, -1).Result()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range members {
		raw, err := s.c.Get(ctx, m).Result()
		if err == nil {
			var row LedgerRow
			if json.Unmarshal([]byte(raw), &row) == nil {
				s.c.ZRem(ctx, byStatusKey(row.Status), m)
			}
		}
		if err := s.c.Del(ctx, m).Err(); err != nil {
			return n, err
		}
		n++
	}
	if err := s.c.Del(ctx, byUserKey(userID)).Err(); err != nil {
		return n, err
	}
	return n, nil
}

// watchRetry runs txf under WATCH on keys, retrying on optimistic-lock
// failure a bounded number of times.
func (s *redisLedgerStore) watchRetry(ctx context.Context, txf func(*redis.Tx) error, keys ...string) error {
	w, ok := s.c.(interface {
		Watch(ctx context.Context, fn func(*redis.Tx) error, keys ...string) error
	})
	if !ok {
		return errors.New("handler ledger redis backend: client does not support WATCH transactions")
	}
	for i := 0; i < redisWatchTries; i++ {
		err := w.Watch(ctx, txf, keys...)
		if err == nil {
			return nil
		}
		if err == redis.TxFailedErr {
			continue
		}
		return err
	}
	return fmt.Errorf("handler ledger redis backend: WATCH contention exceeded %d retries", redisWatchTries)
}
