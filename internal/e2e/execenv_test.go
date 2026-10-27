package e2e

// End-to-end runs through two real daemons in one process, connected by
// the in-memory transport, with the real execenv (supervisor processes,
// slots, blob store), workspace scanner, scheduler, estimator, store and
// metrics. Only SSH is replaced.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/daemon"
	"github.com/hafencity-dev/kran/internal/estimate"
	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/metrics"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/store"
	"github.com/hafencity-dev/kran/internal/transport"
	"github.com/hafencity-dev/kran/internal/workspace"
)

type staticPeers []daemon.PeerConfig

func (p staticPeers) Stamp() string                      { return "1" }
func (p staticPeers) Load() ([]daemon.PeerConfig, error) { return p, nil }

type memDialer struct {
	net  *transport.MemNetwork
	from string
}

func (m memDialer) Dial(ctx context.Context, p daemon.PeerConfig) (transport.Session, error) {
	return m.net.Dial(ctx, m.from, p.Name)
}

type logSink struct {
	mu     sync.Mutex
	lines  []string
	closed bool
}

func (l *logSink) logf(prefix string) func(string, ...any) {
	return func(f string, a ...any) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if !l.closed {
			l.lines = append(l.lines, time.Now().Format("15:04:05.000 ")+prefix+": "+fmt.Sprintf(f, a...))
		}
	}
}

type node struct {
	name   string
	dirs   daemon.Dirs
	exec   execenv.Manager
	d      *daemon.Daemon
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	stopFn func()
}

// stop shuts the daemon down gracefully; supervisors keep running.
func (n *node) stop() { n.once.Do(n.stopFn) }

