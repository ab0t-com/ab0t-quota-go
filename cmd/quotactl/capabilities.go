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
prints the resulting Capabilities snapshot as JSON. Useful as a deploy
smoke test to confirm the lib will behave the way you expect.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cfg, err := config.LoadConfig(configPath)
			if err != nil {
				return errors.New("load config: " + err.Error())
			}
			q, err := quota.Setup(ctx, quota.Options{ConfigOverride: cfg})
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
