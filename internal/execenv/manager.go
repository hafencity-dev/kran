package execenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/selfexe"
	"github.com/hafencity-dev/kran/internal/workspace"
)

// tunables are the manager's timing and policy knobs; tests shrink them.
type tunables struct {
	leaseTTL         time.Duration
	killGrace        time.Duration
	sampleEvery      time.Duration
	watchEvery       time.Duration // job watcher poll
	livenessEvery    time.Duration // supervisor liveness check
	readyTimeout     time.Duration
	reapEvery        time.Duration
	trimEvery        time.Duration
	cacheTrimEvery   time.Duration // container cache trim (ctrtrim.go)
	jobMaxAge        time.Duration // finished job dirs are deleted after this
	slotMaxIdle      time.Duration // free slots unused this long are deleted
	slotHoldIdle     time.Duration // acquired but unused slots are released after this
	scope            *bool         // nil: probe systemd-run; tests force a mode
	probe            bool          // probe the login shell and toolchains
	probeDocker      bool          // look for a Docker daemon
	forceVolume      bool          // tests: keep container slots in the volume on Linux too
	runnerDockerfile string        // tests: a lighter image
	memMax           int64         // tests: force this memory cap
}

func defaultTunables() tunables {
	return tunables{
		leaseTTL:       LeaseTTL,
		killGrace:      5 * time.Second,
		sampleEvery:    time.Second,
		watchEvery:     50 * time.Millisecond,
		livenessEvery:  time.Second,
		readyTimeout:   20 * time.Second,
		reapEvery:      30 * time.Second,
		trimEvery:      10 * time.Minute,
		cacheTrimEvery: cacheTrimEvery,
		jobMaxAge:      7 * 24 * time.Hour,
		slotMaxIdle:    3 * 24 * time.Hour,
		slotHoldIdle:   30 * time.Minute,
		probe:          true,
		probeDocker:    true,
	}
}

type manager struct {
	opts  Options
	tun   tunables
	l     layout
	ns    string
	logf  func(string, ...any)
	now   func() time.Time
	blobs *blobStore
	pool  *slotPool

	jobsMu sync.Mutex
	jobs   map[string]*jobState

	env        *hostEnv // guarded by rtMu after construction (Refresh replaces it)
	refreshed  bool
	systemdRun string // path when transient user scopes work, else ""

	rtMu     sync.Mutex
	runtimes []proto.Runtime
	dk       *dockerEnv // nil when Docker is not usable
	dkReason string     // why not

	img runnerImage
	ct  cacheTrim

	binMu  sync.Mutex // guards binKey, binSum
	binKey struct {
		path  string
		size  int64
		mtime time.Time
	}
	binSum string
}

func newManager(opts Options) (Manager, error) { return newManagerWith(opts, defaultTunables()) }

func newManagerWith(opts Options, tun tunables) (*manager, error) {
	if opts.StateDir == "" {
		return nil, errors.New("execenv: StateDir is required")
	}
	if opts.Self == "" {
		self, err := selfexe.Path()
		if err != nil {
			return nil, fmt.Errorf("execenv: locate kran binary: %w", err)
		}
		opts.Self = self
	}
	if opts.NodeName == "" {
		h, _ := os.Hostname()
		opts.NodeName = strings.ToLower(strings.Split(h, ".")[0])
	}
	if opts.Namespace == "" {
		opts.Namespace = "kran"
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	l := layout{state: opts.StateDir}
	for _, d := range []string{l.state, l.jobsDir(), l.slotsDir(), l.blobsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("execenv: %w", err)
		}
	}
	m := &manager{
		opts: opts, tun: tun, l: l, ns: opts.Namespace, logf: opts.Logf, now: time.Now,
		jobs: map[string]*jobState{},
	}
	var err error
	if m.blobs, err = newBlobStore(l, opts.Logf); err != nil {
		return nil, err
	}
	m.pool = newSlotPool(l, opts.NodeName, m.now)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if tun.probe {
		m.env = captureHostEnv(ctx)
	} else {
		home, _ := os.UserHomeDir()
		m.env = &hostEnv{path: os.Getenv("PATH"), home: home, user: os.Getenv("USER"), shell: "/bin/sh", vars: map[string]string{}, tools: map[string]string{}}
	}
	m.systemdRun = m.probeScope(ctx)
	m.reindex()
	m.Refresh(ctx)
	return m, nil
}

