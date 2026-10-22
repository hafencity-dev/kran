package execenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
)

// syncLog collects manager logs; printed only when a test fails.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *syncLog) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, time.Now().Format("15:04:05.000 ")+f+"\n", a...)
}

type testEnv struct {
	t   *testing.T
	m   *manager
	dir string
	log *syncLog
}

type mgrOpt func(*Options, *tunables)

func withScope(on bool) mgrOpt { return func(_ *Options, t *tunables) { t.scope = &on } }
func withMemMax(b int64) mgrOpt {
	return func(_ *Options, t *tunables) { t.memMax = b }
}

func testTunables() tunables {
	tun := defaultTunables()
	tun.leaseTTL = 3 * time.Second
	tun.killGrace = time.Second
	tun.sampleEvery = 100 * time.Millisecond
	tun.watchEvery = 20 * time.Millisecond
	tun.livenessEvery = 200 * time.Millisecond
	tun.probe = false
	tun.cacheTrimEvery = 0 // tests run the cache trim themselves
	off := false
	tun.scope = &off
	return tun
}

func newTestEnv(t *testing.T, opts ...mgrOpt) *testEnv {
	t.Helper()
	dir := t.TempDir()
	lg := &syncLog{}
	o := Options{StateDir: dir, NodeName: "testnode", Self: testBinary(t), Namespace: "kran-test", Logf: lg.logf, DiskFloor: 1, DiskReserve: 1}
	tun := testTunables()
	for _, f := range opts {
		f(&o, &tun)
	}
	m, err := newManagerWith(o, tun)
	if err != nil {
		t.Fatal(err)
	}
	te := &testEnv{t: t, m: m, dir: dir, log: lg}
	t.Cleanup(func() {
		te.killAll(m)
		if t.Failed() {
			lg.mu.Lock()
			t.Logf("manager log:\n%s", lg.buf.String())
			lg.mu.Unlock()
			for _, id := range listDirs(m.l.jobsDir()) {
				b, _ := os.ReadFile(filepath.Join(m.l.job(id), fileLog))
				t.Logf("supervisor %s:\n%s", id, b)
			}
		}
	})
	return te
}

func listDirs(d string) []string {
	ents, _ := os.ReadDir(d)
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// killAll ends every job a test left running so no process outlives it.
func (te *testEnv) killAll(m *manager) {
	for _, id := range m.Active() {
		p, err := m.Attach(context.Background(), id)
		if err != nil {
			continue
		}
		p.Signal("KILL")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		p.Wait(ctx)
		cancel()
	}
	// The status is a supervisor's last write, but the process may still
	// be exiting; t.TempDir cleanup must not race with it.
	for _, id := range listDirs(m.l.jobsDir()) {
		if !waitSupervisorExit(m.l.job(id), 15*time.Second) {
			te.t.Errorf("supervisor of %s still running at cleanup", id)
		}
	}
}

// waitSupervisorExit blocks until no process holds the job's lock (the
// kernel drops a flock when its holder exits).
func waitSupervisorExit(dir string, d time.Duration) bool {
	f, err := os.OpenFile(filepath.Join(dir, fileLock), os.O_RDWR, 0)
	if err != nil {
		return true
	}
	got := make(chan struct{})
	go func() {
		defer f.Close()
		if unix.Flock(int(f.Fd()), unix.LOCK_EX) == nil {
			unix.Flock(int(f.Fd()), unix.LOCK_UN)
		}
		close(got)
	}()
	select {
	case <-got:
		return true
	case <-time.After(d):
		return false
	}
}

// slot acquires a slot and commits files into it.
func (te *testEnv) slot(repo string, files map[string]string, rt proto.RuntimeKind) proto.SlotRef {
	te.t.Helper()
	ctx := context.Background()
	ref, err := te.m.AcquireSlot(ctx, repo, "wt", rt)
	if err != nil {
		te.t.Fatal(err)
	}
	te.commit(ref, repo, files)
	return ref
}

func (te *testEnv) commit(ref proto.SlotRef, repo string, files map[string]string) {
	te.t.Helper()
	ctx := context.Background()
	mf := &proto.Manifest{RepoID: repo, Root: "/origin/" + repo}
	mt := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC).UnixNano()
	for p, c := range files {
		mf.Entries = append(mf.Entries, proto.ManifestEntry{Path: p, Kind: proto.EntryFile, Mode: 0o644, Size: int64(len(c)), MTime: mt, Hash: hashOf(c)})
	}
	plan, err := te.m.Diff(ctx, ref, mf)
	if err != nil {
		te.t.Fatal(err)
	}
	for _, h := range plan.Missing {
		for _, c := range files {
			if hashOf(c) == h {
				if err := te.m.PutBlob(ctx, h, int64(len(c)), strings.NewReader(c)); err != nil {
					te.t.Fatal(err)
				}
			}
		}
	}
	if err := te.m.Commit(ctx, ref, mf); err != nil {
		te.t.Fatal(err)
	}
}

