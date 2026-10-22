package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/store"
)

// jobTable holds the jobs this node originated: active ones in memory with
// their driver state, and the most recent finished ones for the state view.
type jobTable struct {
	d *Daemon

	mu     sync.Mutex
	active map[string]*jobEntry
	recent []proto.Job // newest first
}

const recentJobs = 100

func newJobTable(d *Daemon) *jobTable {
	return &jobTable{d: d, active: map[string]*jobEntry{}}
}

// jobEntry is one active job. The driver goroutine owns its lifecycle; run
// sessions attach to it as viewers and controllers.
type jobEntry struct {
	d     *Daemon
	id    string
	spool *spool

	mu       sync.Mutex
	job      proto.Job
	placed   bool // the scheduler holds an allocation
	attached int
	finished bool
	restored bool
	started  bool // local: CLI reported started; remote: target reached running
	// streaming: job.start succeeded; signals go to the target.
	streaming bool
	// exitReported: the CLI sent the exit of a local job (its disconnect
	// right after is not a cancel).
	exitReported bool

	cancelOnce   sync.Once
	cancelCh     chan struct{}
	cancelReason string
	cancelSignal string // signal name implied by the cancel (INT for Ctrl-C)
	done         chan struct{}
	attachCh     chan struct{} // closed and replaced on every attach

	localCh chan localEvent
	pid     int
	pgid    int
	scope   string

	outbox       *outbox // remote: frames for the target
	lastPhasePub time.Time
	tty          *proto.TTY
}

type localEvent struct {
	started *proto.Started
	exit    *proto.Exit
}

func (t *jobTable) newEntry(job proto.Job, sp *spool) *jobEntry {
	e := &jobEntry{
		d:        t.d,
		id:       job.Spec.ID,
		spool:    sp,
		job:      job,
		cancelCh: make(chan struct{}),
		done:     make(chan struct{}),
		attachCh: make(chan struct{}),
		localCh:  make(chan localEvent, 4),
		outbox:   newOutbox(),
		tty:      job.Spec.TTY,
	}
	return e
}

func (t *jobTable) add(e *jobEntry) {
	t.mu.Lock()
	t.active[e.id] = e
	t.mu.Unlock()
}

func (t *jobTable) get(id string) *jobEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active[id]
}

func (t *jobTable) list() []*jobEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*jobEntry, 0, len(t.active))
	for _, e := range t.active {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (t *jobTable) retire(e *jobEntry, final proto.Job) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.active, e.id)
	t.recent = append([]proto.Job{final}, t.recent...)
	if len(t.recent) > recentJobs {
		t.recent = t.recent[:recentJobs]
	}
}

func (t *jobTable) loadRecent() {
	ctx, cancel := t.d.storeCtx()
	defer cancel()
	jobs, err := t.d.deps.Store.ListJobs(ctx, store.JobQuery{Limit: recentJobs})
	if err != nil {
		t.d.logf("store: list jobs: %v", err)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, j := range jobs {
		if j.State.Terminal() {
			t.recent = append(t.recent, j)
		}
	}
}

// snapshot returns active jobs (oldest first) followed by recent finished.
func (t *jobTable) snapshot() []proto.Job {
	entries := t.list()
	out := make([]proto.Job, 0, len(entries)+recentJobs)
	for _, e := range entries {
		out = append(out, e.snapshot())
	}
	t.mu.Lock()
	out = append(out, t.recent...)
	t.mu.Unlock()
	return out
}

func (t *jobTable) find(id string) (proto.Job, bool) {
	if e := t.get(id); e != nil {
		return e.snapshot(), true
	}
	t.mu.Lock()
	for _, j := range t.recent {
		if j.Spec.ID == id {
			t.mu.Unlock()
			return j, true
		}
	}
	t.mu.Unlock()
	ctx, cancel := t.d.storeCtx()
	defer cancel()
	j, err := t.d.deps.Store.GetJob(ctx, id)
	if err != nil || j == nil {
		return proto.Job{}, false
	}
	return *j, true
}

func (t *jobTable) persistAll() {
	for _, e := range t.list() {
		e.mu.Lock()
		e.persistLocked()
		e.mu.Unlock()
	}
}

func (t *jobTable) closeSpools() {
	for _, e := range t.list() {
		e.spool.close()
	}
}

// restore resumes jobs that were active when the daemon stopped.
func (t *jobTable) restore() {
	d := t.d
	ctx, cancel := d.storeCtx()
	jobs, err := d.deps.Store.ActiveJobs(ctx)
	cancel()
	if err != nil {
		d.logf("store: active jobs: %v", err)
		return
	}
	for _, j := range jobs {
		if !validID(j.Spec.ID) || j.Origin != "" && j.Origin != d.self {
			continue
		}
		sp, err := openSpool(filepath.Join(d.deps.Dirs.spoolDir(), j.Spec.ID))
		if err != nil {
			d.logf("job %s: reopen output spool: %v", j.Spec.ID, err)
			continue
		}
		e := t.newEntry(j, sp)
		e.restored = true
		t.add(e)
		mode := driveResubmit
		switch {
		case j.Local:
			mode = driveRestoreLocal
		case j.Node != "" && j.Node != d.self && (j.State == proto.JobSetup || j.State == proto.JobRunning):
			mode = driveRestoreRemote
		}
		d.logf("job %s: resuming after restart (%s, %s)", j.Spec.ID, j.State, mode)
		d.startDriver(e, mode)
	}
}

// --- entry ---

func (e *jobEntry) snapshot() proto.Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneJob(e.job)
}

