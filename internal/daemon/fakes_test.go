package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/metrics"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/workspace"
)

// ---------------------------------------------------------------------------
// fake scheduler: a placement policy function over the pushed node states.

type placeFunc func(job *proto.Job, nodes map[string]proto.NodeState, rejected map[string]bool) (scheduler.Placement, string, bool)

type fakeSched struct {
	mu       sync.Mutex
	origin   string
	nodes    map[string]proto.NodeState
	queue    []*fticket
	alloc    map[string]scheduler.Placement
	place    placeFunc
	released []string
	rejects  []string
	jobs     map[string]proto.Job
	rej      map[string]map[string]bool
	foreign  map[string]scheduler.Placement
	usage    map[string]proto.Resources
}

type fticket struct {
	job      proto.Job
	rejected map[string]bool
	done     chan struct{}
	p        scheduler.Placement
	err      error
	updates  chan string
	reason   string
}

func newFakeSched(origin string, place placeFunc) *fakeSched {
	return &fakeSched{origin: origin, nodes: map[string]proto.NodeState{}, alloc: map[string]scheduler.Placement{}, place: place,
		jobs: map[string]proto.Job{}, rej: map[string]map[string]bool{},
		foreign: map[string]scheduler.Placement{}, usage: map[string]proto.Resources{}}
}

func (t *fticket) JobID() string          { return t.job.Spec.ID }
func (t *fticket) Updates() <-chan string { return t.updates }
func (t *fticket) Wait(ctx context.Context) (scheduler.Placement, error) {
	select {
	case <-t.done:
		return t.p, t.err
	case <-ctx.Done():
		return scheduler.Placement{}, ctx.Err()
	}
}

func (s *fakeSched) newTicket(j proto.Job, rejected map[string]bool) *fticket {
	return &fticket{job: j, rejected: rejected, done: make(chan struct{}), updates: make(chan string, 8)}
}

func (s *fakeSched) Submit(job *proto.Job) scheduler.Ticket {
	t := s.newTicket(cloneJob(*job), map[string]bool{})
	s.mu.Lock()
	s.jobs[job.Spec.ID] = cloneJob(*job)
	s.queue = append(s.queue, t)
	s.mu.Unlock()
	s.Kick()
	return t
}

func (s *fakeSched) Cancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.queue {
		if t.job.Spec.ID == id {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			t.err = context.Canceled
			close(t.done)
			return
		}
	}
}

func (s *fakeSched) Release(id string, u *proto.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.alloc, id)
	delete(s.foreign, id)
	s.released = append(s.released, id)
}

func (s *fakeSched) Reject(id, node string, err error) scheduler.Ticket {
	s.mu.Lock()
	delete(s.alloc, id)
	s.rejects = append(s.rejects, node)
	if s.rej[id] == nil {
		s.rej[id] = map[string]bool{}
	}
	s.rej[id][node] = true
	rej := map[string]bool{}
	for k := range s.rej[id] {
		rej[k] = true
	}
	t := s.newTicket(s.jobs[id], rej)
	s.queue = append(s.queue, t)
	s.mu.Unlock()
	s.Kick()
	return t
}

func (s *fakeSched) rejectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rejects)
}

func (s *fakeSched) allocCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.alloc)
}

func (s *fakeSched) UpdateNode(n proto.NodeState) {
	s.mu.Lock()
	s.nodes[n.Info.Name] = n
	s.mu.Unlock()
	s.Kick()
}

func (s *fakeSched) RemoveNode(name string) {
	s.mu.Lock()
	delete(s.nodes, name)
	s.mu.Unlock()
}

func (s *fakeSched) SetPriority(id string, p proto.Priority) {}

func (s *fakeSched) Snapshot() scheduler.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := scheduler.Snapshot{Reasons: map[string]string{}, Allocated: map[string]proto.Resources{},
		Free: map[string]proto.Resources{}, Running: map[string]int{}}
	for _, t := range s.queue {
		snap.Queue = append(snap.Queue, t.job.Spec.ID)
		snap.Reasons[t.job.Spec.ID] = t.reason
	}
	snap.Jobs = map[string][]string{}
	for _, m := range []map[string]scheduler.Placement{s.alloc, s.foreign} {
		for _, p := range m {
			a := snap.Allocated[p.Node]
			a.CPUs += p.Alloc.CPUs
			a.Mem += p.Alloc.Mem
			snap.Allocated[p.Node] = a
			snap.Running[p.Node]++
			snap.Jobs[p.Node] = append(snap.Jobs[p.Node], p.JobID)
		}
	}
	return snap
}

