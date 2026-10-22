//go:build e2e || chaos || smoke

package e2e

// Black-box harness: every node is a real `kran daemon` process with its
// own KRAN_{CONFIG,STATE,RUNTIME,CACHE}_DIR. Nodes reach each other through
// an in-process SSH server per node whose forced command execs the real
// `kran serve --stdio`, behind a TCP proxy that can cut or stall the link.
// Tests drive the mesh through the kran CLI exactly as a user would.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

// nodeOpts is a node's config.toml.
type nodeOpts struct {
	MaxJobs     int
	ReserveCPUs int
	ReserveMem  string // default 1GiB
	NoAccept    bool
	Container   string // auto | true | false (default false)
	Prefer      string // default auto
	// StateDir overrides where the node keeps its state (disk floor test).
	StateDir string
	// Env is added to the daemon's environment.
	Env []string
	// Degraded: the node is expected to be degraded (disk floor test).
	Degraded bool
}

type cluster struct {
	t     *testing.T
	base  string
	nodes map[string]*knode
	names []string
	// raceSeen are race report files that existed before this test.
	raceSeen map[string]bool
}

type knode struct {
	c                                  *cluster
	name, id                           string
	dir                                string
	cfgDir, stateDir, runDir, cacheDir string
	opts                               nodeOpts
	signer                             ssh.Signer
	sshd                               *transport.SSHServer
	proxy                              *faultProxy

	mu     sync.Mutex
	proc   *exec.Cmd
	exited chan struct{}
	starts int
}

// newCluster starts one daemon per name, fully meshed, and waits until
// every node sees every other one online.
func newCluster(t *testing.T, names []string, opts map[string]nodeOpts) *cluster {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "kran-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	c := &cluster{t: t, base: base, nodes: map[string]*knode{}, names: names, raceSeen: map[string]bool{}}
	for _, f := range listDir(raceDir) {
		c.raceSeen[f] = true
	}
	t.Cleanup(c.teardown)
	for _, name := range names {
		c.nodes[name] = c.newNode(name, opts[name])
	}
	for _, n := range c.nodes {
		n.writePeers()
	}
	for _, name := range names {
		c.nodes[name].start()
	}
	c.waitMesh(90 * time.Second)
	return c
}

func (c *cluster) node(name string) *knode { return c.nodes[name] }

func (c *cluster) newNode(name string, o nodeOpts) *knode {
	t := c.t
	n := &knode{c: c, name: name, opts: o, dir: filepath.Join(c.base, name)}
	n.id = randomHex(16)
	n.cfgDir = filepath.Join(n.dir, "cfg")
	n.stateDir = filepath.Join(n.dir, "st")
	if o.StateDir != "" {
		n.stateDir = o.StateDir
	}
	n.runDir = filepath.Join(n.dir, "run")
	n.cacheDir = filepath.Join(n.dir, "cache")
	for _, d := range []string{n.cfgDir, n.stateDir, n.runDir, n.cacheDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "kran@"+name)
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(n.cfgDir, "id_ed25519"), pem.EncodeToMemory(blk), 0o600))
	n.signer, _ = ssh.NewSignerFromKey(priv)
	must(t, os.WriteFile(filepath.Join(n.cfgDir, "id_ed25519.pub"), ssh.MarshalAuthorizedKey(n.signer.PublicKey()), 0o644))
	must(t, os.WriteFile(filepath.Join(n.cfgDir, "node_id"), []byte(n.id+"\n"), 0o600))
	n.writeConfig()
	return n
}

func (n *knode) writeConfig() {
	o := n.opts
	if o.ReserveMem == "" {
		o.ReserveMem = "1GiB"
	}
	if o.Container == "" {
		o.Container = "false"
	}
	if o.Prefer == "" {
		o.Prefer = "auto"
	}
	cfg := fmt.Sprintf(`name = %q
[accept]
enabled = %v
reserve_cpus = %d
reserve_mem = %q
max_jobs = %d
nice = 10
[submit]
prefer = %q
[ui]
listen = "127.0.0.1:0"
[runtime.container]
enabled = %q
`, n.name, !o.NoAccept, o.ReserveCPUs, o.ReserveMem, o.MaxJobs, o.Prefer, o.Container)
	must(n.c.t, os.WriteFile(filepath.Join(n.cfgDir, "config.toml"), []byte(cfg), 0o600))
}

