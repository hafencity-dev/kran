package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/hafencity-dev/kran/internal/estimate"
	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/metrics"
	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/selfexe"
	"github.com/hafencity-dev/kran/internal/store"
	"github.com/hafencity-dev/kran/internal/transport"
	"github.com/hafencity-dev/kran/internal/ui"
	"github.com/hafencity-dev/kran/internal/version"
	"github.com/hafencity-dev/kran/internal/workspace"
)

// run wires the production collaborators and runs the daemon.
func run(ctx context.Context, opts Options) error {
	dirs := Dirs{Config: opts.ConfigDir, State: opts.StateDir, Runtime: opts.RuntimeDir}
	if dirs.Config == "" {
		dirs.Config = paths.ConfigDir()
	}
	if dirs.State == "" {
		dirs.State = paths.StateDir()
	}
	if dirs.Runtime == "" {
		if opts.StateDir != "" {
			dirs.Runtime = opts.StateDir
		} else {
			dirs.Runtime = paths.RuntimeDir()
		}
	}
	for _, dir := range []string{dirs.Config, dirs.State, dirs.Runtime} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	// One daemon per dirs, settled before it restores jobs, dials peers or
	// writes a log line. Under a service manager a second one waits.
	release, err := acquireInstance(ctx, dirs.Runtime, supervisor() != "", func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopped while waiting to take over
		}
		return err
	}
	defer release()
	// stdout/stderr (launchd's StandardErrorPath, the on-demand start's
	// log) only get fatal errors and crash traces; keep that file small.
	capCrashLog(os.Stderr, filepath.Join(dirs.State, CrashLogName), 1<<20, 64<<10)

	rot, err := newRotator(filepath.Join(dirs.State, "daemon.log"), 10<<20, 3)
	if err != nil {
		return fmt.Errorf("open daemon.log: %w", err)
	}
	defer rot.Close()
	lg := &logger{now: time.Now, out: []io.Writer{rot}}
	if opts.Logf == nil && term.IsTerminal(int(os.Stderr.Fd())) {
		lg.out = append(lg.out, os.Stderr)
	}
	logf := lg.Logf
	if opts.Logf != nil {
		user := opts.Logf
		logf = func(format string, args ...any) {
			lg.Logf(format, args...)
			user(format, args...)
		}
	}

	cfg, err := loadNodeConfig(dirs.Config)
	if err != nil {
		logf("config: %v", err)
		return err
	}
	switch opts.UIListen {
	case "":
	case "-":
		cfg.UIListen = ""
	default:
		cfg.UIListen = opts.UIListen
	}

	var st store.Store
	if s, err := store.Open(filepath.Join(dirs.State, "kran.db")); err != nil {
		// Jobs must keep running without history: degrade, don't die.
		logf("store: %v; keeping job history in memory until the database works again (check: kran doctor)", err)
		st = newMemStore()
	} else {
		st = s
	}
	defer st.Close()

	prober := metrics.New(metrics.Options{StateDir: dirs.State, Logf: logf})
	// The estimator's speed prior reads nodes' benchmark scores from the
	// daemon, which exists only after the estimator: late-bound.
	var dref atomic.Pointer[Daemon]
	est := estimate.NewWithOptions(st, estimate.Options{Self: cfg.Name, Logf: logf, Score: func(node string) float64 {
		if d := dref.Load(); d != nil {
			return d.singleCoreScore(node)
		}
		return 0
	}})
	sched := scheduler.New(scheduler.Options{Origin: cfg.Name, Speed: est.Speed, Now: time.Now, Logf: logf})

	// KRAN_NAMESPACE (development and test installs next to a real one)
	// prefixes the systemd slice and scopes and the Docker resources so the
	// two never touch each other's.
	ns, slice := namespace(logf)
	self, err := selfexe.Path()
	if err != nil {
		logf("execenv: %v", err)
	}
	var ex execenv.Manager
	if m, err := execenv.New(execenv.Options{
		StateDir: dirs.State, NodeName: cfg.Name, Self: self, Logf: logf, Namespace: ns,
		Container: cfg.ContainerEnabled, CacheBudget: cfg.CacheBudget,
		// Placed there by `kran link`/`kran upgrade`; the Docker VM runs the
		// host's architecture (Apple Silicon → linux/arm64).
		LinuxBinary: linuxBinary(),
	}); err != nil {
		logf("execenv: %v; this node will not run jobs for other nodes", err)
	} else {
		ex = m
	}
	var scanner workspace.Scanner
	if s, err := workspace.NewScanner(filepath.Join(dirs.State, "cache")); err != nil {
		logf("workspace: %v; jobs will run locally only", err)
	} else {
		scanner = s
		defer s.Close()
	}
	peerSrc := filePeerSource{path: filepath.Join(dirs.Config, "peers.toml")}
	sessCfg := transport.SessionConfig{Logf: logf}
	if ka, err := keepaliveFromEnv(os.Getenv("KRAN_KEEPALIVE")); err != nil {
		logf("KRAN_KEEPALIVE: %v; using the defaults", err)
	} else if ka != nil {
		sessCfg.Keepalive = *ka
	}
	var dialer PeerDialer
	if d, err := transport.NewDialer(filepath.Join(dirs.Config, "id_ed25519"), sessCfg); err != nil {
		logf("peers: %v; not connecting to other nodes", err)
	} else {
		dialer = sshDialer{d: d, save: peerSrc.savePreferred, logf: logf}
	}

	pctx, pcancel := context.WithCancel(context.Background())
	defer pcancel()
	go func() {
		if err := prober.Run(pctx); err != nil && pctx.Err() == nil {
			logf("metrics: %v", err)
		}
	}()

	d, err := New(Deps{
		Config:  cfg,
		Dirs:    dirs,
		Store:   st,
		Sched:   sched,
		Est:     est,
		Prober:  prober,
		Exec:    ex,
		Scanner: scanner,
		Peers:   peerSrc,
		Dialer:  dialer,
		UI:      ui.Handler(),
		Slice:   newSliceController(slice, cfg.Accept.ReserveMem, logf),
		Session: sessCfg,
		Logf:    logf,
		Debug:   os.Getenv("KRAN_DEBUG") == "1",

		Supervisor: supervisor(),
	})
	if err != nil {
		return err
	}
	dref.Store(d)
	return d.Run(ctx)
}