// Admit: capacity of the self node minus every allocation there.
func (s *fakeSched) Admit(job *proto.Job, rt proto.RuntimeKind) (scheduler.Placement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := job.Spec.ID
	if p, ok := s.foreign[id]; ok {
		return p, nil
	}
	n, ok := s.nodes[s.origin]
	if !ok {
		return scheduler.Placement{}, &scheduler.AdmitError{Code: "unknown", Reason: s.origin + ": not ready yet"}
	}
	if n.Status == proto.NodeDraining {
		return scheduler.Placement{}, &scheduler.AdmitError{Code: "draining", Reason: s.origin + ": draining"}
	}
	capMem := n.Capacity.Mem
	if capMem == 0 {
		capMem = n.Info.MemTotal - n.Reserve.Mem
	}
	var used int64
	for _, m := range []map[string]scheduler.Placement{s.alloc, s.foreign} {
		for _, p := range m {
			if p.Node == s.origin {
				used += p.Alloc.Mem
			}
		}
	}
	res := proto.Resources{CPUs: job.Estimate.CPUs, Mem: job.Estimate.Mem}
	if res.Mem > capMem-used {
		return scheduler.Placement{}, &scheduler.AdmitError{Code: "busy",
			Reason: fmt.Sprintf("%s: needs %s, has %s free", s.origin, humanBytes(res.Mem), humanBytes(capMem-used))}
	}
	p := scheduler.Placement{JobID: id, Node: s.origin, Runtime: rt, Alloc: res, Reason: "admitted"}
	s.foreign[id] = p
	return p, nil
}

func (s *fakeSched) UpdateJobUsage(id string, live proto.Resources) {
	s.mu.Lock()
	s.usage[id] = live
	s.mu.Unlock()
}

func (s *fakeSched) Kick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	nodes := map[string]proto.NodeState{}
	for k, v := range s.nodes {
		nodes[k] = v
	}
	var keep []*fticket
	for _, t := range s.queue {
		p, reason, ok := s.place(&t.job, nodes, t.rejected)
		if !ok {
			if reason != t.reason {
				t.reason = reason
				select {
				case t.updates <- reason:
				default:
				}
			}
			keep = append(keep, t)
			continue
		}
		p.JobID = t.job.Spec.ID
		if p.Node == "" {
			p.Node = s.origin
		}
		p.Local = p.Node == s.origin
		if p.Alloc.Mem == 0 {
			p.Alloc = proto.Resources{CPUs: 1, Mem: 1 << 30}
		}
		s.alloc[p.JobID] = p
		t.p = p
		close(t.done)
	}
	s.queue = keep
}

func (s *fakeSched) Tick(time.Time) { s.Kick() }

func (s *fakeSched) node(name string) (proto.NodeState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[name]
	return n, ok
}

// preferRemote places on node `target` when it is online and not rejected,
// else locally (as a fallback when it was rejected).
func preferRemote(target string) placeFunc {
	return func(job *proto.Job, nodes map[string]proto.NodeState, rejected map[string]bool) (scheduler.Placement, string, bool) {
		if job.Spec.Req.Prefer == proto.PreferLocal {
			return scheduler.Placement{Reason: "local"}, "", true
		}
		n, ok := nodes[target]
		if ok && n.Status == proto.NodeOnline && !rejected[target] {
			return scheduler.Placement{Node: target, Runtime: proto.RuntimeNative, Reason: "remote test"}, "", true
		}
		if len(rejected) > 0 {
			return scheduler.Placement{Reason: "fallback", Fallback: true}, "", true
		}
		if !ok || n.Status != proto.NodeOnline {
			return scheduler.Placement{}, "waiting: " + target + " is not online", false
		}
		return scheduler.Placement{Reason: "local"}, "", true
	}
}

func alwaysLocal(job *proto.Job, nodes map[string]proto.NodeState, rejected map[string]bool) (scheduler.Placement, string, bool) {
	return scheduler.Placement{Reason: "local"}, "", true
}

// ---------------------------------------------------------------------------
// fake estimator, prober, scanner

