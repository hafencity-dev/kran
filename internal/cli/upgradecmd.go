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
	"time"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/link"
	"github.com/hafencity-dev/kran/internal/proto"
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
	FetchLinux func(ctx context.Context, ver string) (path string, err error)
	Install    func(src, dst string) error
	// Prepare readies the local restart before anything is replaced: it
	// holds new jobs and waits until no job would be lost (or, with force,
	// says what would be). An error means nothing was changed. nil: none.
	Prepare func(ctx context.Context, force bool) (release func(), err error)
	// Restart restarts the local service; Prepare has waited already.
	Restart func(ctx context.Context, force bool) error
	// DaemonVersion is the version the local daemon reports, "" when none
	// answers. nil: not known (the messages then name this binary's).
	DaemonVersion func(ctx context.Context) string
	// Confirm waits until the restarted daemon reports want and returns
	// what it reports; an error when it does not. nil: not checked.
	Confirm func(ctx context.Context, want string) (string, error)
	Peers   func() []string
	// UpgradePeer brings peer name to ver; newBinary is this machine's kran
	// binary and binVer its version (used for the peer only when it equals
	// ver: an upgrade back to another version takes the cache).
	UpgradePeer func(ctx context.Context, name, newBinary, binVer, ver string, force bool) (*link.UpgradeResult, error)
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
	Node    string // only this linked machine; this one is not touched
}

