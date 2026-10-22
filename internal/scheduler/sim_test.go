package scheduler

import (
	"container/heap"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// A discrete-event simulation of the mesh that was overloaded on
// 2026-10-22: a 20-core GX10 (128 GB unified, CUDA) where five agent
// sessions run, and a 28-core Mac Studio (96 GB) whose Linux jobs go to a
// Colima VM with 16 CPUs / 32 GB. Sessions are closed loops: think, submit a
// burst of 1-4 jobs in parallel (parallel tool calls), wait for all of them.
// The real scheduler places jobs; the simulator plays the machines.

type family struct {
	name     string
	cpu      float64 // cores the job really uses
	mem      int64   // real peak memory
	dur      float64 // real seconds on gx10, mean
	gpu      bool
	repo     string
	jsRepo   bool // setup (pnpm install) on first sync
	anyOS    bool
	estCPUs  float64 // what the scheduler is told (family default / learned)
	speedMac float64 // real duration multiplier on the Mac (native)
	speedVM  float64 // in the Colima VM
}

var simFamilies = map[string]family{
	"go-test":      {name: "go-test", cpu: 6, mem: 3 * gib, dur: 75, repo: "kran", estCPUs: 8, speedMac: 0.75, speedVM: 0.85},
	"go-build":     {name: "go-build", cpu: 4, mem: 2 * gib, dur: 25, repo: "kran", estCPUs: 4, speedMac: 0.75, speedVM: 0.85},
	"next-build":   {name: "next-build", cpu: 3.5, mem: 6 * gib, dur: 110, repo: "web", jsRepo: true, estCPUs: 4, speedMac: 0.7, speedVM: 0.8},
	"vitest":       {name: "vitest", cpu: 4, mem: 3 * gib, dur: 50, repo: "web", jsRepo: true, estCPUs: 4, speedMac: 0.7, speedVM: 0.8},
	"tsc":          {name: "tsc", cpu: 1.5, mem: 3 * gib, dur: 30, repo: "web", jsRepo: true, estCPUs: 2, speedMac: 0.7, speedVM: 0.8},
	"docker-build": {name: "docker-build", cpu: 4, mem: 4 * gib, dur: 160, repo: "infra", estCPUs: 4, speedMac: 0.9, speedVM: 0.9},
	"pytest":       {name: "pytest", cpu: 2, mem: 2 * gib, dur: 60, repo: "ml", estCPUs: 2, speedMac: 0.8, speedVM: 0.9},
	"vllm-gpu":     {name: "vllm-gpu", cpu: 4, mem: 28 * gib, dur: 420, repo: "ml", gpu: true, estCPUs: 4, speedMac: 1, speedVM: 1},
	"lint":         {name: "lint", cpu: 1, mem: 1 * gib, dur: 3, repo: "web", estCPUs: 1, speedMac: 0.7, speedVM: 0.8},
}

type sessionMix struct {
	owner string
	mix   []string // drawn uniformly
}

var incidentSessions = []sessionMix{
	{"backend", []string{"go-test", "go-test", "go-test", "go-build", "go-build", "lint"}},
	{"frontend", []string{"next-build", "next-build", "vitest", "vitest", "tsc", "lint"}},
	{"infra", []string{"docker-build", "docker-build", "docker-build", "go-test", "go-test"}},
	{"ml", []string{"vllm-gpu", "pytest", "pytest", "pytest"}},
	{"agent5", []string{"go-test", "next-build", "docker-build", "vitest", "pytest"}},
}

type simNode struct {
	st      proto.NodeState
	bgCPU   float64
	bgMem   int64
	vmBgMem int64
	// slow is how much longer than its CPU share explains every job on
	// the host takes (memory thrash, I/O); press are the pressure metrics
	// the node reports meanwhile.
	slow  float64
	press proto.NodeMetrics
}

type simJob struct {
	id       string
	owner    string
	f        family
	realDur  float64
	realMem  int64
	realCPU  float64
	est      proto.Estimate
	arrive   time.Time
	tk       Ticket
	pl       Placement
	placedAt time.Time
	startAt  time.Time
	endAt    time.Time
	syncSec  float64
	started  bool
	burst    *burst
}

type burst struct {
	sess    *simSession
	pending int
}

type simSession struct {
	mix   sessionMix
	waits []float64
	done  int
}

type evKind int

const (
	evArrive evKind = iota
	evStart
	evFinish
	evTick
	evBurst
)

type event struct {
	at   time.Time
	seq  int
	kind evKind
	job  *simJob
	sess *simSession
}

type evHeap []*event

func (h evHeap) Len() int { return len(h) }
func (h evHeap) Less(i, j int) bool {
	if !h[i].at.Equal(h[j].at) {
		return h[i].at.Before(h[j].at)
	}
	return h[i].seq < h[j].seq
}
func (h evHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *evHeap) Push(x any)   { *h = append(*h, x.(*event)) }
func (h *evHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

type simResult struct {
	name        string
	completed   int
	submitted   int
	waits       []float64 // queue wait (submit → placed), seconds
	turnarounds []float64 // submit → end
	offloaded   int
	allocCPUSec map[string]float64 // Σ allocated CPU × seconds
	busyCPUSec  map[string]float64 // Σ real CPU in use × seconds
	capCPU      map[string]float64
	perSession  map[string]*simSession
	byNodeFam   map[string]int
	oomEvents   int
	maxQueue    int
	duration    float64
	reasons     map[string]string // sample of distinct waiting reasons
}

type simulator struct {
	t          testing.TB
	r          *rand.Rand
	clk        *fakeClock
	s          *sched
	nodes      map[string]*simNode
	ev         evHeap
	seq        int
	jobs       map[string]*simJob // submitted, not finished
	queued     map[string]*simJob // waiting for placement
	synced     map[string]bool    // repo\x00node
	res        *simResult
	until      time.Time // no new bursts after this
	nextID     int
	lastAcc    time.Time
	thinkScale float64
}

type simConfig struct {
	seed       int64
	withMac    bool
	horizon    time.Duration
	sessions   []sessionMix
	thinkScale float64 // multiplies think time; < 1 = heavier load
	// overloaded makes gx10 thrash as on the incident afternoon: memory
	// PSI 40, swap full, load far above its CPU use; every job there runs
	// originSlow times longer.
	overloaded     bool
	originSlow     float64
	ignorePressure bool // score as kran did before pressure counted
}

func newSimulator(t testing.TB, name string, seed int64, withMac bool, horizon time.Duration) *simulator {
	return newSimulatorCfg(t, name, simConfig{seed: seed, withMac: withMac, horizon: horizon, sessions: incidentSessions, thinkScale: 1})
}

func newSimulatorCfg(t testing.TB, name string, cfg simConfig) *simulator {
	seed, withMac, horizon := cfg.seed, cfg.withMac, cfg.horizon
	clk := &fakeClock{t0}
	sm := &simulator{
		t: t, r: rand.New(rand.NewSource(seed)), clk: clk,
		nodes: map[string]*simNode{}, jobs: map[string]*simJob{}, queued: map[string]*simJob{},
		synced: map[string]bool{}, until: t0.Add(horizon), lastAcc: t0,
		res: &simResult{name: name, allocCPUSec: map[string]float64{}, busyCPUSec: map[string]float64{},
			capCPU: map[string]float64{}, perSession: map[string]*simSession{}, byNodeFam: map[string]int{},
			reasons: map[string]string{}},
	}
	sm.s = New(Options{Origin: "gx10", Now: clk.Now}).(*sched)
	sm.s.ignorePressure = cfg.ignorePressure
	g := self(gx10(t0))
	// The agent sessions, their language servers and the IDE live on gx10.
	sm.nodes["gx10"] = &simNode{st: g, bgCPU: 3, bgMem: 26 * gib, slow: 1}
	if cfg.overloaded {
		gn := sm.nodes["gx10"]
		gn.bgMem, gn.slow = 80*gib, cfg.originSlow
		gn.press = proto.NodeMetrics{Load1: 34, CPUPressure: 12, MemPressure: 40, IOPressure: 25,
			SwapTotal: 16 * gib, SwapUsed: 16*gib - 20*mib}
	}
	if withMac {
		// The Mac also runs a product install.
		sm.nodes["devmac"] = &simNode{st: devmac(t0), bgCPU: 2, bgMem: 30 * gib, vmBgMem: 2 * gib, slow: 1}
	}
	for _, name := range sm.nodeNames() {
		sm.res.capCPU[name] = float64(sm.nodes[name].st.Info.CPUs) - sm.nodes[name].st.Reserve.CPUs
	}
	sm.publishMetrics()
	sm.thinkScale = cfg.thinkScale
	for i := range cfg.sessions {
		ss := &simSession{mix: cfg.sessions[i]}
		sm.res.perSession[ss.mix.owner] = ss
		sm.push(&event{at: t0.Add(time.Duration(sm.r.Intn(20)) * time.Second), kind: evBurst, sess: ss})
	}
	sm.push(&event{at: t0.Add(2 * time.Second), kind: evTick})
	return sm
}

func (sm *simulator) nodeNames() []string {
	var out []string
	for n := range sm.nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (sm *simulator) push(e *event) {
	sm.seq++
	e.seq = sm.seq
	heap.Push(&sm.ev, e)
}

func (sm *simulator) run() *simResult {
	deadline := sm.until.Add(4 * time.Hour)
	for sm.ev.Len() > 0 {
		e := heap.Pop(&sm.ev).(*event)
		if e.at.After(deadline) {
			sm.t.Fatalf("%s: simulation did not drain: %d jobs left, queue %v", sm.res.name, len(sm.jobs), sm.s.Snapshot().Queue)
		}
		sm.account(e.at)
		sm.clk.t = e.at
		switch e.kind {
		case evBurst:
			sm.burst(e.sess)
		case evArrive:
			sm.arrive(e.job)
		case evStart:
			sm.start(e.job)
		case evFinish:
			sm.finish(e.job)
		case evTick:
			sm.publishMetrics()
			sm.s.Tick(sm.clk.Now())
			if len(sm.jobs) > 0 || sm.clk.Now().Before(sm.until) {
				sm.push(&event{at: e.at.Add(2 * time.Second), kind: evTick})
			}
		}
		sm.poll()
		sm.checkInvariants()
		if q := len(sm.queued); q > sm.res.maxQueue {
			sm.res.maxQueue = q
		}
	}
	sm.res.duration = sm.clk.Now().Sub(t0).Seconds()
	return sm.res
}

// account integrates allocated and real CPU use per node since the last event.
func (sm *simulator) account(now time.Time) {
	dt := now.Sub(sm.lastAcc).Seconds()
	sm.lastAcc = now
	if dt <= 0 {
		return
	}
	for _, j := range sm.jobs {
		if j.pl.Node == "" {
			continue
		}
		sm.res.allocCPUSec[j.pl.Node] += j.pl.Alloc.CPUs * dt
		if j.started {
			sm.res.busyCPUSec[j.pl.Node] += j.realCPU * dt
		}
	}
}

func (sm *simulator) burst(ss *simSession) {
	if !sm.clk.Now().Before(sm.until) {
		return
	}
	n := 1 + sm.r.Intn(4)
	b := &burst{sess: ss, pending: n}
	for i := 0; i < n; i++ {
		f := simFamilies[ss.mix.mix[sm.r.Intn(len(ss.mix.mix))]]
		sm.nextID++
		noise := func(sigma float64) float64 { return math.Exp(sm.r.NormFloat64() * sigma) }
		j := &simJob{
			id: fmt.Sprintf("%s-%04d", ss.mix.owner, sm.nextID), owner: ss.mix.owner, f: f,
			realDur: f.dur * noise(0.25), realMem: int64(float64(f.mem) * noise(0.15)), realCPU: f.cpu,
			burst: b,
		}
		// The estimator has learned these jobs: its numbers are near the
		// truth, memory at the p90 (a bit above typical).
		j.est = proto.Estimate{CPUs: f.estCPUs, Mem: int64(float64(f.mem) * 1.2), Duration: f.dur, Samples: 20, Source: "learned"}
		sm.push(&event{at: sm.clk.Now().Add(time.Duration(sm.r.Intn(3000)) * time.Millisecond), kind: evArrive, job: j})
	}
}

func (sm *simulator) arrive(j *simJob) {
	j.arrive = sm.clk.Now()
	pj := newJob(j.id, j.owner, j.est.CPUs, j.est.Mem, j.est.Duration)
	pj.Estimate = j.est
	pj.QueuedAt = j.arrive
	pj.Spec.Workspace.RepoID = j.f.repo
	pj.Fingerprint = j.f.name
	pj.Spec.Req.GPU = j.f.gpu
	if j.f.anyOS {
		pj.Spec.Req.OS = proto.OSAny
	}
	sm.res.submitted++
	sm.jobs[j.id] = j
	sm.queued[j.id] = j
	j.tk = sm.s.Submit(pj)
}

// poll starts every job whose ticket resolved.
func (sm *simulator) poll() {
	ids := make([]string, 0, len(sm.queued))
	for id := range sm.queued {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	snap := sm.s.Snapshot()
	for _, id := range ids {
		j := sm.queued[id]
		pl, err := isPlacedErr(j.tk)
		select {
		case <-j.tk.(*ticket).done:
		default:
			if r := snap.Reasons[id]; r != "" {
				key := strings.SplitN(r, ",", 2)[0]
				if _, ok := sm.res.reasons[key]; !ok && len(sm.res.reasons) < 12 {
					sm.res.reasons[key] = r
				}
			}
			continue
		}
		if err != nil {
			sm.t.Fatalf("%s: job %s failed to place: %v", sm.res.name, id, err)
		}
		delete(sm.queued, id)
		j.pl = pl
		j.placedAt = sm.clk.Now()
		sm.res.waits = append(sm.res.waits, j.placedAt.Sub(j.arrive).Seconds())
		sm.res.perSession[j.owner].waits = append(sm.res.perSession[j.owner].waits, j.placedAt.Sub(j.arrive).Seconds())
		sm.res.byNodeFam[pl.Node+"/"+string(pl.Runtime)+"/"+j.f.name]++
		if !pl.Local {
			sm.res.offloaded++
			key := j.f.repo + "\x00" + pl.Node
			j.syncSec = 2
			if !sm.synced[key] {
				j.syncSec = 12
				if j.f.jsRepo {
					j.syncSec += 25 // pnpm install
				}
				sm.synced[key] = true
			}
			sm.push(&event{at: sm.clk.Now().Add(secs(j.syncSec)), kind: evStart, job: j})
		} else {
			sm.start(j)
		}
	}
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func (sm *simulator) start(j *simJob) {
	j.started = true
	j.startAt = sm.clk.Now()
	n := sm.nodes[j.pl.Node]
	speed := 1.0
	cores := float64(n.st.Info.CPUs)
	demand := n.bgCPU
	switch {
	case j.pl.Node == "devmac" && j.pl.Runtime == proto.RuntimeContainer:
		speed = j.f.speedVM
		cores = float64(n.st.Info.Runtimes[1].CPUs)
		demand = 0
		for _, o := range sm.jobs {
			if o.started && o.pl.Node == "devmac" && o.pl.Runtime == proto.RuntimeContainer {
				demand += o.realCPU
			}
		}
	case j.pl.Node == "devmac":
		speed = j.f.speedMac
		fallthrough
	default:
		for _, o := range sm.jobs {
			if o.started && o.pl.Node == j.pl.Node {
				demand += o.realCPU
			}
		}
	}
	// CPU contention stretches the job (fixed at start: a simplification).
	contention := math.Max(1, demand/cores)
	d := j.realDur * speed * contention * n.slow
	sm.push(&event{at: sm.clk.Now().Add(secs(d)), kind: evFinish, job: j})
}

func (sm *simulator) finish(j *simJob) {
	j.endAt = sm.clk.Now()
	delete(sm.jobs, j.id)
	sm.s.Release(j.id, &proto.Usage{
		WallSeconds: j.endAt.Sub(j.startAt).Seconds(), CPUSeconds: j.realCPU * j.endAt.Sub(j.startAt).Seconds(),
		PeakMem: j.realMem, SyncSeconds: j.syncSec,
	})
	sm.res.completed++
	sm.res.turnarounds = append(sm.res.turnarounds, j.endAt.Sub(j.arrive).Seconds())
	ss := sm.res.perSession[j.owner]
	ss.done++
	j.burst.pending--
	if j.burst.pending == 0 {
		// The agent reads the output, thinks, edits code, runs again.
		think := (20 + sm.r.ExpFloat64()*60) * sm.thinkScale
		sm.push(&event{at: sm.clk.Now().Add(secs(think)), kind: evBurst, sess: ss})
	}
}

// publishMetrics plays each machine's metrics sampler.
func (sm *simulator) publishMetrics() {
	now := sm.clk.Now()
	for _, name := range sm.nodeNames() {
		n := sm.nodes[name]
		var cpu float64
		var mem, vmMem int64
		var vmCPU float64
		for _, j := range sm.jobs {
			if !j.started || j.pl.Node != name {
				continue
			}
			cpu += j.realCPU
			mem += j.realMem
			if j.pl.Runtime == proto.RuntimeContainer && name == "devmac" {
				vmMem += j.realMem
				vmCPU += j.realCPU
			}
			sm.s.UpdateJobUsage(j.id, proto.Resources{CPUs: j.realCPU, Mem: j.realMem})
		}
		bg := n.bgCPU * (0.8 + 0.4*sm.r.Float64()) // agents are bursty too
		st := n.st
		st.Metrics.Time = now
		st.Metrics.CPUBusy = math.Min(1, (bg+cpu)/float64(st.Info.CPUs))
		st.Metrics.MemAvailable = st.Info.MemTotal - n.bgMem - mem
		st.Metrics.Load1 = n.press.Load1
		st.Metrics.CPUPressure, st.Metrics.MemPressure, st.Metrics.IOPressure = n.press.CPUPressure, n.press.MemPressure, n.press.IOPressure
		st.Metrics.SwapTotal, st.Metrics.SwapUsed = n.press.SwapTotal, n.press.SwapUsed
		if name == "devmac" {
			vm := st.Info.Runtimes[1]
			st.Metrics.Runtimes = map[proto.RuntimeKind]proto.RuntimeMetrics{proto.RuntimeContainer: {
				CPUBusy: math.Min(1, vmCPU/float64(vm.CPUs)), MemAvailable: vm.Mem - n.vmBgMem - vmMem}}
		}
		if st.Metrics.MemAvailable < 0 || (name == "devmac" && st.Metrics.Runtimes[proto.RuntimeContainer].MemAvailable < 0) {
			sm.res.oomEvents++
		}
		n.st = st
		sm.s.UpdateNode(st)
	}
}

func (sm *simulator) checkInvariants() {
	s := sm.s
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.nodes {
		v := s.view(name, sm.clk.Now())
		if v.host.allocMem > v.host.capMem || (v.vm != nil && v.vm.allocMem > v.vm.capMem) {
			sm.t.Fatalf("%s: memory overcommitted on %s", sm.res.name, name)
		}
	}
	for id, j := range s.jobs {
		if j.alloc == nil && j.reason == "" {
			sm.t.Fatalf("%s: queued job %s without reason", sm.res.name, id)
		}
	}
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	idx := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(0, idx)]
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}

func (r *simResult) report(t testing.TB) {
	t.Logf("=== %s: %d/%d jobs done in %s, %d offloaded, max queue %d, oom events %d", r.name, r.completed, r.submitted,
		fmtDur(r.duration), r.offloaded, r.maxQueue, r.oomEvents)
	t.Logf("    queue wait  p50 %s  p95 %s  max %s   turnaround mean %s p95 %s",
		fmtDur(percentile(r.waits, .5)), fmtDur(percentile(r.waits, .95)), fmtDur(percentile(r.waits, 1)),
		fmtDur(mean(r.turnarounds)), fmtDur(percentile(r.turnarounds, .95)))
	for _, n := range sortedKeys(r.capCPU) {
		t.Logf("    %-7s allocated %4.0f%% of %2.0f schedulable CPUs, real use %4.0f%%", n,
			100*r.allocCPUSec[n]/(r.capCPU[n]*r.duration), r.capCPU[n], 100*r.busyCPUSec[n]/(r.capCPU[n]*r.duration))
	}
	for _, o := range sortedKeys(r.perSession) {
		ss := r.perSession[o]
		t.Logf("    session %-9s %3d jobs, wait mean %s p95 %s", o, ss.done, fmtDur(mean(ss.waits)), fmtDur(percentile(ss.waits, .95)))
	}
	var place []string
	for _, k := range sortedKeys(r.byNodeFam) {
		place = append(place, fmt.Sprintf("%s=%d", k, r.byNodeFam[k]))
	}
	t.Logf("    placements: %s", strings.Join(place, " "))
	for _, k := range sortedKeys(r.reasons) {
		t.Logf("    reason seen: %s", r.reasons[k])
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestSimulateIncident(t *testing.T) {
	horizon := time.Hour
	if testing.Short() {
		horizon = 20 * time.Minute
	}
	mesh := newSimulator(t, "kran gx10+devmac", 42, true, horizon).run()
	solo := newSimulator(t, "kran gx10 only", 42, false, horizon).run()
	mesh.report(t)
	solo.report(t)

	if mesh.completed != mesh.submitted || solo.completed != solo.submitted {
		t.Fatalf("not drained: %d/%d, %d/%d", mesh.completed, mesh.submitted, solo.completed, solo.submitted)
	}
	if mesh.oomEvents != 0 {
		t.Errorf("mesh: %d samples with real memory above physical memory", mesh.oomEvents)
	}
	// Offloading must add real throughput for the same five sessions.
	if float64(mesh.completed) < 1.25*float64(solo.completed) {
		t.Errorf("throughput: mesh %d jobs vs gx10-only %d", mesh.completed, solo.completed)
	}
	if p95 := percentile(mesh.waits, .95); p95 > 120 {
		t.Errorf("mesh p95 queue wait %s", fmtDur(p95))
	}
	if mx := percentile(mesh.waits, 1); mx > 10*60 {
		t.Errorf("mesh max queue wait %s", fmtDur(mx))
	}
	if mean(mesh.turnarounds) >= mean(solo.turnarounds) {
		t.Errorf("mean turnaround mesh %s >= gx10-only %s", fmtDur(mean(mesh.turnarounds)), fmtDur(mean(solo.turnarounds)))
	}
	if u := mesh.allocCPUSec["devmac"] / (mesh.capCPU["devmac"] * mesh.duration); u < 0.15 {
		t.Errorf("devmac barely used: %.0f%%", 100*u)
	}
	// Fairness: no session waits much longer than the others on average
	// (sessions with GPU jobs compete for gx10 only, so allow some spread).
	var lo, hi float64 = math.Inf(1), 0
	for _, ss := range mesh.perSession {
		m := mean(ss.waits)
		lo, hi = math.Min(lo, m), math.Max(hi, m)
		if ss.done == 0 {
			t.Errorf("session %s completed nothing", ss.mix.owner)
		}
	}
	if hi > 90 && hi > 4*math.Max(lo, 5) {
		t.Errorf("unfair mean waits: %s .. %s", fmtDur(lo), fmtDur(hi))
	}
	// Placement sanity: GPU jobs only on gx10; Linux jobs never native on macOS.
	for k := range mesh.byNodeFam {
		if strings.HasSuffix(k, "/vllm-gpu") && !strings.HasPrefix(k, "gx10/") {
			t.Errorf("GPU job placed at %s", k)
		}
		if strings.HasPrefix(k, "devmac/native/") {
			t.Errorf("Linux job ran natively on macOS: %s", k)
		}
	}
}

// TestSimulateNoStarvationRandom runs random mixes and seeds: every job
// must finish, and no job may wait unboundedly.
func TestSimulateNoStarvationRandom(t *testing.T) {
	seeds := 6
	if testing.Short() {
		seeds = 2
	}
	fams := sortedKeys(simFamilies)
	for seed := int64(1); seed <= int64(seeds); seed++ {
		rr := rand.New(rand.NewSource(seed * 7919))
		var sessions []sessionMix
		for i := 0; i < 3+rr.Intn(6); i++ {
			var mix []string
			for k := 0; k < 1+rr.Intn(5); k++ {
				mix = append(mix, fams[rr.Intn(len(fams))])
			}
			sessions = append(sessions, sessionMix{owner: fmt.Sprintf("s%d", i), mix: mix})
		}
		cfg := simConfig{seed: seed, withMac: seed%2 == 0, horizon: 30 * time.Minute, sessions: sessions, thinkScale: 0.2 + rr.Float64()}
		r := newSimulatorCfg(t, fmt.Sprintf("random seed %d", seed), cfg).run()
		t.Logf("seed %d: sessions %v, %d jobs, wait p95 %s max %s", seed, sessions, r.completed,
			fmtDur(percentile(r.waits, .95)), fmtDur(percentile(r.waits, 1)))
		if r.completed != r.submitted {
			t.Fatalf("seed %d: %d/%d done", seed, r.completed, r.submitted)
		}
		if mx := percentile(r.waits, 1); mx > 30*60 {
			t.Errorf("seed %d: a job waited %s", seed, fmtDur(mx))
		}
	}
}

// TestSimulateOverloadedOrigin replays the incident afternoon: gx10 thrashes
// on memory (PSI 40, swap full, load 34 at 30% CPU) and every job there
// takes 2.5x as long, while devmac is idle; two agent sessions submit
// work. Scoring with pressure must move work off gx10 and finish it sooner.
// When the metrics overstate the real slowdown (1.5x), it must not cost.
func TestSimulateOverloadedOrigin(t *testing.T) {
	horizon, seeds := time.Hour, int64(4)
	if testing.Short() {
		horizon, seeds = 20*time.Minute, 2
	}
	run := func(name string, slow float64, ignore bool) *simResult {
		var all *simResult
		for seed := int64(1); seed <= seeds; seed++ {
			r := newSimulatorCfg(t, name, simConfig{seed: seed, withMac: true, horizon: horizon, sessions: incidentSessions[:2],
				thinkScale: 1, overloaded: true, originSlow: slow, ignorePressure: ignore}).run()
			if r.completed != r.submitted {
				t.Fatalf("%s seed %d: not drained: %d/%d", name, seed, r.completed, r.submitted)
			}
			if r.oomEvents != 0 {
				t.Errorf("%s seed %d: %d samples with real memory above physical memory", name, seed, r.oomEvents)
			}
			if all == nil {
				all = r
				continue
			}
			all.completed += r.completed
			all.offloaded += r.offloaded
			all.turnarounds = append(all.turnarounds, r.turnarounds...)
			all.waits = append(all.waits, r.waits...)
		}
		return all
	}
	for _, c := range []struct {
		slow     float64
		maxRatio float64 // mean turnaround pressure-aware / cpu-only
	}{{2.5, 0.85}, {1.5, 1.0}} {
		before := run(fmt.Sprintf("gx10 %.1fx slow, cpu-only scoring", c.slow), c.slow, true)
		after := run(fmt.Sprintf("gx10 %.1fx slow, pressure-aware", c.slow), c.slow, false)
		t.Logf("gx10 %.1fx slower: jobs done %d -> %d, offloaded %d -> %d, turnaround mean %s -> %s, p95 %s -> %s, queue wait p95 %s -> %s",
			c.slow, before.completed, after.completed, before.offloaded, after.offloaded,
			fmtDur(mean(before.turnarounds)), fmtDur(mean(after.turnarounds)),
			fmtDur(percentile(before.turnarounds, .95)), fmtDur(percentile(after.turnarounds, .95)),
			fmtDur(percentile(before.waits, .95)), fmtDur(percentile(after.waits, .95)))
		if mean(after.turnarounds) > c.maxRatio*mean(before.turnarounds) {
			t.Errorf("%.1fx: mean turnaround %s, cpu-only scoring %s", c.slow,
				fmtDur(mean(after.turnarounds)), fmtDur(mean(before.turnarounds)))
		}
		if after.completed < before.completed {
			t.Errorf("%.1fx: %d jobs done, cpu-only scoring %d", c.slow, after.completed, before.completed)
		}
	}
}