var jobSeq struct {
	mu sync.Mutex
	n  int
}

func newJobID() string {
	jobSeq.mu.Lock()
	defer jobSeq.mu.Unlock()
	jobSeq.n++
	return fmt.Sprintf("t%s%03d", randHex(3), jobSeq.n)
}

type jobOpt func(*StartRequest)

func withTTY(rows, cols uint16) jobOpt {
	return func(r *StartRequest) { r.Spec.TTY = &proto.TTY{Rows: rows, Cols: cols} }
}
func withStdin() jobOpt { return func(r *StartRequest) { r.Spec.Stdin = true } }
func withSetup(run string, key ...string) jobOpt {
	return func(r *StartRequest) { r.Spec.Setup = &proto.SetupSpec{Run: run, Key: key} }
}
func withRel(rel string) jobOpt { return func(r *StartRequest) { r.Spec.Workspace.Rel = rel } }
func withEnv(k, v string) jobOpt {
	return func(r *StartRequest) {
		if r.Spec.Env == nil {
			r.Spec.Env = map[string]string{}
		}
		r.Spec.Env[k] = v
	}
}
func withPhases(ch chan proto.Phase) jobOpt {
	return func(r *StartRequest) { r.OnPhase = func(p proto.Phase) { ch <- p } }
}
func withAlloc(mem int64) jobOpt { return func(r *StartRequest) { r.Alloc.Mem = mem } }

func (te *testEnv) start(ref proto.SlotRef, argv []string, opts ...jobOpt) Process {
	te.t.Helper()
	req := StartRequest{
		Spec: proto.JobSpec{ID: newJobID(), Argv: argv, Workspace: proto.Workspace{Root: "/origin/" + ref.RepoID, RepoID: ref.RepoID}},
		Slot: ref, Runtime: proto.RuntimeNative, Origin: "originnode",
	}
	for _, o := range opts {
		o(&req)
	}
	p, err := te.m.Start(context.Background(), req)
	if err != nil {
		te.t.Fatal(err)
	}
	// After a setup the command waits for a heartbeat; renew like the
	// daemon does (lease tests use plain jobs or opt out).
	if req.Spec.Setup != nil && req.Spec.Labels["test.noheartbeat"] == "" {
		heartbeat(te.t, p)
	}
	return p
}

func waitExit(t *testing.T, p Process, d time.Duration) proto.Exit {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	exit, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("job %s did not end within %s: %v", p.JobID(), d, err)
	}
	return exit
}

// collect reads frames from r until EOF; returns stdout, stderr and the
// number of payload bytes.
func collect(t *testing.T, r io.Reader) (string, string, int64) {
	t.Helper()
	var out, errb bytes.Buffer
	var n int64
	buf := make([]byte, 64<<10)
	for {
		ft, payload, err := proto.ReadFrame(r, buf)
		if err != nil {
			if err == io.EOF {
				return out.String(), errb.String(), n
			}
			t.Fatalf("read output: %v", err)
		}
		n += int64(len(payload))
		switch ft {
		case proto.FrameStdout:
			out.Write(payload)
		case proto.FrameStderr:
			errb.Write(payload)
		default:
			t.Fatalf("unexpected frame %v", ft)
		}
	}
}

func output(t *testing.T, p Process) (string, string) {
	t.Helper()
	r := p.Output(0)
	defer r.Close()
	o, e, _ := collect(t, r)
	return o, e
}

// waitFor polls until the job's output contains want.
func waitFor(t *testing.T, p Process, want string, d time.Duration) {
	t.Helper()
	r := p.Output(0)
	defer r.Close()
	done := make(chan bool, 1)
	go func() {
		var all bytes.Buffer
		buf := make([]byte, 64<<10)
		for {
			_, payload, err := proto.ReadFrame(r, buf)
			if err != nil {
				done <- false
				return
			}
			all.Write(payload)
			if strings.Contains(all.String(), want) {
				done <- true
				return
			}
		}
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatalf("job ended without printing %q", want)
		}
	case <-time.After(d):
		r.Close()
		t.Fatalf("job did not print %q within %s", want, d)
	}
}

func sh(script string) []string { return []string{"/bin/sh", "-c", script} }

const testRepo = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// modes runs a test in pgid mode and, where available, in systemd scope
// mode (Linux).
func modes(t *testing.T, f func(t *testing.T, opts ...mgrOpt)) {
	t.Run("pgid", func(t *testing.T) { f(t, withScope(false)) })
	if runtime.GOOS == "linux" && scopeUsable() {
		t.Run("scope", func(t *testing.T) { f(t, withScope(true)) })
	}
}