// writePeers starts this node's sshd and proxy (once) and writes
// peers.toml naming every other node.
func (n *knode) writePeers() {
	c := n.c
	for _, other := range c.nodes {
		other.ensureSSHD()
	}
	var b strings.Builder
	names := append([]string(nil), c.names...)
	sort.Strings(names)
	for _, name := range names {
		if name == n.name {
			continue
		}
		p := c.nodes[name]
		fmt.Fprintf(&b, `[[peer]]
name = %q
id = %q
addresses = [%q]
user = "kran"
host_key = %q
kran_path = %q
direction = "both"
linked_at = 2026-10-22T12:00:00Z

`, p.name, p.id, p.proxy.Addr(), p.sshd.HostKey(), kranBin)
	}
	must(c.t, os.WriteFile(filepath.Join(n.cfgDir, "peers.toml"), []byte(b.String()), 0o600))
}

// ensureSSHD starts the node's in-process sshd: it accepts the other
// nodes' kran keys and runs the real `kran serve --stdio` as the forced
// command, like authorized_keys does on a linked machine.
func (n *knode) ensureSSHD() {
	if n.sshd != nil {
		return
	}
	var keys []ssh.PublicKey
	for _, o := range n.c.nodes {
		if o != n {
			keys = append(keys, o.signer.PublicKey())
		}
	}
	srv, err := transport.NewSSHServer(transport.SSHServerConfig{
		AuthorizedKeys: keys,
		Handle:         n.serveStdio,
	})
	if err != nil {
		n.c.t.Fatal(err)
	}
	n.sshd = srv
	n.proxy = newFaultProxy(n.c.t, srv.Addr())
}

func (n *knode) serveStdio(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.Command(kranBin, "serve", "--stdio")
	cmd.Env = n.env()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "kran: %v\n", err)
		return 127
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cmd.Process.Kill()
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}

// env is the environment of every kran process of this node.
func (n *knode) env(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KRAN_") || strings.HasPrefix(kv, "GORACE=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"KRAN_CONFIG_DIR="+n.cfgDir,
		"KRAN_STATE_DIR="+n.stateDir,
		"KRAN_RUNTIME_DIR="+n.runDir,
		"KRAN_CACHE_DIR="+n.cacheDir,
		"KRAN_NO_AUTOSTART=1",
		// Own systemd slice/scopes and Docker names: a real kran may run on
		// this machine too.
		"KRAN_NAMESPACE=kran-e2e",
		"KRAN_NO_HOOK=1",
		"GORACE=log_path="+filepath.Join(raceDir, "race")+" halt_on_error=0",
	)
	env = append(env, n.opts.Env...)
	return append(env, extra...)
}

func (n *knode) sock() string { return filepath.Join(n.runDir, "kran.sock") }

