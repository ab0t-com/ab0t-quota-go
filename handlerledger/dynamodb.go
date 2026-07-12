package handlerledger

import "fmt"

// DDB ledger backend selector (TASK P5.1). The real store lives in
// dynamodb_store.go. This file bridges AutoSelectStore's dependency-free
// DDBClient (interface{}) to the concrete aws-sdk client.
//
// Schema (PRODUCT_SPEC §7):
//   Table: ab0t_quota_handler_ledger
//     PK: HANDLER#{handler}#{event_id}   SK: META
//     GSI1: GSI1PK=USER#{user_id},  GSI1SK=attempted_at (ISO)
//     GSI2: GSI2PK=STATUS#{status}, GSI2SK=attempted_at (ISO)
//     TTL attribute: `ttl` (epoch seconds, 90-day retention)
//   Plus business-dedup entity in the same table:
//     PK: BIZDEDUP#{sha256(key)}, SK: META
//
// If the supplied client is not a *dynamodb.Client / ddbAPI (e.g. the QG-02
// honesty test passes struct{}{}), the constructor returns an error so
// AutoSelectStore degrades LOUDLY to memory rather than silently — the QG-02
// contract holds through P5.1.

func newDDBLedgerStore(client DDBClient, table string) (LedgerStore, error) {
	api, ok := client.(ddbAPI)
	if !ok {
		return nil, fmt.Errorf("handler ledger ddb backend: expected a *dynamodb.Client, got %T", client)
	}
	if table == "" {
		table = "ab0t_quota_handler_ledger"
	}
	return &ddbLedgerStore{c: api, table: table}, nil
}