func startNode(t *testing.T, net *transport.MemNetwork, log *logSink, name, peer, base string) *node {
	t.Helper()
	dirs := daemon.Dirs{Config: filepath.Join(base, name, "c"), State: filepath.Join(base, name, "s"), Runtime: filepath.Join(base, name, "r")}
	for _, d := range []string{dirs.Config, dirs.State, dirs.Runtime} {
		os.MkdirAll(d, 0o700)
	}
	self, _ := os.Executable()
	logf := log.logf(name)
	// DiskFloor/DiskReserve: the host disk (a CI runner may have a few GiB free) must not decide.
	ex, err := execenv.New(execenv.Options{StateDir: dirs.State, NodeName: name, Self: self, Namespace: "kran-test", Logf: log.logf(name + " exec"), DiskFloor: 1, DiskReserve: 1})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := workspace.NewScanner(filepath.Join(dirs.State, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dirs.State, "kran.db"))
	if err != nil {
		t.Fatal(err)
	}
	est := estimate.NewWithOptions(st, estimate.Options{Self: name})
	timing := daemon.DefaultTiming()
	timing.Heartbeat = 200 * time.Millisecond
	timing.PeerPoll = 100 * time.Millisecond
	timing.MetricsEvery = 200 * time.Millisecond
	timing.BackoffMin = 50 * time.Millisecond
	timing.BackoffMax = 300 * time.Millisecond
	d, err := daemon.New(daemon.Deps{
		Config: daemon.NodeConfig{Name: name, ID: name + "-id", Prefer: "auto",
			Accept: daemon.AcceptConfig{Enabled: true, ReserveCPUs: 1, ReserveMem: 1 << 30, Nice: 10}},
		Dirs:    dirs,
		Store:   st,
		Sched:   scheduler.New(scheduler.Options{Origin: name, Speed: est.Speed, Logf: log.logf(name + " sched")}),
		Est:     est,
		Prober:  metrics.New(metrics.Options{StateDir: dirs.State}),
		Exec:    ex,
		Scanner: sc,
		Peers:   staticPeers{{Name: peer, ID: peer + "-id", Addresses: []string{"mem"}, Direction: "both"}},
		Dialer:  memDialer{net, name},
		Logf:    logf,
		Timing:  timing,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	net.Listen(name, d.ServePeerConn)
	n := &node{name: name, dirs: dirs, exec: ex, d: d, cancel: cancel, done: done}
	n.stopFn = func() {
		net.Unlisten(name)
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("%s did not stop", name)
		}
		net.Cut(name)
		sc.Close()
		st.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dirs.Runtime, "kran.sock")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: socket did not appear", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(n.stop)
	return n
}

type runResult struct {
	exit           *proto.Exit
	stdout, stderr strings.Builder
	decisions      []proto.Decision
	phases         []proto.Phase
	errFrame       string
}

func runJob(ctx context.Context, sock string, spec proto.JobSpec) (*runResult, error) {
	c, err := client.DialSocket(ctx, sock)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	rs, err := c.Run(ctx)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	stop := context.AfterFunc(ctx, func() { rs.Close() })
	defer stop()
	if err := rs.SendJSON(proto.FrameSubmit, spec); err != nil {
		return nil, err
	}
	res := &runResult{}
	for {
		t, p, err := rs.Recv()
		if err != nil {
			return res, err
		}
		switch t {
		case proto.FrameDecision:
			var d proto.Decision
			json.Unmarshal(p, &d)
			res.decisions = append(res.decisions, d)
		case proto.FramePhase:
			var ph proto.Phase
			json.Unmarshal(p, &ph)
			res.phases = append(res.phases, ph)
		case proto.FrameStdout:
			res.stdout.Write(p)
		case proto.FrameStderr:
			res.stderr.Write(p)
		case proto.FrameError:
			res.errFrame = string(p)
			return res, fmt.Errorf("error frame: %s", p)
		case proto.FrameExit:
			var e proto.Exit
			json.Unmarshal(p, &e)
			res.exit = &e
			return res, nil
		case proto.FrameHeartbeat:
			rs.Send(proto.FrameHeartbeat, nil)
		}
	}
}

func gitRepo(t *testing.T, base string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(base, "repo")
	for p, c := range files {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@e", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return root
}

func TestRemoteJobThroughRealExecenv(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "kran-test-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	os.Setenv("KRAN_CACHE_DIR", filepath.Join(base, "cache"))
	log := &logSink{}
	defer func() {
		log.mu.Lock()
		if t.Failed() {
			t.Logf("daemon logs:\n%s", strings.Join(log.lines, "\n"))
		}
		log.closed = true
		log.mu.Unlock()
	}()
	net := transport.NewMemNetwork(transport.SessionConfig{})
	b := startNode(t, net, log, "b", "a", base)
	a := startNode(t, net, log, "a", "b", base)

	root := gitRepo(t, base, map[string]string{
		"data/in.txt": "payload\n",
		"bin/tool.sh": "#!/bin/sh\necho tool ran\n",
		".env":        "SECRET=never-synced\n",
	})
	os.Chmod(filepath.Join(root, "bin/tool.sh"), 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ws, err := workspace.Detect(ctx, filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.JobSpec{
		Argv:      []string{"sh", "-c", `cat in.txt; ../bin/tool.sh; test -e ../.env && echo LEAKED; echo "node=$KRAN_NODE origin=$KRAN_ORIGIN"; echo to-stderr >&2; exit 3`},
		Cwd:       filepath.Join(root, "data"),
		Workspace: ws,
		Owner:     proto.Owner{Kind: proto.OwnerHuman, ID: "tester", Name: "tester"},
		Req:       proto.Requirements{OS: proto.OSSame, Prefer: proto.PreferRemote, Node: "b", CPUs: 1, Mem: 256 << 20},
		Priority:  proto.PriorityNormal,
		Env:       map[string]string{"FOO": "bar"},
	}

	// Wait until a sees b with its runtimes.
	var res *runResult
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err = runJob(ctx, filepath.Join(a.dirs.Runtime, "kran.sock"), spec)
		if err == nil && res.exit != nil && res.exit.Node == "b" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote run: %v %+v", err, res)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if res.exit.Code != 3 || res.exit.State != proto.JobFailed {
		t.Fatalf("exit %+v", res.exit)
	}
	out := res.stdout.String()
	for _, want := range []string{"payload\n", "tool ran\n", "node=b origin=a"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "LEAKED") {
		t.Error(".env was synced")
	}
	if res.stderr.String() != "to-stderr\n" {
		t.Errorf("stderr %q", res.stderr.String())
	}
	sawRunning := false
	for _, ph := range res.phases {
		sawRunning = sawRunning || ph.State == proto.JobRunning
	}
	if !sawRunning {
		t.Errorf("no running phase: %+v", res.phases)
	}
	if len(b.exec.Active()) != 0 {
		t.Errorf("b still has active jobs: %v", b.exec.Active())
	}

	// Second run: blobs are cached on b, the slot is reused, exit 0.
	spec.Argv = []string{"sh", "-c", "cat in.txt"}
	res, err = runJob(ctx, filepath.Join(a.dirs.Runtime, "kran.sock"), spec)
	if err != nil || res.exit == nil || res.exit.Code != 0 || res.exit.Node != "b" || res.stdout.String() != "payload\n" {
		t.Fatalf("second run: %v %+v %q", err, res.exit, res.stdout.String())
	}
}

func TestTargetRestartMidJob(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "kran-test-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	os.Setenv("KRAN_CACHE_DIR", filepath.Join(base, "cache"))
	log := &logSink{}
	defer func() {
		log.mu.Lock()
		if t.Failed() {
			t.Logf("daemon logs:\n%s", strings.Join(log.lines, "\n"))
		}
		log.closed = true
		log.mu.Unlock()
	}()
	net := transport.NewMemNetwork(transport.SessionConfig{})
	b := startNode(t, net, log, "b", "a", base)
	a := startNode(t, net, log, "a", "b", base)
	root := gitRepo(t, base, map[string]string{"x": "x"})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ws, err := workspace.Detect(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.JobSpec{
		Argv:      []string{"sh", "-c", `i=0; while [ $i -lt 40 ]; do echo "line $i"; i=$((i+1)); sleep 0.1; done`},
		Cwd:       root,
		Workspace: ws,
		Owner:     proto.Owner{Kind: proto.OwnerHuman, ID: "tester", Name: "tester"},
		Req:       proto.Requirements{OS: proto.OSSame, Prefer: proto.PreferRemote, Node: "b", CPUs: 1, Mem: 256 << 20},
		Priority:  proto.PriorityNormal,
	}
	// Restart b's daemon once the job is running there.
	go func() {
		for ctx.Err() == nil {
			if len(b.exec.Active()) > 0 {
				time.Sleep(500 * time.Millisecond)
				b.stop()
				startNode(t, net, log, "b", "a", base)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	var res *runResult
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err = runJob(ctx, filepath.Join(a.dirs.Runtime, "kran.sock"), spec)
		if err == nil && res.exit != nil && res.exit.Node == "b" {
			break
		}
		if time.Now().After(deadline) || res != nil && len(res.decisions) > 0 {
			t.Fatalf("remote run: %v %+v", err, res)
		}
		time.Sleep(300 * time.Millisecond)
	}
	var want strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&want, "line %d\n", i)
	}
	if res.exit.Code != 0 || res.stdout.String() != want.String() {
		t.Fatalf("exit %+v, output complete=%v:\n%s", res.exit, res.stdout.String() == want.String(), res.stdout.String())
	}
	log.mu.Lock()
	reattached := strings.Contains(strings.Join(log.lines, "\n"), "re-attached running job")
	log.mu.Unlock()
	if !reattached {
		t.Fatal("b was not restarted while the job ran")
	}
}
