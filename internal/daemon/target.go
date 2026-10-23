package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/transport"
)

// targetSide serves jobs that other nodes send here. Two-phase admission:
// an origin first reserves resources (admit), which are held for
// ReservationTTL until job.start; all admissions are serialized under one
// lock so two origins can never overbook this node.
type targetSide struct {
	d *Daemon

	mu   sync.Mutex
	jobs map[string]*tjob
	// persistSeq numbers record snapshots (under mu); writes run outside
	// mu and skip a snapshot older than the one already written.
	persistSeq uint64
	writeMu    sync.Mutex
	written    uint64
	// manifests: recent slot.diff manifests by origin and hash, the bases
	// of later deltas (peer protocol 1.1).
	manifests *manifestCache
}

type tjob struct {
	ID         string
	Origin     string
	Alloc      proto.Resources
	Runtime    proto.RuntimeKind
	Owner      proto.Owner
	Argv       []string
	Slot       *proto.SlotRef
	manifest   *proto.Manifest
	blobs      map[string]int64  // manifest file hash → size: what blob.put takes
	sess       transport.Session // session that admitted it
	reservedAt time.Time
	busy       int // in-flight prestart operations (holds the reservation)
	slotOps    int // those of them working in the slot
	// canceled: the origin gave the job up while an operation was in
	// flight; the last one to finish releases (or stops) it.
	canceled bool
	// starting is open while a job.start runs Exec.Start: whether the job
	// starts is not known until it closes.
	starting chan struct{}
	started  bool
	ended    bool
	endedAt  time.Time
	watching bool

	attachMu sync.Mutex // serializes attaching through execenv
	procMu   sync.Mutex // guards proc and phase
	proc     execenv.Process

	phase   proto.Phase
	phaseCh chan struct{}
	// in holds the job's input across streams (created by the first one);
	// startedHere: this daemon started the job, so it saw all its input.
	in          *stdinPump
	startedHere bool
	live        *proto.Resources // sampled every MetricsEvery
}

type tjobRecord struct {
	Origin  string            `json:"origin"`
	Alloc   proto.Resources   `json:"alloc"`
	Runtime proto.RuntimeKind `json:"runtime"`
	Slot    *proto.SlotRef    `json:"slot,omitempty"`
	Owner   proto.Owner       `json:"owner"`
	Argv    []string          `json:"argv,omitempty"`
}

const targetJobsKey = "target.jobs"

func newTargetSide(d *Daemon) *targetSide {
	return &targetSide{d: d, jobs: map[string]*tjob{}, manifests: newManifestCache(manifestCacheEntries)}
}

func (t *targetSide) get(id string) *tjob {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.jobs[id]
}

// persistLocked saves the started, unfinished jobs so a restarted daemon
// can account for them and release their slots.
func (t *targetSide) persistLocked() {
	recs := map[string]tjobRecord{}
	for id, j := range t.jobs {
		if j.started && !j.ended {
			recs[id] = tjobRecord{Origin: j.Origin, Alloc: j.Alloc, Runtime: j.Runtime, Slot: j.Slot, Owner: j.Owner, Argv: j.Argv}
		}
	}
	b, _ := json.Marshal(recs)
	t.persistSeq++
	seq := t.persistSeq
	go func() {
		t.writeMu.Lock()
		defer t.writeMu.Unlock()
		if seq < t.written {
			return // a newer snapshot is already stored
		}
		t.d.kvSet(targetJobsKey, string(b))
		t.written = seq
	}()
}