var scopeOnce struct {
	sync.Once
	ok bool
}

func scopeUsable() bool {
	scopeOnce.Do(func() {
		m := &manager{ns: "kran-test", logf: func(string, ...any) {}, tun: tunables{}}
		scopeOnce.ok = m.probeScope(context.Background()) != ""
	})
	return scopeOnce.ok
}

func TestJobBasics(t *testing.T) {
	modes(t, func(t *testing.T, opts ...mgrOpt) {
		te := newTestEnv(t, opts...)
		ref := te.slot(testRepo, map[string]string{"sub/dir/f.txt": "hello file\n"}, proto.RuntimeNative)
		phases := make(chan proto.Phase, 10)
		p := te.start(ref, sh(`cat f.txt; echo "to stderr" >&2; echo "id=$KRAN_JOB_ID node=$KRAN_NODE origin=$KRAN_ORIGIN root=$KRAN_ORIGIN_ROOT"; echo "tmp=$TMPDIR"; echo "SECRET=$SECRET"; pwd; exit 3`),
			withRel("sub/dir"), withPhases(phases), withEnv("SECRET", "forwarded"), withEnv("PATH", "/evil"))
		exit := waitExit(t, p, 20*time.Second)
		if exit.Code != 3 || exit.State != proto.JobFailed || exit.BeforeStart {
			t.Fatalf("exit %+v", exit)
		}
		out, errOut := output(t, p)
		if errOut != "to stderr\n" {
			t.Errorf("stderr %q", errOut)
		}
		for _, want := range []string{"hello file", "id=" + p.JobID(), "node=testnode", "origin=originnode", "root=/origin/" + testRepo, "SECRET=forwarded",
			filepath.Join(ref.Path, "sub", "dir")} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout missing %q:\n%s", want, out)
			}
		}
		if !strings.Contains(out, "tmp="+filepath.Join(te.m.l.job(p.JobID()), dirTmp)) {
			t.Errorf("TMPDIR not per job:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(te.m.l.job(p.JobID()), dirTmp)); !os.IsNotExist(err) {
			t.Error("job TMPDIR must be removed after the job")
		}
		if exit.Usage.WallSeconds <= 0 {
			t.Errorf("no wall time: %+v", exit.Usage)
		}
		select {
		case ph := <-phases:
			if ph.State != proto.JobRunning {
				t.Errorf("phase %+v", ph)
			}
		case <-time.After(5 * time.Second):
			t.Error("no running phase callback")
		}
		if len(te.m.Active()) != 0 {
			t.Errorf("active after exit: %v", te.m.Active())
		}
		// A finished job can still be attached and read.
		p2, err := te.m.Attach(context.Background(), p.JobID())
		if err != nil {
			t.Fatal(err)
		}
		if out2, _ := output(t, p2); out2 != out {
			t.Error("attach after exit returned different output")
		}
		if _, err := te.m.Attach(context.Background(), "nope"); !errors.Is(err, ErrUnknownJob) {
			t.Errorf("attach unknown: %v", err)
		}
		// Slot is still held until released.
		if err := te.m.Commit(context.Background(), ref, &proto.Manifest{RepoID: testRepo}); err != nil {
			t.Fatal(err)
		}
		te.m.ReleaseSlot(ref)
	})
}

func TestExitCodesAndSignals(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, nil, proto.RuntimeNative)
	cases := []struct {
		argv   []string
		code   int
		sig    string
		state  proto.JobState
		before bool
	}{
		{sh("exit 0"), 0, "", proto.JobSucceeded, false},
		{sh("kill -TERM $$"), 143, "TERM", proto.JobFailed, false},
		{sh("kill -KILL $$"), 137, "KILL", proto.JobFailed, false},
		{[]string{"definitely-not-a-command-kran"}, 127, "", proto.JobFailed, true},
		{[]string{"/etc/hosts"}, 126, "", proto.JobFailed, true},
	}
	for _, c := range cases {
		p := te.start(ref, c.argv)
		exit := waitExit(t, p, 20*time.Second)
		if exit.Code != c.code || exit.Signal != c.sig || exit.State != c.state || exit.BeforeStart != c.before {
			t.Errorf("%v: exit %+v", c.argv, exit)
		}
	}
}