type fakeEst struct {
	mu       sync.Mutex
	observed []proto.Job
}

func (e *fakeEst) Fingerprint(s *proto.JobSpec) string { return strings.Join(s.Argv, " ") }
func (e *fakeEst) Estimate(s *proto.JobSpec, fp string, cpus int) proto.Estimate {
	return proto.Estimate{CPUs: 1, Mem: 1 << 30, Duration: 1, Source: "family:other"}
}
func (e *fakeEst) Speed(fp, node string) float64 { return 1 }
func (e *fakeEst) Observe(j *proto.Job) {
	e.mu.Lock()
	e.observed = append(e.observed, cloneJob(*j))
	e.mu.Unlock()
}
func (e *fakeEst) Family(argv []string) string { return "other" }
func (e *fakeEst) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.observed)
}

type fakeProber struct {
	mu   sync.Mutex
	info proto.NodeInfo
	m    proto.NodeMetrics
}

func newFakeProber(name string) *fakeProber {
	return &fakeProber{
		info: proto.NodeInfo{Name: name, Hostname: name, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: 16, MemTotal: 64 << 30, DiskTotal: 1 << 40},
		m:    proto.NodeMetrics{MemAvailable: 48 << 30, DiskFree: 500 << 30},
	}
}

func (p *fakeProber) Info(ctx context.Context) (proto.NodeInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info, nil
}
func (p *fakeProber) Refresh(ctx context.Context) error { return nil }
func (p *fakeProber) Sample() proto.NodeMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.m
	m.Time = time.Now()
	return m
}
func (p *fakeProber) JobUsage(ref metrics.UsageRef) (proto.Resources, error) {
	return proto.Resources{CPUs: 0.5, Mem: 1 << 20}, nil
}
func (p *fakeProber) Run(ctx context.Context) error { <-ctx.Done(); return nil }

type fakeScanner struct {
	mu    sync.Mutex
	paths map[string]string
	scans int
	// changeOpens: the next Opens report the file changed since the scan.
	changeOpens int
}

func newFakeScanner() *fakeScanner { return &fakeScanner{paths: map[string]string{}} }

func (s *fakeScanner) Scan(ctx context.Context, ws proto.Workspace, include, exclude []string) (*proto.Manifest, error) {
	m := &proto.Manifest{RepoID: ws.RepoID, Root: ws.Root}
	s.mu.Lock()
	s.scans++
	s.mu.Unlock()
	err := filepath.WalkDir(ws.Root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() || !de.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		hash := hex.EncodeToString(h[:])
		info, _ := de.Info()
		rel, _ := filepath.Rel(ws.Root, p)
		m.Entries = append(m.Entries, proto.ManifestEntry{Path: filepath.ToSlash(rel), Mode: uint32(info.Mode().Perm()),
			Size: int64(len(b)), MTime: info.ModTime().UnixNano(), Hash: hash})
		s.mu.Lock()
		s.paths[hash] = p
		s.mu.Unlock()
		return nil
	})
	return m, err
}