// restore rebuilds the job table after a restart: supervisors kept running
// while the daemon was down.
func (t *targetSide) restore() {
	ex := t.d.deps.Exec
	if ex == nil {
		return
	}
	recs := map[string]tjobRecord{}
	if v, ok := t.d.kvGet(targetJobsKey); ok && v != "" {
		json.Unmarshal([]byte(v), &recs)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	active := ex.Active()
	live := map[string]bool{}
	for _, id := range active {
		live[id] = true
	}
	for id, r := range recs {
		// Ended while the daemon was down: execenv keeps its slot busy
		// until it is released with the recorded SlotRef.
		if !live[id] && r.Slot != nil {
			ex.ReleaseSlot(*r.Slot)
			t.d.logf("target: job %s from %s ended while the daemon was down; released its slot", id, r.Origin)
		}
	}
	for _, id := range active {
		r := recs[id]
		j := &tjob{ID: id, Origin: r.Origin, Alloc: r.Alloc, Runtime: r.Runtime, Slot: r.Slot, Owner: r.Owner, Argv: r.Argv,
			started: true, phaseCh: make(chan struct{}), reservedAt: t.d.now()}
		t.jobs[id] = j
		// Account for it again; it runs whether or not it still fits.
		if _, err := t.d.deps.Sched.Admit(j.asJob(), j.Runtime); err != nil {
			t.d.logf("target: job %s is not counted by the scheduler after the restart: %v", id, err)
		}
		t.d.logf("target: job %s from %s still running; waiting for its origin to reattach", id, r.Origin)
		t.startWatchLocked(j)
	}
	t.persistLocked()
}

// --- admission ---

func (t *targetSide) selfCapacity() proto.Resources {
	info := t.d.nodeInfo()
	c := proto.Resources{
		CPUs: float64(info.CPUs) - t.d.cfg.Accept.ReserveCPUs,
		Mem:  info.MemTotal - t.d.cfg.Accept.ReserveMem,
	}
	if c.CPUs < 0 {
		c.CPUs = 0
	}
	if c.Mem < 0 {
		c.Mem = 0
	}
	return c
}

func (t *targetSide) diskFloorReason() string {
	info := t.d.nodeInfo()
	m := t.d.deps.Prober.Sample()
	if m.DiskFree <= 0 || info.DiskTotal <= 0 {
		return ""
	}
	floor := proto.DiskFloor(info.DiskTotal)
	if m.DiskFree < floor {
		return fmt.Sprintf("disk: %s free on %s, below the %s floor", humanBytes(m.DiskFree), t.d.self, humanBytes(floor))
	}
	return ""
}

type admitError struct {
	code string
	msg  string
}

func (e *admitError) Error() string { return e.msg }

func (j *tjob) asJob() *proto.Job {
	return &proto.Job{
		Spec:     proto.JobSpec{ID: j.ID, Argv: j.Argv, Owner: j.Owner},
		Estimate: proto.Estimate{CPUs: j.Alloc.CPUs, Mem: j.Alloc.Mem, Source: "origin"},
		Origin:   j.Origin,
	}
}

// admit reserves resources for a job another origin wants to run here.
// The scheduler decides (Admit is serialized there, so two origins cannot
// overbook this node); this table adds the reservation TTL, slots and the
// job's lifecycle on this side.
func (t *targetSide) admit(id, origin string, b admitBody, sess transport.Session) error {
	d := t.d
	if d.deps.Exec == nil {
		return &admitError{codeNotAccepting, fmt.Sprintf("%s cannot run jobs for other nodes (its executor failed to start; see kran doctor %s)", d.self, d.self)}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if j := t.jobs[id]; j != nil && !j.ended {
		if j.Origin != origin { // IDs are not secret; never follow another origin's job
			return &admitError{codeBusy, fmt.Sprintf("job id %s is already in use on %s by another node; run the job again", id, d.self)}
		}
		if j.started {
			return &admitError{codeJobStarted, fmt.Sprintf("job %s already started on %s", id, d.self)}
		}
		j.reservedAt = d.now()
		j.sess = sess
		return nil
	}
	j := &tjob{ID: id, Origin: origin, Alloc: b.Alloc, Runtime: b.Runtime, Owner: b.Owner, Argv: b.Argv,
		sess: sess, reservedAt: d.now(), phaseCh: make(chan struct{})}
	job := j.asJob()
	job.Estimate.Duration = b.Duration
	job.Spec.Workspace.RepoID = b.RepoID
	pl, err := d.deps.Sched.Admit(job, b.Runtime)
	if err != nil {
		var ae *scheduler.AdmitError
		if errors.As(err, &ae) {
			return &admitError{ae.Code, ae.Reason}
		}
		return &admitError{codeInternal, err.Error()}
	}
	j.Alloc = pl.Alloc
	t.jobs[id] = j
	return nil
}

func runtimeInfo(info proto.NodeInfo, rt proto.RuntimeKind) *proto.Runtime {
	for i := range info.Runtimes {
		if info.Runtimes[i].Kind == rt {
			return &info.Runtimes[i]
		}
	}
	return nil
}

// cancelLocked drops a reservation the origin gave up. With an operation
// in flight (slot.acquire, commit, job.start …) the slot is still in use:
// the operation releases it when it ends (end), or stops the job it
// started (handleJobStart).
func (t *targetSide) cancelLocked(j *tjob, why string) {
	if j.started {
		return
	}
	if j.slotOps > 0 {
		j.canceled = true
		return
	}
	t.releaseLocked(j, why)
}

// releaseLocked drops a reservation that never started.
func (t *targetSide) releaseLocked(j *tjob, why string) {
	if j.started {
		return
	}
	if j.Slot != nil && t.d.deps.Exec != nil {
		t.d.deps.Exec.ReleaseSlot(*j.Slot)
	}
	j.Slot = nil
	delete(t.jobs, j.ID)
	t.d.deps.Sched.Release(j.ID, nil)
	t.d.logf("target: released reservation of job %s from %s (%s)", j.ID, j.Origin, why)
}

// sessionClosed releases reservations made over a session that ended
// before their job started: the origin's pipeline died with it.
func (t *targetSide) sessionClosed(s transport.Session) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, j := range t.jobs {
		if j.sess == s && !j.started && j.busy == 0 {
			t.releaseLocked(j, "origin connection closed")
		}
	}
}

func (t *targetSide) sweepLoop(ctx context.Context) {
	every := t.d.t.ReservationTTL / 6
	if every < 50*time.Millisecond {
		every = 50 * time.Millisecond
	}
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		t.sweep()
	}
}

