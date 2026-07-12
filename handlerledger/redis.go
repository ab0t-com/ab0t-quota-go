package handlerledger

import (
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Redis ledger backend selector (TASK P5.1). The real store lives in
// redis_store.go. This file bridges AutoSelectStore's dependency-free
// RedisClient (interface{}) to a concrete go-redis client.
//
// If the supplied client is not a redis.Cmdable (e.g. the QG-02 honesty
// test passes struct{}{}), we return an error so AutoSelectStore degrades
// LOUDLY to memory rather than silently — the QG-02 contract holds through
// P5.1.

func newRedisLedgerStore(client RedisClient) (LedgerStore, error) {
	cmd, ok := client.(redis.Cmdable)
	if !ok {
		return nil, fmt.Errorf("handler ledger redis backend: expected redis.Cmdable, got %T", client)
	}
	return &redisLedgerStore{c: cmd}, nil
}
