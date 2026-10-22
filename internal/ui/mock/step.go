package mock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

// newID returns a 10-char lowercase base32 ID sortable by time (SPEC §7).
func (s *Sim) newID(at time.Time) string {
	ms := at.UnixMilli()
	if ms <= s.lastID {
		ms = s.lastID + 1
	}
	s.lastID = ms
	var b [10]byte
	v := uint64(ms) & (1<<50 - 1)
	for i := 9; i >= 0; i-- {
		b[i] = crockford[v&31]
		v >>= 5
	}
	return string(b[:])
}

func (s *Sim) pickTemplate() *template {
	total := 0
	for _, t := range templates {
		total += t.weight
	}
	r := s.rng.IntN(total)
	for i := range templates {
		r -= templates[i].weight
		if r < 0 {
			return &templates[i]
		}
	}
	return &templates[0]
}

func (s *Sim) pickOwner() (proto.Owner, proto.Priority) {
	total := 0
	for _, o := range owners {
		total += o.weight
	}
	r := s.rng.IntN(total)
	for _, o := range owners {
		r -= o.weight
		if r < 0 {
			return o.o, o.prio
		}
	}
	return owners[0].o, owners[0].prio
}

func (s *Sim) jitter(lo, hi float64) float64 { return lo + s.rng.Float64()*(hi-lo) }

func fingerprint(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:8])
}

func (s *Sim) newJob(at time.Time, t *template) *simJob {
	owner, prio := s.pickOwner()
	root := "/home/dev/Development/Git/" + t.ws
	spec := proto.JobSpec{
		ID:   s.newID(at),
		Argv: append([]string{}, t.argv...),
		Cwd:  root,
		Workspace: proto.Workspace{
			Root: root, RepoID: fingerprint("repo", t.ws), Git: true, Branch: t.branch, Name: t.ws,
		},
		Env:         map[string]string{"CI": "", "TERM": "xterm-256color"},
		Req:         proto.Requirements{OS: proto.OSSame, Prefer: proto.PreferAuto, GPU: t.gpu},
		Owner:       owner,
		Priority:    prio,
		SubmittedAt: at.UTC(),
		ClientPID:   20000 + s.rng.IntN(40000),
	}
	if owner.Kind == proto.OwnerHuman {
		spec.TTY = &proto.TTY{Rows: 52, Cols: 180}
	}
	if t.gpu {
		spec.Req.CPUs = t.cpus
		spec.Req.Mem = t.mem
	}
	if t.local {
		spec.Req.OriginOnly = true
		spec.Req.OriginOnlyReason = "the command talks to localhost:5432"
	}
	samples := 0
	if t.family == "learned" {
		samples = 8 + s.rng.IntN(30)
	}
	j := &simJob{
		tmpl: t,
		job: proto.Job{
			Spec:        spec,
			State:       proto.JobQueued,
			Estimate:    proto.Estimate{CPUs: t.cpus, Mem: t.mem, Duration: t.dur, Samples: samples, Source: t.family},
			Fingerprint: fingerprint(spec.Workspace.RepoID, strings.Join(t.argv, " ")),
			QueuedAt:    at.UTC(),
			Origin:      s.self().name(),
			Reason:      "queued",
		},
		runFor: t.dur * s.jitter(0.7, 1.35),
		fail:   s.rng.Float64() < t.exitFail,
	}
	j.log.init()
	return j
}

func (s *Sim) submit(at time.Time) *simJob {
	j := s.newJob(at, s.pickTemplate())
	s.jobs[j.job.Spec.ID] = j
	s.queue = append(s.queue, j.job.Spec.ID)
	s.emitJob(j)
	return j
}

func prioRank(p proto.Priority) int {
	switch p {
	case proto.PriorityHigh:
		return 0
	case proto.PriorityLow:
		return 2
	}
	return 1
}

// eligible lists nodes that could ever run j right now, with a reason for
// each node that cannot.
func (s *Sim) eligible(j *simJob) (ok []*simNode, why []string) {
	for _, n := range s.nodes {
		name := n.name()
		switch {
		case n.st.Status == proto.NodeOffline:
			why = append(why, name+" is offline")
		case n.st.Status == proto.NodeDraining:
			why = append(why, name+" is draining")
		case !n.st.Accepts:
			why = append(why, name+" does not accept jobs")
		case j.job.Spec.Req.OriginOnly && !n.st.Self:
			// silent: origin-only jobs consider only the origin
		case j.job.Spec.Req.GPU && !hasNvidia(n):
			// silent: only GPU nodes matter
		default:
			ok = append(ok, n)
		}
	}
	return ok, why
}