// CrashLogName is the file in the state dir that receives the daemon's
// stdout and stderr when a service manager or the on-demand start runs it.
// The daemon's own log is daemon.log; this one only gets what the daemon
// could not log there (a fatal start error, a panic).
const CrashLogName = "daemon.out.log"

// sshDialer adapts transport.Dialer to PeerDialer: the preferred address
// goes first, and a different winner is written back to peers.toml.
type sshDialer struct {
	d    *transport.Dialer
	save func(name, addr string) error
	logf func(string, ...any)
}

func (s sshDialer) Dial(ctx context.Context, p PeerConfig) (transport.Session, error) {
	addrs := orderAddresses(p.Addresses, p.PreferredAddress)
	sess, err := s.d.Dial(ctx, transport.Peer{Name: p.Name, Addresses: addrs, User: p.User, HostKey: p.HostKey})
	if err != nil {
		return nil, err
	}
	if w := s.d.Preferred(p.Name); w != "" && w != p.PreferredAddress && s.save != nil {
		if err := s.save(p.Name, w); err != nil {
			s.logf("peers: remember %s for %s: %v", w, p.Name, err)
		}
	}
	return sess, nil
}

// orderAddresses puts preferred first (if it is one of addrs).
func orderAddresses(addrs []string, preferred string) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a == preferred {
			out = append(out, a)
		}
	}
	for _, a := range addrs {
		if a != preferred {
			out = append(out, a)
		}
	}
	return out
}

// linuxBinary is the Linux kran for the container runtime's sync helper on
// macOS ("" when absent: execenv then reports the runtime unavailable).
func linuxBinary() string {
	if runtime.GOOS == "linux" {
		return ""
	}
	p := paths.BinaryCache(strings.TrimPrefix(version.Version, "v"), "linux", runtime.GOARCH)
	if _, err := os.Stat(p); err != nil {
		if p2 := paths.BinaryCache(version.Version, "linux", runtime.GOARCH); p2 != p {
			if _, err := os.Stat(p2); err == nil {
				return p2
			}
		}
	}
	return p
}

// supervisor names the service manager that started this process, from
// the environment it sets for its services ("" = on demand or by hand).
func supervisor() string {
	switch {
	case os.Getenv("INVOCATION_ID") != "":
		return "systemd"
	case strings.Contains(os.Getenv("XPC_SERVICE_NAME"), "kran"):
		return "launchd"
	}
	return ""
}

// keepaliveFromEnv parses KRAN_KEEPALIVE="<interval>,<dead after>,<max
// dead after>" (e.g. "1s,8s,20s"): link liveness timings for tests and for
// unusual networks. nil when unset.
func keepaliveFromEnv(v string) (*transport.KeepaliveConfig, error) {
	if v == "" {
		return nil, nil
	}
	parts := strings.Split(v, ",")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%q: want <interval>,<dead after>,<max dead after>", v)
	}
	var d [3]time.Duration
	for i, p := range parts {
		x, err := time.ParseDuration(strings.TrimSpace(p))
		if err != nil || x <= 0 {
			return nil, fmt.Errorf("%q: %q is not a positive duration", v, p)
		}
		d[i] = x
	}
	if d[2] < d[1] || d[1] < d[0] {
		return nil, fmt.Errorf("%q: want interval <= dead after <= max dead after", v)
	}
	return &transport.KeepaliveConfig{Interval: d[0], DeadAfter: d[1], MaxDeadAfter: d[2]}, nil
}

var namespaceRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// namespace reads KRAN_NAMESPACE: the execenv namespace and the matching
// slice ("" both = the defaults).
func namespace(logf func(string, ...any)) (ns, slice string) {
	ns = os.Getenv("KRAN_NAMESPACE")
	if ns == "" {
		return "", ""
	}
	if !namespaceRe.MatchString(ns) {
		logf("KRAN_NAMESPACE=%q is not a valid namespace (a-z, 0-9, -); using the default", ns)
		return "", ""
	}
	return ns, strings.ReplaceAll(ns, "-", "_") + ".slice"
}
