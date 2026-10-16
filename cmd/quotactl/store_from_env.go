package main

import (
	"errors"

	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
)

// errNoLedgerAccess is returned by the ledger verbs (events, delete-user).
//
// v0.1.10 (ticket 20261016, C-17): the standalone CLI has no connection to a
// service's handler ledger — it used to run these verbs against a fresh
// in-memory store, so `events` printed nothing ("no failures") and
// `delete-user` printed "deleted 0 rows" and exited 0 for a GDPR request while
// the real ledger kept the data. A success it cannot deliver is now an error.
var errNoLedgerAccess = errors.New("this CLI cannot reach your service's handler ledger (it lives in your " +
	"service's Redis/DynamoDB, behind your service). Run it inside the service: q.LedgerStore.QueryByUser / " +
	"QueryByStatus for events, q.LedgerStore.DeleteUser(ctx, userID) for a deletion request — " +
	"e.g. from an admin route — and keep its result as your deletion record")

// storeFromEnv is retained for the verbs' shape; it never returns a usable
// store from the CLI.
func storeFromEnv() (handlerledger.LedgerStore, error) {
	return nil, errNoLedgerAccess
}
