package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Tunables (SPEC §6.3 where it names them).
const (
	cpuOvercommit       = 1.25
	shortJobSeconds     = 5.0
	defaultStarvation   = 2 * time.Minute
	defaultMaxRemote    = 2
	breakerThreshold    = 3
	breakerBaseCooldown = 30 * time.Second
	breakerMaxCooldown  = 5 * time.Minute
	// A job may wait for a busy but clearly better node at most this long
	// after submission before it takes what is free.
	maxDefer = 60 * time.Second
	// Deferring must beat running now by this factor.
	deferGain = 0.8
	// Memory a new job has not touched yet: assumed fully pending for this
	// long after placement (local) when no live usage is reported.
	rampLocal  = 10 * time.Second
	rampRemote = 60 * time.Second
)

var prioWeight = map[proto.Priority]float64{
	proto.PriorityHigh:   4,
	proto.PriorityNormal: 2,
	proto.PriorityLow:    1,
}

var prioRank = map[proto.Priority]int{
	proto.PriorityHigh:   2,
	proto.PriorityNormal: 1,
	proto.PriorityLow:    0,
}

// sjob is the scheduler's copy of a job: the daemon keeps mutating its own
// *proto.Job, so nothing is shared.
type sjob struct {
	id          string
	owner       string
	prio        proto.Priority
	seq         uint64
	queuedAt    time.Time
	est         proto.Estimate
	fp          string
	repo        string
	req         proto.Requirements
	noFallback  bool
	interactive bool
	t           *ticket

	alloc    *allocation // non-nil once placed
	excluded map[string]string
	rejects  int
	requeued bool
	reason   string
	shape    string // cached shapeKey; reset when priority changes
}

type allocation struct {
	job     string
	node    string
	rt      proto.RuntimeKind
	vm      bool // counts against the node's container VM pool too
	res     proto.Resources
	at      time.Time
	end     time.Time // expected end, for wait estimates
	live    *proto.Resources
	liveAt  time.Time
	foreign bool // admitted for another origin (Admit), or restored (Restore)
	vmKnown bool // vm was derived from the node's runtimes (false: restored before they were known)
	repo    string
	pl      Placement
}

type breaker struct {
	fails     int
	openUntil time.Time
	cooldown  time.Duration
	lastErr   string
}

type nodeEntry struct {
	st     proto.NodeState
	allocs map[string]*allocation
	brk    breaker
}

type ownerState struct {
	vt     float64 // virtual time (WFQ)
	queued int
}

type costEWMA struct {
	v float64
	n int
}

type sched struct {
	mu   sync.Mutex
	opts Options

	nodes   map[string]*nodeEntry
	jobs    map[string]*sjob // own jobs, queued or placed
	foreign map[string]*allocation
	owners  map[string]*ownerState
	seq     uint64
	vnow    float64 // system virtual time
	// syncCost learns seconds of sync+setup per repo\x00node.
	syncCost map[string]*costEWMA

	// ignorePressure scores without memory/I/O contention (simulator
	// baseline only).
	ignorePressure bool

	// Results of the last dispatch pass.
	order []string
	holds map[string]hold // node → resources held for a starving job
}

type hold struct {
	job string
	res proto.Resources
	vm  bool
}

func newScheduler(opts Options) Scheduler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.StarvationAfter <= 0 {
		opts.StarvationAfter = defaultStarvation
	}
	if opts.MaxRemoteAttempts <= 0 {
		opts.MaxRemoteAttempts = defaultMaxRemote
	}
	return &sched{
		opts:     opts,
		nodes:    map[string]*nodeEntry{},
		jobs:     map[string]*sjob{},
		foreign:  map[string]*allocation{},
		owners:   map[string]*ownerState{},
		syncCost: map[string]*costEWMA{},
		holds:    map[string]hold{},
	}
}

func ownerKey(o proto.Owner) string {
	id := o.ID
	if id == "" {
		id = o.Name
	}
	return string(o.Kind) + ":" + id
}

