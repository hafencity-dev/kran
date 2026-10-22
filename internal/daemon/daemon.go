// Package daemon is the long-running node process: local API (unix socket +
// UI listener), event bus, peer manager, job orchestration on origin and
// target side, heartbeats and reattach. SPEC §3, §6, §8.
package daemon

import "context"

// Options configure a daemon. Zero values mean production defaults from
// internal/paths and internal/config.
type Options struct {
	ConfigDir  string
	StateDir   string
	RuntimeDir string
	// UIListen overrides config ui.listen ("" = from config, "-" = disabled).
	UIListen string
	Logf     func(format string, args ...any)
}

// Run runs the daemon until ctx is canceled (SIGTERM/SIGINT handled by the
// caller). It returns nil on clean shutdown.
func Run(ctx context.Context, opts Options) error { return run(ctx, opts) }
