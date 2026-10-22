package daemon

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/metrics"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/store"
	"github.com/hafencity-dev/kran/internal/version"
)

// State is the full snapshot served at /api/v1/state and as the first SSE
// event.
func (d *Daemon) State() proto.State {
	snap := d.deps.Sched.Snapshot()
	nodes := []proto.NodeState{d.selfNodeState(snap, false)}
	for _, p := range d.peers.list() {
		ns := p.nodeState()
		ns.Capacity = p.grossCapacity()
		if f, ok := snap.Free[ns.Info.Name]; ok {
			ns.Free = f
		}
		nodes = append(nodes, ns)
	}
	jobs := d.jobs.snapshot()
	pos := make(map[string]int, len(snap.Queue))
	for i, id := range snap.Queue {
		pos[id] = i + 1
	}
	owners := map[string]*proto.OwnerStat{}
	var order []string
	for i := range jobs {
		j := &jobs[i]
		if j.State == proto.JobQueued {
			j.QueuePos = pos[j.Spec.ID]
			if r := snap.Reasons[j.Spec.ID]; r != "" {
				j.Reason = r
			}
		}
		if j.State.Terminal() {
			continue
		}
		key := string(j.Spec.Owner.Kind) + "/" + j.Spec.Owner.ID + "/" + j.Spec.Owner.Name
		o := owners[key]
		if o == nil {
			o = &proto.OwnerStat{Owner: j.Spec.Owner}
			owners[key] = o
			order = append(order, key)
		}
		if j.State == proto.JobQueued {
			o.Queued++
		} else {
			o.Running++
		}
	}
	st := proto.State{
		Version: version.Info(),
		Self:    d.self,
		Nodes:   nodes,
		Jobs:    jobs,
		Queue:   snap.Queue,
		Stats:   d.stats.snapshot(),
	}
	if st.Queue == nil {
		st.Queue = []string{}
	}
	if st.Jobs == nil {
		st.Jobs = []proto.Job{}
	}
	st.Owners = []proto.OwnerStat{}
	for _, k := range order {
		st.Owners = append(st.Owners, *owners[k])
	}
	return st
}

// grossCapacity is the peer's schedulable capacity before subtracting
// anyone's allocations (for display).
func (p *peer) grossCapacity() proto.Resources {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st.Capacity
}

// selfNodeState describes this node. forScheduler subtracts what remote
// origins (and restored local jobs) hold, so the scheduler only accounts
// for its own placements.
func (d *Daemon) selfNodeState(snap scheduler.Snapshot, forScheduler bool) proto.NodeState {
	info := d.nodeInfo()
	m := d.deps.Prober.Sample()
	capacity := d.target.selfCapacity()
	foreign := d.target.foreignAllocs()
	ns := proto.NodeState{
		Info:     info,
		Metrics:  m,
		Status:   proto.NodeOnline,
		Self:     true,
		Capacity: capacity,
		Reserve:  proto.Resources{CPUs: d.cfg.Accept.ReserveCPUs, Mem: d.cfg.Accept.ReserveMem},
		Accepts:  d.cfg.Accept.Enabled,
		LastSeen: d.now(),
		Link:     proto.LinkInfo{Connected: true},
	}
	if forScheduler {
		ns.Capacity.CPUs = maxf(capacity.CPUs-foreign.CPUs, 0)
		ns.Capacity.Mem = max64(capacity.Mem-foreign.Mem, 0)
	}
	if d.draining.Load() {
		ns.Status, ns.Reason = proto.NodeDraining, d.self+" is draining (undo: kran resume "+d.self+")"
	} else if r := d.target.diskFloorReason(); r != "" {
		ns.Status, ns.Reason = proto.NodeDegraded, r
	}
	a := snap.Allocated[d.self]
	ns.Allocated = proto.Resources{CPUs: a.CPUs + foreign.CPUs, Mem: a.Mem + foreign.Mem}
	ns.Free = snap.Free[d.self]
	ns.Running = snap.Running[d.self] + d.target.runningRemote()
	return ns
}

// publishSelf pushes this node to the scheduler and the UI.
func (d *Daemon) publishSelf() {
	snap := d.deps.Sched.Snapshot()
	d.deps.Sched.UpdateNode(d.selfNodeState(snap, true))
	d.bus.publish(proto.EventNode, d.selfNodeState(snap, false))
}

