package scheduler

import (
	"math"
	"sort"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// pool is one resource budget: a node's host, or the Linux VM that backs
// the container runtime on macOS. Values are recomputed for every pass from
// the node state and this scheduler's allocations, then updated in place
// as the pass places jobs.
type pool struct {
	capCPU   float64 // schedulable (after reserve)
	capMem   int64
	allocCPU float64 // Σ admitted kran jobs
	allocMem int64
	// liveMem is what the live metrics say is free after the reserve and
	// after memory that admitted jobs have not touched yet.
	liveMem int64
	// cpuPenalty is non-kran CPU load beyond the owner's reserve.
	cpuPenalty float64
	holdCPU    float64 // held for a starving job during this pass
	holdMem    int64
	// press is contention beyond CPU allocation (memory, I/O, swap): how
	// much slower a job runs here (pressure.go).
	press pressure
}

const unlimited = math.MaxInt64 / 4

func (p *pool) freeMem() int64 {
	return min(p.capMem-p.allocMem, p.liveMem) - p.holdMem
}

func (p *pool) freeCPU() float64 {
	return p.capCPU - p.allocCPU - p.cpuPenalty - p.holdCPU
}

func (p *pool) take(r proto.Resources) {
	p.allocCPU += r.CPUs
	p.allocMem += r.Mem
	if p.liveMem < unlimited {
		p.liveMem -= r.Mem
	}
}

type rtView struct {
	rt proto.Runtime
	vm bool // uses the node's VM pool (container runtime of a non-Linux host)
}

type nodeView struct {
	name    string
	e       *nodeEntry
	self    bool
	host    pool
	vm      *pool
	running int
	rts     []rtView
	allocs  []*allocation // sorted by expected end
}

func (v *nodeView) pools(rt rtView) []*pool {
	if rt.vm && v.vm != nil {
		return []*pool{&v.host, v.vm}
	}
	return []*pool{&v.host}
}

// view computes the schedulable state of a node. Callers hold s.mu.
func (s *sched) view(name string, now time.Time) *nodeView {
	e := s.nodes[name]
	st := e.st
	info := st.Info
	v := &nodeView{name: name, e: e, self: name == s.opts.Origin}

	capCPU, capMem := st.Capacity.CPUs, st.Capacity.Mem
	if capCPU <= 0 {
		capCPU = float64(info.CPUs) - st.Reserve.CPUs
	}
	if capMem <= 0 {
		capMem = info.MemTotal - st.Reserve.Mem
	}
	v.host = pool{capCPU: math.Max(0, capCPU), capMem: max(0, capMem), liveMem: unlimited,
		press: hostPressure(st.Metrics, info)}

	// Allocations: this scheduler's own (and, on the self node, foreign
	// admits). A peer's report also counts jobs other origins placed there.
	var ownCPU, vmCPU float64
	var ownMem, vmMem int64
	ownCount := 0
	inReport := map[string]bool{}
	for _, id := range st.AllocatedJobs {
		inReport[id] = true
	}
	var notReportedCPU float64
	var notReportedMem int64
	notReported := 0
	for _, a := range e.allocs {
		v.allocs = append(v.allocs, a)
		ownCPU += a.res.CPUs
		ownMem += a.res.Mem
		ownCount++
		if a.vm {
			vmCPU += a.res.CPUs
			vmMem += a.res.Mem
		}
		if !inReport[a.job] {
			notReportedCPU += a.res.CPUs
			notReportedMem += a.res.Mem
			notReported++
		}
	}
	sort.Slice(v.allocs, func(i, k int) bool {
		if !v.allocs[i].end.Equal(v.allocs[k].end) {
			return v.allocs[i].end.Before(v.allocs[k].end)
		}
		return v.allocs[i].job < v.allocs[k].job
	})
	switch {
	case v.self:
		v.host.allocCPU, v.host.allocMem, v.running = ownCPU, ownMem, ownCount
	case st.AllocatedJobs != nil:
		v.host.allocCPU = st.Allocated.CPUs + notReportedCPU
		v.host.allocMem = st.Allocated.Mem + notReportedMem
		v.running = st.Running + notReported
	default:
		// Without job IDs the report and our placements may overlap; the
		// target's own admission check catches what this misses.
		v.host.allocCPU = math.Max(st.Allocated.CPUs, ownCPU)
		v.host.allocMem = max(st.Allocated.Mem, ownMem)
		v.running = max(st.Running, ownCount)
	}
	foreignCPU := v.host.allocCPU - ownCPU // kran jobs of other origins on a peer

	m := st.Metrics
	if !m.Time.IsZero() {
		pending := int64(0)
		visibleCPU := math.Max(0, foreignCPU)
		for _, a := range e.allocs {
			pending += pendingMem(a, m.Time, v.self)
			visibleCPU += visibleCPUOf(a, m.Time)
		}
		v.host.liveMem = m.MemAvailable - st.Reserve.Mem - pending
		busy := m.CPUBusy * float64(info.CPUs)
		nonKran := math.Max(0, busy-visibleCPU)
		v.host.cpuPenalty = math.Max(0, nonKran-st.Reserve.CPUs)
	}

	// Runtimes this node offers to this origin.
	if v.self {
		// The origin runs its jobs itself, natively (the CLI executes them).
		v.rts = []rtView{{rt: proto.Runtime{Kind: proto.RuntimeNative, OS: info.OS, Arch: info.Arch,
			CPUs: info.CPUs, Mem: info.MemTotal, Available: true}}}
	} else if len(info.Runtimes) == 0 {
		// Peer did not report runtimes: assume its native runtime.
		v.rts = []rtView{{rt: proto.Runtime{Kind: proto.RuntimeNative, OS: info.OS, Arch: info.Arch,
			CPUs: info.CPUs, Mem: info.MemTotal, Available: true}}}
	} else {
		for _, rt := range info.Runtimes {
			v.rts = append(v.rts, rtView{rt: rt, vm: rt.Kind == proto.RuntimeContainer && rt.OS != info.OS})
		}
	}
	// The VM pool exists whenever the node runs Linux containers in a VM,
	// also on the self node (foreign jobs are admitted into it).
	for _, rt := range info.Runtimes {
		rv := rtView{rt: rt, vm: rt.Kind == proto.RuntimeContainer && rt.OS != info.OS}
		if !rv.vm || !rv.rt.Available || v.vm != nil {
			continue
		}
		// The VM keeps some memory for its own kernel and Docker.
		overhead := max(512*mib, rv.rt.Mem/20)
		vp := &pool{capCPU: float64(rv.rt.CPUs), capMem: max(0, rv.rt.Mem-overhead), liveMem: unlimited,
			allocCPU: vmCPU, allocMem: vmMem, press: vmPressure(v.host.press, 0)}
		if rm, ok := m.Runtimes[proto.RuntimeContainer]; ok && !m.Time.IsZero() {
			pending := int64(0)
			visible := 0.0
			for _, a := range e.allocs {
				if a.vm {
					pending += pendingMem(a, m.Time, false)
					visible += visibleCPUOf(a, m.Time)
				}
			}
			if rm.MemAvailable > 0 {
				vp.liveMem = rm.MemAvailable - pending
			}
			vp.cpuPenalty = math.Max(0, rm.CPUBusy*float64(rv.rt.CPUs)-visible)
			vp.press = vmPressure(v.host.press, rm.CPUBusy)
		}
		v.vm = vp
	}
	return v
}

// pendingMem is the part of an allocation the node's live metrics (sampled
// at mt) do not show as used yet: the job may still grow into it.
func pendingMem(a *allocation, mt time.Time, local bool) int64 {
	if !a.at.Before(mt) {
		return a.res.Mem // placed after the sample
	}
	if a.live != nil {
		return max(0, a.res.Mem-a.live.Mem)
	}
	ramp := rampRemote
	if local {
		ramp = rampLocal
	}
	if mt.Sub(a.at) < ramp {
		return a.res.Mem
	}
	return 0
}

// visibleCPUOf is the kran CPU load of an allocation as the sample at mt saw it.
func visibleCPUOf(a *allocation, mt time.Time) float64 {
	if !a.at.Before(mt) {
		return 0
	}
	if a.live != nil {
		return a.live.CPUs
	}
	return a.res.CPUs
}

// fits reports whether need fits the runtime's pools now. cpu is the
// clamped CPU need.
func (v *nodeView) fits(rt rtView, cpu float64, mem int64, ignoreHolds bool) bool {
	for _, p := range v.pools(rt) {
		freeMem, freeCPU := p.freeMem(), p.freeCPU()
		if ignoreHolds {
			freeMem += p.holdMem
			freeCPU += p.holdCPU
		}
		if mem > freeMem || cpu > freeCPU*cpuOvercommit+1e-9 {
			return false
		}
	}
	if max := v.e.st.MaxJobs; max > 0 && v.running >= max {
		return false
	}
	return true
}

// pressure is the contention a job in the runtime would run under.
func (v *nodeView) pressure(rt rtView) pressure {
	if rt.vm && v.vm != nil {
		return v.vm.press
	}
	return v.host.press
}

// capMem is the largest job memory the runtime can ever take.
func (v *nodeView) capMem(rt rtView) int64 {
	c := v.host.capMem
	if rt.vm && v.vm != nil && v.vm.capMem < c {
		c = v.vm.capMem
	}
	return c
}

func (v *nodeView) capCPU(rt rtView) float64 {
	c := v.host.capCPU
	if rt.vm && v.vm != nil && v.vm.capCPU < c {
		c = v.vm.capCPU
	}
	if c <= 0 {
		c = 1
	}
	return c
}

// empty reports whether no kran job holds resources in the runtime's pools.
func (v *nodeView) empty(rt rtView) bool {
	for _, p := range v.pools(rt) {
		if p.allocMem > 0 || p.allocCPU > 0 || p.holdMem > 0 {
			return false
		}
	}
	return v.running == 0
}

// waitFor estimates seconds until need fits on the runtime, assuming
// running jobs end as estimated and nothing else arrives. +Inf when it
// would not fit even with every known job gone (non-kran load, foreign
// jobs with unknown ends).
func (v *nodeView) waitFor(rt rtView, cpu float64, mem int64, now time.Time) float64 {
	if v.fits(rt, cpu, mem, false) {
		return 0
	}
	type state struct {
		freeCPU float64
		freeMem int64
	}
	ps := v.pools(rt)
	cur := make([]state, len(ps))
	for i, p := range ps {
		cur[i] = state{p.freeCPU(), p.freeMem()}
	}
	running := v.running
	for _, a := range v.allocs {
		for i, p := range ps {
			if p == v.vm && !a.vm {
				continue
			}
			cur[i].freeCPU += a.res.CPUs
			cur[i].freeMem += a.res.Mem
		}
		running--
		ok := true
		for i := range ps {
			if mem > cur[i].freeMem || cpu > cur[i].freeCPU*cpuOvercommit+1e-9 {
				ok = false
			}
		}
		if mx := v.e.st.MaxJobs; mx > 0 && running >= mx {
			ok = false
		}
		if ok {
			left := a.end.Sub(now).Seconds()
			if left < 5 {
				// Overdue or nearly done: estimates run late; assume a bit more.
				left = 5
			}
			return left
		}
	}
	return math.Inf(1)
}

// load is a tie-break measure: the fraction of the host in use.
func (v *nodeView) load() float64 {
	if v.host.capCPU <= 0 {
		return 1
	}
	return (v.host.allocCPU + v.host.cpuPenalty) / v.host.capCPU
}

// freeCPUFor is the free CPU of the runtime's tightest pool.
func (v *nodeView) freeCPUFor(rt rtView) float64 {
	f := math.Inf(1)
	for _, p := range v.pools(rt) {
		f = math.Min(f, p.freeCPU())
	}
	return f
}

// shortCPUOvercommit bounds how far short local jobs may push allocated
// CPU past capacity (they are admitted on memory alone).
const shortCPUOvercommit = 2.0

// fitsShort is the admission check for short local jobs: memory must fit,
// CPU only within shortCPUOvercommit × capacity.
func (v *nodeView) fitsShort(rt rtView, cpu float64, mem int64) bool {
	for _, p := range v.pools(rt) {
		if mem > p.freeMem() || p.allocCPU+p.holdCPU+cpu > shortCPUOvercommit*p.capCPU+1e-9 {
			return false
		}
	}
	if mx := v.e.st.MaxJobs; mx > 0 && v.running >= mx {
		return false
	}
	return true
}