// start launches the daemon and waits until its API answers.
func (n *knode) start() {
	t := n.c.t
	n.mu.Lock()
	if n.proc != nil {
		n.mu.Unlock()
		t.Fatalf("%s: daemon already running", n.name)
	}
	out, err := os.OpenFile(filepath.Join(n.dir, "daemon.out"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		n.mu.Unlock()
		t.Fatal(err)
	}
	cmd := exec.Command(kranBin, "daemon")
	cmd.Env = n.env()
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	n.starts++
	fmt.Fprintf(out, "=== e2e: start #%d at %s\n", n.starts, time.Now().Format(time.RFC3339Nano))
	if err := cmd.Start(); err != nil {
		out.Close()
		n.mu.Unlock()
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		out.Close()
		close(exited)
	}()
	n.proc, n.exited = cmd, exited
	n.mu.Unlock()
	if err := n.waitAPI(60 * time.Second); err != nil {
		t.Fatalf("%s: daemon did not come up: %v\n%s", n.name, err, n.tail("daemon.out", 40))
	}
}

func (n *knode) waitAPI(d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		n.mu.Lock()
		exited := n.exited
		n.mu.Unlock()
		select {
		case <-exited:
			return errors.New("daemon exited")
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cl, err := client.DialSocket(ctx, n.sock())
		if err == nil {
			_, err = cl.Version(ctx)
			cl.Close()
		}
		cancel()
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

// stop shuts the daemon down gracefully (SIGTERM); supervisors survive.
func (n *knode) stop() { n.signalDaemon(syscall.SIGTERM, 20*time.Second) }

// crash kills the daemon with SIGKILL (no cleanup at all).
func (n *knode) crash() { n.signalDaemon(syscall.SIGKILL, 10*time.Second) }

func (n *knode) restart() {
	n.stop()
	n.start()
}

func (n *knode) running() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.proc != nil
}

func (n *knode) signalDaemon(sig syscall.Signal, wait time.Duration) {
	n.mu.Lock()
	cmd, exited := n.proc, n.exited
	n.proc = nil
	n.mu.Unlock()
	if cmd == nil {
		return
	}
	cmd.Process.Signal(sig)
	select {
	case <-exited:
	case <-time.After(wait):
		n.c.t.Errorf("%s: daemon did not exit %v after %v; killing it", n.name, sig, wait)
		cmd.Process.Kill()
		<-exited
	}
}

func (n *knode) pid() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.proc == nil {
		return 0
	}
	return n.proc.Process.Pid
}

// tail returns the last lines of a file in the node dir (debug output).
func (n *knode) tail(file string, lines int) string {
	p := filepath.Join(n.dir, file)
	if file == "daemon.log" {
		p = filepath.Join(n.stateDir, file)
	}
	data, _ := os.ReadFile(p)
	ls := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(ls) > lines {
		ls = ls[len(ls)-lines:]
	}
	return strings.Join(ls, "\n")
}

// daemonLog is the node's daemon.log so far.
func (n *knode) daemonLog() string {
	data, _ := os.ReadFile(filepath.Join(n.stateDir, "daemon.log"))
	return string(data)
}

// ---------------------------------------------------------------------------
// state via the API (polling only; assertions use the CLI)

func (n *knode) state() (*proto.State, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cl, err := client.DialSocket(ctx, n.sock())
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	return cl.State(ctx)
}

func (n *knode) apiJobs() ([]proto.Job, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cl, err := client.DialSocket(ctx, n.sock())
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	return cl.Jobs(ctx, 1000)
}

// nodeView returns how n sees node name.
func nodeView(st *proto.State, name string) (proto.NodeState, bool) {
	for _, ns := range st.Nodes {
		if ns.Info.Name == name {
			return ns, true
		}
	}
	return proto.NodeState{}, false
}

func runtimeAvailable(ns proto.NodeState) bool {
	for _, r := range ns.Info.Runtimes {
		if r.Available {
			return true
		}
	}
	return false
}

// waitMesh waits until every running node sees every other running node
// online, connected and with a usable runtime.
func (c *cluster) waitMesh(d time.Duration) {
	c.t.Helper()
	var why string
	ok := poll(d, func() bool {
		for _, a := range c.nodes {
			if !a.running() {
				continue
			}
			st, err := a.state()
			if err != nil {
				why = fmt.Sprintf("%s: %v", a.name, err)
				return false
			}
			for _, b := range c.nodes {
				if b == a || !b.running() || b.opts.NoAccept {
					continue
				}
				ns, ok := nodeView(st, b.name)
				status := ns.Status == proto.NodeOnline || b.opts.Degraded && ns.Status == proto.NodeDegraded
				if !ok || !status || !ns.Link.Connected || !runtimeAvailable(ns) || ns.Info.CPUs == 0 {
					why = fmt.Sprintf("%s sees %s as %+v (connected %v)", a.name, b.name, ns.Status, ns.Link.Connected)
					return false
				}
			}
		}
		return true
	})
	if !ok {
		c.t.Fatalf("mesh did not form: %s", why)
	}
}

// waitNodeStatus waits until `from` sees `name` in one of the statuses.
func (c *cluster) waitNodeStatus(from, name string, d time.Duration, want ...proto.NodeStatus) proto.NodeState {
	c.t.Helper()
	var last proto.NodeState
	ok := poll(d, func() bool {
		st, err := c.nodes[from].state()
		if err != nil {
			return false
		}
		ns, ok := nodeView(st, name)
		if !ok {
			return false
		}
		last = ns
		for _, w := range want {
			if ns.Status == w {
				return true
			}
		}
		return false
	})
	if !ok {
		c.t.Fatalf("%s never saw %s in %v (last %s: %s)", from, name, want, last.Status, last.Reason)
	}
	return last
}

// ---------------------------------------------------------------------------
// teardown and invariants

func (c *cluster) teardown() {
	t := c.t
	ids := c.knownJobIDs()
	for _, n := range c.nodes {
		n.stop()
	}
	for _, n := range c.nodes {
		if n.sshd != nil {
			n.proxy.Close()
			n.sshd.Close()
		}
	}
	// Supervisors and jobs of ended jobs must be gone.
	var left []procInfo
	poll(20*time.Second, func() bool {
		left = processesWith(c.base)
		return len(left) == 0
	})
	for _, p := range left {
		t.Errorf("orphan process after teardown: pid %d %s", p.pid, p.cmd)
		syscall.Kill(p.pid, syscall.SIGKILL)
	}
	for _, u := range leftoverUnits(ids) {
		t.Errorf("leftover systemd unit: %s", u)
		exec.Command("systemctl", "--user", "stop", u).Run()
	}
	for _, f := range listDir(raceDir) {
		if c.raceSeen[f] {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(raceDir, f))
		if bytes.Contains(data, []byte("DATA RACE")) {
			t.Errorf("data race in a kran process (%s):\n%s", f, data)
		}
	}
	if t.Failed() {
		for _, name := range c.names {
			n := c.nodes[name]
			t.Logf("=== %s daemon.log (tail)\n%s", name, n.tail("daemon.log", 80))
			if s := n.tail("daemon.out", 30); strings.Contains(s, "panic") || strings.Contains(s, "goroutine ") {
				t.Logf("=== %s daemon.out (tail)\n%s", name, s)
			}
		}
	}
	if !t.Failed() || os.Getenv("KRAN_E2E_KEEP") == "" {
		os.RemoveAll(c.base)
		for _, n := range c.nodes {
			if n.opts.StateDir != "" {
				os.RemoveAll(n.opts.StateDir)
			}
		}
	} else {
		t.Logf("kept %s", c.base)
	}
}

// knownJobIDs collects every job ID any node knows (for unit cleanup).
func (c *cluster) knownJobIDs() map[string]bool {
	ids := map[string]bool{}
	for _, n := range c.nodes {
		if !n.running() {
			continue
		}
		jobs, err := n.apiJobs()
		if err != nil {
			continue
		}
		for _, j := range jobs {
			ids[j.Spec.ID] = true
		}
	}
	return ids
}

type procInfo struct {
	pid int
	cmd string
}

// processesWith lists processes whose environment mentions marker (every
// process of a test cluster carries its base dir in some KRAN_* variable;
// remote jobs carry KRAN_ORIGIN_ROOT under it).
func processesWith(marker string) []procInfo {
	if hostOS() == "darwin" {
		return processesWithDarwin(marker)
	}
	ents, _ := os.ReadDir("/proc")
	var out []procInfo
	self := os.Getpid()
	for _, e := range ents {
		var pid int
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid == self {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil || !bytes.Contains(env, []byte(marker)) {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if st, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "stat")); bytes.Contains(st, []byte(") Z ")) {
			continue // zombie of a process we still have to reap
		}
		out = append(out, procInfo{pid, strings.TrimSpace(strings.ReplaceAll(string(cmd), "\x00", " "))})
	}
	return out
}

func processesWithDarwin(marker string) []procInfo {
	// ps -E appends the environment separated by spaces; a marker ends in
	// NUL for /proc/<pid>/environ, so match it followed by a space or the
	// end of the line instead.
	exact := strings.HasSuffix(marker, "\x00")
	marker = strings.TrimSuffix(marker, "\x00")
	out, err := exec.Command("ps", "-Eww", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var res []procInfo
	self := os.Getpid()
	for _, l := range strings.Split(string(out), "\n") {
		if exact && !strings.Contains(l+" ", marker+" ") || !exact && !strings.Contains(l, marker) {
			continue
		}
		var pid int
		fmt.Sscanf(strings.TrimSpace(l), "%d", &pid)
		if pid == 0 || pid == self {
			continue
		}
		cmd := strings.TrimSpace(l)
		if len(cmd) > 160 {
			cmd = cmd[:160]
		}
		res = append(res, procInfo{pid, cmd})
	}
	return res
}

// processesWithJobID lists processes carrying KRAN_JOB_ID=id.
func processesWithJobID(id string) []procInfo { return processesWith("KRAN_JOB_ID=" + id + "\x00") }

// leftoverUnits lists kran systemd user units of the given jobs.
func leftoverUnits(ids map[string]bool) []string {
	if hostOS() != "linux" || len(ids) == 0 {
		return nil
	}
	out, err := exec.Command("systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "--no-pager",
		"kran-e2e-job-*", "kran-local-*").Output()
	if err != nil {
		return nil
	}
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		unit := f[0]
		id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(unit, "kran-e2e-job-"), "kran-local-"), ".scope")
		if ids[id] {
			res = append(res, unit)
		}
	}
	return res
}

