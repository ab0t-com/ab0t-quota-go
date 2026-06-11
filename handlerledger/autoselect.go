package handlerledger

import "log/slog"

// RedisClient is the subset of go-redis we need. Defined as an interface
// so this package doesn't pull go-redis at compile time when the consumer
// only uses InMemory/DDB backends.
type RedisClient interface{}

// DDBClient is the subset of aws-sdk-go-v2/dynamodb we need.
type DDBClient interface{}

// AutoSelectOptions selects the backend at Setup time.
type AutoSelectOptions struct {
	Redis     RedisClient
	DDBClient DDBClient
	DDBTable  string // default "ab0t_quota_handler_ledger"
}

// AutoSelectStore returns the best available LedgerStore. Priority:
// DDB > Redis > Memory. Memory backend logs a loud warning so it's not
// silently used in prod.
func AutoSelectStore(opts AutoSelectOptions) LedgerStore {
	if opts.DDBClient != nil {
		slog.Info("handler ledger backend: DDB", "table", ddbTableOrDefault(opts.DDBTable))
		return newDDBLedgerStore(opts.DDBClient, ddbTableOrDefault(opts.DDBTable))
	}
	if opts.Redis != nil {
		slog.Info("handler ledger backend: Redis (72h TTL)")
		return newRedisLedgerStore(opts.Redis)
	}
	slog.Warn("handler ledger: NO PERSISTENT STORE — using InMemoryLedgerStore. " +
		"Ledger rows will be lost on restart. Provide Redis or DDB to fix.")
	return NewInMemoryLedgerStore()
}

func ddbTableOrDefault(t string) string {
	if t == "" {
		return "ab0t_quota_handler_ledger"
	}
	return t
}
