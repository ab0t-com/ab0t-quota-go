package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/quota"
	"github.com/spf13/cobra"
)

func newCapabilitiesCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "capabilities",
		Short: "Print which subsystems the lib will turn on given current env + config",
		Long: `Loads the config, runs the Setup flow with minimal options, and
prints the resulting Capabilities snapshot as JSON — including the
Resolved map: WHERE each dependency's value came from (config key, env
var, or unset; secrets shown presence-only). Useful as a deploy smoke
test to confirm the lib will behave the way you expect.

Honest side-effect statement (this run is NOT read-only): it performs
the same one-shot verification a real boot performs — connects to the
DECLARED Redis (PING, topology/eviction probes, SCRIPT LOAD of the
counter script) and, when DynamoDB is declared, ensures/verifies the
library's tables (which CREATES them if absent). It does NOT start the
ongoing loops: no billing heartbeats are sent and the outbox drain loop
(which publishes and settles money events) stays OFF.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cfg, err := config.LoadConfig(configPath)
			if err != nil {
				return errors.New("load config: " + err.Error())
			}
			// D-8: assess the chain, move no money — see Options.SkipBackgroundLoops.
			q, err := quota.Setup(ctx, quota.Options{ConfigOverride: cfg, SkipBackgroundLoops: true})
			if err != nil {
				return err
			}
			defer q.Close(context.Background())
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(q.Capabilities())
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file path (defaults to env/search)")
	return cmd
}
