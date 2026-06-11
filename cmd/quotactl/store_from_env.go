package main

import (
	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

// storeFromEnv returns the ledger backend selected by env vars. v0.1.0
// only supports in-memory + the warning that operations against it are
// process-scoped. v0.2 will wire Redis + DDB.
func storeFromEnv() (handlerledger.LedgerStore, string) {
	return handlerledger.NewInMemoryLedgerStore(), "memory (stub — events/replay/backfill operate on this process only)"
}
