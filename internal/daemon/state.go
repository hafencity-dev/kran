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
	nodes := []proto.NodeState{d.selfNodeState(snap)}
	for _, p := range d.peers.list() {
		ns := p.nodeState()
		if f, ok := snap.Free[ns.Info.Name]; ok {
			ns.Free = f
		}
		if r := snap.Unavailable[ns.Info.Name]; r != "" && ns.Status == proto.NodeOnline {
			ns.Reason = r
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
		jobs[i] = publicJob(jobs[i])
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

// selfNodeState describes this node: gross capacity, the scheduler's own
// totals (local jobs and admitted guests) and the guests themselves.
func (d *Daemon) selfNodeState(snap scheduler.Snapshot) proto.NodeState {
	info := d.nodeInfo()
	ns := proto.NodeState{
		Info:          info,
		Metrics:       d.deps.Prober.Sample(),
		Status:        proto.NodeOnline,
		Self:          true,
		Capacity:      d.target.selfCapacity(),
		Reserve:       proto.Resources{CPUs: d.cfg.Accept.ReserveCPUs, Mem: d.cfg.Accept.ReserveMem},
		Accepts:       d.cfg.Accept.Enabled,
		LastSeen:      d.now(),
		Link:          proto.LinkInfo{Connected: true},
		MaxJobs:       d.cfg.Accept.MaxJobs,
		Allocated:     snap.Allocated[d.self],
		AllocatedJobs: append([]string{}, snap.Jobs[d.self]...),
		Free:          snap.Free[d.self],
		Running:       snap.Running[d.self],
		Guests:        d.target.guests(),
	}
	if d.draining.Load() {
		ns.Status, ns.Reason = proto.NodeDraining, d.self+" is draining (undo: kran resume "+d.self+")"
	} else if r := d.target.diskFloorReason(); r != "" {
		ns.Status, ns.Reason = proto.NodeDegraded, r
	} else if r := snap.Unavailable[d.self]; r != "" {
		ns.Reason = r
	}
	return ns
}

// publishSelf pushes this node to the scheduler and the UI.
func (d *Daemon) publishSelf() {
	ns := d.selfNodeState(d.deps.Sched.Snapshot())
	d.deps.Sched.UpdateNode(ns)
	d.bus.publish(proto.EventNode, ns)
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
		d.target.sampleLive()
		ns := d.selfNodeState(d.deps.Sched.Snapshot())
		d.deps.Sched.UpdateNode(ns)
		d.history.add(d.self, ns.Metrics)
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
		d.deps.Sched.UpdateJobUsage(e.id, r)
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
	now := t.now().Local()
	day := now.Format("2006-01-02")
	if day != t.day {
		t.day = day
		t.s = proto.Stats{CPUSecondsByNode: map[string]float64{},
			Since: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)}
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