func (a *App) upgradeCommand() *cobra.Command {
	var f upgradeFlags
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update kran here and on all linked machines",
		Long: `Download the newest kran release from GitHub (no login needed; gh or
GITHUB_TOKEN are used only as a fallback, e.g. when rate limited), replace this binary, restart the local service once
running jobs have finished, then do the same on every linked machine.

A build between releases (--version 0.1.12-5-gf7c1755) comes only from the
binary cache, checked against the SHA256SUMS that ` + "`make cache`" + ` wrote
beside it; it is never downloaded or built. --version installs exactly that
version, also an older one (the way back). --local-only touches this machine
only, -n <machine> that linked machine only.`,
		Example: "  kran upgrade\n  kran upgrade --check\n  kran upgrade --version 0.4.1 --force\n" +
			"  kran upgrade --version 0.1.12-5-gf7c1755 --local-only\n  kran upgrade --version 0.1.12-5-gf7c1755 -n devmac",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.Node != "" && f.Local {
				return &ExitError{Code: 2, Msg: "--local-only and -n cannot be combined: -n already leaves this machine alone"}
			}
			if f.Node != "" && f.Version == "" {
				return &ExitError{Code: 2, Msg: "-n needs --version: name the version that machine should run"}
			}
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
	cmd.Flags().StringVarP(&f.Node, "node", "n", "", "upgrade only this linked machine (needs --version); this machine is not touched")
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
		Prepare: func(ctx context.Context, force bool) (func(), error) {
			return a.prepareRestart(ctx, force, a.Err)
		},
		DaemonVersion: a.daemonVersion,
		Confirm: func(ctx context.Context, want string) (string, error) {
			if a.Dial == nil {
				return "", nil // no daemon can be asked (as in prepareRestart)
			}
			m, err := a.serviceManager()
			if err != nil {
				return "", nil
			}
			if st, err := m.Status(ctx); err != nil || !st.Installed {
				return "", nil // on demand: the next command starts the new binary
			}
			return awaitDaemon(ctx, a.daemonVersion, want, proto.DaemonStartBound, time.Second, time.Now, sleepCtx)
		},
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
		UpgradePeer: func(ctx context.Context, name, bin, binVer, ver string, force bool) (*link.UpgradeResult, error) {
			o := lo
			o.SelfPath, o.SelfVersion, o.Version, o.Force = bin, binVer, ver, force
			o.CacheOnly = true
			o.Exact = f.Version != ""
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
	if f.Version != "" {
		// The version becomes a cache path here and on a peer: checked
		// before any file is touched with it.
		if err := link.ValidVersion(f.Version); err != nil {
			return err
		}
	}
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
	bin, binVer := u.Self, cur
	if f.Node != "" {
		return u.upgradeOne(ctx, f.Node, bin, binVer, target, f.Force)
	}
	// What the daemon runs, not what this binary is: after a binary was
	// put in place by hand the two differ until a restart.
	running := cur
	if u.DaemonVersion != nil {
		if v := u.DaemonVersion(ctx); v != "" {
			running = v
		}
	}
	// Nothing to replace and nothing to restart when the daemon already
	// runs the version asked for (a node that needs a linux runtime still
	// goes through, to fill in a missing one).
	upToDate := target == cur && running == target && u.DaemonVersion != nil && u.FetchLinux == nil
	switch {
	case upToDate:
		fmt.Fprintf(u.Out, "kran %s is already installed here and its daemon runs it.\n", cur)
	case target == cur:
		fmt.Fprintf(u.Out, "kran %s is already installed here.\n", cur)
	case cur == "dev" && f.Version == "":
		return fmt.Errorf("this is a development build; upgrading would replace it with release %s. Pass --version %s to do that", target, target)
	}
	if !upToDate {
		v, err := u.replaceHere(ctx, f, target, cur, running)
		if err != nil {
			return err
		}
		binVer = v
	}
	if f.Local {
		return nil
	}
	var failed []string
	for _, name := range u.Peers() {
		res, err := u.UpgradePeer(ctx, name, bin, binVer, target, f.Force)
		switch {
		case err != nil:
			fmt.Fprintf(u.Err, "kran: %s: %v\n", name, err)
			failed = append(failed, name)
		default:
			fmt.Fprintln(u.Out, peerOutcome(name, res))
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

// replaceHere puts target in place on this machine and restarts its
// daemon in an order that never leaves a half-done state unreported:
// everything is fetched and verified first, then the restart's
// precondition is checked (a refused wait changes nothing), then the
// binary is replaced, the daemon restarted and its version confirmed. It
// returns the version of the binary now on disk.
func (u *upgrader) replaceHere(ctx context.Context, f upgradeFlags, target, cur, running string) (string, error) {
	// Everything is fetched and verified before anything is replaced, so a
	// failure leaves the node exactly as it was. (A node that already runs
	// the target but lacks its linux binary gets it here too.)
	var src, linuxPath string
	if target != cur {
		fmt.Fprintf(u.Out, "Getting kran %s (a build between releases only from the binary cache)...\n", target)
		var err error
		if src, err = u.Fetch(ctx, target); err != nil {
			return "", err
		}
	}
	if u.FetchLinux != nil {
		fmt.Fprintf(u.Out, "Downloading the linux runtime for container jobs (kran %s)...\n", target)
		var err error
		if linuxPath, err = u.FetchLinux(ctx, target); err != nil {
			return "", fmt.Errorf("cannot get the linux runtime of kran %s that container jobs on this node need: %w. Nothing was changed. Retry `kran upgrade`, or set runtime.container.enabled = false in the config if this node should not run container jobs", target, err)
		}
	}
	// The restart's precondition before anything is replaced: a restart
	// that cannot happen now leaves this machine exactly as it was.
	hold := func() {}
	if u.Prepare != nil {
		var err error
		if hold, err = u.Prepare(ctx, f.Force); err != nil {
			return "", fmt.Errorf("%v (kran %s was not installed; this machine still runs %s)", err, target, running)
		}
	}
	if src != "" {
		if err := u.Install(src, u.Self); err != nil {
			hold()
			return "", fmt.Errorf("cannot replace %s: %w (is it writable? install into ~/.local/bin or rerun with the needed permissions)", u.Self, err)
		}
		fmt.Fprintf(u.Out, "Installed kran %s at %s.\n", target, u.Self)
	}
	if linuxPath != "" {
		fmt.Fprintf(u.Out, "Linux runtime for container jobs: kran %s at %s.\n", target, linuxPath)
	}
	if err := u.Restart(ctx, true); err != nil {
		hold()
		return "", fmt.Errorf("the binary on disk is now kran %s while the daemon still runs %s: its restart failed (%v). Finish it with: kran service restart", target, running, err)
	}
	if u.Confirm != nil {
		got, err := u.Confirm(ctx, target)
		if err != nil {
			if got == "" {
				got = running
			}
			return "", fmt.Errorf("the binary on disk is now kran %s while the daemon still runs %s (%v). Finish it with: kran service restart", target, got, err)
		}
	}
	fmt.Fprintln(u.Out, "Local service is running the new version.")
	if src != "" {
		return target, nil
	}
	return cur, nil
}

// upgradeOne brings one linked machine to target and leaves this one alone:
// nothing fetched here, nothing installed or restarted here.
func (u *upgrader) upgradeOne(ctx context.Context, name, bin, binVer, target string, force bool) error {
	known := false
	for _, p := range u.Peers() {
		known = known || p == name
	}
	if !known {
		return fmt.Errorf("%s is not a linked machine this one can reach; `kran status` lists them", name)
	}
	res, err := u.UpgradePeer(ctx, name, bin, binVer, target, force)
	if err != nil {
		return fmt.Errorf("could not upgrade %s: %w; check `kran doctor %s`", name, err, name)
	}
	fmt.Fprintln(u.Out, peerOutcome(name, res))
	for _, n := range res.Notes {
		fmt.Fprintln(u.Out, n)
	}
	return nil
}

// peerOutcome says what a peer runs after an upgrade. "already runs" only
// when nothing was done there; with --version the daemon's own report is
// named (the upgrade fails when it is not the version asked for).
func peerOutcome(name string, res *link.UpgradeResult) string {
	switch {
	case res.Daemon != "" && !res.Installed && !res.Restarted:
		return fmt.Sprintf("%s already runs %s (its daemon reports %s).", name, res.To, res.Daemon)
	case res.Daemon != "":
		return fmt.Sprintf("%s upgraded %s -> %s (its daemon reports %s).", name, orUnknown(res.From), res.To, res.Daemon)
	case !res.Installed && !res.Restarted:
		return fmt.Sprintf("%s already runs %s.", name, res.To)
	}
	return fmt.Sprintf("%s upgraded %s -> %s.", name, orUnknown(res.From), res.To)
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