func (e *jobEntry) persistLocked() {
	j := cloneJob(e.job)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.d.deps.Store.PutJob(ctx, &j); err != nil {
		e.d.logf("store: save job %s: %v", e.id, err)
	}
}

// update applies fn, persists and publishes the job.
func (e *jobEntry) update(fn func(j *proto.Job)) proto.Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return cloneJob(e.job)
	}
	fn(&e.job)
	e.persistLocked()
	snap := cloneJob(e.job)
	e.d.bus.publish(proto.EventJob, publicJob(snap))
	return snap
}

func (e *jobEntry) setState(s proto.JobState, reason string) {
	e.update(func(j *proto.Job) {
		j.State = s
		if reason != "" {
			j.Reason = reason
		}
		if s == proto.JobRunning && j.StartedAt.IsZero() {
			j.StartedAt = e.d.now().UTC()
			j.RunningAt = j.StartedAt
			j.Phase = nil
		}
	})
}

func (e *jobEntry) markPlaced(p scheduler.Placement) {
	e.update(func(j *proto.Job) {
		e.placed = true
		j.Node = p.Node
		j.Runtime = p.Runtime
		j.Local = p.Local
		j.Alloc = p.Alloc
		j.Reason = p.Reason
		if p.Fallback {
			j.Fallback = true
		}
		j.QueuePos = 0
		j.PlacedAt = e.d.now().UTC()
	})
}

func (e *jobEntry) isPlaced() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.placed
}

// unplace forgets the scheduler allocation (after Reject, which frees it).
func (e *jobEntry) unplace() {
	e.mu.Lock()
	e.placed = false
	e.mu.Unlock()
}

func (e *jobEntry) notice(kind, msg string) {
	b, _ := json.Marshal(proto.Notice{Kind: kind, Message: msg, JobID: e.id})
	e.spool.append(proto.FrameNotice, b)
}

// phase records a sync/setup progress report: to the CLI (spool) and, at
// most twice a second unless the state changes, to Job.Phase for the UI.
func (e *jobEntry) phase(ph proto.Phase) {
	e.sendFrame(proto.FramePhase, ph)
	if ph.State == proto.JobRunning {
		return
	}
	now := e.d.now()
	e.mu.Lock()
	skip := e.job.Phase != nil && e.job.Phase.State == ph.State && now.Sub(e.lastPhasePub) < 500*time.Millisecond && ph.Progress < 1
	if !skip {
		e.lastPhasePub = now
	}
	e.mu.Unlock()
	if !skip {
		p := ph
		e.update(func(j *proto.Job) { j.Phase = &p })
	}
}

func (e *jobEntry) sendFrame(t proto.FrameType, v any) {
	b, _ := json.Marshal(v)
	if err := e.spool.append(t, b); err != nil {
		e.d.logf("job %s: %v", e.id, err)
	}
}

// requestCancel asks the driver to stop the job. sig names the signal the
// user meant (INT for Ctrl-C); it shapes the exit code of a job that never
// started.
func (e *jobEntry) requestCancel(reason, sig string) {
	e.cancelOnce.Do(func() {
		e.mu.Lock()
		e.cancelReason = reason
		e.cancelSignal = sig
		e.mu.Unlock()
		close(e.cancelCh)
	})
}

