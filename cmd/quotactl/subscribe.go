package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/config"
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
		configPath   string
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
			types := subscribeEventTypes(cmd, eventTypes, configPath)
			if bad := authevents.UndeliveredEventTypes(types); len(bad) > 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "WARNING:", authevents.UndeliveredSubscriptionWarning(types))
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
	// v0.3.2: the default is the events auth DELIVERS for the sign-up gift — auth.user.registered,
	// plus org.member.added when the config sets auth_events.grant_on_member_joined. It was
	// "org.created,user.org_assigned", which auth never delivers: the gift never arrived.
	cmd.Flags().StringVar(&eventTypes, "events", "",
		"comma-separated event types (default: auth.user.registered, plus org.member.added when the "+
			"config sets auth_events.grant_on_member_joined)")
	cmd.Flags().StringVar(&configPath, "config", "",
		"quota-config.json for the default events (default: $QUOTA_CONFIG_PATH, then ./quota-config.json)")
	cmd.Flags().StringVar(&orgSlug, "org-slug", "", "filter to org by login slug")
	cmd.Flags().StringVar(&orgID, "org-id", "", "filter to org_id directly")
	cmd.Flags().StringVar(&mountPrefix, "prefix", "/api", "API mount prefix")
	cmd.Flags().StringVar(&name, "name", "ab0t-quota-credit-grant", "subscription name")
	cmd.Flags().BoolVar(&rotateSecret, "rotate-secret", false,
		"replace an existing subscription (delete + create) with the current secret and --events")
	return cmd
}

// subscribeEventTypes (v0.3.2): --events when given; else the sign-up gift's delivered events for the
// config (auth.user.registered, + org.member.added when auth_events.grant_on_member_joined). A config
// that cannot be read is said, not guessed: the default without the member-joined event is used.
func subscribeEventTypes(cmd *cobra.Command, eventsFlag, configPath string) []string {
	var types []string
	for _, t := range strings.Split(eventsFlag, ",") {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}
	if len(types) > 0 {
		return types
	}
	memberJoined := false
	if cfg, err := config.LoadConfig(configPath); err == nil {
		memberJoined = cfg.AuthEvents.GrantOnMemberJoined
	} else {
		fmt.Fprintln(cmd.ErrOrStderr(), "note: no readable quota config ("+err.Error()+"); subscribing to "+
			authevents.EventUserRegistered+" only — pass --config, or --events "+authevents.EventUserRegistered+","+
			authevents.EventMemberJoined+" if your config sets auth_events.grant_on_member_joined")
	}
	return authevents.SignupGiftEventTypes(memberJoined)
}
