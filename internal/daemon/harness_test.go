package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/estimate"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/transport"
)

func shortDir(t testing.TB) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "kd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func testTiming() Timing {
	t := DefaultTiming()
	t.Heartbeat = 100 * time.Millisecond
	t.StreamSilence = 700 * time.Millisecond
	t.LeaseTTL = 1500 * time.Millisecond
	t.LeaseGrace = 300 * time.Millisecond
	// Long enough that a sync on a loaded machine never outlives its
	// reservation; TestReservationExpires shortens it.
	t.ReservationTTL = 15 * time.Second
	t.BreakerCooldown = 3 * time.Second
	t.PeerPoll = 50 * time.Millisecond
	t.BackoffMin = 50 * time.Millisecond
	t.BackoffMax = 300 * time.Millisecond
	t.MetricsEvery = 100 * time.Millisecond
	t.SliceEvery = time.Hour
	t.CLIReattachWait = 3 * time.Second
	t.SlotWait = 3 * time.Second
	t.PrestartTimeout = 10 * time.Second
	t.KillGrace = 300 * time.Millisecond
	t.CLIDropGrace = 2 * time.Second
	t.StdinWindow = 64 << 10
	t.StdinQueue = 256 << 10
	return t
}

func testSessionConfig() transport.SessionConfig {
	return transport.SessionConfig{Keepalive: transport.KeepaliveConfig{Interval: 100 * time.Millisecond, Timeout: 100 * time.Millisecond, Misses: 3}}
}

// safeLog drops lines after the test finished (stray goroutines).
type safeLog struct {
	mu     sync.Mutex
	t      testing.TB
	closed bool
}

func (l *safeLog) logf(prefix string) func(string, ...any) {
	return func(format string, args ...any) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if !l.closed {
			l.t.Logf("%s: %s", prefix, fmt.Sprintf(format, args...))
		}
	}
}

func (l *safeLog) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

type memDialer struct {
	net  *transport.MemNetwork
	from string
}

func (m memDialer) Dial(ctx context.Context, p PeerConfig) (transport.Session, error) {
	return m.net.Dial(ctx, m.from, p.Name)
}

type testNode struct {
	t       *testing.T
	name    string
	dirs    Dirs
	store   *memStore
	exec    *fakeExec
	prober  *fakeProber
	scanner *fakeScanner
	est     *fakeEst
	peers   *staticPeers
	place   placeFunc
	net     *transport.MemNetwork
	timing  Timing
	log     *safeLog
	cfg     NodeConfig
	dialer  PeerDialer // nil = in-memory network
	real    bool       // C's scheduler and estimator instead of fakes
	src     PeerSource // nil = peers
	// clientAlive overrides the liveness check of a dropped CLI's process
	// (nil: CLIs in this test process count as gone).
	clientAlive func(pid int) bool
	// logs, when set, also records every daemon log line.
	logs *logCapture
	// netSig overrides the network signature (nil: a constant).
	netSig func() string

	mu     sync.Mutex
	d      *Daemon
	sched  *fakeSched
	rsched scheduler.Scheduler
	cancel context.CancelFunc
	done   chan error
	// stopped is closed when the last stop finished; start waits for it,
	// so a restart never races the old daemon's socket.
	stopped chan struct{}
}

type cluster struct {
	t     *testing.T
	net   *transport.MemNetwork
	nodes map[string]*testNode
	log   *safeLog
}

func newCluster(t *testing.T, names ...string) *cluster {
	c := &cluster{t: t, net: transport.NewMemNetwork(testSessionConfig()), nodes: map[string]*testNode{}, log: &safeLog{t: t}}
	for _, name := range names {
		dir := shortDir(t)
		n := &testNode{
			t: t, name: name,
			dirs:    Dirs{Config: filepath.Join(dir, "c"), State: filepath.Join(dir, "s"), Runtime: filepath.Join(dir, "r")},
			store:   newMemStore(),
			prober:  newFakeProber(name),
			scanner: newFakeScanner(),
			est:     &fakeEst{},
			peers:   &staticPeers{},
			place:   alwaysLocal,
			net:     c.net,
			timing:  testTiming(),
			log:     c.log,
			cfg: NodeConfig{Name: name, Accept: AcceptConfig{Enabled: true, ReserveCPUs: 2, ReserveMem: 8 << 30, Nice: 10},
				Prefer: "auto"},
		}
		n.exec = newFakeExec(name, filepath.Join(dir, "x"), n.timing.LeaseTTL)
		c.nodes[name] = n
	}
	for _, n := range c.nodes {
		var ps []PeerConfig
		for _, o := range names {
			if o != n.name {
				ps = append(ps, PeerConfig{Name: o, ID: o + "-id", Addresses: []string{"mem"}, Direction: "both"})
			}
		}
		n.peers.set(ps)
	}
	t.Cleanup(func() {
		for _, n := range c.nodes {
			n.stop()
		}
		for _, n := range c.nodes {
			n.exec.killAll()
		}
		c.log.close()
	})
	return c
}

