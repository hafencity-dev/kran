package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/term"

	"github.com/hafencity-dev/kran/internal/estimate"
	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/metrics"
	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/store"
	"github.com/hafencity-dev/kran/internal/transport"
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

	rot, err := newRotator(filepath.Join(dirs.State, "daemon.log"), 10<<20, 3)
	if err != nil {
		return fmt.Errorf("open daemon.log: %w", err)
	}
	defer rot.Close()
	lg := &logger{now: time.Now, out: []io.Writer{rot}}
	if term.IsTerminal(int(os.Stderr.Fd())) {
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

	cfg, err := loadNodeConfig(filepath.Join(dirs.Config, "config.toml"))
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
	est := estimate.New(st)
	sched := scheduler.New(scheduler.Options{Origin: cfg.Name, Speed: est.Speed, Now: time.Now, Logf: logf})

	self, _ := os.Executable()
	var ex execenv.Manager
	if m, err := execenv.New(execenv.Options{StateDir: dirs.State, NodeName: cfg.Name, Self: self, Logf: logf}); err != nil {
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
	var dialer PeerDialer
	if d, err := transport.NewDialer(filepath.Join(dirs.Config, "id_ed25519"), transport.SessionConfig{Logf: logf}); err != nil {
		logf("peers: %v; not connecting to other nodes", err)
	} else {
		dialer = sshDialer{d}
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
		Peers:   filePeerSource{path: filepath.Join(dirs.Config, "peers.toml")},
		Dialer:  dialer,
		UI:      nil, // lead: ui.Handler() once internal/ui lands
		Slice:   newSliceController(cfg.Accept.ReserveMem, logf),
		Session: transport.SessionConfig{Logf: logf},
		Logf:    logf,
	})
	if err != nil {
		return err
	}
	return d.Run(ctx)
}

// sshDialer adapts transport.Dialer to PeerDialer.
type sshDialer struct{ d *transport.Dialer }

func (s sshDialer) Dial(ctx context.Context, p PeerConfig) (transport.Session, error) {
	return s.d.Dial(ctx, transport.Peer{Name: p.Name, Addresses: p.Addresses, User: p.User, HostKey: p.HostKey})
}
