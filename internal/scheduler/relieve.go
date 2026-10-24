package scheduler

import (
	"fmt"
	"math"
	"strings"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Relieving a busy origin (SPEC §6.6). The placement score asks only which
// node finishes this job first; on a busy origin that keeps a job here that
// another node would run about as fast, and every other process here (the
// user's sessions, servers, jobs started without kran) pays for it. So
// while the origin is busy, a check (Estimate.ResultOnly: nothing it writes
// is needed here) of at least relieveMinSeconds may go to a node where it is
// expected to take up to relieveMaxSlowdown longer, scaled by how busy the
// origin is. It takes only spare room there: after it, the node keeps
// relieveSpare of its CPUs and memory free for jobs that must run there
// (a pinned or --remote job is never kept waiting for a relieving one).
const (
	relieveMinSeconds  = 30.0
	relieveMaxSlowdown = 0.25
	relieveSpare       = 0.5
	// The origin counts as fully busy from relieveFullStall (contention
	// that slows a job ~1.33x) or relieveCPUFull of its CPUs in use;
	// below relieveCPUFrom in use, CPUs alone do not make it busy.
	relieveFullStall = 0.25
	relieveCPUFrom   = 0.6
	relieveCPUFull   = 0.9
)

// relieveOrigin returns a remote candidate to take a job that would run on
// the origin, and the reason note, or nil.
func (s *sched) relieveOrigin(j *sjob, self *cand, fits []*cand) (*cand, string) {
	if self == nil || !self.fits || !j.est.ResultOnly || j.est.Duration < relieveMinSeconds ||
		j.req.Prefer != proto.PreferAuto || j.req.Node != "" || j.originOnly() || j.req.GPU {
		return nil, ""
	}
	busy, why := originBusy(self)
	if busy <= 0 {
		return nil, ""
	}
	limit := self.total() * (1 + relieveMaxSlowdown*busy)
	for _, c := range fits { // best first
		if c.v.self || c.total() > limit || !c.v.keepsSpare(c.rt, c.cpu, c.mem, relieveSpare) {
			continue
		}
		return c, fmt.Sprintf("; %s is busy (%s): relieving it is worth up to %.0f%% longer", s.opts.Origin, why, relieveMaxSlowdown*busy*100)
	}
	return nil, ""
}

// originBusy rates how busy the origin is, 0 (calm) to 1, from its
// contention and its CPUs in use (kran jobs and other load), with the
// cause for the reason.
func originBusy(self *cand) (float64, string) {
	stall := stallOf(self.press.slow)
	byStall := stall / relieveFullStall
	p := self.v.host
	used := 0.0
	if p.capCPU > 0 {
		used = 1 - p.freeCPU()/p.capCPU
	}
	byCPU := (used - relieveCPUFrom) / (relieveCPUFull - relieveCPUFrom)
	busy := math.Min(1, math.Max(0, math.Max(byStall, byCPU)))
	if busy <= 0 {
		return 0, ""
	}
	if byStall >= byCPU && len(self.press.causes) > 0 {
		return busy, strings.Join(self.press.causes, ", ")
	}
	return busy, fmt.Sprintf("%.0f%% of its CPUs in use", math.Min(100, used*100))
}

// keepsSpare reports whether the runtime keeps at least share of its CPUs
// and memory free after taking cpu and mem.
func (v *nodeView) keepsSpare(rt rtView, cpu float64, mem int64, share float64) bool {
	for _, p := range v.pools(rt) {
		if p.freeCPU()-cpu < share*p.capCPU || float64(p.freeMem()-mem) < share*float64(p.capMem) {
			return false
		}
	}
	return true
}
