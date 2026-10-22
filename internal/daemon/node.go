package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hafencity-dev/kran/internal/estimate"
	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/metrics"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/store"
	"github.com/hafencity-dev/kran/internal/transport"
	"github.com/hafencity-dev/kran/internal/version"
	"github.com/hafencity-dev/kran/internal/workspace"
)

// PeerDialer opens a session to a peer daemon.
type PeerDialer interface {
	Dial(ctx context.Context, p PeerConfig) (transport.Session, error)
}

// Dirs are the node's directories (SPEC §4).
type Dirs struct {
	Config  string
	State   string
	Runtime string
}

func (d Dirs) socket() string     { return filepath.Join(d.Runtime, "kran.sock") }
func (d Dirs) peerSocket() string { return filepath.Join(d.Runtime, "kran-peer.sock") }
func (d Dirs) tokenFile() string  { return filepath.Join(d.State, "ui.token") }
func (d Dirs) spoolDir() string   { return filepath.Join(d.State, "spool") }

// Timing holds every interval the daemon uses; tests shrink them.
type Timing struct {
	Heartbeat        time.Duration // origin↔target and daemon→CLI heartbeats (2 s)
	StreamSilence    time.Duration // a job stream silent this long is dead (10 s)
	LeaseTTL         time.Duration // target lease (execenv.LeaseTTL)
	LeaseGrace       time.Duration // extra time before giving up a reattach (5 s)
	ReservationTTL   time.Duration // admit without job.start expires (30 s)
	BreakerThreshold int           // consecutive pre-start failures (3)
	BreakerCooldown  time.Duration // degraded period (60 s)
	PeerPoll         time.Duration // peers.toml mtime poll (2 s)
	BackoffMin       time.Duration // reconnect backoff (1 s)
	BackoffMax       time.Duration // (20 s)
	UnreachableLog   time.Duration // "still unreachable" summary while a peer keeps failing (10 min)
	MetricsEvery     time.Duration // self/peer status (2 s)
	SliceEvery       time.Duration // kran.slice refresh (20 s)
	CLIReattachWait  time.Duration // restored job waits for its CLI (20 s)
	CLIDropGrace     time.Duration // a live CLI that dropped its session may reattach (12 s)
	StdinWindow      int64         // input in flight to a remote job, kran run to target (8 MiB)
	StdinQueue       int64         // input a target holds for a job before refusing more (64 MiB)
	ClockEvery       time.Duration // peer clock offset measurement (5 min)
	SlotWait         time.Duration // waiting for a free slot on a target (60 s)
	PrestartTimeout  time.Duration // one remote prestart RPC (2 min)
	KillGrace        time.Duration // TERM→KILL (5 s)
	ShutdownTimeout  time.Duration // graceful shutdown bound (5 s)
}

// DefaultTiming are the production values.
func DefaultTiming() Timing {
	return Timing{
		Heartbeat:        2 * time.Second,
		StreamSilence:    10 * time.Second,
		LeaseTTL:         execenv.LeaseTTL,
		LeaseGrace:       5 * time.Second,
		ReservationTTL:   30 * time.Second,
		BreakerThreshold: 3,
		BreakerCooldown:  60 * time.Second,
		PeerPoll:         2 * time.Second,
		BackoffMin:       time.Second,
		BackoffMax:       20 * time.Second,
		UnreachableLog:   10 * time.Minute,
		MetricsEvery:     2 * time.Second,
		SliceEvery:       20 * time.Second,
		CLIReattachWait:  20 * time.Second,
		CLIDropGrace:     12 * time.Second,
		StdinWindow:      8 << 20,
		StdinQueue:       64 << 20,
		ClockEvery:       5 * time.Minute,
		SlotWait:         60 * time.Second,
		PrestartTimeout:  2 * time.Minute,
		KillGrace:        5 * time.Second,
		ShutdownTimeout:  5 * time.Second,
	}
}

