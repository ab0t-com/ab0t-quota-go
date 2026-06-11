package counters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// IdempotencyStore wraps SetIfAbsent for the request-level "did I already
// run this idempotency key" check.
//
// Wire-level: `quota:idempotency:{key_hash}` with the value = the result
// hash. Python lib uses SHA-256 hex of the client's idempotency key as the
// inner key. The Go port matches.
type IdempotencyStore struct {
	Store  FloatStore
	Prefix KeyPrefix
}

// HashKey returns the SHA-256 hex of the user-supplied idempotency key.
// Empty input → empty output.
func HashKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Key returns the Redis key for an idempotency entry.
func (i IdempotencyStore) Key(hash string) string {
	return i.Prefix.Build("idempotency", hash)
}

// Claim attempts to claim ownership of key for ttl. Returns:
//   - claimed=true when this caller wins the race
//   - claimed=false when another caller already has it (caller should
//     return the cached response or no-op)
func (i IdempotencyStore) Claim(ctx context.Context, key string, value string, ttl time.Duration) (claimed bool, err error) {
	hash := HashKey(key)
	if hash == "" {
		return true, nil // empty key — skip dedup
	}
	return i.Store.SetIfAbsent(ctx, i.Key(hash), value, ttl)
}

// Release explicitly removes an idempotency entry (e.g. on permanent
// failure, freeing the key for retry).
func (i IdempotencyStore) Release(ctx context.Context, key string) error {
	hash := HashKey(key)
	if hash == "" {
		return nil
	}
	return i.Store.Delete(ctx, i.Key(hash))
}