func TestSignalsAndStdin(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, nil, proto.RuntimeNative)

	p := te.start(ref, sh(`trap 'echo got INT; exit 42' INT; echo ready; while :; do sleep 0.05; done`))
	waitFor(t, p, "ready", 10*time.Second)
	if err := p.Signal("INT"); err != nil {
		t.Fatal(err)
	}
	if exit := waitExit(t, p, 10*time.Second); exit.Code != 42 {
		t.Fatalf("INT: %+v", exit)
	}

	p = te.start(ref, []string{"cat"}, withStdin())
	w := p.Stdin()
	io.WriteString(w, "hello ")
	io.WriteString(w, strings.Repeat("x", 100_000))
	w.Close()
	exit := waitExit(t, p, 10*time.Second)
	out, _ := output(t, p)
	if exit.Code != 0 || out != "hello "+strings.Repeat("x", 100_000) {
		t.Fatalf("stdin echo: %+v %d bytes", exit, len(out))
	}

	// Input sent before the command started (here: during a slow setup)
	// waits for it instead of being lost.
	p = te.start(ref, []string{"cat"}, withStdin(), withSetup("sleep 1"))
	w = p.Stdin()
	io.WriteString(w, "early input")
	w.Close()
	exit = waitExit(t, p, 20*time.Second)
	if out, _ := output(t, p); exit.Code != 0 || out != "early input" {
		t.Fatalf("early stdin: %+v %q", exit, out)
	}

	p = te.start(ref, sh(`trap '' TERM; echo ready; sleep 30`))
	waitFor(t, p, "ready", 10*time.Second)
	t0 := time.Now()
	p.Signal("TERM") // ignored by the job: escalates to KILL after the grace
	exit = waitExit(t, p, 15*time.Second)
	if exit.State != proto.JobCanceled || time.Since(t0) < te.m.tun.killGrace {
		t.Fatalf("cancel: %+v after %s", exit, time.Since(t0))
	}
	if err := p.Signal("INT"); err == nil {
		t.Error("signal after exit should fail")
	}
}

func TestPTY(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, nil, proto.RuntimeNative)
	p := te.start(ref, sh(`test -t 0 && test -t 1 && echo "is a tty"; stty size; echo "term=$TERM"; echo err >&2`), withTTY(24, 80))
	exit := waitExit(t, p, 10*time.Second)
	out, errOut := output(t, p)
	if exit.Code != 0 || !strings.Contains(out, "is a tty") || !strings.Contains(out, "24 80") || !strings.Contains(out, "term=xterm-256color") || !strings.Contains(out, "err") || errOut != "" {
		t.Fatalf("pty output %q / %q (%+v)", out, errOut, exit)
	}

	p = te.start(ref, sh(`trap 'stty size' WINCH; echo ready; while :; do sleep 0.05; done`), withTTY(24, 80))
	waitFor(t, p, "ready", 10*time.Second)
	if err := p.Resize(proto.TTY{Rows: 50, Cols: 132}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, p, "50 132", 10*time.Second)
	// Ctrl-C typed into the terminal reaches the job like locally.
	io.WriteString(p.Stdin(), "\x03")
	exit = waitExit(t, p, 10*time.Second)
	if exit.Code != 130 {
		t.Fatalf("^C: %+v", exit)
	}
}

func TestReattachByOffset(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, nil, proto.RuntimeNative)
	p := te.start(ref, []string{testBinary(t), "helper", "lines", "60", "20"})
	stop := make(chan struct{})
	defer close(stop)
	go func() { // heartbeats from "the origin"
		for {
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
				p.Renew()
			}
		}
	}()
	// Read part of the output, then "lose the daemon".
	r := p.Output(0)
	var got bytes.Buffer
	buf := make([]byte, 64<<10)
	for got.Len() < 200 {
		_, payload, err := proto.ReadFrame(r, buf)
		if err != nil {
			t.Fatal(err)
		}
		got.Write(payload)
	}
	r.Close()
	offset := int64(got.Len())

	// A new manager on the same state dir = the daemon restarted.
	m2, err := newManagerWith(te.m.opts, te.m.tun)
	if err != nil {
		t.Fatal(err)
	}
	if a := m2.Active(); len(a) != 1 || a[0] != p.JobID() {
		t.Fatalf("restarted manager sees active %v", a)
	}
	p2, err := m2.Attach(context.Background(), p.JobID())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
				p2.Renew()
			}
		}
	}()
	r2 := p2.Output(offset)
	rest, _, _ := collect(t, r2)
	r2.Close()
	exit := waitExit(t, p2, 20*time.Second)
	if exit.Code != 0 {
		t.Fatalf("exit %+v", exit)
	}
	var want strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&want, "line %04d\n", i)
	}
	if got.String()+rest != want.String() {
		t.Fatalf("resumed output differs:\n%q\n+\n%q", got.String(), rest)
	}
	// Mid-frame offsets work too.
	r3 := p2.Output(offset + 3)
	rest3, _, _ := collect(t, r3)
	if rest3 != rest[3:] {
		t.Fatal("mid-frame offset")
	}
	// The slot stayed busy across the restart until the daemon releases it
	// with its persisted ref.
	if _, err := m2.pool.lookup(ref); err != nil {
		t.Fatalf("slot of re-indexed job not busy: %v", err)
	}
	m2.ReleaseSlot(ref)
	if _, err := m2.pool.lookup(ref); err == nil {
		t.Fatal("slot not released")
	}
}