func (t *targetSide) sweep() {
	now := t.d.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, j := range t.jobs {
		switch {
		case !j.started && j.busy == 0 && now.Sub(j.reservedAt) > t.d.t.ReservationTTL:
			t.releaseLocked(j, "no job.start within "+t.d.t.ReservationTTL.String())
		case j.ended && now.Sub(j.endedAt) > 10*time.Minute:
			delete(t.jobs, id)
		}
	}
}

// status is what this node tells subscribed peers.
func (t *targetSide) status() peerStatus {
	d := t.d
	snap := d.deps.Sched.Snapshot()
	st := peerStatus{
		Info:          d.nodeInfo(),
		Metrics:       d.deps.Prober.Sample(),
		Capacity:      t.selfCapacity(),
		Reserve:       proto.Resources{CPUs: d.cfg.Accept.ReserveCPUs, Mem: d.cfg.Accept.ReserveMem},
		Accepts:       d.cfg.Accept.Enabled && d.deps.Exec != nil,
		Draining:      d.draining.Load(),
		Degraded:      t.diskFloorReason(),
		MaxJobs:       d.cfg.Accept.MaxJobs,
		Allocated:     snap.Allocated[d.self],
		AllocatedJobs: append([]string{}, snap.Jobs[d.self]...),
		Running:       snap.Running[d.self],
		Free:          snap.Free[d.self],
		Guests:        t.guests(),
	}
	t.mu.Lock()
	for _, j := range t.jobs {
		if !j.ended {
			st.Allocs = append(st.Allocs, allocInfo{JobID: j.ID, Origin: j.Origin, Res: j.Alloc, Running: j.started, Live: j.live})
		}
	}
	t.mu.Unlock()
	sort.Slice(st.Allocs, func(i, j int) bool { return st.Allocs[i].JobID < st.Allocs[j].JobID })
	return st
}

