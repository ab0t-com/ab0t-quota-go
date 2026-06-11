package handlerledger

// DDB backend stub for v0.1.0. Same staging strategy as redis.go.
//
// Schema (PRODUCT_SPEC §7):
//   Table: ab0t_quota_handler_ledger
//     PK: HANDLER#{handler}#{event_id}
//     SK: META
//     GSI1: PK=USER#{user_id},  SK=attempted_at (ISO)
//     GSI2: PK=STATUS#{status}, SK=attempted_at (ISO)
//     TTL attribute: `ttl` (epoch seconds, 90-day retention)
//   Plus business-dedup entity in the same table:
//     PK: BIZDEDUP#{sha256(key)}, SK: META

func newDDBLedgerStore(_ DDBClient, _ string) LedgerStore {
	// TODO(v0.2): replace with full aws-sdk-go-v2/dynamodb implementation.
	return NewInMemoryLedgerStore()
}