// ---------------------------------------------------------------------------
// fault proxy: the "network" between nodes

type faultProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	down  bool
	stall chan struct{} // non-nil while stalled; closed to resume
	// budget: bytes still allowed through before the link stalls (-1 =
	// unlimited); lets a test stall a transfer in the middle.
	budget int64
	done   chan struct{}
	wg     sync.WaitGroup
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}, done: make(chan struct{}), budget: -1}
	p.wg.Add(1)
	go p.serve()
	return p
}

func (p *faultProxy) Addr() string { return p.ln.Addr().String() }

func (p *faultProxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		down := p.down
		p.mu.Unlock()
		if down {
			c.Close()
			continue
		}
		up, err := net.DialTimeout("tcp", p.target, 5*time.Second)
		if err != nil {
			c.Close()
			continue
		}
		p.mu.Lock()
		p.conns[c] = struct{}{}
		p.conns[up] = struct{}{}
		p.mu.Unlock()
		p.wg.Add(2)
		go p.pipe(c, up)
		go p.pipe(up, c)
	}
}

func (p *faultProxy) pipe(dst, src net.Conn) {
	defer p.wg.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if !p.gate(n) {
				break
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	dst.Close()
	src.Close()
	p.mu.Lock()
	delete(p.conns, dst)
	delete(p.conns, src)
	p.mu.Unlock()
}

// gate blocks while the link is stalled; false when the proxy closed.
func (p *faultProxy) gate(n int) bool {
	p.mu.Lock()
	if p.budget >= 0 {
		p.budget -= int64(n)
		if p.budget < 0 && p.stall == nil {
			p.stall = make(chan struct{})
		}
	}
	ch := p.stall
	p.mu.Unlock()
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	case <-p.done:
		return false
	}
}

