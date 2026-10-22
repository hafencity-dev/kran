package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/service"
)

func (a *App) serviceManager() (service.Manager, error) { return a.serviceManagerFor(false) }

const noGUIMessage = "There is no GUI login session on this Mac, so a per-user launch agent cannot load. " +
	"kran still works: its daemon starts on demand when a job or a linked machine connects. " +
	"To keep it running permanently, run `kran service install --system` (asks for your password once)."

func (a *App) serviceManagerFor(system bool) (service.Manager, error) {
	if a.ServiceManager != nil {
		return a.ServiceManager(system)
	}
	self, err := a.Self()
	if err != nil {
		return nil, &ExitError{Code: 1, Msg: "cannot find the kran binary: " + err.Error()}
	}
	m, err := service.New(service.Options{KranPath: self, System: system, Logf: func(f string, args ...any) { fmt.Fprintf(a.Err, "kran: "+f+"\n", args...) }})
	if err != nil {
		return nil, &ExitError{Code: 1, Msg: err.Error() + " (run `kran daemon` in a terminal instead)"}
	}
	return m, nil
}

// idleCheck reports whether the local daemon may restart now: no job is
// starting (syncing or setup). Queued and running jobs carry over a restart,
// so on a busy machine a restart waits seconds, not until it is idle. An
// unreachable daemon counts as idle: there is nothing to protect.
func (a *App) idleCheck(ctx context.Context) (bool, string, error) {
	cl, err := a.Dial(ctx)
	if err != nil {
		return true, "", nil
	}
	defer cl.Close()
	st, err := cl.State(ctx)
	if err != nil {
		return true, "", nil
	}
	n := 0
	for _, j := range st.Jobs {
		if j.State.Starting() {
			n++
		}
	}
	if n == 0 {
		return true, "", nil
	}
	return false, fmt.Sprintf("%d jobs are starting", n), nil
}

func (a *App) serviceCommand() *cobra.Command {
	svc := &cobra.Command{
		Use:   "service",
		Short: "Install and control the kran background service",
		Long:  "The kran daemon runs as a systemd user unit (Linux) or a launchd agent (macOS).",
	}
	simple := func(use, short string, fn func(ctx context.Context, m service.Manager) error) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			if err := fn(cmd.Context(), m); err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			return nil
		}}
	}
	svc.AddCommand(a.serviceInstallCommand(),
		simple("uninstall", "Stop and remove the service", func(ctx context.Context, m service.Manager) error {
			if err := m.Uninstall(ctx); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "service removed")
			return nil
		}),
		a.serviceStartCommand(),
		simple("stop", "Stop the service", func(ctx context.Context, m service.Manager) error {
			if err := m.Stop(ctx); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "service stopped")
			return nil
		}),
	)
	var force bool
	restart := &cobra.Command{
		Use:   "restart [--force]",
		Short: "Restart the service; running and queued jobs carry over",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			release, err := a.handOver(cmd.Context(), m, force)
			if err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			defer release()
			if err := m.Restart(cmd.Context(), service.RestartOptions{Force: force, Idle: a.idleCheck}); err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			a.waitDaemon(cmd.Context(), 15*time.Second)
			fmt.Fprintln(a.Out, "service restarted")
			return nil
		},
	}
	restart.Flags().BoolVar(&force, "force", false, "restart now even if jobs are starting")
	svc.AddCommand(restart)

	var asJSON bool
	status := &cobra.Command{
		Use: "status", Short: "Show whether the service is installed and running (exit 3 if not running)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			st, err := m.Status(cmd.Context())
			if err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			if asJSON {
				return json.NewEncoder(a.Out).Encode(st)
			}
			switch {
			case !st.Installed && st.NoGUI:
				fmt.Fprintln(a.Out, "no background service installed: the daemon starts on demand.")
			case !st.Installed:
				fmt.Fprintf(a.Out, "not installed (%s).\n", st.Manager)
			case st.Running:
				fmt.Fprintf(a.Out, "running as %s (pid %d), unit %s\n", modeLabel(st.Mode, st.Manager), st.PID, st.UnitPath)
			default:
				fmt.Fprintf(a.Out, "installed under %s but not running.\n", st.Manager)
			}
			for _, adv := range st.Advice {
				fmt.Fprintln(a.Out, adv)
			}
			if !st.Running && !st.NoGUI {
				return &ExitError{Code: 3}
			}
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	svc.AddCommand(status)
	return svc
}