// TestLeaseExpiryKillsTree: without heartbeats the supervisor kills the
// whole tree, including grandchildren and setsid escapees.
func TestLeaseExpiryKillsTree(t *testing.T) {
	modes(t, func(t *testing.T, opts ...mgrOpt) {
		te := newTestEnv(t, opts...)
		ref := te.slot(testRepo, nil, proto.RuntimeNative)
		pids := t.TempDir()
		self := testBinary(t)
		script := fmt.Sprintf(`%[1]s helper escapee %[2]s/escapee.pid; (%[1]s helper sleeper %[2]s/grandchild.pid &); %[1]s helper sleeper %[2]s/main.pid`, self, pids)
		t0 := time.Now()
		p := te.start(ref, sh(script))
		var all []int
		for _, n := range []string{"escapee", "grandchild", "main"} {
			all = append(all, readPID(t, filepath.Join(pids, n+".pid"), 10*time.Second))
		}
		exit := waitExit(t, p, 30*time.Second)
		if exit.State != proto.JobLost || exit.Code != proto.ExitInfra || !strings.Contains(exit.Reason, "heartbeat") {
			t.Fatalf("exit %+v", exit)
		}
		if el := time.Since(t0); el < te.m.tun.leaseTTL {
			t.Fatalf("killed after %s, before the lease expired", el)
		}
		for _, pid := range all {
			if alive(pid) && !isZombie(pid) {
				t.Errorf("pid %d survived the lease expiry", pid)
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

func TestSupervisorSurvivesManagerAndRenew(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, nil, proto.RuntimeNative)
	p := te.start(ref, sh("sleep 7; echo done"))
	// Heartbeats keep the job alive past the TTL.
	deadline := time.Now().Add(te.m.tun.leaseTTL + 2*time.Second)
	for time.Now().Before(deadline) {
		p.Renew()
		time.Sleep(300 * time.Millisecond)
	}
	if len(te.m.Active()) != 1 {
		t.Fatal("renewed job died")
	}
	go func() {
		for i := 0; i < 20; i++ {
			p.Renew()
			time.Sleep(300 * time.Millisecond)
		}
	}()
	exit := waitExit(t, p, 20*time.Second)
	if exit.State != proto.JobSucceeded {
		t.Fatalf("exit %+v", exit)
	}
}

func TestMemoryCap(t *testing.T) {
	modes(t, func(t *testing.T, opts ...mgrOpt) {
		te := newTestEnv(t, append(opts, withMemMax(96<<20))...)
		ref := te.slot(testRepo, nil, proto.RuntimeNative)
		p := te.start(ref, []string{testBinary(t), "helper", "alloc", "400"}, withAlloc(1<<30))
		exit := waitExit(t, p, 60*time.Second)
		if exit.Code != 137 || exit.State != proto.JobFailed || !strings.Contains(exit.Reason, "memory cap on testnode") || !strings.Contains(exit.Reason, "--mem") {
			t.Fatalf("exit %+v", exit)
		}
		if exit.Usage.PeakMem < 32<<20 {
			t.Errorf("peak %d not recorded", exit.Usage.PeakMem)
		}
	})
}

func TestSetupStep(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, map[string]string{"lock": "v1"}, proto.RuntimeNative)
	phases := make(chan proto.Phase, 10)
	setup := `echo setting up; echo x >> setup-runs`
	p := te.start(ref, sh("echo main; wc -l < setup-runs"), withSetup(setup, "lock"), withPhases(phases))
	exit := waitExit(t, p, 20*time.Second)
	out, _ := output(t, p)
	if exit.Code != 0 || !strings.Contains(out, "setting up\nmain\n") || !strings.Contains(out, "1") {
		t.Fatalf("first run: %+v %q", exit, out)
	}
	if ph := <-phases; ph.State != proto.JobSetup || !strings.Contains(ph.Message, "setting up") {
		t.Fatalf("phase %+v", ph)
	}
	if ph := <-phases; ph.State != proto.JobRunning {
		t.Fatalf("phase %+v", ph)
	}
	// Same key: no setup.
	p = te.start(ref, sh("wc -l < setup-runs"), withSetup(setup, "lock"))
	waitExit(t, p, 20*time.Second)
	if out, _ := output(t, p); strings.TrimSpace(out) != "1" {
		t.Fatalf("setup ran again: %q", out)
	}
	// Changed key file: setup runs again.
	te.commit(ref, testRepo, map[string]string{"lock": "v2"})
	p = te.start(ref, sh("echo ok"), withSetup(setup, "lock"))
	waitExit(t, p, 20*time.Second)
	if out, _ := output(t, p); !strings.Contains(out, "setting up") {
		t.Fatalf("setup did not rerun: %q", out)
	}
	// A failing setup ends the job before the command with a clear reason.
	p = te.start(ref, sh("echo must-not-run"), withSetup("echo broken >&2; exit 7", "lock"))
	exit = waitExit(t, p, 20*time.Second)
	out, errOut := output(t, p)
	if !exit.BeforeStart || exit.State != proto.JobInfraErr || !strings.Contains(exit.Reason, "exit code 7") || strings.Contains(out, "must-not-run") || !strings.Contains(errOut, "broken") {
		t.Fatalf("failed setup: %+v %q %q", exit, out, errOut)
	}
	// Auto-detected setup from the committed manifest (go.sum + go cmd).
	te.commit(ref, testRepo, map[string]string{"go.sum": "x"})
	sl, _ := te.m.pool.lookup(ref)
	st := te.m.setupFor(&StartRequest{Spec: proto.JobSpec{Argv: []string{"go", "test"}}}, sl, "")
	if st == nil || st.Run != "go mod download" {
		t.Fatalf("auto setup %+v", st)
	}
}

// TestSetupWaitsForHeartbeat: when no heartbeat arrives after the setup
// (the origin gave up, our daemon died, the link is cut), the command never
// starts: the origin may already run the job elsewhere.
func TestSetupWaitsForHeartbeat(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, map[string]string{"lock": "v1"}, proto.RuntimeNative)
	marker := filepath.Join(t.TempDir(), "ran")
	p := te.start(ref, sh("echo ran > "+marker), withSetup("sleep 0.3", "lock"),
		func(r *StartRequest) { r.Spec.Labels = map[string]string{"test.noheartbeat": "1"} })
	exit := waitExit(t, p, 20*time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran without a heartbeat after setup")
	}
	if exit.State != proto.JobLost || !exit.BeforeStart || exit.Code != proto.ExitInfra {
		t.Fatalf("exit %+v", exit)
	}
	// With heartbeats it runs (TestSetupStep covers the rest).
	te.commit(ref, testRepo, map[string]string{"lock": "v2"})
	p = te.start(ref, sh("echo ran > "+marker), withSetup("sleep 0.3", "lock"))
	if exit := waitExit(t, p, 20*time.Second); exit.State != proto.JobSucceeded {
		t.Fatalf("with heartbeats: %+v", exit)
	}
}

func TestSlotPool(t *testing.T) {
	te := newTestEnv(t)
	ctx := context.Background()
	var refs []proto.SlotRef
	for i := 0; i < maxSlotsPerRepo; i++ {
		r, err := te.m.AcquireSlot(ctx, testRepo, fmt.Sprintf("wt%d", i), proto.RuntimeNative)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, r)
	}
	seen := map[int]bool{}
	for _, r := range refs {
		if seen[r.Index] {
			t.Fatalf("slot %d handed out twice", r.Index)
		}
		seen[r.Index] = true
	}
	// The fifth waits.
	sctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err := te.m.AcquireSlot(sctx, testRepo, "wt9", proto.RuntimeNative)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "free slot") {
		t.Fatalf("expected wait for slot: %v", err)
	}
	got := make(chan proto.SlotRef)
	go func() {
		r, err := te.m.AcquireSlot(ctx, testRepo, "wt-new", proto.RuntimeNative)
		if err != nil {
			t.Error(err)
		}
		got <- r
	}()
	time.Sleep(50 * time.Millisecond)
	te.m.ReleaseSlot(refs[1])
	if r := <-got; r.Index != refs[1].Index {
		t.Fatalf("waiter got slot %d, want the released %d", r.Index, refs[1].Index)
	}
	// Worktree affinity beats recency: slot 0 was released last, but wt2
	// gets its own slot 2 back.
	te.m.ReleaseSlot(refs[2])
	time.Sleep(5 * time.Millisecond)
	te.m.ReleaseSlot(refs[0])
	r, err := te.m.AcquireSlot(ctx, testRepo, "wt2", proto.RuntimeNative)
	if err != nil || r.Index != refs[2].Index {
		t.Fatalf("worktree affinity: got slot %d (%v), want %d", r.Index, err, refs[2].Index)
	}
	// Without affinity the most recently used free slot is taken.
	r0, err := te.m.AcquireSlot(ctx, testRepo, "unknown", proto.RuntimeNative)
	if err != nil || r0.Index != refs[0].Index {
		t.Fatalf("recency: got slot %d, want %d", r0.Index, refs[0].Index)
	}
	// Other repos are independent.
	other := strings.Repeat("ab", 32)
	if _, err := te.m.AcquireSlot(ctx, other, "", proto.RuntimeNative); err != nil {
		t.Fatal(err)
	}
	if _, err := te.m.AcquireSlot(ctx, "../evil", "", proto.RuntimeNative); err == nil {
		t.Fatal("bad repo id accepted")
	}
	// Committing to a slot that is not acquired fails.
	te.m.ReleaseSlot(r)
	if err := te.m.Commit(ctx, r, &proto.Manifest{}); err == nil {
		t.Fatal("commit to released slot accepted")
	}
}