// slice is the systemd slice of job scopes ("kran.slice"). Dashes would
// nest slices, so other namespaces use underscores.
func (m *manager) slice() string { return strings.ReplaceAll(m.ns, "-", "_") + ".slice" }

func (m *manager) scopeName(id string) string { return m.ns + "-job-" + id + ".scope" }

// probeScope checks that transient systemd user scopes with delegation
// work here (Linux with a user manager); "" means use process groups.
func (m *manager) probeScope(ctx context.Context) string {
	if m.tun.scope != nil && !*m.tun.scope {
		return ""
	}
	if runtime.GOOS != "linux" {
		return ""
	}
	bin, err := exec.LookPath("systemd-run")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	unit := m.ns + "-probe-" + randHex(4)
	cmd := exec.CommandContext(ctx, bin, "--user", "--scope", "--quiet", "--collect", "--unit="+unit, "--slice="+m.slice(), "-p", "Delegate=yes", "--", "/bin/true")
	if out, err := cmd.CombinedOutput(); err != nil {
		m.logf("systemd user scopes unavailable, jobs use process groups: %v %s", err, strings.TrimSpace(string(out)))
		return ""
	}
	return bin
}

// reindex picks up jobs whose supervisors survived a daemon restart.
func (m *manager) reindex() {
	ents, err := os.ReadDir(m.l.jobsDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || validJobID(e.Name()) != nil {
			continue
		}
		dir := m.l.job(e.Name())
		if _, err := os.Stat(filepath.Join(dir, fileStatus)); err == nil {
			continue
		}
		var spec superSpec
		if readJSON(filepath.Join(dir, fileSpec), &spec) != nil {
			continue
		}
		if !supervisorAlive(dir) {
			exit := proto.Exit{JobID: spec.JobID, Node: m.opts.NodeName, State: proto.JobLost, Code: proto.ExitInfra,
				Reason: fmt.Sprintf("the job's supervisor on %s was gone after a restart", m.opts.NodeName)}
			writeJSONAtomic(filepath.Join(dir, fileStatus), exit)
			continue
		}
		// The slot stays busy across the restart; the daemon releases it
		// with its persisted SlotRef once the job ended (and the reaper
		// frees it if nobody does within slotHoldIdle).
		m.track(spec.JobID, dir, spec, nil, m.pool.markBusy(spec.Slot, spec.JobID))
		m.logf("re-attached running job %s", spec.JobID)
	}
}

// track registers a job and starts its watcher.
// The slot is set before the watcher starts: a job that ends at once must
// still find it to free it.
func (m *manager) track(id, dir string, spec superSpec, onPhase func(proto.Phase), sl *slot) *jobState {
	js := &jobState{m: m, id: id, dir: dir, spec: spec, done: make(chan struct{}), onPhase: onPhase, slot: sl}
	m.jobsMu.Lock()
	m.jobs[id] = js
	m.jobsMu.Unlock()
	go js.watch()
	return js
}

func (m *manager) Runtimes(ctx context.Context) []proto.Runtime {
	m.rtMu.Lock()
	rts := append([]proto.Runtime(nil), m.runtimes...)
	m.rtMu.Unlock()
	paused := m.cachePaused()
	for i := range rts {
		if rts[i].Kind == proto.RuntimeContainer {
			rts[i].Caches = m.cacheStatus()
			if paused && rts[i].Available {
				rts[i].Available, rts[i].Reason = false, cachePauseReason
			}
		}
	}
	return rts
}