func (s *sched) Submit(job *proto.Job) Ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := job.Spec.ID
	if id == "" {
		return resolvedTicket(id, Placement{}, &UnplaceableError{Reason: "job has no ID"})
	}
	if _, dup := s.jobs[id]; dup {
		return resolvedTicket(id, Placement{}, &UnplaceableError{Reason: fmt.Sprintf("job %s is already scheduled", id)})
	}
	now := s.opts.Now()
	s.seq++
	j := &sjob{
		id:          id,
		owner:       ownerKey(job.Spec.Owner),
		prio:        job.Spec.Priority,
		seq:         s.seq,
		queuedAt:    job.QueuedAt,
		est:         job.Estimate,
		fp:          job.Fingerprint,
		repo:        job.Spec.Workspace.RepoID,
		req:         job.Spec.Req,
		noFallback:  job.Spec.NoFallback,
		interactive: job.Spec.TTY != nil,
		t:           newTicket(id),
	}
	if j.req.Toolchains != nil {
		tc := make(map[string]string, len(j.req.Toolchains))
		for k, v := range j.req.Toolchains {
			tc[k] = v
		}
		j.req.Toolchains = tc
	}
	if j.queuedAt.IsZero() {
		j.queuedAt = now
	} else {
		// QueuedAt is wall time (persisted across restarts). Re-anchor it on
		// now, which carries the monotonic clock in production, so queue age
		// is immune to wall-clock steps from here on; a QueuedAt in the
		// future (the clock went back) counts as age zero.
		j.queuedAt = now.Add(-max(0, now.Sub(j.queuedAt)))
	}
	if _, ok := prioWeight[j.prio]; !ok {
		// Interactive jobs have someone watching the terminal (SPEC §6.3).
		j.prio = proto.PriorityNormal
		if j.interactive {
			j.prio = proto.PriorityHigh
		}
	}
	normalizeEstimate(&j.est)
	if err := s.validate(j); err != nil {
		return resolvedTicket(id, Placement{}, err)
	}
	s.jobs[id] = j
	s.enqueueOwner(j)
	s.dispatch(now)
	return j.t
}

func normalizeEstimate(e *proto.Estimate) {
	if e.CPUs <= 0 {
		e.CPUs = 1
	}
	if e.Mem <= 0 {
		e.Mem = gib
	}
	if e.Duration <= 0 {
		e.Duration = 30
	}
}

// validate rejects jobs whose flags can never be satisfied, so they fail
// fast instead of waiting forever.
func (s *sched) validate(j *sjob) error {
	r := j.req
	origin := s.opts.Origin
	local := r.Prefer == proto.PreferLocal || r.OriginOnly
	switch {
	case r.Prefer == proto.PreferRemote && r.OriginOnly:
		why := r.OriginOnlyReason
		if why == "" {
			why = "this job can only run here"
		}
		return &UnplaceableError{Reason: why + ", but --remote was given"}
	case r.Prefer == proto.PreferRemote && r.Node == origin && origin != "":
		return &UnplaceableError{Reason: fmt.Sprintf("--remote and --node %s conflict: %s is this machine", r.Node, r.Node)}
	case local && r.Node != "" && r.Node != origin:
		if r.OriginOnly && r.OriginOnlyReason != "" {
			return &UnplaceableError{Reason: fmt.Sprintf("%s, so it cannot run on %s", r.OriginOnlyReason, r.Node)}
		}
		return &UnplaceableError{Reason: fmt.Sprintf("--local and --node %s conflict", r.Node)}
	}
	if r.Node != "" {
		if _, ok := s.nodes[r.Node]; !ok {
			return &UnplaceableError{Reason: fmt.Sprintf("no machine named %q (known: %s). Link it with: kran link %s",
				r.Node, strings.Join(s.nodeNames(), ", "), r.Node)}
		}
	}
	if r.Prefer == proto.PreferRemote {
		remote := 0
		for name := range s.nodes {
			if name != origin {
				remote++
			}
		}
		if remote == 0 {
			return &UnplaceableError{Reason: "--remote was given but no other machine is linked (link one with: kran link <host>)"}
		}
	}
	if r.GPU && !local {
		known, any := 0, false
		for _, n := range s.nodes {
			if n.st.Info.CPUs == 0 {
				continue // never probed; may have a GPU
			}
			known++
			if s.gpuMatch(n) {
				any = true
			}
		}
		if known == len(s.nodes) && known > 0 && !any {
			return &UnplaceableError{Reason: "no node has " + s.gpuWord()}
		}
	}
	return nil
}