func TestConcurrentSlotsRunJobs(t *testing.T) {
	te := newTestEnv(t)
	var wg sync.WaitGroup
	for i := 0; i < maxSlotsPerRepo; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := fmt.Sprintf("slot content %d", i)
			ref, err := te.m.AcquireSlot(context.Background(), testRepo, fmt.Sprintf("wt%d", i), proto.RuntimeNative)
			if err != nil {
				t.Error(err)
				return
			}
			mf := &proto.Manifest{RepoID: testRepo, Entries: []proto.ManifestEntry{{Path: "f", Mode: 0o644, Size: int64(len(content)), MTime: 1, Hash: hashOf(content)}}}
			te.m.PutBlob(context.Background(), hashOf(content), int64(len(content)), strings.NewReader(content))
			if err := te.m.Commit(context.Background(), ref, mf); err != nil {
				t.Error(err)
				return
			}
			req := StartRequest{Spec: proto.JobSpec{ID: newJobID(), Argv: []string{"cat", "f"}}, Slot: ref, Runtime: proto.RuntimeNative}
			p, err := te.m.Start(context.Background(), req)
			if err != nil {
				t.Error(err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p.Wait(ctx)
			r := p.Output(0)
			out, _, _ := collect(t, r)
			r.Close()
			if out != content {
				t.Errorf("slot %d: %q", i, out)
			}
			te.m.ReleaseSlot(ref)
		}(i)
	}
	wg.Wait()
}

