// quotactl is the ab0t-quota CLI. Mirrors the Python quota_admin commands
// (subscribe-events, events, replay, backfill, delete-user) so a mixed
// Python/Go deployment can manage either runtime from one binary.
//
// Run `quotactl --help` for the full list.
package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Populated at link-time by scripts/build.sh via -ldflags. A build without
// ldflags (`go install github.com/ab0t-com/ab0t-quota-go/cmd/quotactl@vX.Y.Z`)
// reads the module version and VCS stamp the Go toolchain embeds instead
// (v0.1.10; before that every `go install` build printed "version dev").
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func init() {
	bi, ok := debug.ReadBuildInfo()
	version, commit, buildTime = resolveBuildInfo(version, commit, buildTime, bi, ok)
}

// resolveBuildInfo fills the values ldflags did not set from the toolchain's
// build info: the module version (set by `go install module@version`), and
// vcs.revision / vcs.time (set when built from a VCS checkout). ldflags win.
func resolveBuildInfo(v, c, t string, bi *debug.BuildInfo, ok bool) (string, string, string) {
	if !ok || bi == nil {
		return v, c, t
	}
	if v == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		v = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if c == "unknown" && s.Value != "" {
				c = s.Value
				if len(c) > 12 {
					c = c[:12]
				}
			}
		case "vcs.time":
			if t == "unknown" && s.Value != "" {
				t = s.Value
			}
		}
	}
	return v, c, t
}

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
		newDoctorCmd(),    // ST-CLI-1: posture grading, honest about limits (D-8)
		newProvisionCmd(), // ST-CLI-1: the vehicle — emit-and-let-them-apply
	)
	return root
}
