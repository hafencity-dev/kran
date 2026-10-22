package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

// targetSide serves jobs that other nodes send here. Two-phase admission:
// an origin first reserves resources (admit), which are held for
// ReservationTTL until job.start; all admissions are serialized under one
// lock so two origins can never overbook this node.
type targetSide struct {
	d *Daemon

	mu    sync.Mutex
	jobs  map[string]*tjob
	local map[string]localAlloc // this node's own local jobs
}

type localAlloc struct {
	res   proto.Resources
	sched bool // the scheduler of this daemon tracks it
}

type tjob struct {
	ID         string
	Origin     string
	Alloc      proto.Resources
	Runtime    proto.RuntimeKind
	Slot       *proto.SlotRef
	manifest   *proto.Manifest
	sess       transport.Session // session that admitted it
	reservedAt time.Time
	busy       int // in-flight prestart operations (holds the reservation)
	started    bool
	ended      bool
	endedAt    time.Time
	watching   bool

	attachMu sync.Mutex // serializes attaching through execenv
	procMu   sync.Mutex // guards proc and phase
	proc     execenv.Process

	phase   proto.Phase
	phaseCh chan struct{}
}

type tjobRecord struct {
	Origin  string            `json:"origin"`
	Alloc   proto.Resources   `json:"alloc"`
	Runtime proto.RuntimeKind `json:"runtime"`
	Slot    *proto.SlotRef    `json:"slot,omitempty"`
}

const targetJobsKey = "target.jobs"

func newTargetSide(d *Daemon) *targetSide {
	return &targetSide{d: d, jobs: map[string]*tjob{}, local: map[string]localAlloc{}}
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
			recs[id] = tjobRecord{Origin: j.Origin, Alloc: j.Alloc, Runtime: j.Runtime, Slot: j.Slot}
		}
	}
	b, _ := json.Marshal(recs)
	go t.d.kvSet(targetJobsKey, string(b))
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
	for _, id := range ex.Active() {
		r := recs[id]
		j := &tjob{ID: id, Origin: r.Origin, Alloc: r.Alloc, Runtime: r.Runtime, Slot: r.Slot,
			started: true, phaseCh: make(chan struct{}), reservedAt: t.d.now()}
		t.jobs[id] = j
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
	floor := int64(10 << 30)
	if pct := info.DiskTotal / 20; pct > floor {
		floor = pct
	}
	if m.DiskFree < floor {
		return fmt.Sprintf("disk: %s free on %s, below the %s floor", humanBytes(m.DiskFree), t.d.self, humanBytes(floor))
	}
	return ""
}

// allocatedLocked sums every allocation on this node; pendingMem is the
// part reserved but not started (not yet visible in live memory).
func (t *targetSide) allocatedLocked(rt proto.RuntimeKind) (all proto.Resources, pendingMem int64, rtMem int64) {
	for _, j := range t.jobs {
		if j.ended {
			continue
		}
		all.CPUs += j.Alloc.CPUs
		all.Mem += j.Alloc.Mem
		if !j.started {
			pendingMem += j.Alloc.Mem
		}
		if j.Runtime == rt {
			rtMem += j.Alloc.Mem
		}
	}
	for _, l := range t.local {
		all.CPUs += l.res.CPUs
		all.Mem += l.res.Mem
	}
	return
}

type admitError struct {
	code string
	msg  string
}

func (e *admitError) Error() string { return e.msg }