func TestDiskFloorAndValidation(t *testing.T) {
	te := newTestEnv(t, func(o *Options, _ *tunables) { o.DiskFloor = 1 << 62 })
	_, err := te.m.AcquireSlot(context.Background(), testRepo, "", proto.RuntimeNative)
	if !errors.Is(err, ErrDiskFloor) || !strings.Contains(err.Error(), "free some space") {
		t.Fatalf("err = %v", err)
	}
	te2 := newTestEnv(t)
	ref := te2.slot(testRepo, nil, proto.RuntimeNative)
	for _, id := range []string{"", "../x", "a/b", strings.Repeat("x", 80)} {
		_, err := te2.m.Start(context.Background(), StartRequest{Spec: proto.JobSpec{ID: id, Argv: []string{"true"}}, Slot: ref, Runtime: proto.RuntimeNative})
		if err == nil {
			t.Errorf("job id %q accepted", id)
		}
	}
	p := te2.start(ref, sh("sleep 2"))
	_, err = te2.m.Start(context.Background(), StartRequest{Spec: proto.JobSpec{ID: newJobID(), Argv: []string{"true"}}, Slot: ref, Runtime: proto.RuntimeNative})
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second job in busy slot: %v", err)
	}
	if err := te2.m.Commit(context.Background(), ref, &proto.Manifest{RepoID: testRepo}); err == nil {
		t.Fatal("commit into a slot with a running job accepted")
	}
	_, err = te2.m.Start(context.Background(), StartRequest{Spec: proto.JobSpec{ID: p.JobID(), Argv: []string{"true"}}, Slot: ref, Runtime: proto.RuntimeNative})
	if err == nil {
		t.Fatal("duplicate job id accepted")
	}
	p.Signal("KILL")
	waitExit(t, p, 10*time.Second)
}

func TestReaper(t *testing.T) {
	modes(t, testReaper)
}

