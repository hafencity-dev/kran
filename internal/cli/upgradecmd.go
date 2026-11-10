package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/link"
	"github.com/hafencity-dev/kran/internal/service"
	"github.com/hafencity-dev/kran/internal/version"
)

// upgrader holds the side effects of `kran upgrade` so the order of steps is
// testable.
type upgrader struct {
	Latest func(ctx context.Context) (string, error)
	Fetch  func(ctx context.Context, ver string) (path string, err error)
	// FetchLinux downloads and verifies the linux runtime binary of release
	// ver into this node's cache and returns its path. nil when this node
	// does not need one (a Linux node, or container jobs switched off).
	FetchLinux  func(ctx context.Context, ver string) (path string, err error)
	Install     func(src, dst string) error
	Restart     func(ctx context.Context, force bool) error
	Peers       func() []string
	UpgradePeer func(ctx context.Context, name, newBinary, ver string, force bool) (*link.UpgradeResult, error)
	Self        string
	Current     string
	Out         io.Writer
	Err         io.Writer
}

type upgradeFlags struct {
	Version string
	Check   bool
	Force   bool
	Local   bool
}

func (a *App) upgradeCommand() *cobra.Command {
	var f upgradeFlags
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update kran here and on all linked machines",
		Long: `Download the newest kran release from GitHub (no login needed; gh or
GITHUB_TOKEN are used only as a fallback, e.g. when rate limited), replace this binary, restart the local service once
running jobs have finished, then do the same on every linked machine.`,
		Example: "  kran upgrade\n  kran upgrade --check\n  kran upgrade --version 0.4.1 --force",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			self, err := a.Self()
			if err != nil {
				return &ExitError{Code: 1, Msg: "cannot find the kran binary: " + err.Error()}
			}
			u := a.newUpgrader(self, f)
			if err := u.run(cmd.Context(), f); err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&f.Version, "version", "", "install this release instead of the latest")
	cmd.Flags().BoolVar(&f.Check, "check", false, "only report whether a newer release exists")
	cmd.Flags().BoolVar(&f.Force, "force", false, "restart services now even if jobs are running")
	cmd.Flags().BoolVar(&f.Local, "local-only", false, "do not touch linked machines")
	return cmd
}

func (a *App) newUpgrader(self string, f upgradeFlags) *upgrader {
	lo := link.Options{Version: version.Version, SelfPath: self, Force: f.Force}
	enabled := ""
	if cfg, err := config.Load(""); err == nil {
		enabled = cfg.Runtime.Container.Enabled
	}
	return a.upgraderFor(self, f, lo, runtime.GOOS, enabled)
}

// containerJobsOff reads runtime.container.enabled the way the daemon does.
func containerJobsOff(enabled string) bool {
	switch strings.ToLower(strings.TrimSpace(enabled)) {
	case "false", "off", "no", "0", "disabled":
		return true
	}
	return false
}

// linuxRuntimeWanted: a node that is not Linux runs container jobs with the
// linux kran of its own version in its cache, unless container jobs are off.
func linuxRuntimeWanted(goos, enabled string) bool {
	return goos != "linux" && !containerJobsOff(enabled)
}

// upgraderFor builds the upgrader for a node of the given OS. goos and the
// container setting are parameters so the darwin path is testable anywhere.
func (a *App) upgraderFor(self string, f upgradeFlags, lo link.Options, goos, enabled string) *upgrader {
	u := &upgrader{
		Self: self, Current: version.Version, Out: a.Out, Err: a.Err,
		Latest: func(ctx context.Context) (string, error) { return link.LatestRelease(ctx, lo) },
		Fetch: func(ctx context.Context, ver string) (string, error) {
			return link.FetchRelease(ctx, lo, ver, goos, runtime.GOARCH)
		},
		Install: installBinary,
		Restart: func(ctx context.Context, force bool) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			st, err := m.Status(ctx)
			if err != nil || !st.Installed {
				// No service: a daemon started on demand still runs the old
				// binary. Stop it (once idle); the next connection starts
				// the new one.
				return a.stopOnDemand(ctx, force)
			}
			release, err := a.handOver(ctx, m, force)
			if err != nil {
				return err
			}
			defer release()
			return m.Restart(ctx, service.RestartOptions{Force: force, Idle: a.idleCheck})
		},
		Peers: func() []string {
			ps, err := config.LoadPeers("")
			if err != nil {
				return nil
			}
			var out []string
			for _, p := range ps.Peer {
				if p.Direction != "inbound" { // we cannot ssh into inbound-only peers
					out = append(out, p.Name)
				}
			}
			return out
		},
		UpgradePeer: func(ctx context.Context, name, bin, ver string, force bool) (*link.UpgradeResult, error) {
			o := lo
			o.SelfPath, o.Version, o.Force = bin, ver, force
			return link.Upgrade(ctx, name, o)
		},
	}
	if linuxRuntimeWanted(goos, enabled) {
		// The same download, checksum check and atomic cache install a
		// peer's linux binary goes through (link.FetchRelease).
		u.FetchLinux = func(ctx context.Context, ver string) (string, error) {
			p, err := link.FetchRelease(ctx, lo, ver, "linux", runtime.GOARCH)
			if err != nil {
				return "", err
			}
			if prob := execenv.CheckLinuxRuntime(p, ver); prob != nil {
				return "", errors.New(execenv.LinuxRuntimeCause(prob))
			}
			return p, nil
		}
	}
	return u
}

