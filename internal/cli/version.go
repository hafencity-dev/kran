package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/version"
)

func versionString() string {
	// Untagged builds use the commit as the version; do not print it twice.
	if version.Commit == "" || version.Commit == "none" || strings.HasPrefix(version.Version, version.Commit) {
		return fmt.Sprintf("kran %s (built %s)", version.Version, version.BuildDate)
	}
	return fmt.Sprintf("kran %s (%s, built %s)", version.Version, version.Commit, version.BuildDate)
}

func (a *App) versionCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the kran version (and the running daemon's)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := version.Info()
			var daemonVer *string
			if cl, err := a.Dial(cmd.Context()); err == nil {
				defer cl.Close()
				ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
				defer cancel()
				if v, err := cl.Version(ctx); err == nil {
					daemonVer = &v.Version
				}
			}
			if asJSON {
				out := map[string]any{"version": info.Version, "commit": info.Commit, "build_date": info.BuildDate,
					"api": info.API, "protocol": info.Protocol, "daemon_version": daemonVer}
				b, _ := json.Marshal(out)
				fmt.Fprintln(a.Out, string(b))
				return nil
			}
			fmt.Fprintln(a.Out, versionString())
			switch {
			case daemonVer == nil:
				fmt.Fprintln(a.Out, "daemon: not running (start it: kran service start)")
			case *daemonVer != info.Version:
				fmt.Fprintf(a.Out, "daemon: %s (differs from this binary; restart it: kran service restart)\n", *daemonVer)
			default:
				fmt.Fprintf(a.Out, "daemon: %s (running)\n", *daemonVer)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}
