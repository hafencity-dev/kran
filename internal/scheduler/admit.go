package scheduler

import (
	"fmt"
	"math"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Admit reserves resources on this node for a job another origin placed
// here. It never queues: the origin re-places the job when this fails.
// Starving local jobs keep their holds, so a busy peer cannot starve this
// node's own queue.
func (s *sched) Admit(job *proto.Job, kind proto.RuntimeKind) (Placement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	self := s.opts.Origin
	id := job.Spec.ID
	if a := s.foreign[id]; a != nil {
		return a.pl, nil // retried admit: idempotent
	}
	e := s.nodes[self]
	if e == nil || (e.st.Info.CPUs == 0 && e.st.Info.MemTotal == 0) {
		return Placement{}, &AdmitError{Code: "unknown", Reason: self + ": not ready yet"}
	}
	st := e.st
	switch {
	case st.Status == proto.NodeDraining:
		return Placement{}, &AdmitError{Code: "draining", Reason: self + ": draining"}
	case !st.Accepts:
		return Placement{}, &AdmitError{Code: "not_accepting", Reason: self + ": not accepting jobs (accept.enabled is off)"}
	}
	floor := proto.DiskFloor(st.Info.DiskTotal)
	if m := st.Metrics; !m.Time.IsZero() && m.DiskFree > 0 && m.DiskFree < floor {
		return Placement{}, &AdmitError{Code: "disk_floor", Reason: fmt.Sprintf("%s: disk almost full (%s free)", self, fmtBytes(m.DiskFree))}
	}
	v := s.view(self, now)
	if lim := st.MaxJobs; lim > 0 && v.running >= lim {
		return Placement{}, &AdmitError{Code: "max_jobs", Reason: fmt.Sprintf("%s: at its limit of %d jobs", self, lim)}
	}
	rv, ok := s.selfRuntime(v, kind)
	if !ok {
		return Placement{}, &AdmitError{Code: "no_runtime", Reason: fmt.Sprintf("%s: no %s runtime available", self, kind)}
	}
	if h, ok := s.holds[self]; ok {
		v.host.holdCPU += h.res.CPUs
		v.host.holdMem += h.res.Mem
		if h.vm && v.vm != nil {
			v.vm.holdCPU += h.res.CPUs
			v.vm.holdMem += h.res.Mem
		}
	}
	est := job.Estimate
	normalizeEstimate(&est)
	res := proto.Resources{CPUs: math.Min(est.CPUs, v.capCPU(rv)), Mem: est.Mem}
	reason := "admitted"
	switch {
	case res.Mem > v.capMem(rv):
		if !v.empty(rv) {
			return Placement{}, &AdmitError{Code: "busy", Reason: fmt.Sprintf("%s: needs %s, more than it has; it runs only when %s is idle",
				self, fmtBytes(res.Mem), self)}
		}
		res.Mem = v.capMem(rv)
		reason = "admitted alone (larger than this node)"
	case !v.fits(rv, res.CPUs, res.Mem, false):
		var fm int64 = math.MaxInt64
		fc := math.Inf(1)
		for _, p := range v.pools(rv) {
			fm = min(fm, p.freeMem())
			fc = math.Min(fc, p.freeCPU())
		}
		return Placement{}, &AdmitError{Code: "busy", Reason: fmt.Sprintf("%s: needs %s and %s, has %s and %s free",
			self, fmtCPUs(res.CPUs), fmtBytes(res.Mem), fmtCPUs(nonNeg(fc)), fmtBytes(max(0, fm)))}
	}
	pl := Placement{JobID: id, Node: self, Runtime: kind, Alloc: res, Reason: reason}
	a := &allocation{
		job: id, node: self, rt: kind, vm: rv.vm, vmKnown: true, res: res, at: now,
		end:     now.Add(time.Duration(est.Duration * float64(time.Second))),
		foreign: true, repo: job.Spec.Workspace.RepoID, pl: pl,
	}
	e.allocs[id] = a
	s.foreign[id] = a
	s.dispatch(now) // queue reasons now see less room
	return pl, nil
}

func (s *sched) selfRuntime(v *nodeView, kind proto.RuntimeKind) (rtView, bool) {
	info := v.e.st.Info
	for _, rt := range info.Runtimes {
		if rt.Kind == kind && rt.Available {
			return rtView{rt: rt, vm: rt.Kind == proto.RuntimeContainer && rt.OS != info.OS}, true
		}
	}
	if len(info.Runtimes) == 0 && kind == proto.RuntimeNative {
		return v.rts[0], true
	}
	return rtView{}, false
}