func (s *fakeScanner) Open(hash string) (workspace.ReadSeekCloser, int64, error) {
	s.mu.Lock()
	p, ok := s.paths[hash]
	changed := s.changeOpens > 0
	if changed {
		s.changeOpens--
	}
	s.mu.Unlock()
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	if changed {
		return nil, 0, fmt.Errorf("blob %s: %s: %w", hash, p, workspace.ErrBlobChanged)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	st, _ := f.Stat()
	return f, st.Size(), nil
}

func (s *fakeScanner) Close() error { return nil }

// ---------------------------------------------------------------------------
// fake execenv: real `sh -c` processes in their own process group, an
// in-memory framed output log with offsets, a lease watchdog. One instance
// per node survives daemon restarts (like detached supervisors).

type fakeExec struct {
	node     string
	dir      string
	leaseTTL time.Duration

	mu          sync.Mutex
	blobs       map[string][]byte
	slots       map[string]bool
	procs       map[string]*fakeProc
	startErr    error
	acquireGate chan struct{} // AcquireSlot blocks until closed (nil = no wait)
	putBlobGate chan struct{}
	startGate   chan struct{} // Start blocks until closed (nil = no wait); starting counts entries
	// endBeforeStart: Start succeeds but the job ends with this exit
	// without running the command (setup failed, command not found …).
	endBeforeStart *proto.Exit
	// setup: the job reports a setup phase this long before the command
	// starts (a TERM meanwhile cancels it).
	setup    time.Duration
	starting int
	putBlobs int
	acquires int
	releases int
	runtimes []proto.Runtime   // nil: one native runtime
	diffed   []*proto.Manifest // manifests Diff was called with
}

func newFakeExec(node, dir string, lease time.Duration) *fakeExec {
	return &fakeExec{node: node, dir: dir, leaseTTL: lease, blobs: map[string][]byte{}, slots: map[string]bool{}, procs: map[string]*fakeProc{}}
}

func (x *fakeExec) Runtimes(ctx context.Context) []proto.Runtime {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.runtimes != nil {
		return append([]proto.Runtime(nil), x.runtimes...)
	}
	return []proto.Runtime{{Kind: proto.RuntimeNative, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: 16, Mem: 0, Available: true}}
}
func (x *fakeExec) Refresh(ctx context.Context) error { return nil }

func (x *fakeExec) AcquireSlot(ctx context.Context, repoID, hint string, rt proto.RuntimeKind) (proto.SlotRef, error) {
	x.mu.Lock()
	gate := x.acquireGate
	x.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return proto.SlotRef{}, ctx.Err()
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("%s/%d", repoID, i)
		if !x.slots[key] {
			x.slots[key] = true
			x.acquires++
			p := filepath.Join(x.dir, "slots", repoID, fmt.Sprint(i))
			os.MkdirAll(p, 0o755)
			return proto.SlotRef{Node: x.node, RepoID: repoID, Index: i, Path: p}, nil
		}
	}
	return proto.SlotRef{}, errors.New("all slots busy")
}

func (x *fakeExec) counts() (starting, acquires, releases int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.starting, x.acquires, x.releases
}

func (x *fakeExec) proc(id string) *fakeProc {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.procs[id]
}

func (x *fakeExec) heldSlots() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.slots)
}

func (x *fakeExec) Diff(ctx context.Context, slot proto.SlotRef, m *proto.Manifest) (proto.SyncPlan, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.diffed = append(x.diffed, m)
	plan := proto.SyncPlan{Slot: slot}
	seen := map[string]bool{}
	for _, e := range m.Entries {
		if e.Hash == "" || seen[e.Hash] {
			continue
		}
		seen[e.Hash] = true
		if _, ok := x.blobs[e.Hash]; !ok {
			plan.Missing = append(plan.Missing, e.Hash)
			plan.Bytes += e.Size
		}
		plan.Changed++
	}
	return plan, nil
}

func (x *fakeExec) PutBlob(ctx context.Context, hash string, size int64, r io.Reader) error {
	x.mu.Lock()
	gate := x.putBlobGate
	x.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("size %d, want %d", len(b), size)
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != hash {
		return errors.New("hash mismatch")
	}
	x.mu.Lock()
	x.blobs[hash] = b
	x.putBlobs++
	x.mu.Unlock()
	return nil
}

func (x *fakeExec) Commit(ctx context.Context, slot proto.SlotRef, m *proto.Manifest) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, e := range m.Entries {
		b, ok := x.blobs[e.Hash]
		if !ok {
			return fmt.Errorf("missing blob for %s", e.Path)
		}
		p := filepath.Join(slot.Path, filepath.FromSlash(e.Path))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, fs.FileMode(e.Mode)); err != nil {
			return err
		}
		os.Chtimes(p, time.Unix(0, e.MTime), time.Unix(0, e.MTime))
	}
	return nil
}

func (x *fakeExec) ReleaseSlot(slot proto.SlotRef) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.slots, fmt.Sprintf("%s/%d", slot.RepoID, slot.Index))
	x.releases++
}