func hasNvidia(n *simNode) bool {
	for _, g := range n.st.Info.GPUs {
		if g.Vendor == "nvidia" {
			return true
		}
	}
	return false
}

// freeFor is what node n can still admit for a Linux job (the Docker VM
// bounds it on a Mac).
func (s *Sim) freeFor(n *simNode) proto.Resources {
	f := n.st.Free
	if n.linuxJobsVia() == proto.RuntimeContainer {
		var vmAlloc proto.Resources
		for _, j := range s.jobs {
			if j.job.Node == n.name() && !j.job.State.Terminal() && j.job.State != proto.JobQueued {
				vmAlloc.CPUs += j.job.Alloc.CPUs
				vmAlloc.Mem += j.job.Alloc.Mem
			}
		}
		f.CPUs = math.Min(f.CPUs, n.vm.CPUs-vmAlloc.CPUs)
		f.Mem = min(f.Mem, n.vm.Mem-vmAlloc.Mem-2*gib)
	}
	f.CPUs = math.Max(0, f.CPUs)
	f.Mem = max(0, f.Mem)
	return f
}

func (s *Sim) speed(n *simNode) float64 { return 1 / n.st.Info.SingleCoreScore }

func (s *Sim) schedule(now time.Time) {
	sort.SliceStable(s.queue, func(a, b int) bool {
		ja, jb := s.jobs[s.queue[a]], s.jobs[s.queue[b]]
		ra, rb := prioRank(ja.job.Spec.Priority), prioRank(jb.job.Spec.Priority)
		if now.Sub(ja.job.QueuedAt) > 2*time.Minute {
			ra--
		}
		if now.Sub(jb.job.QueuedAt) > 2*time.Minute {
			rb--
		}
		if ra != rb {
			return ra < rb
		}
		return ja.job.Spec.ID < jb.job.Spec.ID
	})
	var still []string
	for _, id := range s.queue {
		j := s.jobs[id]
		if j.job.State != proto.JobQueued {
			continue
		}
		cands, why := s.eligible(j)
		need := proto.Resources{CPUs: j.job.Estimate.CPUs, Mem: j.job.Estimate.Mem}
		var best *simNode
		bestT := math.Inf(1)
		for _, n := range cands {
			f := s.freeFor(n)
			if need.Mem > f.Mem || need.CPUs > f.CPUs*1.25 {
				continue
			}
			t := j.job.Estimate.Duration * s.speed(n)
			if !n.st.Self {
				t += 4 + float64(s.rng.IntN(3))
			}
			if n.st.Self {
				t -= 0.5 // tie-break: origin first
			}
			if t < bestT {
				best, bestT = n, t
			}
		}
		if best == nil {
			reason := s.waitReason(j, cands, why, need)
			if reason != j.job.Reason {
				j.job.Reason = reason
				j.job.QueuePos = len(still) + 1
				s.emitJob(j)
			}
			still = append(still, id)
			continue
		}
		s.place(j, best, now, bestT)
	}
	changed := len(still) != len(s.queue)
	for i, id := range still {
		if !changed && s.queue[i] != id {
			changed = true
		}
		j := s.jobs[id]
		if j.job.QueuePos != i+1 {
			j.job.QueuePos = i + 1
			s.emitJob(j)
		}
	}
	s.queue = still
	if changed {
		s.emit(proto.EventQueue, map[string][]string{"queue": append([]string{}, still...)})
	}
}