func (e *jobEntry) canceled() (bool, string, string) {
	select {
	case <-e.cancelCh:
		e.mu.Lock()
		defer e.mu.Unlock()
		return true, e.cancelReason, e.cancelSignal
	default:
		return false, "", ""
	}
}

func (e *jobEntry) attach() {
	e.mu.Lock()
	e.attached++
	close(e.attachCh)
	e.attachCh = make(chan struct{})
	e.mu.Unlock()
}

// detach is called when a run session ends. The last CLI going away
// without an exit cancels the job (SPEC §6.1: closing the CLI connection is
// a cancel), except while the daemon itself shuts down.
func (e *jobEntry) detach() {
	e.mu.Lock()
	e.attached--
	left := e.attached
	fin := e.finished || e.exitReported
	e.mu.Unlock()
	if left > 0 || fin || e.d.stopping.Load() {
		return
	}
	e.requestCancel("the kran run that started it went away", "TERM")
}

func (e *jobEntry) attachedCount() (int, <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.attached, e.attachCh
}

// finish records the terminal state exactly once: spool, store, events,
// scheduler release, estimates and stats.
func (e *jobEntry) finish(x proto.Exit) {
	d := e.d
	e.mu.Lock()
	if e.finished {
		e.mu.Unlock()
		return
	}
	j := &e.job
	if x.State == "" {
		x.State = stateForExit(x)
	}
	x.JobID = e.id
	if x.Node == "" {
		x.Node = j.Node
	}
	if x.Node == "" {
		x.Node = d.self
	}
	x.Fallback = x.Fallback || j.Fallback
	j.Fallback = x.Fallback
	j.Phase = nil
	j.State = x.State
	code := x.Code
	j.ExitCode = &code
	j.Signal = x.Signal
	// The target measures the command; the origin measured the sync.
	if x.Usage.SyncBytes == 0 && x.Usage.SyncSeconds == 0 {
		x.Usage.SyncBytes, x.Usage.SyncSeconds = j.Usage.SyncBytes, j.Usage.SyncSeconds
	}
	j.Usage = x.Usage
	j.EndedAt = d.now().UTC()
	j.Node = x.Node
	j.Live = nil
	j.QueuePos = 0
	if x.Reason != "" {
		j.Reason = x.Reason
	}
	e.persistLocked()
	final := cloneJob(*j)
	placed := e.placed
	e.placed = false
	e.finished = true
	d.bus.publish(proto.EventJob, publicJob(final))
	e.mu.Unlock()

	e.sendFrame(proto.FrameExit, x)
	e.spool.finish()
	e.spool.close() // readers keep their own handles
	if placed {
		u := x.Usage
		d.deps.Sched.Release(e.id, &u)
	}
	if final.State == proto.JobSucceeded || final.State == proto.JobFailed {
		d.deps.Est.Observe(&final)
	}
	d.stats.record(final)
	d.bus.publish(proto.EventStats, d.stats.snapshot())
	d.jobs.retire(e, final)
	close(e.done)
	d.publishQueue()
	d.publishSelf()
}

// publicJob is a job as the API shows it: without the forwarded
// environment (secret-filtered already, but personal and never shown).
func publicJob(j proto.Job) proto.Job {
	j.Spec.Env = nil
	return j
}

func stateForExit(x proto.Exit) proto.JobState {
	if x.Code == 0 && x.Signal == "" {
		return proto.JobSucceeded
	}
	return proto.JobFailed
}

// --- outbox: frames from the origin to the target, kept across reattach ---

type outbox struct {
	mu     sync.Mutex
	frames []outFrame
	bytes  int
	notify chan struct{}
}

type outFrame struct {
	t proto.FrameType
	p []byte
}

const outboxMax = 16 << 20

func newOutbox() *outbox { return &outbox{notify: make(chan struct{}, 1)} }

func (o *outbox) wake() {
	select {
	case o.notify <- struct{}{}:
	default:
	}
}

// put queues a frame; data beyond outboxMax is refused (control frames are
// always accepted).
func (o *outbox) put(t proto.FrameType, p []byte) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if t == proto.FrameStdin && o.bytes+len(p) > outboxMax {
		return false
	}
	o.frames = append(o.frames, outFrame{t, append([]byte(nil), p...)})
	o.bytes += len(p)
	o.wake()
	return true
}

func (o *outbox) take() []outFrame {
	o.mu.Lock()
	defer o.mu.Unlock()
	f := o.frames
	o.frames = nil
	o.bytes = 0
	return f
}