// Cut closes every live connection (the link drops; reconnects work).
func (p *faultProxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
	}
}

// SetDown cuts the link and refuses new connections until SetDown(false).
func (p *faultProxy) SetDown(down bool) {
	p.mu.Lock()
	p.down = down
	p.mu.Unlock()
	if down {
		p.Cut()
	}
}

// StallAfter lets n more bytes through, then stalls the link.
func (p *faultProxy) StallAfter(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.budget = n
}

// Stall stops delivering bytes (both directions) without closing anything.
func (p *faultProxy) Stall(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.budget = -1
	switch {
	case on && p.stall == nil:
		p.stall = make(chan struct{})
	case !on && p.stall != nil:
		close(p.stall)
		p.stall = nil
	}
}

func (p *faultProxy) Close() {
	select {
	case <-p.done:
		return
	default:
	}
	close(p.done)
	p.ln.Close()
	p.Stall(false)
	p.Cut()
	p.wg.Wait()
}

// ---------------------------------------------------------------------------
// the kran CLI

type runOpts struct {
	dir   string
	env   []string // added to the node env
	stdin io.Reader
	// owner sets KRAN_OWNER (default human:e2e).
	owner   string
	timeout time.Duration
}

type result struct {
	code           int
	stdout, stderr []byte
	dur            time.Duration
}

func (r result) String() string {
	return fmt.Sprintf("exit %d after %v\nstdout: %q\nstderr: %q", r.code, r.dur.Round(time.Millisecond), clip(r.stdout), clip(r.stderr))
}