// Deps are the daemon's collaborators. Every interface may be faked.
type Deps struct {
	Config  NodeConfig
	Dirs    Dirs
	Store   store.Store         // required (use the memory store when the db is unavailable)
	Sched   scheduler.Scheduler // required
	Est     estimate.Estimator  // required
	Prober  metrics.Prober      // required
	Exec    execenv.Manager     // nil: this node accepts no remote jobs
	Scanner workspace.Scanner   // nil: remote placement fails over to local
	Peers   PeerSource          // nil: no peers
	Dialer  PeerDialer          // nil: no outbound connections
	UI      http.Handler        // nil: placeholder page
	Slice   SliceController     // nil: no kran.slice management
	// Session configures server-side peer sessions (keepalives).
	Session transport.SessionConfig
	// Debug serves GET /api/v1/debug/goroutines on the unix socket
	// (KRAN_DEBUG=1; used by the chaos suite).
	Debug bool
	// KillLocal kills a local job's process tree (default: scope or process
	// group kill with KillGrace).
	KillLocal func(pid, pgid int, scope string)
	// ClientAlive reports whether the process of a CLI that dropped its run
	// session still exists (nil: signal 0 to its pid).
	ClientAlive func(pid int) bool
	// Supervisor names the service manager running this daemon ("" = on
	// demand or by hand); reported by GET /api/v1/daemon.
	Supervisor string
	// NoListen skips the unix sockets and the UI listener (in-process tests
	// that drive Handler and ServePeerConn directly).
	NoListen bool
	// NetSignature summarizes this machine's network addresses; a change
	// makes disconnected peers retry at once (nil: the real interfaces).
	NetSignature func() string
	Now          func() time.Time
	Logf         func(format string, args ...any)
	Timing       Timing
}

// Daemon is one node's daemon.
type Daemon struct {
	deps   Deps
	cfg    NodeConfig
	self   string
	t      Timing
	now    func() time.Time
	logf   func(format string, args ...any)
	bus    *bus
	ids    idGen
	jobs   *jobTable
	peers  *peerManager
	target *targetSide
	// manifests: what this origin synced where (peer protocol 1.1).
	manifests *originManifests
	stats     *statsTracker
	history   metricsHistory
	token     string

	draining atomic.Bool

	startedAt time.Time
	stopOnce  sync.Once
	stopReq   chan struct{} // closed when asked to stop (handover to a managed daemon)

	ctx      context.Context // daemon lifetime
	cancel   context.CancelFunc
	stopping atomic.Bool // graceful shutdown in progress: CLI drops are not cancels
	wg       sync.WaitGroup

	connMu   sync.Mutex
	conns    map[net.Conn]struct{} // hijacked run sessions and peer conns
	sessions map[transport.Session]struct{}

	infoMu   sync.Mutex
	info     proto.NodeInfo
	infoTime time.Time

	unixHandler http.Handler
	tcpHandler  http.Handler
	uiAddr      string
}

