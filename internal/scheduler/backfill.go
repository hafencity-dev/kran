package scheduler

import (
	"fmt"
	"math"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Conservative backfill (SPEC §6.3). A starving job holds its best node
// (maybeHold): later jobs see the held room as taken. Without more, one
// large job that waits for room holds every later job behind it, also a
// 3-second test that would be done long before the large one can start.
// A later job may therefore pass the held job when, by the estimates, it
// cannot delay it:
//
//   - it ends before the held job can start (the earliest time enough
//     running jobs end for it to fit); only when that time is known: with
//     non-kran load, jobs of other origins, or a job larger than any node
//     (which holds the whole node) in the way there is no bound, and a
//     long job that "ends before never" would delay the held job by all
//     of its run; or
//   - it fits in the room that is left beside the held job once that one
//     starts, so it never takes what the held job waits for. CPU room: the
//     held job fits when its CPUs ≤ (free − passer's) × 1.25, so a passer
//     may take at most free − held/1.25, compared without the factor.
//
// Aging bound: once the held job would fit if the jobs that passed it by
// ending first were gone, nothing passes it any more except into room it
// does not need. From then on the set of jobs in its way only shrinks, so
// it starts at the latest when the jobs that already passed it end, however
// long they run past their estimates and however many small jobs keep
// arriving.

// shadow is what a hold looks like ahead in time.
type shadow struct {
	// start is the seconds until the held job fits by the estimates of the
	// jobs running there (+Inf: not by known ends; non-kran load or jobs of
	// other origins stand in its way).
	start float64
	// frozen: the held job would fit now but for jobs that passed it by
	// ending first.
	frozen bool
	// extra is the room per pool left beside the held job once it starts
	// (nil when unknown: start is +Inf).
	extra map[*pool]proto.Resources
}

// shadowOf computes the shadow of hold h on v from v's current numbers
// (which include the jobs placed earlier in this pass).
func (v *nodeView) shadowOf(h hold, now time.Time) shadow {
	ps := v.pools(rtView{vm: h.vm})
	type room struct {
		cpu float64
		mem int64
	}
	free := make([]room, len(ps))
	for i, p := range ps {
		// The held job's own hold is not in its way.
		free[i] = room{p.freeCPU() + p.holdCPU, p.freeMem() + p.holdMem}
	}
	fits := func(r []room, running int) bool {
		for i := range r {
			if h.res.Mem > r[i].mem || h.res.CPUs > r[i].cpu*cpuOvercommit+1e-9 {
				return false
			}
		}
		mx := v.e.st.MaxJobs
		return mx <= 0 || running < mx
	}
	extra := func(r []room) map[*pool]proto.Resources {
		out := make(map[*pool]proto.Resources, len(ps))
		for i, p := range ps {
			// The held job fits when h ≤ (r − c)·overcommit, so a passer may
			// take at most r − h/overcommit CPUs (no overcommit on its side).
			out[p] = proto.Resources{CPUs: r[i].cpu - h.res.CPUs/cpuOvercommit, Mem: r[i].mem - h.res.Mem}
		}
		return out
	}
	add := func(r []room, a *allocation) {
		for i, p := range ps {
			if p == v.vm && !a.vm {
				continue
			}
			r[i].cpu += a.res.CPUs
			r[i].mem += a.res.Mem
		}
	}

	// The aging bound: are only jobs that passed it by ending first in its
	// way? (Jobs that passed into room it does not need are not.)
	without := append([]room(nil), free...)
	running, passed := v.running, 0
	for _, a := range v.allocs {
		if a.passed != "" && a.passedFirst {
			add(without, a)
			running--
			passed++
		}
	}
	if passed > 0 && fits(without, running) {
		return shadow{frozen: true, extra: extra(without)}
	}

	// When does it fit as running jobs end as estimated (as waitFor)?
	cur := append([]room(nil), free...)
	running = v.running
	for _, a := range v.allocs {
		add(cur, a)
		running--
		if fits(cur, running) {
			left := a.end.Sub(now).Seconds()
			if left < 5 {
				// Overdue or nearly done: estimates run late.
				left = 5
			}
			return shadow{start: left, extra: extra(cur)}
		}
	}
	return shadow{start: math.Inf(1)}
}

// mayPass decides whether job j may start on candidate c although c's
// node is held for another job (fitsIgnoringHold: j fits there with the
// hold not counted). It sets c.passes to the held job, c.passFirst when j
// ends before that one can start (otherwise it fits beside it) and
// c.passNote to why (for the log).
func (s *sched) mayPass(j *sjob, c *cand, holds map[string]hold, now time.Time, fitsIgnoringHold bool) bool {
	h, ok := holds[c.v.name]
	if !ok || h.job == j.id || !fitsIgnoringHold {
		return false
	}
	sh := c.v.shadowOf(h, now)
	// An unknown start (+Inf: non-kran load, jobs of other origins, a job
	// that holds a whole node) gives no bound: nothing passes first then.
	if !sh.frozen && !math.IsInf(sh.start, 1) && c.total() <= sh.start {
		c.passes, c.passFirst = h.job, true
		c.passNote = fmt.Sprintf("ends in ~%s, before %s can start (~%s)", fmtDur(c.total()), h.job, fmtDur(sh.start))
		return true
	}
	if sh.extra == nil || c.v.e.st.MaxJobs > 0 {
		return false
	}
	for _, p := range c.v.pools(c.rt) {
		e, ok := sh.extra[p]
		if !ok {
			continue // a pool the held job does not use
		}
		if c.mem > e.Mem || c.cpu > e.CPUs+1e-9 {
			return false
		}
	}
	c.passes, c.passFirst, c.passNote = h.job, false, "uses only room "+h.job+" does not need"
	return true
}
