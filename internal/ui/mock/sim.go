// Package mock simulates a small kran mesh (nodes, jobs, queue, metrics,
// logs) for developing and testing the web UI and `kran top` without real
// daemons. It is never imported by the kran binary.
package mock

import (
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Options shape the simulation.
type Options struct {
	Seed uint64
	// ExtraNodes adds synthetic Linux build nodes to the three-machine fleet
	// (gx10, devmac, e2e) for load testing the UI.
	ExtraNodes int
	// TargetActive is how many jobs the simulation keeps queued or running
	// (0 = a realistic 6..10 with bursts).
	TargetActive int
	// Now is the clock (tests inject a fake one).
	Now func() time.Time
	// History is how many finished jobs to pre-populate (default 60).
	History int
}

// ErrNotFound is returned for unknown jobs or nodes.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when an action does not apply in the current state.
var ErrConflict = errors.New("conflict")

// Sim is a deterministic (given Seed and clock) simulation of one daemon's
// view of the mesh. All methods are safe for concurrent use.
type Sim struct {
	mu    sync.Mutex
	opt   Options
	now   func() time.Time
	rng   *rand.Rand
	nodes []*simNode
	jobs  map[string]*simJob
	// finished holds terminal job IDs, oldest first.
	finished []string
	queue    []string
	stats    proto.Stats
	seq      uint64
	subs     map[int]chan proto.Event
	nextSub  int
	idSeq    uint32
	lastID   int64
	burstEnd time.Time
}

type simJob struct {
	job      proto.Job
	tmpl     *template
	phaseEnd time.Time // end of syncing/setup/running
	runFor   float64
	log      logBuf
	fail     bool
}

// New builds a simulation with a pre-populated history so the UI has
// something to show immediately.
func New(opt Options) *Sim {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.History == 0 {
		opt.History = 60
	}
	s := &Sim{
		opt:  opt,
		now:  opt.Now,
		rng:  rand.New(rand.NewPCG(opt.Seed, opt.Seed^0x9e3779b97f4a7c15)),
		jobs: map[string]*simJob{},
		subs: map[int]chan proto.Event{},
	}
	now := s.now()
	s.nodes = fleet(now, opt.ExtraNodes)
	s.stats = proto.Stats{CPUSecondsByNode: map[string]float64{}}
	s.seedHistory(now)
	for _, n := range s.nodes {
		s.sampleMetrics(n, now, 2)
	}
	// Start with some work in flight.
	for i := 0; i < 14; i++ {
		s.submit(now.Add(-time.Duration(s.rng.IntN(90)) * time.Second))
	}
	s.recompute() // free capacity must be known before the first placement
	s.schedule(now)
	s.recompute()
	return s
}

func (s *Sim) self() *simNode { return s.nodes[0] }

func (s *Sim) node(name string) *simNode {
	for _, n := range s.nodes {
		if n.name() == name {
			return n
		}
	}
	return nil
}

// State returns a snapshot as the daemon's /api/v1/state would.
func (s *Sim) State() *proto.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *Sim) stateLocked() *proto.State {
	st := &proto.State{
		Version: proto.VersionInfo{Version: "0.4.0", Commit: "mock", BuildDate: "2026-10-22", API: proto.APIVersion, Protocol: "1.0"},
		Self:    s.self().name(),
		Queue:   append([]string{}, s.queue...),
		Stats:   copyStats(s.stats),
	}
	for _, n := range s.nodes {
		st.Nodes = append(st.Nodes, n.st)
	}
	var active []proto.Job
	for _, j := range s.jobs {
		if !j.job.State.Terminal() {
			active = append(active, j.job)
		}
	}
	sort.Slice(active, func(a, b int) bool { return active[a].Spec.ID < active[b].Spec.ID })
	st.Jobs = append(st.Jobs, active...)
	for i := len(s.finished) - 1; i >= 0 && i >= len(s.finished)-100; i-- {
		st.Jobs = append(st.Jobs, s.jobs[s.finished[i]].job)
	}
	st.Owners = s.ownerStats()
	return st
}

func (s *Sim) ownerStats() []proto.OwnerStat {
	by := map[string]*proto.OwnerStat{}
	var order []string
	for _, j := range s.jobs {
		if j.job.State.Terminal() {
			continue
		}
		o := j.job.Spec.Owner
		os, ok := by[o.ID]
		if !ok {
			os = &proto.OwnerStat{Owner: o}
			by[o.ID] = os
			order = append(order, o.ID)
		}
		if j.job.State == proto.JobQueued {
			os.Queued++
		} else {
			os.Running++
		}
	}
	sort.Strings(order)
	out := make([]proto.OwnerStat, 0, len(order))
	for _, id := range order {
		out = append(out, *by[id])
	}
	return out
}

