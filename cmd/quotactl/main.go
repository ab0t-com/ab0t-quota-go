// quotactl is the ab0t-quota CLI. Mirrors the Python quota_admin commands
// (subscribe-events, events, replay, backfill, delete-user) so a mixed
// Python/Go deployment can manage either runtime from one binary.
//
// Run `quotactl --help` for the full list.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Populated at link-time by scripts/build.sh via -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	root := newRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "quotactl",
		Short:         "ab0t-quota admin CLI",
		Version:       fmt.Sprintf("%s (commit %s, built %s)", version, commit, buildTime),
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(
		newSubscribeCmd(),
		newEventsCmd(),
		newReplayCmd(),
		newBackfillCmd(),
		newDeleteUserCmd(),
		newCapabilitiesCmd(),
	)
	return root
}