func (s *Sim) waitReason(j *simJob, cands []*simNode, why []string, need proto.Resources) string {
	if j.job.Spec.Req.GPU && len(cands) == 0 {
		return "waiting: needs an NVIDIA GPU and gx10, the only node with one, is " + string(s.node("gx10").st.Status)
	}
	if len(cands) == 0 {
		if j.job.Spec.Req.OriginOnly {
			return "waiting: must run here (" + j.job.Spec.Req.OriginOnlyReason + ") and this node is not accepting jobs"
		}
		return "waiting: no node can take jobs (" + strings.Join(why, ", ") + ")"
	}
	where := func(n *simNode) string {
		if n.linuxJobsVia() == proto.RuntimeContainer {
			return n.name() + "'s Docker VM"
		}
		return n.name()
	}
	memShort := true
	for _, n := range cands {
		if s.freeFor(n).Mem >= need.Mem {
			memShort = false
		}
	}
	var parts []string
	for _, n := range cands {
		f := s.freeFor(n)
		if memShort {
			parts = append(parts, fmt.Sprintf("%s has %s free", where(n), fmtGiB(f.Mem)))
		} else {
			parts = append(parts, fmt.Sprintf("%s has %s free", where(n), fmtCores(f.CPUs)))
		}
	}
	var r string
	if memShort {
		r = fmt.Sprintf("waiting: needs %s; %s", fmtGiB(need.Mem), strings.Join(parts, ", "))
	} else {
		r = fmt.Sprintf("waiting: needs %s cores; %s", fmtCores(need.CPUs), strings.Join(parts, ", "))
	}
	if j.job.Spec.Req.OriginOnly {
		r += " (must run here: " + j.job.Spec.Req.OriginOnlyReason + ")"
	} else if j.job.Spec.Req.GPU {
		r += " (needs the GPU)"
	} else if len(why) > 0 {
		r += "; " + strings.Join(why, ", ")
	}
	return r
}

func fmtGiB(b int64) string {
	v := float64(b) / float64(gib)
	if v >= 10 || v == math.Trunc(v) {
		return fmt.Sprintf("%.0f GiB", v)
	}
	return fmt.Sprintf("%.1f GiB", v)
}

func fmtCores(c float64) string {
	if c == math.Trunc(c) {
		return fmt.Sprintf("%.0f", c)
	}
	return fmt.Sprintf("%.1f", c)
}

func fmtSecs(sec float64) string {
	if sec < 60 {
		return fmt.Sprintf("%.0f s", sec)
	}
	return fmt.Sprintf("%d m %02d s", int(sec)/60, int(sec)%60)
}

func (s *Sim) place(j *simJob, n *simNode, now time.Time, expected float64) {
	j.job.Node = n.name()
	j.job.Runtime = n.linuxJobsVia()
	j.job.Alloc = proto.Resources{CPUs: j.job.Estimate.CPUs, Mem: j.job.Estimate.Mem}
	j.job.StartedAt = now.UTC()
	j.job.QueuePos = 0
	j.runFor *= s.speed(n)
	f := s.freeFor(n)
	// Later placements in the same pass must see this allocation.
	n.st.Free.CPUs = math.Max(0, n.st.Free.CPUs-j.job.Alloc.CPUs)
	n.st.Free.Mem = max(0, n.st.Free.Mem-j.job.Alloc.Mem)
	n.st.Allocated.CPUs += j.job.Alloc.CPUs
	n.st.Allocated.Mem += j.job.Alloc.Mem
	here := j.job.Estimate.Duration * s.speed(s.self())
	switch {
	case j.job.Spec.Req.OriginOnly:
		j.job.Reason = "stays here: " + j.job.Spec.Req.OriginOnlyReason
	case j.job.Spec.Req.GPU:
		j.job.Reason = fmt.Sprintf("%s: the only node with an NVIDIA GPU, %s cores and %s free", n.name(), fmtCores(f.CPUs), fmtGiB(f.Mem))
	case n.st.Self:
		j.job.Reason = fmt.Sprintf("%s (here): %s cores free, est. %s", n.name(), fmtCores(f.CPUs), fmtSecs(expected))
	default:
		j.job.Reason = fmt.Sprintf("%s: %s cores free, est. %s vs %s here", n.name(), fmtCores(f.CPUs), fmtSecs(expected), fmtSecs(here))
	}
	if n.st.Self {
		j.job.Local = true
		s.startRunning(j, now)
	} else {
		j.job.State = proto.JobSyncing
		bytes := int64(200_000 + s.rng.IntN(9_000_000))
		j.job.Usage.SyncBytes = bytes
		secs := 0.6 + float64(bytes)/n.st.Link.BandwidthBps*4
		j.job.Usage.SyncSeconds = math.Round(secs*10) / 10
		j.phaseEnd = now.Add(time.Duration(secs * float64(time.Second)))
	}
	s.emitJob(j)
}

