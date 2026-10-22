package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/service"
)

func (a *App) serviceManager() (service.Manager, error) {
	self, err := a.Self()
	if err != nil {
		return nil, &ExitError{Code: 1, Msg: "cannot find the kran binary: " + err.Error()}
	}
	m, err := service.New(service.Options{KranPath: self, Logf: func(f string, args ...any) { fmt.Fprintf(a.Err, "kran: "+f+"\n", args...) }})
	if err != nil {
		return nil, &ExitError{Code: 1, Msg: err.Error() + " (run `kran daemon` in a terminal instead)"}
	}
	return m, nil
}

// idleCheck reports whether the local daemon has no active jobs. An
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
		if !j.State.Terminal() {
			n++
		}
	}
	if n == 0 {
		return true, "", nil
	}
	return false, fmt.Sprintf("%d jobs are active", n), nil
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
	svc.AddCommand(
		simple("install", "Install and start the service (safe to repeat)", func(ctx context.Context, m service.Manager) error {
			changed, err := m.Install(ctx)
			if err != nil {
				return err
			}
			if changed {
				fmt.Fprintln(a.Out, "service installed and started")
			} else {
				fmt.Fprintln(a.Out, "service already installed; running")
			}
			a.printServiceAdvice(ctx, m)
			return nil
		}),
		simple("uninstall", "Stop and remove the service", func(ctx context.Context, m service.Manager) error {
			if err := m.Uninstall(ctx); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "service removed")
			return nil
		}),
		simple("start", "Start the service", func(ctx context.Context, m service.Manager) error {
			if err := m.Start(ctx); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "service started")
			return nil
		}),
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
		Short: "Restart the service once running jobs have finished",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			if err := m.Restart(cmd.Context(), service.RestartOptions{Force: force, Idle: a.idleCheck}); err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			fmt.Fprintln(a.Out, "service restarted")
			return nil
		},
	}
	restart.Flags().BoolVar(&force, "force", false, "restart now even if jobs are running (they are reattached or lost)")
	svc.AddCommand(restart)

	var asJSON bool
	status := &cobra.Command{
		Use: "status", Short: "Show whether the service is installed and running", Args: cobra.NoArgs,
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
			case !st.Installed:
				fmt.Fprintf(a.Out, "not installed (%s). Install it with `kran service install`.\n", st.Manager)
			case st.Running:
				fmt.Fprintf(a.Out, "running under %s (pid %d), unit %s\n", st.Manager, st.PID, st.UnitPath)
			default:
				fmt.Fprintf(a.Out, "installed under %s but not running. Start it with `kran service start`.\n", st.Manager)
			}
			for _, adv := range st.Advice {
				fmt.Fprintln(a.Out, adv)
			}
			if !st.Running {
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