func (x *fakeExec) Start(ctx context.Context, req execenv.StartRequest) (execenv.Process, error) {
	x.mu.Lock()
	x.starting++
	gate := x.startGate
	x.mu.Unlock()
	if gate != nil {
		// Like a container image build on first use: Start outlives the
		// origin's interest; it does not watch ctx for the origin.
		<-gate
	}
	x.mu.Lock()
	err, ended, setup := x.startErr, x.endBeforeStart, x.setup
	x.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if ended != nil {
		p := &fakeProc{id: req.Spec.ID, notify: make(chan struct{}), doneCh: make(chan struct{}), done: true, exit: *ended}
		p.exit.JobID = req.Spec.ID
		close(p.doneCh)
		x.mu.Lock()
		x.procs[req.Spec.ID] = p
		x.mu.Unlock()
		return p, nil
	}
	dir := filepath.Join(req.Slot.Path, filepath.FromSlash(req.Spec.Workspace.Rel))
	p, err := startFakeProc(req.Spec, dir, x.leaseTTL, req.OnPhase, setup)
	if err != nil {
		return nil, err
	}
	x.mu.Lock()
	x.procs[req.Spec.ID] = p
	x.mu.Unlock()
	return p, nil
}

func (x *fakeExec) Attach(ctx context.Context, id string) (execenv.Process, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	p, ok := x.procs[id]
	if !ok {
		return nil, fmt.Errorf("no job %s", id)
	}
	return p, nil
}

func (x *fakeExec) Active() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []string
	for id, p := range x.procs {
		if !p.isDone() {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (x *fakeExec) Reap(ctx context.Context) error { return nil }
func (x *fakeExec) Run(ctx context.Context) error  { <-ctx.Done(); return nil }

func (x *fakeExec) killAll() {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, p := range x.procs {
		p.Signal("KILL")
	}
}

type frameRec struct {
	t    proto.FrameType
	data []byte
}

type fakeProc struct {
	id       string
	leaseTTL time.Duration
	cmd      *exec.Cmd
	stdin    io.WriteCloser

	mu        sync.Mutex
	frames    []frameRec
	notify    chan struct{}
	done      bool
	exit      proto.Exit
	lastRenew time.Time
	lost      bool
	doneCh    chan struct{}

	setupCancel chan struct{}
	cancelOnce  sync.Once
}

type procWriter struct {
	p *fakeProc
	t proto.FrameType
}

func (w procWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	w.p.frames = append(w.p.frames, frameRec{w.t, append([]byte(nil), b...)})
	close(w.p.notify)
	w.p.notify = make(chan struct{})
	w.p.mu.Unlock()
	return len(b), nil
}

func startFakeProc(spec proto.JobSpec, dir string, lease time.Duration, onPhase func(proto.Phase), setup time.Duration) (*fakeProc, error) {
	p := &fakeProc{id: spec.ID, leaseTTL: lease, notify: make(chan struct{}), doneCh: make(chan struct{}), lastRenew: time.Now(),
		setupCancel: make(chan struct{})}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = dir
	cmd.Stdout = procWriter{p, proto.FrameStdout}
	cmd.Stderr = procWriter{p, proto.FrameStderr}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "KRAN_JOB_ID="+spec.ID)
	if spec.Stdin {
		w, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		p.stdin = w
	}
	if onPhase == nil {
		onPhase = func(proto.Phase) {}
	}
	if setup > 0 {
		onPhase(proto.Phase{State: proto.JobSetup, Message: "running setup: fake"})
		go func() {
			endBeforeStart := func(x proto.Exit) {
				p.mu.Lock()
				x.JobID, x.BeforeStart = p.id, true
				p.exit = x
				p.done = true
				close(p.notify)
				p.notify = make(chan struct{})
				close(p.doneCh)
				p.mu.Unlock()
			}
			canceled := proto.Exit{Code: 143, Signal: "TERM", State: proto.JobCanceled, Reason: "canceled during setup"}
			lost := proto.Exit{Code: proto.ExitInfra, State: proto.JobLost, Reason: "lease expired during setup"}
			setupEnd := time.Now().Add(setup)
			// Like the supervisor: the lease runs during the setup, and the
			// command starts only after a renewal newer than the setup.
			for {
				p.mu.Lock()
				renewed, expired := p.lastRenew.After(setupEnd), time.Since(p.lastRenew) > p.leaseTTL
				p.mu.Unlock()
				if renewed {
					break
				}
				if expired {
					endBeforeStart(lost)
					return
				}
				select {
				case <-time.After(10 * time.Millisecond):
				case <-p.setupCancel:
					endBeforeStart(canceled)
					return
				}
			}
			onPhase(proto.Phase{State: proto.JobRunning})
			p.mu.Lock()
			err := cmd.Start()
			if err == nil {
				p.cmd = cmd
			}
			p.mu.Unlock()
			if err != nil {
				return
			}
			go p.wait()
			go p.watchLease()
		}()
		return p, nil
	}
	onPhase(proto.Phase{State: proto.JobRunning})
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.cmd = cmd
	go p.wait()
	go p.watchLease()
	return p, nil
}

func (p *fakeProc) wait() {
	err := p.cmd.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	x := proto.Exit{JobID: p.id, State: proto.JobSucceeded}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			x.Code = 128 + int(ws.Signal())
			x.Signal = strings.TrimPrefix(strings.ToUpper(ws.Signal().String()), "SIG")
			x.Signal = sigName(ws.Signal())
		} else {
			x.Code = ee.ExitCode()
		}
		x.State = proto.JobFailed
	}
	if p.lost {
		x.State = proto.JobLost
		x.Code = proto.ExitInfra
		x.Reason = "lease expired"
	}
	p.exit = x
	p.done = true
	close(p.notify)
	p.notify = make(chan struct{})
	close(p.doneCh)
}