func clip(b []byte) string {
	if len(b) > 2000 {
		return string(b[:1000]) + " … " + string(b[len(b)-1000:])
	}
	return string(b)
}

// events parses --json-events lines from stderr.
func (r result) events() []map[string]any { return parseEvents(r.stderr) }

func parseEvents(stderr []byte) []map[string]any {
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(stderr))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := sc.Bytes()
		if !bytes.HasPrefix(l, []byte(`{"kran":`)) {
			continue
		}
		var m map[string]any
		if json.Unmarshal(l, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// event returns the first event of that kind.
func event(evs []map[string]any, kind string) map[string]any {
	for _, e := range evs {
		if e["kran"] == kind {
			return e
		}
	}
	return nil
}

func (n *knode) command(o runOpts, args ...string) *exec.Cmd {
	cmd := exec.Command(kranBin, args...)
	owner := o.owner
	if owner == "" {
		owner = "human:e2e"
	}
	cmd.Env = n.env(append([]string{"KRAN_OWNER=" + owner}, o.env...)...)
	cmd.Dir = o.dir
	if cmd.Dir == "" {
		cmd.Dir = n.dir
	}
	cmd.Stdin = o.stdin
	return cmd
}

// kran runs the CLI to completion.
func (n *knode) kran(o runOpts, args ...string) result {
	n.c.t.Helper()
	p := n.startKran(o, args...)
	return p.wait()
}

// cliProc is a kran CLI running in the background.
type cliProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdout  syncBuffer
	stderr  syncBuffer
	start   time.Time
	timeout time.Duration
	done    chan struct{}
	res     result
}

func (n *knode) startKran(o runOpts, args ...string) *cliProc {
	n.c.t.Helper()
	cmd := n.command(o, args...)
	p := &cliProc{t: n.c.t, cmd: cmd, timeout: o.timeout, done: make(chan struct{})}
	if p.timeout == 0 {
		p.timeout = 2 * time.Minute
	}
	cmd.Stdout, cmd.Stderr = &p.stdout, &p.stderr
	p.start = time.Now()
	if err := cmd.Start(); err != nil {
		n.c.t.Fatal(err)
	}
	go func() {
		err := cmd.Wait()
		p.res = result{code: exitCode(cmd, err), stdout: p.stdout.Bytes(), stderr: p.stderr.Bytes(), dur: time.Since(p.start)}
		close(p.done)
	}()
	return p
}

func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState == nil {
		return -1
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

func (p *cliProc) wait() result {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(p.timeout):
		p.cmd.Process.Signal(syscall.SIGQUIT)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			p.cmd.Process.Kill()
			<-p.done
		}
		p.t.Fatalf("kran %v did not finish within %v:\n%s", p.cmd.Args[1:], p.timeout, p.res)
	}
	return p.res
}

func (p *cliProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *cliProc) signal(sig syscall.Signal) { p.cmd.Process.Signal(sig) }

// waitStdout waits until the job's stdout contains s.
func (p *cliProc) waitStdout(s string, d time.Duration) bool {
	return poll(d, func() bool { return strings.Contains(p.stdout.String(), s) || p.exited() }) &&
		strings.Contains(p.stdout.String(), s)
}

// waitEvent waits for a --json-events event of that kind.
func (p *cliProc) waitEvent(kind string, d time.Duration) map[string]any {
	var ev map[string]any
	poll(d, func() bool {
		ev = event(parseEvents(p.stderr.Bytes()), kind)
		return ev != nil || p.exited()
	})
	if ev == nil {
		ev = event(parseEvents(p.stderr.Bytes()), kind)
	}
	return ev
}

// decisionWait bounds the wait for a job's placement. A loaded CI runner
// (two daemons on three cores) is overloaded by kran's own measure, so a
// job may rightly wait for CPU a minute or more before it is placed.
const decisionWait = 150 * time.Second

