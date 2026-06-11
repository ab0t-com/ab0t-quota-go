package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/spf13/cobra"
)

func newSubscribeCmd() *cobra.Command {
	var (
		eventTypes  string
		orgSlug     string
		orgID       string
		mountPrefix string
		name        string
	)
	cmd := &cobra.Command{
		Use:   "subscribe-events",
		Short: "Register this service's webhook receiver with auth (idempotent)",
		Long: `Subscribes to auth events on the configured AB0T_AUTH_AUTH_URL.

Env required:
  AB0T_AUTH_AUTH_URL              (or AUTH_SERVICE_URL)
  AB0T_AUTH_ADMIN_TOKEN
  AB0T_AUTH_WEBHOOK_PUBLIC_URL
  AB0T_AUTH_WEBHOOK_SECRET`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var types []string
			if eventTypes != "" {
				for _, t := range strings.Split(eventTypes, ",") {
					t = strings.TrimSpace(t)
					if t != "" {
						types = append(types, t)
					}
				}
			}
			id, err := authevents.SubscribeOnStartup(ctx, authevents.SubscribeInput{
				EventTypes:   types,
				WatchOrgSlug: orgSlug,
				WatchOrgID:   orgID,
				Name:         name,
				MountPrefix:  mountPrefix,
			})
			if err != nil {
				return err
			}
			fmt.Println("subscription_id:", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&eventTypes, "events", "org.created,user.org_assigned",
		"comma-separated event types")
	cmd.Flags().StringVar(&orgSlug, "org-slug", "", "filter to org by login slug")
	cmd.Flags().StringVar(&orgID, "org-id", "", "filter to org_id directly")
	cmd.Flags().StringVar(&mountPrefix, "prefix", "/api", "API mount prefix")
	cmd.Flags().StringVar(&name, "name", "ab0t-quota-credit-grant", "subscription name")
	return cmd
}