// New builds a daemon; Run starts it.
func New(deps Deps) (*Daemon, error) {
	if deps.Store == nil || deps.Sched == nil || deps.Est == nil || deps.Prober == nil {
		return nil, errors.New("daemon: store, scheduler, estimator and prober are required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	if deps.Timing.Heartbeat == 0 {
		deps.Timing = DefaultTiming()
	}
	if deps.Timing.UnreachableLog <= 0 {
		deps.Timing.UnreachableLog = DefaultTiming().UnreachableLog
	}
	if deps.NetSignature == nil {
		deps.NetSignature = netSignature
	}
	if deps.Config.Name == "" {
		deps.Config.Name = defaultNodeName()
	}
	d := &Daemon{
		deps:     deps,
		cfg:      deps.Config,
		self:     deps.Config.Name,
		t:        deps.Timing,
		now:      deps.Now,
		logf:     deps.Logf,
		conns:    map[net.Conn]struct{}{},
		sessions: map[transport.Session]struct{}{},
		stopReq:  make(chan struct{}),
	}
	if d.deps.ClientAlive == nil {
		self := os.Getpid()
		// A client inside this process (tests) is never waited for.
		d.deps.ClientAlive = func(pid int) bool { return pid != self && processAlive(pid) }
	}
	if d.deps.KillLocal == nil {
		d.deps.KillLocal = func(pid, pgid int, scope string) { killLocal(pid, pgid, scope, d.t.KillGrace, d.logf) }
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.bus = newBus(d.now)
	d.jobs = newJobTable(d)
	d.stats = newStatsTracker(d.now)
	d.target = newTargetSide(d)
	d.manifests = newOriginManifests()
	d.peers = newPeerManager(d)
	d.unixHandler = d.routes(true)
	d.tcpHandler = d.routes(false)
	return d, nil
}

// Name is the node name.
func (d *Daemon) Name() string { return d.self }

// Run serves until ctx ends, then shuts down gracefully.
func (d *Daemon) Run(ctx context.Context) error {
	defer d.cancel()
	d.startedAt = d.now()
	if err := os.MkdirAll(d.deps.Dirs.spoolDir(), 0o700); err != nil {
		return fmt.Errorf("create spool dir: %w", err)
	}
	if err := d.loadToken(); err != nil {
		return err
	}
	d.loadNodeID()
	d.loadDrains()
	d.refreshInfo(true)

	// Restore before listening: a CLI or origin reattaching right after a
	// restart must find its job.
	d.stats.rebuild(d)
	d.jobs.loadRecent()
	d.target.restore()
	d.jobs.restore()

	// Every linked peer is known to the scheduler (offline until it
	// connects) before the first job arrives, so --node and --gpu checks
	// see the whole mesh.
	if d.deps.Peers != nil {
		d.peers.reload()
	}

	// The scheduler knows this node's capacity before the first admit or
	// submit arrives (otherwise it would see "0 B free" until selfLoop's
	// first pass).
	d.publishSelf()

	var listeners []func()
	if !d.deps.NoListen {
		stop, err := d.listen()
		if err != nil {
			d.cancel()
			d.wg.Wait()
			return err
		}
		listeners = stop
	}

	d.goLoop(d.selfLoop)
	d.goLoop(d.target.sweepLoop)
	d.goLoop(d.peers.run)
	d.goLoop(d.janitorLoop)
	if d.deps.Slice != nil {
		d.goLoop(d.sliceLoop)
	}
	if d.deps.Exec != nil {
		d.goLoop(func(ctx context.Context) {
			if err := d.deps.Exec.Run(ctx); err != nil && ctx.Err() == nil {
				d.logf("execenv: background loops ended: %v", err)
			}
		})
	}
	d.logf("kran daemon %s started on %s (protocol %d.%d)", version.Version, d.self, proto.ProtocolMajor, proto.ProtocolMinor)

	select {
	case <-ctx.Done():
	case <-d.stopReq:
		d.logf("stopping as asked (a managed daemon takes over)")
	}
	d.shutdown(listeners)
	return nil
}

func (d *Daemon) goLoop(fn func(ctx context.Context)) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		fn(d.ctx)
	}()
}

// shutdown stops accepting, lets detached supervisors and CLI-owned local
// jobs run on, persists what is active and returns within ShutdownTimeout.
func (d *Daemon) shutdown(listeners []func()) {
	d.logf("shutting down")
	d.stopping.Store(true)
	for _, stop := range listeners {
		stop()
	}
	d.jobs.persistAll()
	d.cancel()
	d.bus.closeAll()
	d.connMu.Lock()
	for c := range d.conns {
		c.Close()
	}
	for s := range d.sessions {
		s.Close()
	}
	d.connMu.Unlock()
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d.t.ShutdownTimeout - 500*time.Millisecond):
		d.logf("shutdown: some goroutines did not finish in time")
	}
	d.jobs.closeSpools()
	d.logf("stopped")
}

func (d *Daemon) trackConn(c net.Conn) func() {
	d.connMu.Lock()
	d.conns[c] = struct{}{}
	d.connMu.Unlock()
	return func() {
		d.connMu.Lock()
		delete(d.conns, c)
		d.connMu.Unlock()
	}
}

func (d *Daemon) trackSession(s transport.Session) func() {
	d.connMu.Lock()
	d.sessions[s] = struct{}{}
	d.connMu.Unlock()
	return func() {
		d.connMu.Lock()
		delete(d.sessions, s)
		d.connMu.Unlock()
	}
}

func (d *Daemon) clientAlive(pid int) bool { return d.deps.ClientAlive(pid) }

func (d *Daemon) storeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func (d *Daemon) kvGet(key string) (string, bool) {
	ctx, cancel := d.storeCtx()
	defer cancel()
	v, ok, err := d.deps.Store.Get(ctx, key)
	if err != nil {
		d.logf("store: get %s: %v", key, err)
		return "", false
	}
	return v, ok
}