func (m *manager) Refresh(ctx context.Context) error {
	m.rtMu.Lock()
	again := m.refreshed
	m.refreshed = true
	m.rtMu.Unlock()
	if m.tun.probe && again {
		// Toolchains change when the node owner installs or upgrades them.
		env := captureHostEnv(ctx)
		m.rtMu.Lock()
		m.env = env
		m.rtMu.Unlock()
	}
	env := m.hostEnv()
	native := proto.Runtime{
		Kind: proto.RuntimeNative, OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), Mem: memTotal(), Available: true,
		Toolchains: copyMap(env.tools),
	}
	dk, reason := m.probeDocker(ctx)
	if dk != nil {
		old, _ := m.docker()
		dk.inherit(old)
	}
	native.Docker = dk != nil
	rts := []proto.Runtime{native}
	if crt, ok := m.containerRuntime(ctx, dk, reason); ok {
		if crt.Available {
			crt.FirstUse = !m.imageBuilt(ctx, dk)
		}
		rts = append(rts, crt)
	}
	m.rtMu.Lock()
	m.runtimes, m.dk, m.dkReason = rts, dk, reason
	m.rtMu.Unlock()
	return nil
}

func (m *manager) hostEnv() *hostEnv {
	m.rtMu.Lock()
	defer m.rtMu.Unlock()
	return m.env
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (m *manager) docker() (*dockerEnv, string) {
	m.rtMu.Lock()
	defer m.rtMu.Unlock()
	return m.dk, m.dkReason
}

// slotLocFor maps a runtime to where its slots live: container slots on a
// macOS node live in the Docker VM's volume; everywhere else on the host.
func (m *manager) slotLocFor(rt proto.RuntimeKind) slotLoc {
	if rt == proto.RuntimeContainer && (runtime.GOOS != "linux" || m.tun.forceVolume) {
		return locVolume
	}
	return locHost
}

func (m *manager) checkDiskFloor() error {
	free, total, err := diskSpace(m.l.state)
	if err != nil {
		return nil // unknown: do not block jobs on a failed statfs
	}
	floor := m.opts.DiskFloor
	if floor <= 0 {
		floor = diskFloor(total)
	}
	if free < floor {
		return fmt.Errorf("%w: %s has %s free, needs at least %s; free some space there", ErrDiskFloor, m.opts.NodeName, fmtGiB(free), fmtGiB(floor))
	}
	return nil
}

func (m *manager) AcquireSlot(ctx context.Context, repoID, hint string, rt proto.RuntimeKind) (proto.SlotRef, error) {
	if err := m.checkDiskFloor(); err != nil {
		return proto.SlotRef{}, err
	}
	if rt == proto.RuntimeContainer {
		if err := m.checkDockerDiskFloor(); err != nil {
			return proto.SlotRef{}, err
		}
		if m.cachePaused() {
			return proto.SlotRef{}, fmt.Errorf("%w: %s", ErrRuntimeUnavailable, cachePauseReason)
		}
	}
	loc := m.slotLocFor(rt)
	if loc == locVolume {
		if err := m.ensureContainerReady(ctx, nil); err != nil {
			return proto.SlotRef{}, err
		}
	}
	s, err := m.pool.acquire(ctx, repoID, hint, loc)
	if err != nil {
		return proto.SlotRef{}, err
	}
	return m.pool.ref(s), nil
}

func (m *manager) ReleaseSlot(ref proto.SlotRef) {
	s, err := m.pool.lookup(ref)
	if err != nil {
		return
	}
	m.pool.release(s)
}

// Diff scans the slot once and plans the update. When the blob store
// already holds every file the slot lacks (an unchanged repo, or content
// another worktree sent), it carries the plan out right away and reports
// Committed: an unchanged sync then costs one scan (one helper run for a
// slot in the Docker volume), and Commit of the same manifest is a no-op.
func (m *manager) Diff(ctx context.Context, ref proto.SlotRef, mf *proto.Manifest) (proto.SyncPlan, error) {
	s, err := m.pool.lookup(ref)
	if err != nil {
		return proto.SyncPlan{}, err
	}
	if mf.RepoID != "" && mf.RepoID != s.repo {
		return proto.SyncPlan{}, fmt.Errorf("manifest of repo %s does not belong to slot of %s", short(mf.RepoID), short(s.repo))
	}
	if job := m.pool.jobOf(s); job != "" {
		return proto.SyncPlan{}, fmt.Errorf("slot %s/%d is in use by job %s", short(s.repo), s.index, job)
	}
	m.pool.setSynced(s, nil)
	plan := proto.SyncPlan{Slot: ref}
	// Shared: no blob the plan relies on is trimmed until it is carried out.
	m.blobs.trimMu.RLock()
	defer m.blobs.trimMu.RUnlock()
	stored := func(needed []string) bool {
		for _, h := range needed {
			if !m.blobs.has(h) {
				return false
			}
		}
		return true
	}
	var needed []string
	if s.loc == locHost {
		root := m.pool.hostPath(s)
		p, err := planSlot(root, mf, runtime.GOOS == "darwin")
		if err != nil {
			return plan, err
		}
		needed, plan.Changed, plan.Deleted = p.needed, p.changed(), len(p.deletes)
		if stored(needed) {
			if err := applyPlanLocal(root, p, m.blobs, max(2, runtime.NumCPU()/2)); err != nil {
				return plan, fmt.Errorf("sync to %s: %w", m.opts.NodeName, err)
			}
			plan.Committed = true
		}
	} else {
		need, done, err := m.volumeSync(ctx, ref.Path, mf, func(n applyNeed) bool { return stored(n.Needed) })
		if err != nil {
			return plan, err
		}
		needed, plan.Changed, plan.Deleted, plan.Committed = need.Needed, need.Changed, need.Deleted, done
	}
	if plan.Committed {
		m.pool.setSynced(s, mf)
		return plan, nil
	}
	sizes := make(map[string]int64, len(needed))
	for _, e := range mf.Entries {
		if e.Kind == proto.EntryFile {
			sizes[e.Hash] = e.Size
		}
	}
	for _, h := range needed {
		if !m.blobs.has(h) {
			plan.Missing = append(plan.Missing, h)
			plan.Bytes += sizes[h]
		}
	}
	return plan, nil
}

func (m *manager) PutBlob(ctx context.Context, hash string, size int64, r io.Reader) error {
	return m.blobs.put(ctx, hash, size, r)
}

func (m *manager) Commit(ctx context.Context, ref proto.SlotRef, mf *proto.Manifest) error {
	s, err := m.pool.lookup(ref)
	if err != nil {
		return err
	}
	if job := m.pool.jobOf(s); job != "" {
		return fmt.Errorf("slot %s/%d is in use by job %s", short(s.repo), s.index, job)
	}
	if m.pool.syncedTo(s) == mf {
		return nil // Diff carried it out already
	}
	if s.loc == locHost {
		if _, err := commitLocal(m.pool.hostPath(s), mf, m.blobs, max(2, runtime.NumCPU()/2), runtime.GOOS == "darwin"); err != nil {
			return fmt.Errorf("sync to %s: %w", m.opts.NodeName, err)
		}
	} else {
		m.blobs.trimMu.RLock()
		_, err := m.volumeApply(ctx, applyReq{Op: "commit", Slot: ref.Path, Manifest: mf}, m.openBlob)
		m.blobs.trimMu.RUnlock()
		if err != nil {
			return fmt.Errorf("sync to %s: %w", m.opts.NodeName, err)
		}
	}
	m.pool.setManifest(s, mf)
	return nil
}

func (m *manager) openBlob(hash string) (io.ReadCloser, int64, error) {
	if err := validHash(hash); err != nil { // the apply side names it
		return nil, 0, err
	}
	f, err := os.Open(m.blobs.path(hash))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %s", ErrMissingBlob, short(hash))
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	m.blobs.touch(hash)
	return f, fi.Size(), nil
}

func (m *manager) Start(ctx context.Context, req StartRequest) (Process, error) {
	id := req.Spec.ID
	if err := validJobID(id); err != nil {
		return nil, err
	}
	if len(req.Spec.Argv) == 0 {
		return nil, errors.New("job has no command")
	}
	s, err := m.pool.lookup(req.Slot)
	if err != nil {
		return nil, err
	}
	if want := m.slotLocFor(req.Runtime); want != s.loc {
		return nil, fmt.Errorf("slot %s/%d does not belong to the %s runtime", short(s.repo), s.index, req.Runtime)
	}
	if job := m.pool.jobOf(s); job != "" {
		return nil, fmt.Errorf("slot %s/%d is in use by job %s", short(s.repo), s.index, job)
	}
	m.jobsMu.Lock()
	_, dup := m.jobs[id]
	m.jobsMu.Unlock()
	dir := m.l.job(id)
	if dup {
		return nil, fmt.Errorf("job %s already exists on %s", id, m.opts.NodeName)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("job %s already exists on %s", id, m.opts.NodeName)
		}
		return nil, fmt.Errorf("job dir: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()

	spec := superSpec{
		JobID: id, Node: m.opts.NodeName, Namespace: m.ns, Runtime: req.Runtime,
		TTY: req.Spec.TTY, Stdin: req.Spec.Stdin, Nice: req.Nice,
		LeaseTTL: m.tun.leaseTTL, KillGrace: m.tun.killGrace, SampleEvery: m.tun.sampleEvery,
		MemMax: memCap(req.Alloc.Mem, memAvailable(), req.Spec.Req.Mem > 0), CPUs: req.Alloc.CPUs,
		CtlPath: ctlPath(dir, id), Slot: req.Slot,
	}
	if m.tun.memMax > 0 {
		spec.MemMax = m.tun.memMax
	}
	switch req.Runtime {
	case proto.RuntimeNative, "":
		spec.Runtime = proto.RuntimeNative
		err = m.nativeSpec(&spec, &req, s, dir)
	case proto.RuntimeContainer:
		// Until the job is tracked (it then counts as active), no cache
		// trim may start; one running now is stopped.
		defer m.holdCaches(ctx)()
		err = m.containerSpec(ctx, &spec, &req, s)
	default:
		err = fmt.Errorf("%w: unknown runtime %q", ErrRuntimeUnavailable, req.Runtime)
	}
	if err != nil {
		return nil, err
	}
	if spec.Runtime == proto.RuntimeNative && m.systemdRun != "" {
		spec.Cgroup, spec.Scope = true, m.scopeName(id)
	}
	b, err := spec.marshal()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, fileSpec), b, 0o600); err != nil {
		return nil, fmt.Errorf("job spec: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileLease), nil, 0o600); err != nil {
		return nil, fmt.Errorf("job lease: %w", err)
	}
	m.pool.setJob(s, id)
	if err := m.launch(ctx, dir, &spec); err != nil {
		m.pool.setJob(s, "")
		return nil, fmt.Errorf("start job on %s: %w", m.opts.NodeName, err)
	}
	ok = true
	return &process{js: m.track(id, dir, spec, req.OnPhase, s)}, nil
}

// nativeSpec fills spec for the native runtime.
func (m *manager) nativeSpec(spec *superSpec, req *StartRequest, s *slot, dir string) error {
	root := m.pool.hostPath(s)
	he := m.hostEnv()
	tp, err := planToolchains(he, req.Spec.Req.Toolchains, m.opts.NodeName)
	if err != nil {
		return err
	}
	spec.ToolchainNote = tp.note
	tmp := filepath.Join(dir, dirTmp)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fmt.Errorf("job tmp: %w", err)
	}
	spec.Env = nativeEnv(he, req, m.opts.NodeName, root, tmp, tp)
	spec.Argv = nativeArgv(req.Spec.Argv, he.path, tp, he.mise)
	rel := path.Clean("/" + req.Spec.Workspace.Rel)[1:]
	spec.Dir = filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return fmt.Errorf("job dir in slot: %w", err)
	}
	if st := m.setupFor(req, s, root); st != nil {
		st.Argv = []string{"/bin/sh", "-c", st.Run}
		st.Dir = root
		if len(tp.mise) > 0 && he.mise != "" {
			st.Argv = append(append(append([]string{he.mise, "exec"}, tp.mise...), "--"), st.Argv...)
		}
		spec.Setup = st
	}
	return nil
}