func (s *sched) nodeNames() []string {
	names := make([]string, 0, len(s.nodes))
	for n := range s.nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (s *sched) enqueueOwner(j *sjob) {
	o := s.owners[j.owner]
	if o == nil {
		o = &ownerState{}
		s.owners[j.owner] = o
	}
	if o.queued == 0 && o.vt < s.vnow {
		// An owner returning from idle starts at the current virtual time:
		// idling earns no credit to monopolize the queue later.
		o.vt = s.vnow
	}
	o.queued++
}

func (s *sched) dequeueOwner(j *sjob) {
	if o := s.owners[j.owner]; o != nil {
		o.queued--
		if o.queued <= 0 {
			o.queued = 0
		}
	}
}

func (s *sched) Cancel(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	if j == nil || j.alloc != nil {
		return
	}
	s.removeQueued(j)
	j.t.resolve(Placement{}, ErrCanceled)
	s.dispatch(s.opts.Now())
}

func (s *sched) removeQueued(j *sjob) {
	delete(s.jobs, j.id)
	s.dequeueOwner(j)
	for i, id := range s.order {
		if id == j.id {
			s.order = append(s.order[:i:i], s.order[i+1:]...)
			break
		}
	}
}

func (s *sched) Release(jobID string, usage *proto.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	if a := s.foreign[jobID]; a != nil {
		delete(s.foreign, jobID)
		s.freeAlloc(a)
		s.dispatch(now)
		return
	}
	j := s.jobs[jobID]
	if j == nil {
		return
	}
	if j.alloc == nil {
		s.removeQueued(j)
		j.t.resolve(Placement{}, ErrCanceled)
		s.dispatch(now)
		return
	}
	a := j.alloc
	delete(s.jobs, jobID)
	s.freeAlloc(a)
	if usage != nil && a.node != s.opts.Origin {
		if usage.WallSeconds > 0 {
			// The job started there: the node works.
			if n := s.nodes[a.node]; n != nil {
				n.brk = breaker{}
			}
		}
		if c := usage.SyncSeconds + usage.SetupSeconds; usage.WallSeconds > 0 && c >= 0 && a.repo != "" {
			s.learnSyncCost(a.repo, a.node, c)
		}
	}
	s.dispatch(now)
}

func (s *sched) freeAlloc(a *allocation) {
	if n := s.nodes[a.node]; n != nil {
		delete(n.allocs, a.job)
	}
}

func (s *sched) Reject(jobID string, node string, err error) Ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	j := s.jobs[jobID]
	if j == nil {
		return resolvedTicket(jobID, Placement{}, &UnplaceableError{Reason: fmt.Sprintf("job %s is not scheduled", jobID)})
	}
	if j.alloc == nil {
		// Already queued again (double Reject): keep the live ticket.
		return j.t
	}
	s.freeAlloc(j.alloc)
	j.alloc = nil
	msg := "start failed"
	if err != nil {
		msg = err.Error()
	}
	if j.excluded == nil {
		j.excluded = map[string]string{}
	}
	j.excluded[node] = msg
	// A node that is full (another origin got there first; our view of it
	// lags) is busy, not broken: no breaker, no used-up remote attempt.
	var ae *AdmitError
	busy := errors.As(err, &ae) && (ae.Code == "busy" || ae.Code == "max_jobs")
	if node != s.opts.Origin && !busy {
		j.rejects++
		if n := s.nodes[node]; n != nil {
			s.tripBreaker(n, msg, now)
		}
	}
	j.requeued = true
	j.t = newTicket(jobID)
	if !s.canStillRun(j) {
		delete(s.jobs, jobID)
		reason := fmt.Sprintf("%s failed to start the job (%s) and no other node can run it", node, msg)
		switch {
		case j.req.Prefer == proto.PreferRemote:
			reason += "; rerun without --remote to run it here"
		case j.noFallback:
			reason += "; rerun without --no-fallback to run it here"
		}
		j.t.resolve(Placement{}, &UnplaceableError{Reason: reason})
		return j.t
	}
	s.enqueueOwner(j)
	s.dispatch(now)
	return j.t
}

