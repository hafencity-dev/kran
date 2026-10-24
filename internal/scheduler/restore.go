package scheduler

import (
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Restore records the allocation of a job that kept running while the
// daemon restarted: the new scheduler would otherwise see its CPUs and
// memory as free until it ends. It is not checked against free room (the
// job runs either way), and the node may not be known yet: its entry is
// created and filled by the first UpdateNode.
func (s *sched) Restore(job *proto.Job, pl Placement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := job.Spec.ID
	if id == "" || pl.Node == "" || s.foreign[id] != nil || s.jobs[id] != nil {
		return
	}
	now := s.opts.Now()
	e := s.nodes[pl.Node]
	if e == nil {
		e = &nodeEntry{allocs: map[string]*allocation{}}
		s.nodes[pl.Node] = e
	}
	est := job.Estimate
	normalizeEstimate(&est)
	left := time.Duration(est.Duration * float64(time.Second))
	if !job.RunningAt.IsZero() {
		left = max(0, left-now.Sub(job.RunningAt)) // it has run a while already
	}
	end := now.Add(left)
	pl.JobID = id
	a := &allocation{job: id, node: pl.Node, rt: pl.Runtime, res: pl.Alloc, at: now, end: end,
		foreign: true, repo: job.Spec.Workspace.RepoID, pl: pl}
	if e.st.Info.Name != "" {
		a.vm, a.vmKnown = runsInVM(e.st.Info, pl.Runtime)
	}
	e.allocs[id] = a
	s.foreign[id] = a
	s.dispatch(now)
}

// runsInVM reports whether a job of that runtime kind counts against the
// node's Linux VM pool (a container runtime for another OS than the host's).
func runsInVM(info proto.NodeInfo, kind proto.RuntimeKind) (vm, known bool) {
	if kind != proto.RuntimeContainer {
		return false, true
	}
	for _, rt := range info.Runtimes {
		if rt.Kind == kind {
			return rt.OS != info.OS, true
		}
	}
	return false, len(info.Runtimes) > 0
}