// setupFor decides whether the project setup must run in this slot: the
// spec's setup or the auto-detected one, when its key (command + content
// of the key files) differs from the last successful run in the slot.
func (m *manager) setupFor(req *StartRequest, s *slot, hostRoot string) *setupStep {
	mf := m.pool.manifestOf(s)
	var hashes map[string]string
	if mf != nil {
		hashes = make(map[string]string, len(mf.Entries))
		for _, e := range mf.Entries {
			if e.Kind == proto.EntryFile {
				hashes[e.Path] = e.Hash
			}
		}
	}
	exists := func(rel string) bool {
		if hashes != nil {
			_, ok := hashes[rel]
			return ok
		}
		if hostRoot == "" {
			return false
		}
		_, err := os.Lstat(filepath.Join(hostRoot, filepath.FromSlash(rel)))
		return err == nil
	}
	sp := req.Spec.Setup
	if sp == nil {
		sp = workspace.SetupFor(exists, req.Spec.Argv)
	}
	if sp == nil || strings.TrimSpace(sp.Run) == "" {
		return nil
	}
	h := sha256.New()
	h.Write([]byte(sp.Run))
	keys := append([]string(nil), sp.Key...)
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		if hashes != nil {
			h.Write([]byte(hashes[k]))
		} else if hostRoot != "" {
			if b, err := os.ReadFile(filepath.Join(hostRoot, filepath.FromSlash(k))); err == nil {
				sum := sha256.Sum256(b)
				h.Write(sum[:])
			}
		}
	}
	key := hex.EncodeToString(h.Sum(nil))
	keyFile := m.pool.setupKeyPath(s)
	if b, err := os.ReadFile(keyFile); err == nil && strings.TrimSpace(string(b)) == key {
		return nil
	}
	return &setupStep{Run: sp.Run, KeyFile: keyFile, Key: key}
}