// canStillRun reports whether any node is still allowed for the job after
// exclusions (ignoring transient state such as resources or offline).
func (s *sched) canStillRun(j *sjob) bool {
	for name := range s.nodes {
		if _, bad := j.excluded[name]; bad {
			continue
		}
		if s.policyAllows(j, name) {
			return true
		}
	}
	return false
}

func (s *sched) tripBreaker(n *nodeEntry, msg string, now time.Time) {
	b := &n.brk
	b.fails++
	b.lastErr = msg
	if b.fails < breakerThreshold {
		return
	}
	if b.cooldown == 0 {
		b.cooldown = breakerBaseCooldown
	} else if now.After(b.openUntil) {
		// Failed again right after a cool-down (half-open probe): back off.
		b.cooldown *= 2
		if b.cooldown > breakerMaxCooldown {
			b.cooldown = breakerMaxCooldown
		}
	}
	b.openUntil = now.Add(b.cooldown)
	s.opts.Logf("scheduler: %s failed %d starts in a row (%s); not placing jobs there for %s", n.st.Info.Name, b.fails, msg, b.cooldown)
}

func (s *sched) UpdateNode(n proto.NodeState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := n.Info.Name
	if name == "" {
		return
	}
	e := s.nodes[name]
	if e == nil {
		e = &nodeEntry{allocs: map[string]*allocation{}}
		s.nodes[name] = e
	}
	e.st = n
	for _, a := range e.allocs {
		if !a.vmKnown {
			a.vm, a.vmKnown = runsInVM(n.Info, a.rt)
		}
	}
	s.dispatch(s.opts.Now())
}

func (s *sched) RemoveNode(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, name)
	delete(s.holds, name)
	s.dispatch(s.opts.Now())
}

func (s *sched) SetPriority(jobID string, p proto.Priority) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	if j == nil || j.alloc != nil {
		return
	}
	if _, ok := prioWeight[p]; !ok {
		return
	}
	j.prio = p
	j.shape = ""
	s.dispatch(s.opts.Now())
}

func (s *sched) Kick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatch(s.opts.Now())
}

func (s *sched) Tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.IsZero() {
		now = s.opts.Now()
	}
	s.dispatch(now)
}

func (s *sched) UpdateJobUsage(jobID string, live proto.Resources) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.foreign[jobID]
	if a == nil {
		if j := s.jobs[jobID]; j != nil {
			a = j.alloc
		}
	}
	if a == nil {
		return
	}
	l := live
	a.live = &l
	a.liveAt = s.opts.Now()
}

func (s *sched) learnSyncCost(repo, node string, sec float64) {
	k := repo + "\x00" + node
	c := s.syncCost[k]
	if c == nil {
		c = &costEWMA{}
		s.syncCost[k] = c
	}
	if c.n == 0 {
		c.v = sec
	} else {
		c.v = 0.3*sec + 0.7*c.v
	}
	c.n++
}

func (s *sched) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	snap := Snapshot{
		Queue:       append([]string(nil), s.order...),
		Reasons:     make(map[string]string, len(s.order)),
		Allocated:   make(map[string]proto.Resources, len(s.nodes)),
		Free:        make(map[string]proto.Resources, len(s.nodes)),
		Running:     make(map[string]int, len(s.nodes)),
		Jobs:        make(map[string][]string, len(s.nodes)),
		Unavailable: map[string]string{},
		Placements:  map[string]Placement{},
	}
	for _, id := range s.order {
		if j := s.jobs[id]; j != nil {
			snap.Reasons[id] = j.reason
		}
	}
	for _, name := range s.nodeNames() {
		v := s.view(name, now)
		snap.Allocated[name] = proto.Resources{CPUs: v.host.allocCPU, Mem: v.host.allocMem}
		snap.Free[name] = proto.Resources{CPUs: nonNeg(v.host.freeCPU()), Mem: max(0, v.host.freeMem())}
		snap.Running[name] = v.running
		ids := make([]string, 0, len(v.e.allocs))
		for id := range v.e.allocs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		snap.Jobs[name] = ids
		if r := s.breakerReason(v.e, now); r != "" {
			snap.Unavailable[name] = r
		}
	}
	for id, j := range s.jobs {
		if j.alloc != nil {
			snap.Placements[id] = j.alloc.pl
		}
	}
	return snap
}

func nonNeg(x float64) float64 {
	if x < 0 {
		return 0
	}
	return x
}