func sigName(s syscall.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGKILL:
		return "KILL"
	}
	return s.String()
}

func (p *fakeProc) watchLease() {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-p.doneCh:
			return
		case <-t.C:
		}
		p.mu.Lock()
		expired := time.Since(p.lastRenew) > p.leaseTTL
		if expired {
			p.lost = true
		}
		p.mu.Unlock()
		if expired {
			syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			return
		}
	}
}

func (p *fakeProc) isDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

func (p *fakeProc) JobID() string { return p.id }

func (p *fakeProc) Output(offset int64) io.ReadCloser {
	return &fakeOutReader{p: p, offset: offset, closed: make(chan struct{})}
}

type fakeOutReader struct {
	p      *fakeProc
	idx    int
	pos    int64
	offset int64
	buf    bytes.Buffer
	closed chan struct{}
	once   sync.Once
}

func (r *fakeOutReader) Read(b []byte) (int, error) {
	for r.buf.Len() == 0 {
		r.p.mu.Lock()
		if r.idx < len(r.p.frames) {
			f := r.p.frames[r.idx]
			r.idx++
			r.p.mu.Unlock()
			start := r.pos
			r.pos += int64(len(f.data))
			if r.pos <= r.offset {
				continue
			}
			data := f.data
			if start < r.offset {
				data = data[r.offset-start:]
			}
			proto.WriteFrame(&r.buf, f.t, data)
			continue
		}
		if r.p.done {
			r.p.mu.Unlock()
			return 0, io.EOF
		}
		ch := r.p.notify
		r.p.mu.Unlock()
		select {
		case <-ch:
		case <-r.closed:
			return 0, io.ErrClosedPipe
		}
	}
	return r.buf.Read(b)
}

func (r *fakeOutReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func (p *fakeProc) Stdin() io.WriteCloser  { return p.stdin }
func (p *fakeProc) Resize(proto.TTY) error { return nil }
func (p *fakeProc) Signal(name string) error {
	sig := map[string]syscall.Signal{"INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "HUP": syscall.SIGHUP, "QUIT": syscall.SIGQUIT}[name]
	if sig == 0 {
		return fmt.Errorf("unknown signal %s", name)
	}
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd == nil {
		if p.setupCancel != nil && (sig == syscall.SIGTERM || sig == syscall.SIGKILL || sig == syscall.SIGINT) {
			p.cancelOnce.Do(func() { close(p.setupCancel) })
		}
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}
func (p *fakeProc) Renew() error {
	p.mu.Lock()
	p.lastRenew = time.Now()
	p.mu.Unlock()
	return nil
}
func (p *fakeProc) Live() proto.Resources { return proto.Resources{CPUs: 0.25, Mem: 64 << 20} }
func (p *fakeProc) Wait(ctx context.Context) (proto.Exit, error) {
	select {
	case <-p.doneCh:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.exit, nil
	case <-ctx.Done():
		return proto.Exit{}, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// static peer source

type staticPeers struct {
	mu    sync.Mutex
	peers []PeerConfig
	ver   int
}

func (s *staticPeers) Stamp() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprint(s.ver)
}

func (s *staticPeers) Load() ([]PeerConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PeerConfig(nil), s.peers...), nil
}

func (s *staticPeers) set(p []PeerConfig) {
	s.mu.Lock()
	s.peers = p
	s.ver++
	s.mu.Unlock()
}