func (u *upgrader) run(ctx context.Context, f upgradeFlags) error {
	target := strings.TrimPrefix(f.Version, "v")
	if target == "" {
		v, err := u.Latest(ctx)
		if err != nil {
			return err
		}
		target = v
	}
	cur := strings.TrimPrefix(u.Current, "v")
	if f.Check {
		if target == cur {
			fmt.Fprintf(u.Out, "kran %s is the latest release.\n", cur)
		} else {
			fmt.Fprintf(u.Out, "kran %s is available (you have %s). Run `kran upgrade`.\n", target, cur)
		}
		return nil
	}
	bin := u.Self
	if target == cur {
		fmt.Fprintf(u.Out, "kran %s is already installed here.\n", cur)
	} else if cur == "dev" && f.Version == "" {
		return fmt.Errorf("this is a development build; upgrading would replace it with release %s. Pass --version %s to do that", target, target)
	}
	// Everything is fetched and verified before anything is replaced, so a
	// failure leaves the node exactly as it was. (A node that already runs
	// the target but lacks its linux binary gets it here too.)
	var src, linuxPath string
	if target != cur {
		fmt.Fprintf(u.Out, "Downloading kran %s...\n", target)
		var err error
		if src, err = u.Fetch(ctx, target); err != nil {
			return err
		}
	}
	if u.FetchLinux != nil {
		fmt.Fprintf(u.Out, "Downloading the linux runtime for container jobs (kran %s)...\n", target)
		var err error
		if linuxPath, err = u.FetchLinux(ctx, target); err != nil {
			return fmt.Errorf("cannot get the linux runtime of kran %s that container jobs on this node need: %w. Nothing was changed. Retry `kran upgrade`, or set runtime.container.enabled = false in the config if this node should not run container jobs", target, err)
		}
	}
	if src != "" {
		if err := u.Install(src, u.Self); err != nil {
			return fmt.Errorf("cannot replace %s: %w (is it writable? install into ~/.local/bin or rerun with the needed permissions)", u.Self, err)
		}
		fmt.Fprintf(u.Out, "Installed kran %s at %s.\n", target, u.Self)
	}
	if linuxPath != "" {
		fmt.Fprintf(u.Out, "Linux runtime for container jobs: kran %s at %s.\n", target, linuxPath)
	}
	if err := u.Restart(ctx, f.Force); err != nil {
		fmt.Fprintf(u.Err, "kran: could not restart the local service: %v\n", err)
		fmt.Fprintln(u.Err, "kran: restart it yourself with `kran service restart --force` when you are ready.")
	} else {
		fmt.Fprintln(u.Out, "Local service is running the new version.")
	}
	if f.Local {
		return nil
	}
	var failed []string
	for _, name := range u.Peers() {
		res, err := u.UpgradePeer(ctx, name, bin, target, f.Force)
		switch {
		case err != nil:
			fmt.Fprintf(u.Err, "kran: %s: %v\n", name, err)
			failed = append(failed, name)
		case !res.Installed && !res.Restarted:
			fmt.Fprintf(u.Out, "%s already runs %s.\n", name, res.To)
		default:
			fmt.Fprintf(u.Out, "%s upgraded %s -> %s.\n", name, orUnknown(res.From), res.To)
		}
		if res != nil {
			for _, n := range res.Notes {
				fmt.Fprintln(u.Out, n)
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not upgrade %s; check `kran doctor <machine>` and rerun `kran upgrade`", strings.Join(failed, ", "))
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// installBinary atomically replaces dst with src (same directory temp+rename,
// so a running kran keeps its old inode and nothing sees a half-written file).
func installBinary(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	real, err := filepath.EvalSymlinks(dst)
	if err != nil {
		real = dst
	}
	tmp, err := os.CreateTemp(filepath.Dir(real), ".kran-upgrade-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, real); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
