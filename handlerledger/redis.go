package handlerledger

// Redis backend stub for v0.1.0.
//
// Real impl uses go-redis. Wired via AutoSelectStore. Kept separate from
// the InMemory backend so consumers who don't import go-redis don't pay
// for the dependency unless they actually pass a Redis client.
//
// Schema (PRODUCT_SPEC §7):
//   ledger:row:{handler}:{event_id}      JSON LedgerRow (72h TTL)
//   ledger:by_user:{user_id}             sorted set (score=epoch, member=row_key)
//   ledger:by_status:{status}            sorted set (score=epoch, member=row_key)
//   ledger:bizdedup:{sha256(key)}        JSON dedup row (NO TTL — promotional credits don't expire)
//
// For v0.1.0 the implementation deliberately delegates to InMemoryLedgerStore
// behind the type to avoid pulling go-redis into the public surface before
// the schema is exercised against a real Redis. Phase 4 in the impl tasklist
// upgrades this to the live impl + adds the conformance test against fakeredis.

func newRedisLedgerStore(_ RedisClient) LedgerStore {
	// TODO(v0.2): replace with full go-redis implementation.
	// For now we degrade to in-memory + log a clear warning.
	return NewInMemoryLedgerStore()
}
