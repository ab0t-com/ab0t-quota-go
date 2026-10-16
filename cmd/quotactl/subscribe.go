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
		eventTypes   string
		orgSlug      string
		orgID        string
		mountPrefix  string
		name         string
		rotateSecret bool
	)
	cmd := &cobra.Command{
		Use:   "subscribe-events",
		Short: "Register this service's webhook receiver with auth (idempotent; fails loudly)",
		Long: `Subscribes to auth events on the configured AB0T_AUTH_AUTH_URL, for the endpoint
  $AB0T_AUTH_WEBHOOK_PUBLIC_URL + --prefix + /quotas/_webhooks/auth

Env required:
  AB0T_AUTH_AUTH_URL
  AB0T_AUTH_ADMIN_TOKEN          (events.read + events.subscribe; + events.delete for --rotate-secret)
  AB0T_AUTH_WEBHOOK_PUBLIC_URL
  AB0T_AUTH_WEBHOOK_SECRET

Re-running is safe: an existing subscription with the same events (and, where
auth discloses it, the same secret) is left alone. Any failure exits non-zero
(v0.1.10; earlier versions exited 0 with an empty subscription_id).

Rotating the secret: auth cannot edit a subscription's secret, so
--rotate-secret DELETES the existing subscription and creates it again with
the current AB0T_AUTH_WEBHOOK_SECRET and --events. Before running it, deploy
your service with the NEW secret in AB0T_AUTH_WEBHOOK_SECRET and the old one in
AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS (the receiver accepts both). Events between
the delete and the create are not delivered: run 'quotactl backfill' for
sign-ups in that window. Remove AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS afterwards.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			var types []string
			for _, t := range strings.Split(eventTypes, ",") {
				if t = strings.TrimSpace(t); t != "" {
					types = append(types, t)
				}
			}
			res, err := authevents.Subscribe(ctx, authevents.SubscribeInput{
				EventTypes:   types,
				WatchOrgSlug: orgSlug,
				WatchOrgID:   orgID,
				Name:         name,
				MountPrefix:  mountPrefix,
				RotateSecret: rotateSecret,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "subscription_id:", res.SubscriptionID)
			fmt.Fprintln(out, "action:", res.Action)
			fmt.Fprintln(out, "endpoint:", res.Endpoint)
			fmt.Fprintln(out, "events:", strings.Join(res.EventTypes, ","))
			if res.ReplacedID != "" {
				fmt.Fprintln(out, "replaced:", res.ReplacedID,
					"(backfill sign-ups since the delete; remove AB0T_AUTH_WEBHOOK_SECRET_PREVIOUS once deliveries verify)")
			}
			if !res.SecretVerified {
				fmt.Fprintln(out, "secret: not verified (auth does not disclose the stored secret); "+
					"if events fail with 401 invalid signature, re-run with --rotate-secret")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&eventTypes, "events", "org.created,user.org_assigned",
		"comma-separated event types")
	cmd.Flags().StringVar(&orgSlug, "org-slug", "", "filter to org by login slug")
	cmd.Flags().StringVar(&orgID, "org-id", "", "filter to org_id directly")
	cmd.Flags().StringVar(&mountPrefix, "prefix", "/api", "API mount prefix")
	cmd.Flags().StringVar(&name, "name", "ab0t-quota-credit-grant", "subscription name")
	cmd.Flags().BoolVar(&rotateSecret, "rotate-secret", false,
		"replace an existing subscription (delete + create) with the current secret and --events")
	return cmd
}