func (m *manager) Attach(ctx context.Context, jobID string) (Process, error) {
	if err := validJobID(jobID); err != nil {
		return nil, err
	}
	m.jobsMu.Lock()
	js, ok := m.jobs[jobID]
	m.jobsMu.Unlock()
	if ok {
		return &process{js: js}, nil
	}
	dir := m.l.job(jobID)
	var spec superSpec
	if err := readJSON(filepath.Join(dir, fileSpec), &spec); err != nil {
		return nil, fmt.Errorf("%w %s on %s", ErrUnknownJob, jobID, m.opts.NodeName)
	}
	m.jobsMu.Lock()
	if js, ok := m.jobs[jobID]; ok {
		m.jobsMu.Unlock()
		return &process{js: js}, nil
	}
	m.jobsMu.Unlock()
	return &process{js: m.track(jobID, dir, spec, nil, nil)}, nil
}

func (m *manager) Active() []string {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	var out []string
	for id, js := range m.jobs {
		if !js.isDone() {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (m *manager) Run(ctx context.Context) error {
	// The runner image is built on the first container job (see
	// ensureImage), not here: most nodes never run one, and a build at
	// every start would cost minutes of CPU and gigabytes of disk.
	m.Reap(ctx)
	reap := time.NewTicker(m.tun.reapEvery)
	defer reap.Stop()
	trim := time.NewTicker(m.tun.trimEvery)
	defer trim.Stop()
	refresh := time.NewTicker(10 * time.Minute)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-reap.C:
			if err := m.Reap(ctx); err != nil {
				m.logf("reaper: %v", err)
			}
		case <-trim.C:
			m.trimBlobs()
		case <-refresh.C:
			m.Refresh(ctx)
		}
	}
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
