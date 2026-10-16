package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/handlerledger"
	"github.com/spf13/cobra"
)

func newEventsCmd() *cobra.Command {
	var (
		userID string
		status string
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Query the ledger for handler attempts/outcomes",
		Long: `Lists ledger rows. NOTE (v0.1.10): the standalone CLI cannot reach your
service's handler ledger, so this verb refuses (non-zero) instead of printing an
empty result; query q.LedgerStore inside your service.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if userID == "" && status == "" {
				return errors.New("specify at least one of --user or --status")
			}
			store, err := storeFromEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var rows []*handlerledger.LedgerRow
			opt := handlerledger.QueryOptions{Limit: limit}
			if userID != "" {
				rows, err = store.QueryByUser(ctx, userID, opt)
			} else {
				rows, err = store.QueryByStatus(ctx, handlerledger.LedgerStatus(status), opt)
			}
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			for _, r := range rows {
				_ = enc.Encode(r)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&userID, "user", "", "filter by user_id")
	cmd.Flags().StringVar(&status, "status", "", "filter by status (success/skipped/failed_permanent/processing)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows")
	return cmd
}