// jobID is the job ID from the decision event (needs --json-events).
func (p *cliProc) jobID(d time.Duration) string {
	if ev := p.waitEvent("decision", d); ev != nil {
		id, _ := ev["job_id"].(string)
		return id
	}
	return ""
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

func (s *syncBuffer) String() string { return string(s.Bytes()) }

// statusJSON runs `kran status --json`.
func (n *knode) statusJSON() proto.State {
	n.c.t.Helper()
	r := n.kran(runOpts{}, "status", "--json")
	if r.code != 0 {
		n.c.t.Fatalf("kran status --json on %s: %s", n.name, r)
	}
	var st proto.State
	if err := json.Unmarshal(r.stdout, &st); err != nil {
		n.c.t.Fatalf("kran status --json: %v\n%s", err, r.stdout)
	}
	return st
}

// jobsJSON runs `kran jobs --json -a`.
func (n *knode) jobsJSON() []proto.Job {
	n.c.t.Helper()
	r := n.kran(runOpts{}, "jobs", "--json", "-a")
	if r.code != 0 {
		n.c.t.Fatalf("kran jobs --json on %s: %s", n.name, r)
	}
	var jobs []proto.Job
	if err := json.Unmarshal(r.stdout, &jobs); err != nil {
		n.c.t.Fatalf("kran jobs --json: %v\n%s", err, r.stdout)
	}
	return jobs
}

func (n *knode) job(id string) (proto.Job, bool) {
	for _, j := range n.jobsJSON() {
		if j.Spec.ID == id {
			return j, true
		}
	}
	return proto.Job{}, false
}

// waitJob waits until the origin reports the job in a matching state.
func (n *knode) waitJob(id string, d time.Duration, ok func(proto.Job) bool) proto.Job {
	n.c.t.Helper()
	var last proto.Job
	if !poll(d, func() bool {
		jobs, err := n.apiJobs()
		if err != nil {
			return false
		}
		for _, j := range jobs {
			if j.Spec.ID == id {
				last = j
				return ok(j)
			}
		}
		return false
	}) {
		n.c.t.Fatalf("job %s on %s: condition not reached; last state %s reason %q", id, n.name, last.State, last.Reason)
	}
	return last
}

// waitIdle waits until a node has no active jobs, no queue and nothing
// allocated on itself — the steady state every test must return to.
func (n *knode) waitIdle(d time.Duration) {
	n.c.t.Helper()
	var why string
	if !poll(d, func() bool {
		st, err := n.state()
		if err != nil {
			why = err.Error()
			return false
		}
		why = idleProblem(st)
		return why == ""
	}) {
		n.c.t.Errorf("%s not idle: %s", n.name, why)
	}
}

// idleProblem describes why a state is not idle ("" = idle).
func idleProblem(st *proto.State) string {
	var p []string
	if len(st.Queue) > 0 {
		p = append(p, fmt.Sprintf("queue %v", st.Queue))
	}
	for _, j := range st.Jobs {
		if !j.State.Terminal() {
			p = append(p, fmt.Sprintf("job %s %s", j.Spec.ID, j.State))
		}
	}
	for _, ns := range st.Nodes {
		if !ns.Self {
			continue
		}
		if ns.Allocated.CPUs > 0.001 || ns.Allocated.Mem > 0 || len(ns.AllocatedJobs) > 0 || ns.Running > 0 || len(ns.Guests) > 0 {
			p = append(p, fmt.Sprintf("self allocated %+v jobs %v running %d guests %d", ns.Allocated, ns.AllocatedJobs, ns.Running, len(ns.Guests)))
		}
	}
	return strings.Join(p, "; ")
}

// ---------------------------------------------------------------------------
// repos and small helpers

// makeRepo creates a git repository with the given files under dir.
func makeRepo(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, p)
		must(t, os.MkdirAll(filepath.Dir(full), 0o755))
		must(t, os.WriteFile(full, []byte(c), 0o644))
	}
	git(t, dir, "init", "-q")
	git(t, dir, "add", "-A")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-qm", "init", "--allow-empty")
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func poll(d time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if fn() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func listDir(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func itoa(i int) string { return fmt.Sprint(i) }

// dockerPS lists containers matching a filter ("" when Docker is absent).
func dockerPS(t *testing.T, filter string) string {
	out, err := exec.Command("docker", "ps", "-a", "--filter", filter, "--format", "{{.Names}} {{.Status}}").Output()
	if err != nil {
		return ""
	}
	return string(out)
}