func copyStats(st proto.Stats) proto.Stats {
	m := make(map[string]float64, len(st.CPUSecondsByNode))
	for k, v := range st.CPUSecondsByNode {
		m[k] = v
	}
	st.CPUSecondsByNode = m
	return st
}

// Subscribe returns a channel of events whose first element is a full
// state. The channel is closed when cancel is called or when the
// subscriber falls too far behind (as the daemon does to slow clients).
func (s *Sim) Subscribe() (<-chan proto.Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan proto.Event, 512)
	// The state carries everything up to the current seq; the next delta
	// is seq+1, so each subscriber sees a contiguous sequence.
	ch <- proto.Event{Type: proto.EventState, Seq: s.seq, Time: s.now().UTC(), Data: s.stateLocked()}
	id := s.nextSub
	s.nextSub++
	s.subs[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
	}
}

func (s *Sim) emit(t proto.EventType, data any) {
	s.seq++
	ev := proto.Event{Type: t, Seq: s.seq, Time: s.now().UTC(), Data: data}
	for id, ch := range s.subs {
		select {
		case ch <- ev:
		default:
			delete(s.subs, id)
			close(ch)
		}
	}
}

func (s *Sim) emitJob(j *simJob) { s.emit(proto.EventJob, j.job) }

// Run advances the simulation in real time until ctx ends.
func (s *Sim) Run(done <-chan struct{}) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			s.Step()
		}
	}
}

// Jobs returns history newest first, filtered like /api/v1/jobs.
func (s *Sim) Jobs(limit int, owner, node, state string) []proto.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]*simJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		all = append(all, j)
	}
	sort.Slice(all, func(a, b int) bool { return all[a].job.Spec.ID > all[b].job.Spec.ID })
	var out []proto.Job
	for _, j := range all {
		if owner != "" && j.job.Spec.Owner.ID != owner && j.job.Spec.Owner.Name != owner {
			continue
		}
		if node != "" && j.job.Node != node {
			continue
		}
		if state != "" && string(j.job.State) != state {
			continue
		}
		out = append(out, j.job)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Job returns one job.
func (s *Sim) Job(id string) (proto.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return proto.Job{}, ErrNotFound
	}
	return j.job, nil
}

// Cancel ends a queued or running job.
func (s *Sim) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return ErrNotFound
	}
	if j.job.State.Terminal() {
		return ErrConflict
	}
	now := s.now()
	j.log.append(now, "\r\nkran: canceled from the dashboard\r\n")
	s.finish(j, now, proto.JobCanceled, 130, "INT")
	s.schedule(now)
	s.recompute()
	return nil
}

// SetPriority changes a job's priority; queued jobs are re-ordered.
func (s *Sim) SetPriority(id string, p proto.Priority) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return ErrNotFound
	}
	switch p {
	case proto.PriorityHigh, proto.PriorityNormal, proto.PriorityLow:
	default:
		return ErrConflict
	}
	if j.job.State.Terminal() {
		return ErrConflict
	}
	j.job.Spec.Priority = p
	s.emitJob(j)
	s.schedule(s.now())
	s.recompute()
	return nil
}

// Drain stops new placements on a node; Resume undoes it.
func (s *Sim) Drain(name string) error  { return s.setDrain(name, true) }
func (s *Sim) Resume(name string) error { return s.setDrain(name, false) }

func (s *Sim) setDrain(name string, drain bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.node(name)
	if n == nil {
		return ErrNotFound
	}
	switch {
	case drain && n.st.Status == proto.NodeOnline:
		n.st.Status = proto.NodeDraining
		n.st.Reason = "drained from the dashboard"
	case !drain && n.st.Status == proto.NodeDraining:
		n.st.Status = proto.NodeOnline
		n.st.Reason = ""
	default:
		return ErrConflict
	}
	s.emit(proto.EventNode, n.st)
	s.emit(proto.EventLog, map[string]any{"level": "info", "msg": name + map[bool]string{true: " drained", false: " resumed"}[drain], "time": s.now().UTC()})
	s.schedule(s.now())
	s.recompute()
	return nil
}