func (d *Daemon) publishState() {
	d.bus.publish(proto.EventState, d.State())
}

func (d *Daemon) publishQueue() {
	q := d.deps.Sched.Snapshot().Queue
	if q == nil {
		q = []string{}
	}
	d.bus.publish(proto.EventQueue, proto.QueueEvent{Queue: q})
}

// selfLoop refreshes this node's state every MetricsEvery: scheduler input,
// throttled metrics for the UI, live usage of local jobs, scheduler ticks.
func (d *Daemon) selfLoop(ctx context.Context) {
	tk := time.NewTicker(d.t.MetricsEvery)
	defer tk.Stop()
	lastSig := ""
	for {
		d.refreshInfo(false)
		snap := d.deps.Sched.Snapshot()
		d.deps.Sched.UpdateNode(d.selfNodeState(snap, true))
		ns := d.selfNodeState(snap, false)
		sig := fmt.Sprintf("%s|%s|%d", ns.Status, ns.Reason, ns.Running)
		if sig != lastSig {
			lastSig = sig
			d.bus.publish(proto.EventNode, ns)
		} else {
			d.bus.publishMetrics(d.self, ns.Metrics)
		}
		d.sampleLocalJobs()
		d.deps.Sched.Tick(d.now())
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}

// sampleLocalJobs updates Live for local jobs from their process groups.
func (d *Daemon) sampleLocalJobs() {
	for _, e := range d.jobs.list() {
		e.mu.Lock()
		pgid, started, local := e.pgid, e.started, e.job.Local
		e.mu.Unlock()
		if !local || !started || pgid <= 1 {
			continue
		}
		r, err := d.deps.Prober.JobUsage(metrics.UsageRef{PGID: pgid})
		if err != nil {
			continue
		}
		e.update(func(j *proto.Job) { j.Live = &r })
	}
}

func (d *Daemon) sliceLoop(ctx context.Context) {
	tk := time.NewTicker(d.t.SliceEvery)
	defer tk.Stop()
	for {
		if d.deps.Slice.Available() {
			d.deps.Slice.Refresh()
		}
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}

// --- stats ---

type statsTracker struct {
	mu  sync.Mutex
	now func() time.Time
	day string
	s   proto.Stats
}

func newStatsTracker(now func() time.Time) *statsTracker {
	return &statsTracker{now: now, s: proto.Stats{CPUSecondsByNode: map[string]float64{}}}
}

func (t *statsTracker) rollLocked() {
	day := t.now().Local().Format("2006-01-02")
	if day != t.day {
		t.day = day
		t.s = proto.Stats{CPUSecondsByNode: map[string]float64{}}
	}
}

func (t *statsTracker) record(j proto.Job) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollLocked()
	if j.EndedAt.Local().Format("2006-01-02") != t.day {
		return
	}
	t.s.JobsToday++
	offloaded := !j.Local && j.Node != "" && j.Node != j.Origin
	if offloaded {
		t.s.OffloadedToday++
		if j.State == proto.JobSucceeded || j.State == proto.JobFailed {
			saved := j.Estimate.Duration
			if j.Estimate.Samples == 0 || saved <= 0 {
				saved = j.Usage.WallSeconds
			}
			t.s.SavedSecondsToday += saved
		}
	}
	if j.Fallback {
		t.s.FallbacksToday++
	}
	if j.State == proto.JobInfraErr || j.State == proto.JobLost {
		t.s.FailedInfraToday++
	}
	if j.Node != "" && j.Usage.CPUSeconds > 0 {
		t.s.CPUSecondsByNode[j.Node] += j.Usage.CPUSeconds
	}
}

func (t *statsTracker) snapshot() proto.Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollLocked()
	s := t.s
	s.CPUSecondsByNode = make(map[string]float64, len(t.s.CPUSecondsByNode))
	for k, v := range t.s.CPUSecondsByNode {
		s.CPUSecondsByNode[k] = v
	}
	return s
}

// rebuild recomputes today's stats from the history.
func (t *statsTracker) rebuild(d *Daemon) {
	now := d.now().Local()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	ctx, cancel := d.storeCtx()
	defer cancel()
	jobs, err := d.deps.Store.ListJobs(ctx, store.JobQuery{Since: midnight, Limit: 100000})
	if err != nil {
		return
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Spec.ID < jobs[j].Spec.ID })
	for _, j := range jobs {
		if j.State.Terminal() {
			t.record(j)
		}
	}
}