func testReaper(t *testing.T, opts ...mgrOpt) {
	te := newTestEnv(t, opts...)
	ref := te.slot(testRepo, nil, proto.RuntimeNative)
	pids := t.TempDir()
	p := te.start(ref, sh(fmt.Sprintf(`%s helper escapee %s/esc.pid; sleep 60`, testBinary(t), pids)))
	esc := readPID(t, filepath.Join(pids, "esc.pid"), 10*time.Second)
	// Kill the supervisor itself (as an OOM killer or a careless user might).
	var ev event
	b, _ := os.ReadFile(filepath.Join(te.m.l.job(p.JobID()), fileEvents))
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, `"ready"`) {
			jsonUnmarshal(t, line, &ev)
		}
	}
	syscall.Kill(ev.PID, syscall.SIGKILL)
	exit := waitExit(t, p, 10*time.Second)
	if exit.State != proto.JobInfraErr || !strings.Contains(exit.Reason, "supervisor") {
		t.Fatalf("exit %+v", exit)
	}
	if err := te.m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if alive(esc) && !isZombie(esc) {
		syscall.Kill(esc, syscall.SIGKILL)
		t.Fatal("reaper left the orphaned job process alive")
	}
	if te.m.systemdRun != "" {
		out, _ := exec.Command("systemctl", "--user", "is-active", te.m.scopeName(p.JobID())).Output()
		if strings.TrimSpace(string(out)) == "active" {
			t.Fatal("orphan scope still active")
		}
	}
	// Old job dirs go, slots unused for long go, abandoned acquisitions are released.
	te.m.tun.jobMaxAge = 0
	te.m.tun.slotMaxIdle = 0
	te.m.ReleaseSlot(ref)
	time.Sleep(10 * time.Millisecond)
	ref2, _ := te.m.AcquireSlot(context.Background(), strings.Repeat("cd", 32), "", proto.RuntimeNative)
	te.m.tun.slotHoldIdle = 0
	if err := te.m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(te.m.l.job(p.JobID())); !os.IsNotExist(err) {
		t.Error("old job dir kept")
	}
	if _, err := os.Stat(ref.Path); !os.IsNotExist(err) {
		t.Error("idle slot kept")
	}
	if _, err := te.m.pool.lookup(ref2); err == nil {
		t.Error("abandoned acquisition not released")
	}
}

func TestBlobCapTrimmedByReaper(t *testing.T) {
	te := newTestEnv(t, func(o *Options, _ *tunables) { o.BlobCap = 1000 })
	for i := 0; i < 30; i++ {
		c := strings.Repeat(fmt.Sprint(i%10), 100) + fmt.Sprint(i)
		te.m.PutBlob(context.Background(), hashOf(c), int64(len(c)), strings.NewReader(c))
	}
	te.m.Reap(context.Background())
	if _, total := te.m.blobs.list(); total > 1000 {
		t.Fatalf("blob store at %d bytes, cap 1000", total)
	}
}

// isZombie: exited but not yet reaped by its (new) parent; holds nothing.
func isZombie(pid int) bool {
	ps, err := listProcs()
	if err != nil {
		return false
	}
	for _, p := range ps {
		if p.pid == pid {
			return false
		}
	}
	return true
}

func jsonUnmarshal(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatal(err)
	}
}

func TestBusySlots(t *testing.T) {
	te := newTestEnv(t)
	if b := te.m.BusySlots(); len(b) != 0 {
		t.Fatalf("busy before any job: %v", b)
	}
	ref := te.slot(testRepo, nil, proto.RuntimeNative)
	if b := te.m.BusySlots(); len(b) != 1 || !strings.HasPrefix(b[0], testRepo[:12]+"/") {
		t.Fatalf("busy %v", b)
	}
	te.m.ReleaseSlot(ref)
	if b := te.m.BusySlots(); len(b) != 0 {
		t.Fatalf("busy after release: %v", b)
	}
}

func TestProcessRunning(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, map[string]string{"lock": "v1"}, proto.RuntimeNative)
	p := te.start(ref, sh("echo ready; sleep 1"), withSetup("sleep 0.5", "lock"))
	r := p.(interface{ Running() bool })
	if r.Running() {
		t.Fatal("running during setup")
	}
	waitFor(t, p, "ready", 10*time.Second)
	// The supervisor's events are polled; the phase lands shortly.
	deadline := time.Now().Add(5 * time.Second)
	for !r.Running() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !r.Running() {
		t.Fatal("not running after the command printed")
	}
	waitExit(t, p, 10*time.Second)
}

// TestRunningPhaseBeforeOutput: whoever reads a job's output has been told
// the command started before seeing its first byte (the origin decides
// fallback vs. exit 125 by that), even for jobs that print and exit at once.
func TestRunningPhaseBeforeOutput(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, nil, proto.RuntimeNative)
	for i := 0; i < 10; i++ {
		var running atomic.Bool
		p := te.start(ref, sh("echo hi"), func(r *StartRequest) {
			r.OnPhase = func(ph proto.Phase) {
				if ph.State == proto.JobRunning {
					running.Store(true)
				}
			}
		})
		r := p.Output(0)
		buf := make([]byte, 64<<10)
		if _, _, err := proto.ReadFrame(r, buf); err != nil {
			t.Fatal(err)
		}
		if !running.Load() {
			t.Fatalf("run %d: output before the running phase", i)
		}
		r.Close()
		waitExit(t, p, 20*time.Second)
	}
}
