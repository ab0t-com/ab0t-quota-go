package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newDeleteUserCmd() *cobra.Command {
	var userID string
	cmd := &cobra.Command{
		Use:   "delete-user",
		Short: "Forget a user's ledger rows (compliance / GDPR)",
		Long: `Deletes all handler-ledger rows for the given user_id. Use
when honoring a deletion request. The ledger is the only place this lib
holds per-user data; counters key by org by default.

NOTE (v0.1.10): the standalone CLI cannot reach your service's ledger, so this
verb refuses (non-zero) instead of reporting a deletion it did not do. Call
q.LedgerStore.DeleteUser(ctx, userID) inside your service.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if userID == "" {
				return errors.New("--user-id required")
			}
			store, err := storeFromEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			n, err := store.DeleteUser(ctx, userID)
			if err != nil {
				return err
			}
			fmt.Printf("deleted %d rows for user_id=%s\n", n, userID)
			return nil
		},
	}
	cmd.Flags().StringVar(&userID, "user-id", "", "user_id to forget")
	return cmd
}
