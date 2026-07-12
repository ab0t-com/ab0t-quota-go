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
// DDB > Redis > Memory. The log line always names the backend actually
// returned (finding QG-02): if a requested durable backend is unavailable,
// the fallback to memory is a loud DEGRADED warning, never a silent swap.
func AutoSelectStore(opts AutoSelectOptions) LedgerStore {
	if opts.DDBClient != nil {
		s, err := newDDBLedgerStore(opts.DDBClient, ddbTableOrDefault(opts.DDBTable))
		if err == nil {
			slog.Info("handler ledger backend: DDB", "table", ddbTableOrDefault(opts.DDBTable))
			return s
		}
		slog.Warn("handler ledger: DDB requested but unavailable — DEGRADED to InMemoryLedgerStore (memory). "+
			"Idempotency/ledger rows are process-local and lost on restart; duplicate event side-effects are NOT durably prevented.",
			"err", err)
		return NewInMemoryLedgerStore()
	}
	if opts.Redis != nil {
		s, err := newRedisLedgerStore(opts.Redis)
		if err == nil {
			slog.Info("handler ledger backend: Redis (72h TTL)")
			return s
		}
		slog.Warn("handler ledger: Redis requested but unavailable — DEGRADED to InMemoryLedgerStore (memory). "+
			"Idempotency/ledger rows are process-local and lost on restart; duplicate event side-effects are NOT durably prevented.",
			"err", err)
		return NewInMemoryLedgerStore()
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