func (d *Daemon) kvSet(key, value string) {
	ctx, cancel := d.storeCtx()
	defer cancel()
	if err := d.deps.Store.Set(ctx, key, value); err != nil {
		d.logf("store: set %s: %v", key, err)
	}
}

func (d *Daemon) loadNodeID() {
	if d.cfg.ID != "" {
		return
	}
	if v, ok := d.kvGet("node.id"); ok && v != "" {
		d.cfg.ID = v
		return
	}
	d.cfg.ID = randomHex(16)
	d.kvSet("node.id", d.cfg.ID)
}

// refreshInfo re-probes static info (every 10 minutes, or when forced).
// While a runtime is unavailable it re-probes every 30 s instead: the
// usual causes (Docker or Colima not started yet, the Linux kran binary
// arriving with `kran link`) are fixed from outside, and the node should
// offer the runtime again right away rather than up to 10 minutes later.
func (d *Daemon) refreshInfo(force bool) proto.NodeInfo {
	d.infoMu.Lock()
	fresh := !force && !d.infoTime.IsZero() && d.now().Sub(d.infoTime) < infoTTL(d.info.Runtimes)
	info := d.info
	d.infoMu.Unlock()
	if fresh {
		return info
	}
	ctx, cancel := context.WithTimeout(d.lifetime(), 30*time.Second)
	defer cancel()
	if !force {
		if err := d.deps.Prober.Refresh(ctx); err != nil {
			d.logf("metrics: refresh: %v", err)
		}
	}
	ni, err := d.deps.Prober.Info(ctx)
	if err != nil {
		d.logf("metrics: node info: %v", err)
	}
	ni.ID = d.cfg.ID
	ni.Name = d.self
	ni.Version = version.Version
	if d.deps.Exec != nil {
		if !force {
			if err := d.deps.Exec.Refresh(ctx); err != nil {
				d.logf("execenv: refresh: %v", err)
			}
		}
		ni.Runtimes = d.deps.Exec.Runtimes(ctx)
	}
	d.infoMu.Lock()
	d.info = ni
	d.infoTime = d.now()
	d.infoMu.Unlock()
	return ni
}

// infoTTL is how long probed node info stays fresh (see refreshInfo).
func infoTTL(rts []proto.Runtime) time.Duration {
	for _, rt := range rts {
		if !rt.Available {
			return 30 * time.Second
		}
	}
	return 10 * time.Minute
}

func (d *Daemon) nodeInfo() proto.NodeInfo {
	d.infoMu.Lock()
	info := d.info
	d.infoMu.Unlock()
	if d.deps.Exec != nil && info.Runtimes != nil {
		// Cheap (execenv caches them) and changes between refreshes, e.g.
		// when the first container job has built the runtime image.
		info.Runtimes = d.deps.Exec.Runtimes(d.lifetime())
	}
	return info
}

// lifetime is the daemon context: canceled at shutdown.
func (d *Daemon) lifetime() context.Context { return d.ctx }

func (d *Daemon) janitorLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	d.janitor()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.janitor()
		}
	}
}

// notable logs a line and shows it in the UI's event feed.
func (d *Daemon) notable(level, node, jobID, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	d.logf("%s", msg)
	d.bus.publish(proto.EventLog, proto.LogEvent{Level: level, Msg: msg, Time: d.now().UTC(), Node: node, JobID: jobID})
}

// singleCoreScore is a node's benchmark score (0 when unknown).
func (d *Daemon) singleCoreScore(node string) float64 {
	if node == d.self {
		return d.nodeInfo().SingleCoreScore
	}
	if p := d.peers.get(node); p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.st.Info.SingleCoreScore
	}
	return 0
}

// janitor prunes history and deletes spools older than 7 days.
func (d *Daemon) janitor() {
	ctx, cancel := context.WithTimeout(d.lifetime(), time.Minute)
	if n, err := d.deps.Store.PruneJobs(ctx, d.now().Add(-7*24*time.Hour)); err != nil {
		d.logf("store: prune: %v", err)
	} else if n > 0 {
		d.logf("store: pruned %d jobs older than 7 days", n)
	}
	cancel()
	dir := d.deps.Dirs.spoolDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := d.now().Add(-7 * 24 * time.Hour)
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if d.jobs.get(e.Name()) != nil {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}
