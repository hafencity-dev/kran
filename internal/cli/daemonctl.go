package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/agents"
	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/daemon"
	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/transport"
)

const noDaemonHint = "the kran daemon is not running; start it with `kran service start` (or run `kran setup`)"

// withClient dials the local daemon and runs fn; a missing daemon becomes a
// message that says what to do.
func (a *App) withClient(ctx context.Context, fn func(client.Client) error) error {
	cl, err := a.Dial(ctx)
	if err != nil {
		if errors.Is(err, client.ErrNoDaemon) {
			return &ExitError{Code: 1, Msg: noDaemonHint}
		}
		return &ExitError{Code: 1, Msg: fmt.Sprintf("cannot reach the kran daemon: %v (try `kran doctor`)", err)}
	}
	defer cl.Close()
	return fn(cl)
}

// getuid is a variable so tests can pretend to be root.
var getuid = os.Getuid

func (a *App) daemonCommand() *cobra.Command {
	var allowRoot bool
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the kran daemon in the foreground (normally started by the service)",
		Long: `Run the kran daemon: scheduler, peer connections, local API and web UI.
Normally the service manager runs this for you (kran service install).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The daemon runs the jobs linked machines send, and its state
			// lives in the home of whoever runs it: as root, every peer
			// job would run as root (SPEC §9).
			if getuid() == 0 && !allowRoot {
				return &ExitError{Code: 1, Msg: "the kran daemon must not run as root: it runs the jobs linked machines send, and they would run as root too. Run it as your user (kran service install, without sudo); if this machine really has only root, use kran daemon --allow-root"}
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			// The daemon logs to daemon.log (rotated) and, in a terminal,
			// to stderr too. Under launchd or systemd stderr is a file or
			// the journal: mirroring every line there would only keep a
			// second, unrotated copy; it gets fatal errors and crashes.
			if err := daemon.Run(ctx, daemon.Options{}); err != nil {
				return &ExitError{Code: 1, Msg: "daemon stopped: " + err.Error()}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowRoot, "allow-root", false, "run as root anyway (every job, including those of linked machines, then runs as root)")
	return cmd
}

func (a *App) serveCommand() *cobra.Command {
	var stdio bool
	var peer string
	cmd := &cobra.Command{
		Use:    "serve --stdio",
		Short:  "Serve the peer protocol on stdin/stdout (used by linked machines over SSH)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !stdio {
				return usagef("serve needs --stdio; it is started by peers over SSH, not by hand")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			if code := transport.ServeStdioAs(ctx, peer); code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&stdio, "stdio", false, "bridge stdin/stdout to the local daemon's peer socket")
	cmd.Flags().StringVar(&peer, "peer", "", "the linked name of the machine this key belongs to (written by kran link)")
	return cmd
}

// internalCommand hosts hidden plumbing: the job supervisor and the link
// protocol steps that run on the remote end.
func (a *App) internalCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "internal", Short: "Internal plumbing (not for direct use)", Hidden: true}
	cmd.AddCommand(&cobra.Command{
		Use:                "supervise",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(c *cobra.Command, args []string) error {
			if code := execenv.Supervise(args); code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:                "apply",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(c *cobra.Command, args []string) error {
			// Runs inside the kran-sync container (container runtime on
			// macOS): applies a slot update streamed on stdin.
			if code := execenv.Apply(args); code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		},
	})
	for _, c := range []struct {
		use string
		run func([]string) int
	}{
		// Both run inside the container runtime's containers (SPEC §6.8):
		// the job's Docker socket proxy and the shared cache trim.
		{"docker-proxy", execenv.DockerProxy},
		{"cache-trim", execenv.CacheTrim},
	} {
		run := c.run
		cmd.AddCommand(&cobra.Command{
			Use:                c.use,
			Hidden:             true,
			DisableFlagParsing: true,
			RunE: func(_ *cobra.Command, args []string) error {
				if code := run(args); code != 0 {
					return &ExitError{Code: code}
				}
				return nil
			},
		})
	}
	cmd.AddCommand(&cobra.Command{
		Use:    "watch-parent <pgid>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			// Started by kran run without a daemon (lifeline.go).
			pgid, err := strconv.Atoi(args[0])
			if err != nil {
				return usagef("watch-parent needs a process group id")
			}
			if code := watchParent(os.Stdin, pgid, 5*time.Second); code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		},
	})
	a.addInternalLink(cmd)
	return cmd
}

func (a *App) hookCommand() *cobra.Command {
	hook := &cobra.Command{Use: "hook", Short: "Agent hook entry points", Hidden: true}
	hook.AddCommand(&cobra.Command{
		Use:   "claude",
		Short: "Claude Code PreToolUse hook: wraps heavy shell commands in `kran run`",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			self, _ := a.Self()
			agents.HookClaude(a.In, a.Out, self)
			return nil // a hook must never fail the user's tool call
		},
	})
	return hook
}

func (a *App) topCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "top",
		Short: "Live terminal view of the mesh and its jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.Hooks.Top == nil {
				return &ExitError{Code: 1, Msg: "kran top is not available in this build; use `kran status` or `kran ui`"}
			}
			return a.withClient(cmd.Context(), func(cl client.Client) error {
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				return a.Hooks.Top(ctx, cl)
			})
		},
	}
}