// guests lists the jobs other origins run (or reserved) here.
func (t *targetSide) guests() []proto.JobRef {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []proto.JobRef
	for _, j := range t.jobs {
		if j.ended {
			continue
		}
		out = append(out, proto.JobRef{ID: j.ID, Origin: j.Origin, Owner: j.Owner,
			Argv: append([]string(nil), j.Argv...), Alloc: j.Alloc, Live: j.live})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// sampleLive measures running guest jobs and tells the scheduler (called
// every MetricsEvery; Live() runs outside the lock).
func (t *targetSide) sampleLive() {
	type item struct {
		j *tjob
		p execenv.Process
	}
	var items []item
	t.mu.Lock()
	for _, j := range t.jobs {
		if j.started && !j.ended {
			if p := j.process(); p != nil {
				items = append(items, item{j, p})
			}
		}
	}
	t.mu.Unlock()
	for _, it := range items {
		l := it.p.Live()
		t.mu.Lock()
		it.j.live = &l
		t.mu.Unlock()
		t.d.deps.Sched.UpdateJobUsage(it.j.ID, l)
	}
}

func (t *targetSide) runningRemote() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, j := range t.jobs {
		if j.started && !j.ended {
			n++
		}
	}
	return n
}

// --- job processes ---

func (j *tjob) setPhase(p proto.Phase) {
	// called by execenv from its own goroutines
	j.procMu.Lock()
	j.phase = p
	close(j.phaseCh)
	j.phaseCh = make(chan struct{})
	j.procMu.Unlock()
}

func (j *tjob) currentPhase() (proto.Phase, <-chan struct{}) {
	j.procMu.Lock()
	defer j.procMu.Unlock()
	return j.phase, j.phaseCh
}

func (j *tjob) process() execenv.Process {
	j.procMu.Lock()
	defer j.procMu.Unlock()
	return j.proc
}

// ensureProc returns the job's process, attaching through execenv when this
// daemon did not start it (restart).
func (t *targetSide) ensureProc(ctx context.Context, j *tjob) (execenv.Process, error) {
	j.attachMu.Lock()
	defer j.attachMu.Unlock()
	if p := j.process(); p != nil {
		return p, nil
	}
	p, err := t.d.deps.Exec.Attach(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	j.procMu.Lock()
	j.proc = p
	j.procMu.Unlock()
	return p, nil
}

func (j *tjob) setProc(p execenv.Process) {
	j.procMu.Lock()
	j.proc = p
	j.procMu.Unlock()
}

func (t *targetSide) startWatchLocked(j *tjob) {
	if j.watching {
		return
	}
	j.watching = true
	t.d.goLoop(func(ctx context.Context) { t.watch(ctx, j) })
}

// watch waits for the job to end, then frees its slot and allocation.
func (t *targetSide) watch(ctx context.Context, j *tjob) {
	p, err := t.ensureProc(ctx, j)
	var ex proto.Exit
	if err == nil {
		ex, err = p.Wait(ctx)
	}
	if ctx.Err() != nil {
		return // daemon stopping: the supervisor lives on; the next daemon resumes
	}
	if err != nil {
		t.d.logf("target: job %s: %v", j.ID, err)
	}
	t.mu.Lock()
	j.ended = true
	j.endedAt = t.d.now()
	if j.in != nil {
		j.in.close()
	}
	slot := j.Slot
	t.persistLocked()
	t.mu.Unlock()
	if slot != nil && t.d.deps.Exec != nil {
		t.d.deps.Exec.ReleaseSlot(*slot)
	}
	u := ex.Usage
	t.d.deps.Sched.Release(j.ID, &u)
	t.d.publishSelf()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB"} {
		f /= unit
		if f < unit {
			if f >= 10 {
				return fmt.Sprintf("%.0f %s", f, u)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
	}
	return fmt.Sprintf("%.0f PiB", f/unit)
}

// inputPump returns the job's input queue, starting its writer on first use.
func (t *targetSide) inputPump(j *tjob, proc execenv.Process) *stdinPump {
	t.mu.Lock()
	defer t.mu.Unlock()
	if j.in != nil {
		return j.in
	}
	q := newStdinPump(t.d.t.StdinQueue, j.startedHere)
	j.in = q
	if j.ended {
		q.close()
		return q
	}
	t.d.goLoop(func(ctx context.Context) {
		w := proc.Stdin()
		if w == nil {
			q.close()
			return
		}
		q.run(ctx, w)
	})
	return q
}

// reservations counts admitted jobs that have not started yet (they would
// not survive a daemon restart).
func (t *targetSide) reservations() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, j := range t.jobs {
		if !j.started && !j.ended {
			n++
		}
	}
	return n
}