func (n *testNode) start() *Daemon {
	n.t.Helper()
	n.mu.Lock()
	stopped := n.stopped
	n.mu.Unlock()
	if stopped != nil {
		<-stopped
	}
	sched := newFakeSched(n.name, n.place)
	var dialer PeerDialer = memDialer{n.net, n.name}
	if n.dialer != nil {
		dialer = n.dialer
	}
	var src PeerSource = n.peers
	if n.src != nil {
		src = n.src
	}
	var sch scheduler.Scheduler = sched
	var est estimate.Estimator = n.est
	if n.real {
		est = estimate.NewWithOptions(n.store, estimate.Options{Self: n.name})
		sch = scheduler.New(scheduler.Options{Origin: n.name, Speed: est.Speed, Logf: n.log.logf(n.name + " sched")})
	}
	d, err := New(Deps{
		Config:  n.cfg,
		Dirs:    n.dirs,
		Store:   n.store,
		Sched:   sch,
		Est:     est,
		Prober:  n.prober,
		Exec:    n.exec,
		Scanner: n.scanner,
		Peers:   src,
		Dialer:  dialer,
		Session: testSessionConfig(),
		Logf:    n.logf(),
		Timing:  n.timing,

		ClientAlive:  n.clientAlive,
		NetSignature: n.netSignature(),
	})
	if err != nil {
		n.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	n.mu.Lock()
	n.d, n.sched, n.cancel, n.done = d, sched, cancel, done
	n.rsched = sch
	n.mu.Unlock()
	n.net.Listen(n.name, d.ServePeerConn)
	deadline := time.Now().Add(waitCeiling)
	for {
		if _, err := os.Stat(n.dirs.socket()); err == nil {
			break
		}
		select {
		case err := <-done:
			n.t.Fatalf("%s: daemon exited: %v", n.name, err)
		default:
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("%s: socket did not appear", n.name)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return d
}

func (n *testNode) logf() func(string, ...any) {
	base := n.log.logf(n.name)
	if n.logs == nil {
		return base
	}
	return func(format string, args ...any) {
		base(format, args...)
		n.logs.add(fmt.Sprintf(format, args...))
	}
}

func (n *testNode) netSignature() func() string {
	if n.netSig != nil {
		return n.netSig
	}
	return func() string { return "test" }
}

// logCapture records log lines for assertions.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) add(s string) {
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

// count returns how many lines contain sub.
func (l *logCapture) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func (l *logCapture) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// stop shuts the daemon down gracefully (supervisors keep running).
func (n *testNode) stop() {
	n.mu.Lock()
	cancel, done := n.cancel, n.done
	n.cancel = nil
	stopped := make(chan struct{})
	if cancel != nil {
		n.stopped = stopped
	}
	n.mu.Unlock()
	if cancel == nil {
		return
	}
	defer close(stopped)
	n.net.Unlisten(n.name)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			n.t.Errorf("%s: Run: %v", n.name, err)
		}
	case <-time.After(waitCeiling):
		n.t.Errorf("%s: daemon did not stop within %s", n.name, waitCeiling)
	}
	// TestGracefulShutdownIsFast holds the 5 s budget; here only a hang
	// fails, so a loaded machine does not make every test flaky.
	_ = start
	n.net.Cut(n.name)
}

func (n *testNode) daemon() *Daemon {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.d
}

func (n *testNode) scheduler() *fakeSched {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sched
}

func (n *testNode) sock() string { return n.dirs.socket() }

// waitFor polls cond until it holds or the deadline passes.
// waitFor polls cond until it holds. within is how long it normally takes;
// the test fails only after waitCeiling (longer under -race): on a loaded
// machine a correct daemon can be many times slower, and a deadline close
// to the usual time only makes the suite flaky. Waits for something that
// must not happen are not written with waitFor.
func waitFor(t testing.TB, within time.Duration, what string, cond func() bool) {
	t.Helper()
	limit := max(within, waitCeiling)
	start := time.Now()
	for !cond() {
		if time.Since(start) > limit {
			t.Fatalf("timed out after %s waiting for %s (usually takes < %s)", limit, what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// testContext is a deadline for a whole test scenario: at least d, and
// never less than waitCeiling.
func testContext(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), max(d, waitCeiling))
}

// waitCeiling bounds every waitFor; see raceEnabled.
var waitCeiling = func() time.Duration {
	if raceEnabled {
		return 60 * time.Second
	}
	return 30 * time.Second
}()

// waitOnline waits until origin sees peer online.
func (c *cluster) waitOnline(origin, peer string) {
	c.t.Helper()
	n := c.nodes[origin]
	waitFor(c.t, 10*time.Second, peer+" online at "+origin, func() bool {
		s := n.scheduler()
		ns, ok := s.node(peer)
		return ok && ns.Status == proto.NodeOnline
	})
}

func workspaceDir(t *testing.T, files map[string]string) proto.Workspace {
	dir := shortDir(t)
	for p, content := range files {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return proto.Workspace{Root: dir, RepoID: "repo-" + filepath.Base(dir), Name: filepath.Base(dir)}
}

func shSpec(ws proto.Workspace, script string) proto.JobSpec {
	return proto.JobSpec{
		Argv:      []string{"sh", "-c", script},
		Cwd:       ws.Root,
		Workspace: ws,
		Owner:     proto.Owner{Kind: proto.OwnerHuman, ID: "tester", Name: "tester"},
		Req:       proto.Requirements{OS: proto.OSSame, Prefer: proto.PreferAuto},
	}
}

// ---------------------------------------------------------------------------
// A minimal CLI: what `kran run` does with a run session.

type cliResult struct {
	mu        sync.Mutex
	jobID     string
	exit      *proto.Exit
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	notices   []proto.Notice
	decisions []proto.Decision
	phases    []proto.Phase
	offset    int64
}

func (r *cliResult) out() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stdout.String()
}

func (r *cliResult) errOut() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stderr.String()
}

func (r *cliResult) noticeKinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var k []string
	for _, n := range r.notices {
		k = append(k, n.Kind)
	}
	return k
}

type cliOpts struct {
	stdin    []byte
	reattach *proto.Reattach
	// hook sees every frame; returning an error ends the session (CLI dies).
	hook func(rs client.RunSession, t proto.FrameType, p []byte, r *cliResult) error
}

var errCLIDies = errors.New("cli killed")

func runCLI(ctx context.Context, sock string, spec proto.JobSpec, o cliOpts) (*cliResult, error) {
	res := &cliResult{}
	return res, runCLIInto(ctx, sock, spec, o, res)
}

func runCLIInto(ctx context.Context, sock string, spec proto.JobSpec, o cliOpts, res *cliResult) error {
	c, err := client.DialSocket(ctx, sock)
	if err != nil {
		return err
	}
	defer c.Close()
	rs, err := c.Run(ctx)
	if err != nil {
		return err
	}
	defer rs.Close()
	stop := context.AfterFunc(ctx, func() { rs.Close() })
	defer stop()
	if o.reattach != nil {
		err = rs.SendJSON(proto.FrameReattach, o.reattach)
	} else {
		err = rs.SendJSON(proto.FrameSubmit, spec)
	}
	if err != nil {
		return err
	}
	// Input as kran run sends it: within the credit, again from Resume.
	in := &harnessInput{data: o.stdin, rs: rs}
	if o.reattach != nil {
		in.started = true // nothing goes out before the daemon's credit
	}
	for {
		t, p, err := rs.Recv()
		if err != nil {
			return err
		}
		if o.stdin != nil {
			in.onFrame(t, p)
		}
		if o.hook != nil {
			if err := o.hook(rs, t, p, res); err != nil {
				return err
			}
		}
		res.mu.Lock()
		switch t {
		case proto.FrameNotice:
			var n proto.Notice
			json.Unmarshal(p, &n)
			res.notices = append(res.notices, n)
			res.jobID = n.JobID
		case proto.FramePhase:
			var ph proto.Phase
			json.Unmarshal(p, &ph)
			res.phases = append(res.phases, ph)
		case proto.FrameStdout:
			res.stdout.Write(p)
			res.offset += int64(len(p))
		case proto.FrameStderr:
			res.stderr.Write(p)
			res.offset += int64(len(p))
		case proto.FrameError:
			res.mu.Unlock()
			var ae proto.APIError
			json.Unmarshal(p, &ae)
			return errors.New(ae.Error)
		case proto.FrameExit:
			var x proto.Exit
			json.Unmarshal(p, &x)
			res.exit = &x
			res.mu.Unlock()
			return nil
		case proto.FrameDecision:
			var dc proto.Decision
			json.Unmarshal(p, &dc)
			res.decisions = append(res.decisions, dc)
			res.jobID = dc.JobID
			res.mu.Unlock()
			if dc.Mode == "local" {
				if err := runLocalChild(rs, spec, dc, res); err != nil {
					return err
				}
			}
			continue
		}
		res.mu.Unlock()
	}
}

// harnessInput is the test CLI's stdin: everything is kept, so any resume
// can be served.
type harnessInput struct {
	data    []byte
	rs      client.RunSession
	sent    int
	limit   int64 // -1: no limit
	started bool
	eofSent bool
}

func (in *harnessInput) onFrame(t proto.FrameType, p []byte) {
	switch t {
	case proto.FrameDecision:
		var dc proto.Decision
		json.Unmarshal(p, &dc)
		if dc.Mode != "remote" || in.started {
			return
		}
		in.started = true
		in.limit = dc.StdinLimit
		if in.limit == 0 {
			in.limit = -1 // a daemon without flow control
		}
	case proto.FrameStdinCredit:
		var c proto.StdinCredit
		json.Unmarshal(p, &c)
		in.limit = c.Limit
		if c.Resume != nil {
			in.sent = int(*c.Resume)
			in.eofSent = false
			in.rs.SendJSON(proto.FrameStdinResume, proto.StdinResume{Offset: *c.Resume})
		}
	default:
		return
	}
	for in.sent < len(in.data) && (in.limit < 0 || int64(in.sent) < in.limit) {
		end := min(len(in.data), in.sent+32<<10)
		if in.limit >= 0 {
			end = min(end, int(in.limit))
		}
		if in.rs.Send(proto.FrameStdin, in.data[in.sent:end]) != nil {
			return
		}
		in.sent = end
	}
	if in.sent == len(in.data) && !in.eofSent {
		in.eofSent = true
		in.rs.Send(proto.FrameStdinEOF, nil)
	}
}

// runLocalChild runs the job as the CLI would for a local decision.
func runLocalChild(rs client.RunSession, spec proto.JobSpec, dc proto.Decision, res *cliResult) error {
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Cwd
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return rs.SendJSON(proto.FrameExit, proto.Exit{Code: 127})
	}
	pgid, _ := syscall.Getpgid(cmd.Process.Pid)
	if err := rs.SendJSON(proto.FrameStarted, proto.Started{JobID: dc.JobID, PID: cmd.Process.Pid, PGID: pgid}); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	err := cmd.Wait()
	res.mu.Lock()
	res.stdout.Write(so.Bytes())
	res.stderr.Write(se.Bytes())
	res.mu.Unlock()
	// Like kran run: a copy of the output for the history, before the exit.
	for _, f := range []struct {
		t proto.FrameType
		b []byte
	}{{proto.FrameStdout, so.Bytes()}, {proto.FrameStderr, se.Bytes()}} {
		if len(f.b) > 0 {
			if err := rs.Send(f.t, f.b); err != nil {
				return err
			}
		}
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	return rs.SendJSON(proto.FrameExit, proto.Exit{JobID: dc.JobID, Code: code, Usage: proto.Usage{WallSeconds: 0.1}})
}

// reachedLine reports whether the output so far plus frame p contains the
// line want. Frames coalesce under load ("2\n3\n4\n"), so checking a
// frame's prefix can miss the line and the disruption never happens.
func reachedLine(r *cliResult, p []byte, want string) bool {
	return strings.Contains("\n"+r.out()+string(p), "\n"+want+"\n")
}

func storedJob(t *testing.T, n *testNode, id string) proto.Job {
	t.Helper()
	j, err := n.store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("job %s not stored: %v", id, err)
	}
	return *j
}

var _ = io.EOF

type netConn = net.Conn

func jsonUnmarshal(b []byte, v any) { json.Unmarshal(b, v) }