func (a *App) printServiceAdvice(ctx context.Context, m service.Manager) {
	if st, err := m.Status(ctx); err == nil {
		for _, adv := range st.Advice {
			fmt.Fprintln(a.Out, adv)
		}
	}
}

// waitDaemon polls until the daemon answers (after a service start).
func (a *App) waitDaemon(ctx context.Context, d time.Duration) (*proto.VersionInfo, error) {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		var v proto.VersionInfo
		err := func() error {
			cl, err := a.Dial(ctx)
			if err != nil {
				return err
			}
			defer cl.Close()
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			v, err = cl.Version(cctx)
			return err
		}()
		if err == nil {
			return &v, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil, last
}

var _ client.Client

func (a *App) serviceInstallCommand() *cobra.Command {
	var system, dry, force bool
	cmd := &cobra.Command{
		Use:   "install [--system] [--dry-run] [--force]",
		Short: "Install and start the service (safe to repeat)",
		Long: `Install the kran daemon as a background service: a systemd user unit on Linux,
a launchd agent on macOS.

--system (macOS) installs a launch daemon in /Library/LaunchDaemons that runs as
you. Use it on a Mac without a GUI login (a headless Mac Studio, a build box),
where launch agents cannot load. It needs sudo once. On Linux,
"loginctl enable-linger $USER" gives the same effect for the user unit.`,
		Example: "  kran service install\n  kran service install --system --dry-run",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if system && runtime.GOOS != "darwin" {
				return &ExitError{Code: 2, Msg: "--system is for macOS. On Linux the user service already survives logout once you run `loginctl enable-linger $USER`."}
			}
			m, err := a.serviceManagerFor(system)
			if err != nil {
				return err
			}
			if dry {
				fmt.Fprint(a.Out, m.Unit())
				if p, ok := m.(interface{ Plan() []string }); ok {
					fmt.Fprintln(a.Out, "\nWould run:")
					for _, step := range p.Plan() {
						fmt.Fprintln(a.Out, "  "+step)
					}
				}
				fmt.Fprintln(a.Out, "\nDry run: nothing was changed.")
				return nil
			}
			if runtime.GOOS == "darwin" && !system && service.Mode(service.Options{}) != "launchd-system" &&
				!service.HasGUISession(ctx, service.Options{}) {
				return &ExitError{Code: 1, Msg: noGUIMessage}
			}
			release, err := a.handOver(ctx, m, force)
			if err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			defer release()
			changed, err := m.Install(ctx)
			if err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			a.waitDaemon(ctx, 15*time.Second)
			if changed {
				fmt.Fprintln(a.Out, "service installed and started")
			} else {
				fmt.Fprintln(a.Out, "service already installed; running")
			}
			a.printServiceAdvice(ctx, m)
			return nil
		},
	}
	cmd.Flags().BoolVar(&system, "system", false, "macOS: install a system launch daemon (for Macs without a GUI login; needs sudo)")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print what would be installed and run, change nothing")
	cmd.Flags().BoolVar(&force, "force", false, "stop a daemon the service does not run right away instead of when its jobs are done")
	return cmd
}

func (a *App) serviceStartCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{Use: "start [--force]", Short: "Start the service", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			release, err := a.handOver(ctx, m, force)
			if err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			defer release()
			if err := m.Start(ctx); err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			a.waitDaemon(ctx, 15*time.Second)
			fmt.Fprintln(a.Out, "service started")
			return nil
		}}
	cmd.Flags().BoolVar(&force, "force", false, "stop a daemon the service does not run right away instead of when its jobs are done")
	return cmd
}

func modeLabel(mode, manager string) string {
	switch mode {
	case "launchd-agent":
		return "a launchd agent"
	case "launchd-system":
		return "a launchd system daemon"
	case "systemd":
		return "a systemd user unit"
	}
	return manager
}