func (s *Sim) startRunning(j *simJob, now time.Time) {
	j.job.State = proto.JobRunning
	j.phaseEnd = now.Add(time.Duration(j.runFor * float64(time.Second)))
	j.job.Live = &proto.Resources{CPUs: j.job.Alloc.CPUs * 0.3, Mem: j.job.Alloc.Mem / 8}
}

// Step advances the simulation by one tick.
func (s *Sim) Step() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.flap(now)
	s.advanceJobs(now)
	s.spawn(now)
	s.schedule(now)
	s.recompute()
	for _, n := range s.nodes {
		if n.st.Status != proto.NodeOffline && now.Sub(n.st.Metrics.Time) >= 2*time.Second {
			s.sampleMetrics(n, now, now.Sub(n.st.Metrics.Time).Seconds())
			s.emit(proto.EventNodeMetrics, proto.NodeMetricsEvent{Name: n.name(), Metrics: n.st.Metrics})
		}
	}
}

func (s *Sim) flap(now time.Time) {
	for _, n := range s.nodes {
		if !n.flaky {
			continue
		}
		switch {
		case n.st.Status == proto.NodeOffline && now.After(n.offlineUntil):
			n.st.Status = proto.NodeOnline
			n.st.Reason = ""
			n.st.Link.Connected = true
			n.st.LastSeen = now.UTC()
			n.nextFlap = now.Add(time.Duration(60+s.rng.IntN(90)) * time.Second)
			s.emit(proto.EventNode, n.st)
			s.emit(proto.EventLog, map[string]any{"level": "info", "msg": n.name() + " is back online", "time": now.UTC()})
		case n.st.Status == proto.NodeOnline && now.After(n.nextFlap) && n.st.Running == 0:
			n.st.Status = proto.NodeOffline
			n.st.Reason = "ssh: dial tcp " + n.st.Link.Address + ": i/o timeout"
			n.st.Link.Connected = false
			n.offlineUntil = now.Add(time.Duration(20+s.rng.IntN(40)) * time.Second)
			s.emit(proto.EventNode, n.st)
			s.emit(proto.EventLog, map[string]any{"level": "warn", "msg": n.name() + " went offline: " + n.st.Reason, "time": now.UTC()})
		}
	}
}

