package doctor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/service"
)

func (c *checker) checkIdentity(ctx context.Context) {
	if config.NodeID() == "" {
		r := Result{ID: "identity.node", Title: "node ID", Level: Fail, Detail: "no node ID yet; this machine has not been set up", Fix: "kran setup"}
		r.fix = func(context.Context) error { _, _, err := config.EnsureNodeID(); return err }
		c.add(r)
	} else {
		c.add(Result{ID: "identity.node", Title: "node ID", Level: OK})
	}
	if _, err := config.PublicKey(); err != nil {
		r := Result{ID: "identity.key", Title: "kran SSH key", Level: Fail, Detail: "missing: " + paths.KeyFile(), Fix: "kran setup"}
		r.fix = func(context.Context) error {
			cfg, err := config.Load("")
			if err != nil {
				return err
			}
			_, _, err = config.EnsureKey(cfg.Name)
			return err
		}
		c.add(r)
	} else {
		c.add(Result{ID: "identity.key", Title: "kran SSH key", Level: OK})
	}
	if _, err := config.Load(""); err != nil {
		c.add(Result{ID: "config", Title: "config.toml", Level: Fail, Detail: err.Error(), Fix: "edit " + paths.ConfigFile() + " or delete it to use defaults"})
	}
}

// checkFiles verifies SPEC §9: private dirs and files, 0600 sockets.
func (c *checker) checkFiles(ctx context.Context) {
	type item struct {
		path string
		max  os.FileMode // permitted bits
		dir  bool
	}
	items := []item{
		{paths.ConfigDir(), 0o700, true}, {paths.StateDir(), 0o700, true}, {paths.RuntimeDir(), 0o700, true},
		{paths.KeyFile(), 0o600, false}, {paths.PeersFile(), 0o600, false}, {paths.ConfigFile(), 0o600, false},
		{paths.Socket(), 0o600, false}, {paths.PeerSocket(), 0o600, false}, {paths.TokenFile(), 0o600, false}, {paths.DBFile(), 0o600, false},
	}
	var bad []string
	for _, it := range items {
		st, err := os.Stat(it.path)
		if err != nil {
			continue
		}
		if st.Mode().Perm()&^it.max != 0 {
			bad = append(bad, fmt.Sprintf("%s is %04o, should be %04o", it.path, st.Mode().Perm(), it.max))
		}
	}
	if len(bad) == 0 {
		c.add(Result{ID: "files.perms", Title: "file permissions", Level: OK})
		return
	}
	r := Result{ID: "files.perms", Title: "file permissions", Level: Warn, Detail: strings.Join(bad, "; "),
		Fix: "chmod the listed paths (kran doctor --fix does it)"}
	r.fix = func(context.Context) error {
		for _, it := range items {
			st, err := os.Stat(it.path)
			if err != nil || st.Mode().Perm()&^it.max == 0 {
				continue
			}
			if err := os.Chmod(it.path, st.Mode().Perm()&it.max); err != nil {
				return err
			}
		}
		return nil
	}
	c.add(r)
}

func (c *checker) checkService(ctx context.Context) {
	c.connect(ctx)
	if c.o.Service == nil {
		return
	}
	m, err := c.o.Service()
	if err != nil {
		lvl := Warn
		if c.daemon != nil {
			lvl = Info
		}
		c.add(Result{ID: "service", Title: "background service", Level: lvl, Detail: err.Error(), Fix: "run `kran daemon` in a terminal or under your own supervisor"})
		return
	}
	st, err := m.Status(ctx)
	if err != nil {
		c.add(Result{ID: "service", Title: "background service", Level: Warn, Detail: err.Error()})
		return
	}
	switch {
	case st.NoGUI && !st.Running && (!st.Installed || st.Mode == "launchd-agent"):
		// A LaunchAgent cannot load without a GUI login. kran copes: incoming
		// connections start the daemon, so this is a mode, not a fault.
		d := "on-demand: no GUI login session on this Mac, so the daemon starts when a job or a linked machine connects"
		if c.daemon != nil {
			d += " (running now)"
		}
		c.add(Result{ID: "service", Title: "background service", Level: OK,
			Detail: d + "; to keep it running permanently: kran service install --system"})
	case !st.Installed:
		r := Result{ID: "service", Title: "background service", Level: Fail, Detail: "not installed, so kran stops when you log out or reboot", Fix: "kran service install"}
		r.fix = func(ctx context.Context) error { _, err := m.Install(ctx); return err }
		c.add(r)
	case !st.Running:
		r := Result{ID: "service", Title: "background service", Level: Fail, Detail: "installed but not running" + logTail(st.LogPath), Fix: "kran service start"}
		r.fix = func(ctx context.Context) error { return m.Start(ctx) }
		c.add(r)
	case c.daemon == nil:
		c.add(Result{ID: "service", Title: "background service", Level: Fail,
			Detail: fmt.Sprintf("running (pid %d) but the daemon does not answer on %s%s", st.PID, paths.Socket(), logTail(st.LogPath)),
			Fix:    "kran service restart --force"})
	default:
		c.add(Result{ID: "service", Title: "background service", Level: OK, Detail: fmt.Sprintf("%s, pid %d", modeText(st.Mode, st.Manager), st.PID)})
	}
	if st.Installed && st.Outdated {
		detail := st.UnitPath + " was written by an older kran: the daemon runs without the scheduling priority that keeps it responsive (and its links up) on a loaded machine"
		r := Result{ID: "service.unit", Title: "service definition", Level: Warn, Detail: detail,
			Fix: "kran doctor --fix (or kran service install), then kran service restart"}
		r.fix = func(ctx context.Context) error { _, err := m.Refresh(ctx); return err }
		c.add(r)
	}
	if st.Linger != nil && !*st.Linger {
		r := Result{ID: "linger", Title: "stays running after logout", Level: Warn,
			Detail: "systemd stops your user services when you log out, so jobs from other machines would stop being accepted",
			Fix:    "loginctl enable-linger $USER"}
		r.fix = func(ctx context.Context) error { return service.EnableLinger(ctx, service.Options{}) }
		c.add(r)
	}
}

// logTail returns the last few lines of a log file for context.
func logTail(path string) string {
	if path == "" {
		path = paths.LogFile()
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > 3 {
			lines = lines[1:]
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "; last log lines (" + filepath.Base(path) + "): " + strings.Join(lines, " | ")
}

func (c *checker) checkLocalResources(ctx context.Context) {
	free, total, err := c.o.DiskFree(paths.StateDir())
	if err != nil {
		return
	}
	floor := proto.DiskFloor(total)
	detail := fmt.Sprintf("%s free of %s", gib(free), gib(total))
	if free < floor {
		c.add(Result{ID: "disk", Title: "disk space", Level: Warn, Detail: detail + fmt.Sprintf("; below kran's floor of %s, so this machine refuses remote jobs", gib(floor)),
			Fix: "free space or run `docker system prune` / clear build caches; kran trims its own blob cache automatically"})
		return
	}
	c.add(Result{ID: "disk", Title: "disk space", Level: OK, Detail: detail})
}

func gib(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// modeText names how the daemon is supervised.
func modeText(mode, manager string) string {
	switch mode {
	case "launchd-agent":
		return "launchd agent"
	case "launchd-system":
		return "launchd system daemon"
	case "systemd":
		return "systemd user unit"
	}
	return manager
}