func (t *targetSide) admit(id, origin string, alloc proto.Resources, rt proto.RuntimeKind, sess transport.Session) error {
	d := t.d
	if d.deps.Exec == nil {
		return &admitError{codeNotAccepting, fmt.Sprintf("%s cannot run jobs for other nodes (its executor failed to start; see kran doctor %s)", d.self, d.self)}
	}
	if !d.cfg.Accept.Enabled {
		return &admitError{codeNotAccepting, fmt.Sprintf("%s does not accept jobs (accept.enabled = false)", d.self)}
	}
	if d.draining.Load() {
		return &admitError{codeDraining, fmt.Sprintf("%s is draining", d.self)}
	}
	if r := t.diskFloorReason(); r != "" {
		return &admitError{codeDisk, r}
	}
	capacity := t.selfCapacity()
	live := d.deps.Prober.Sample()
	rtInfo := runtimeInfo(d.nodeInfo(), rt)

	t.mu.Lock()
	defer t.mu.Unlock()
	if j := t.jobs[id]; j != nil && !j.ended {
		if j.started {
			return &admitError{codeJobStarted, fmt.Sprintf("job %s already started on %s", id, d.self)}
		}
		j.reservedAt = d.now()
		j.sess = sess
		return nil
	}
	if max := d.cfg.Accept.MaxJobs; max > 0 {
		n := 0
		for _, j := range t.jobs {
			if !j.ended {
				n++
			}
		}
		if n >= max {
			return &admitError{codeBusy, fmt.Sprintf("%s runs its maximum of %d jobs", d.self, max)}
		}
	}
	used, pending, rtUsed := t.allocatedLocked(rt)
	freeMem := capacity.Mem - used.Mem
	if live.MemAvailable > 0 {
		if lf := live.MemAvailable - d.cfg.Accept.ReserveMem - pending; lf < freeMem {
			freeMem = lf
		}
	}
	if rtInfo != nil && rtInfo.Mem > 0 {
		if rf := rtInfo.Mem - rtUsed; rf < freeMem {
			freeMem = rf
		}
	}
	freeCPU := capacity.CPUs - used.CPUs
	alone := used.Mem == 0 && used.CPUs == 0 && alloc.Mem > capacity.Mem
	if !alone && (alloc.Mem > freeMem || alloc.CPUs > freeCPU*1.25) {
		return &admitError{codeBusy, fmt.Sprintf("needs %s and %.1f CPUs, %s has %s and %.1f CPUs free",
			humanBytes(alloc.Mem), alloc.CPUs, d.self, humanBytes(max64(freeMem, 0)), maxf(freeCPU, 0))}
	}
	t.jobs[id] = &tjob{ID: id, Origin: origin, Alloc: alloc, Runtime: rt, sess: sess,
		reservedAt: d.now(), phaseCh: make(chan struct{})}
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

// addLocal records a local job of this node in the admission table so
// remote origins see the memory as taken.
func (t *targetSide) addLocal(id string, res proto.Resources, sched bool) {
	t.mu.Lock()
	t.local[id] = localAlloc{res: res, sched: sched}
	t.mu.Unlock()
}

func (t *targetSide) removeLocal(id string) {
	t.mu.Lock()
	delete(t.local, id)
	t.mu.Unlock()
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
	st := peerStatus{
		Info:     d.nodeInfo(),
		Metrics:  d.deps.Prober.Sample(),
		Capacity: t.selfCapacity(),
		Reserve:  proto.Resources{CPUs: d.cfg.Accept.ReserveCPUs, Mem: d.cfg.Accept.ReserveMem},
		Accepts:  d.cfg.Accept.Enabled && d.deps.Exec != nil,
		Draining: d.draining.Load(),
		Degraded: t.diskFloorReason(),
	}
	var procs []execenv.Process
	t.mu.Lock()
	for _, j := range t.jobs {
		if !j.ended {
			st.Allocs = append(st.Allocs, allocInfo{JobID: j.ID, Origin: j.Origin, Res: j.Alloc, Running: j.started})
			var p execenv.Process
			if j.started {
				p = j.process()
			}
			procs = append(procs, p)
		}
	}
	for id, l := range t.local {
		st.Allocs = append(st.Allocs, allocInfo{JobID: id, Origin: d.self, Res: l.res, Running: true})
	}
	t.mu.Unlock()
	// Sampling happens outside the admission lock.
	for i, p := range procs {
		if p != nil {
			l := p.Live()
			st.Allocs[i].Live = &l
		}
	}
	sort.Slice(st.Allocs, func(i, j int) bool { return st.Allocs[i].JobID < st.Allocs[j].JobID })
	return st
}

// selfAllocs returns what other origins hold here and the local jobs the
// scheduler does not know (restored after a restart).
func (t *targetSide) foreignAllocs() proto.Resources {
	var r proto.Resources
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, j := range t.jobs {
		if !j.ended {
			r.CPUs += j.Alloc.CPUs
			r.Mem += j.Alloc.Mem
		}
	}
	for _, l := range t.local {
		if !l.sched {
			r.CPUs += l.res.CPUs
			r.Mem += l.res.Mem
		}
	}
	return r
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
	if err == nil {
		_, err = p.Wait(ctx)
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
	slot := j.Slot
	t.persistLocked()
	t.mu.Unlock()
	if slot != nil && t.d.deps.Exec != nil {
		t.d.deps.Exec.ReleaseSlot(*slot)
	}
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