func (s *Sim) advanceJobs(now time.Time) {
	ids := make([]string, 0, len(s.jobs))
	for id, j := range s.jobs {
		if !j.job.State.Terminal() && j.job.State != proto.JobQueued {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		j := s.jobs[id]
		switch j.job.State {
		case proto.JobSyncing:
			if now.Before(j.phaseEnd) {
				continue
			}
			if s.rng.Float64() < 0.04 && s.freeFor(s.self()).Mem > j.job.Alloc.Mem {
				// The remote path failed before the command started:
				// kran falls back to running locally (SPEC §1.1).
				j.job.Reason = j.job.Node + ": sync failed (connection reset by peer); ran here instead"
				j.job.Node = s.self().name()
				j.job.Runtime = proto.RuntimeNative
				j.job.Local = true
				j.job.Fallback = true
				s.startRunning(j, now)
			} else if j.tmpl.setup && s.rng.Float64() < 0.4 {
				j.job.State = proto.JobSetup
				secs := s.jitter(4, 12)
				j.job.Usage.SetupSeconds = math.Round(secs*10) / 10
				j.phaseEnd = now.Add(time.Duration(secs * float64(time.Second)))
				j.log.append(now, setupLog(j.tmpl))
			} else {
				s.startRunning(j, now)
			}
			s.emitJob(j)
		case proto.JobSetup:
			if now.Before(j.phaseEnd) {
				continue
			}
			s.startRunning(j, now)
			s.emitJob(j)
		case proto.JobRunning:
			dt := 0.5
			frac := 1 - j.phaseEnd.Sub(now).Seconds()/j.runFor
			live := proto.Resources{
				CPUs: math.Round(j.job.Alloc.CPUs*s.jitter(0.55, 1.04)*10) / 10,
				Mem:  int64(float64(j.job.Alloc.Mem) * math.Min(1.15, 0.25+frac*s.jitter(0.6, 1.0))),
			}
			j.job.Live = &live
			j.job.Usage.CPUSeconds += live.CPUs * dt
			j.job.Usage.PeakMem = max(j.job.Usage.PeakMem, live.Mem)
			if s.rng.Float64() < 0.6 {
				j.log.append(now, logLines(j.tmpl.log, s.rng, frac))
			}
			if !now.Before(j.phaseEnd) {
				if j.fail {
					s.finish(j, now, proto.JobFailed, 1, "")
				} else {
					s.finish(j, now, proto.JobSucceeded, 0, "")
				}
				continue
			}
			// Live usage reaches the UI every few seconds, like the daemon's
			// usage sampling, not on every tick.
			if (now.UnixMilli()/500+int64(len(id)+int(id[9])))%6 == 0 {
				s.emitJob(j)
			}
		}
	}
}

func (s *Sim) finish(j *simJob, now time.Time, st proto.JobState, code int, sig string) {
	wasQueued := j.job.State == proto.JobQueued
	j.job.State = st
	j.job.EndedAt = now.UTC()
	j.job.Live = nil
	j.job.QueuePos = 0
	if !wasQueued {
		j.job.Usage.WallSeconds = math.Round(now.Sub(j.job.StartedAt).Seconds()*10) / 10
	}
	c := code
	j.job.ExitCode = &c
	j.job.Signal = sig
	switch st {
	case proto.JobFailed:
		j.log.append(now, failTail(j.tmpl.log))
		j.job.Reason = fmt.Sprintf("exited with code %d", code)
	case proto.JobCanceled:
		j.job.Reason = "canceled from the dashboard"
		if wasQueued {
			j.job.ExitCode = nil
		}
	case proto.JobSucceeded:
		j.log.append(now, okTail(j.tmpl.log, j.job.Usage.WallSeconds))
	}
	j.log.close()
	if wasQueued {
		for i, id := range s.queue {
			if id == j.job.Spec.ID {
				s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
				break
			}
		}
		s.emit(proto.EventQueue, map[string][]string{"queue": append([]string{}, s.queue...)})
	} else {
		s.count(j)
		s.emit(proto.EventStats, copyStats(s.stats))
	}
	s.finished = append(s.finished, j.job.Spec.ID)
	for len(s.finished) > 400 {
		delete(s.jobs, s.finished[0])
		s.finished = s.finished[1:]
	}
	s.emitJob(j)
}

func (s *Sim) count(j *simJob) {
	s.stats.JobsToday++
	if j.job.Node != s.self().name() {
		s.stats.OffloadedToday++
		s.stats.SavedSecondsToday += j.job.Usage.WallSeconds * 0.55
	}
	if j.job.Fallback {
		s.stats.FallbacksToday++
	}
	if j.job.State == proto.JobInfraErr || j.job.State == proto.JobLost {
		s.stats.FailedInfraToday++
	}
	s.stats.CPUSecondsByNode[j.job.Node] += j.job.Usage.CPUSeconds
}

func (s *Sim) spawn(now time.Time) {
	active := 0
	for _, j := range s.jobs {
		if !j.job.State.Terminal() {
			active++
		}
	}
	target := s.opt.TargetActive
	if target == 0 {
		target = 11
		if now.Before(s.burstEnd) {
			target = 20
		} else if s.rng.Float64() < 0.01 {
			// An agent fans out a batch of checks at once.
			s.burstEnd = now.Add(25 * time.Second)
		}
	}
	p := 0.08
	if active < target {
		p = 0.45
	}
	if active > target+4 {
		return
	}
	if s.opt.TargetActive > 0 && active < target {
		// Load tests: fill up quickly so the queue really holds hundreds.
		for i := 0; i < min(20, target-active); i++ {
			s.submit(now)
		}
		return
	}
	if s.rng.Float64() < p {
		s.submit(now)
	}
}

// recompute derives allocated/free/running per node and emits node events
// for the ones that changed.
func (s *Sim) recompute() {
	alloc := map[string]proto.Resources{}
	running := map[string]int{}
	for _, j := range s.jobs {
		if j.job.State.Terminal() || j.job.State == proto.JobQueued {
			continue
		}
		a := alloc[j.job.Node]
		a.CPUs += j.job.Alloc.CPUs
		a.Mem += j.job.Alloc.Mem
		alloc[j.job.Node] = a
		running[j.job.Node]++
	}
	for _, n := range s.nodes {
		st := n.st
		a := alloc[n.name()]
		outsideExcess := math.Max(0, n.outside-n.st.Reserve.CPUs)
		free := proto.Resources{
			CPUs: math.Max(0, math.Round((st.Capacity.CPUs-a.CPUs-outsideExcess)*10)/10),
			Mem:  max(0, min(st.Capacity.Mem-a.Mem, st.Metrics.MemAvailable-st.Reserve.Mem)),
		}
		if st.Status == proto.NodeOffline {
			free = proto.Resources{}
		}
		n.st.Allocated = a
		n.st.Free = free
		n.st.Running = running[n.name()]
		if n.st.Allocated != st.Allocated || n.st.Running != st.Running || math.Abs(float64(n.st.Free.Mem-st.Free.Mem)) > float64(gib)/2 || n.st.Free.CPUs != st.Free.CPUs {
			s.emit(proto.EventNode, n.st)
		}
	}
}

func (s *Sim) sampleMetrics(n *simNode, now time.Time, dt float64) {
	n.outside = math.Max(n.outsideLo, math.Min(n.outsideHi, n.outside+s.jitter(-0.6, 0.6)))
	var cpu float64
	var mem int64
	gpu := 0.0
	syncing := false
	for _, j := range s.jobs {
		if j.job.Node != n.name() || j.job.State.Terminal() {
			continue
		}
		if j.job.Live != nil {
			cpu += j.job.Live.CPUs
			mem += j.job.Live.Mem
		}
		if j.job.Spec.Req.GPU && j.job.State == proto.JobRunning {
			gpu = s.jitter(0.78, 0.97)
		}
		if j.job.State == proto.JobSyncing {
			syncing = true
		}
	}
	cpus := float64(n.st.Info.CPUs)
	m := &n.st.Metrics
	m.Time = now.UTC()
	m.CPUBusy = math.Min(1, (n.outside+cpu)/cpus)
	m.Load1 = math.Round((m.CPUBusy*cpus*1.05+s.jitter(-0.3, 0.3))*100) / 100
	m.MemUsed = min(n.st.Info.MemTotal, n.baseMem+mem+int64(s.jitter(-0.3, 0.3)*float64(gib)))
	m.MemAvailable = n.st.Info.MemTotal - m.MemUsed
	if n.st.Info.OS == "linux" {
		m.CPUPressure = math.Round(math.Max(0, (m.CPUBusy-0.65)*120+s.jitter(0, 2))*10) / 10
		m.IOPressure = math.Round(s.jitter(0, 1.5)*10) / 10
	}
	m.MemPressure = math.Round(math.Max(0, (float64(m.MemUsed)/float64(n.st.Info.MemTotal)-0.8)*200)*10) / 10
	m.SwapTotal = 8 * gib
	m.SwapUsed = int64(math.Max(0, float64(m.MemUsed)/float64(n.st.Info.MemTotal)-0.75) * float64(m.SwapTotal))
	m.DiskFree = max(0, m.DiskFree-int64(dt*float64(s.rng.IntN(4_000_000))))
	if hasNvidia(n) {
		m.GPUUtil = gpu
		m.GPUMemUsed = int64(gpu * 20 * float64(gib))
	}
	m.NetRxBps = s.jitter(2e3, 4e4)
	m.NetTxBps = s.jitter(2e3, 3e4)
	if syncing {
		m.NetRxBps = n.st.Link.BandwidthBps * s.jitter(0.5, 0.9)
	}
	if n.linuxJobsVia() == proto.RuntimeContainer {
		var vmCPU float64
		var vmMem int64
		for _, j := range s.jobs {
			if j.job.Node == n.name() && j.job.Live != nil {
				vmCPU += j.job.Live.CPUs
				vmMem += j.job.Live.Mem
			}
		}
		m.Runtimes = map[proto.RuntimeKind]proto.RuntimeMetrics{
			proto.RuntimeContainer: {CPUBusy: math.Min(1, vmCPU/n.vm.CPUs), MemAvailable: max(0, n.vm.Mem-vmMem-2*gib)},
		}
	}
	n.st.LastSeen = now.UTC()
}

// seedHistory fills the last hours with finished jobs so history, stats and
// filters have content from the first frame.
func (s *Sim) seedHistory(now time.Time) {
	n := s.opt.History
	start := now.Add(-5 * time.Hour)
	step := 5 * time.Hour / time.Duration(n+1)
	for i := 0; i < n; i++ {
		at := start.Add(time.Duration(i)*step + time.Duration(s.rng.IntN(60))*time.Second)
		j := s.newJob(at, s.pickTemplate())
		target := s.nodes[s.rng.IntN(min(3, len(s.nodes)))]
		if j.job.Spec.Req.GPU || j.job.Spec.Req.OriginOnly {
			target = s.self()
		}
		if len(s.nodes) > 3 && s.rng.Float64() < 0.5 {
			target = s.nodes[3+s.rng.IntN(len(s.nodes)-3)]
		}
		j.job.Node = target.name()
		j.job.Runtime = target.linuxJobsVia()
		j.job.Local = target.st.Self
		j.job.Alloc = proto.Resources{CPUs: j.job.Estimate.CPUs, Mem: j.job.Estimate.Mem}
		wait := s.jitter(0, 4)
		if i%9 == 4 {
			wait = s.jitter(20, 140)
		}
		j.job.StartedAt = at.Add(time.Duration(wait * float64(time.Second))).UTC()
		wall := j.runFor * s.speed(target)
		if !target.st.Self {
			j.job.Usage.SyncBytes = int64(300_000 + s.rng.IntN(6_000_000))
			j.job.Usage.SyncSeconds = math.Round(s.jitter(0.5, 3)*10) / 10
			wall += j.job.Usage.SyncSeconds
		}
		j.job.Usage.WallSeconds = math.Round(wall*10) / 10
		j.job.Usage.CPUSeconds = math.Round(wall*j.job.Alloc.CPUs*s.jitter(0.5, 0.95)*10) / 10
		j.job.Usage.PeakMem = int64(float64(j.job.Alloc.Mem) * s.jitter(0.4, 1.1))
		j.job.EndedAt = j.job.StartedAt.Add(time.Duration(wall * float64(time.Second)))
		code := 0
		j.job.State = proto.JobSucceeded
		j.job.Reason = fmt.Sprintf("%s: %s cores free, est. %s", target.name(), fmtCores(target.st.Capacity.CPUs-float64(s.rng.IntN(8))), fmtSecs(j.job.Estimate.Duration*s.speed(target)))
		switch {
		case i == 17:
			j.job.State = proto.JobInfraErr
			code = 125
			j.job.Reason = "lost contact with e2e after 41 s (lease expired); not re-run"
			j.job.Node = "e2e"
		case i == 31:
			j.job.Fallback = true
			j.job.Local = true
			j.job.Node = s.self().name()
			j.job.Runtime = proto.RuntimeNative
			j.job.Reason = "devmac: sync failed (connection reset by peer); ran here instead"
		case i%13 == 7:
			j.job.State = proto.JobCanceled
			code = 130
			j.job.Signal = "INT"
			j.job.Reason = "canceled (Ctrl-C)"
		case j.fail:
			j.job.State = proto.JobFailed
			code = 1
			j.job.Reason = "exited with code 1"
		}
		j.job.ExitCode = &code
		var b strings.Builder
		for k := 0; k < 40; k++ {
			b.WriteString(logLines(j.tmpl.log, s.rng, float64(k)/40))
		}
		if j.job.State == proto.JobFailed {
			b.WriteString(failTail(j.tmpl.log))
		} else if j.job.State == proto.JobSucceeded {
			b.WriteString(okTail(j.tmpl.log, j.job.Usage.WallSeconds))
		}
		j.log.append(j.job.EndedAt, b.String())
		j.log.close()
		s.jobs[j.job.Spec.ID] = j
		s.finished = append(s.finished, j.job.Spec.ID)
		s.count(j)
	}
	// Earlier today, before the seeded window.
	s.stats.JobsToday += 38
	s.stats.OffloadedToday += 24
	s.stats.SavedSecondsToday += 1460
	s.stats.CPUSecondsByNode["devmac"] += 9100
	s.stats.CPUSecondsByNode["gx10"] += 6400
	s.stats.CPUSecondsByNode["e2e"] += 2100
}
