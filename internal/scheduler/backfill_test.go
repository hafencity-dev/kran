package scheduler

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// heldGX10 replays 2026-11-15 on gx10: two prechecks run (20 GiB and 3.4
// CPUs asked each, 25 minutes estimated), a third waits for 30 GiB and
// has starved past the boost, so gx10 is held for it. 16 GiB and 11 CPUs
// are free beside the two running prechecks.
func heldGX10(t *testing.T) (*sched, *fakeClock, Ticket) {
	t.Helper()
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 64 * gib // 64 − 8 reserve − 2 × 20 asked = 16 GiB free
	s := newTestSched(t, clk, g)
	placed(t, s.Submit(newJob("pre1", "b1", 3.4, 20*gib, 25*60, withReq(func(r *proto.Requirements) { r.Prefer = proto.PreferLocal }))))
	placed(t, s.Submit(newJob("pre2", "b2", 3.4, 20*gib, 25*60, withReq(func(r *proto.Requirements) { r.Prefer = proto.PreferLocal }))))
	head := s.Submit(newJob("pre3", "b3", 3.4, 30*gib, 25*60, withReq(func(r *proto.Requirements) { r.Prefer = proto.PreferLocal })))
	pending(t, head)
	clk.Advance(3 * time.Minute)
	s.Tick(clk.Now())
	pending(t, head)
	if r := reason(s, "pre3"); !strings.Contains(r, "next in line on gx10") {
		t.Fatalf("head job is not holding gx10: %q", r)
	}
	return s, clk, head
}

// TestBackfillSmallJobsPassAHeldJob: jobs that end before the held job can
// start, or that use only room it does not need, start at once instead of
// waiting behind it; the held job keeps its place.
func TestBackfillSmallJobsPassAHeldJob(t *testing.T) {
	s, clk, head := heldGX10(t)
	// Six 3-second tests of several agents (short: they stay here).
	for i := range 6 {
		pl := placed(t, s.Submit(newJob(fmt.Sprintf("t%d", i), fmt.Sprintf("agent%d", i), 1, gib, 3)))
		if pl.Node != "gx10" || strings.Contains(pl.Reason, "pre3") {
			t.Errorf("t%d: %+v", i, pl)
		}
	}
	// A 2-minute build of 8 GiB: ends long before pre1 or pre2 can.
	placed(t, s.Submit(newJob("build", "agent7", 4, 4*gib, 120)))
	pending(t, head)
	if r := reason(s, "pre3"); !strings.Contains(r, "next in line on gx10") {
		t.Errorf("head job lost its hold: %q", r)
	}
	// Everything that passed still leaves pre3 its 30 GiB the moment pre1
	// ends as estimated.
	clk.Advance(2 * time.Minute)
	for _, id := range []string{"t0", "t1", "t2", "t3", "t4", "t5", "build"} {
		s.Release(id, nil)
	}
	clk.Advance(20 * time.Minute)
	s.Release("pre1", nil)
	if pl := placed(t, head); pl.Node != "gx10" {
		t.Fatalf("head job: %+v", pl)
	}
}

// TestBackfillLongJobIntoRoomTheHeldJobDoesNotNeed: a long job may pass
// when it fits beside the held job once that one starts.
func TestBackfillLongJobIntoRoomTheHeldJobDoesNotNeed(t *testing.T) {
	s, _, head := heldGX10(t)
	// When pre1 ends, 16 + 20 = 36 GiB are free: pre3 takes 30, 6 stay.
	placed(t, s.Submit(newJob("long-small", "a", 1, 5*gib, 3600)))
	// 10 GiB for an hour (11 GiB are free now) would take what pre3 waits
	// for: it waits too.
	long := s.Submit(newJob("long-big", "b", 1, 10*gib, 3600))
	pending(t, long)
	if r := reason(s, "long-big"); !strings.Contains(r, "gx10 is held for a job that has waited longer") {
		t.Errorf("waiting reason %q", r)
	}
	pending(t, head)
}

// TestBackfillNeverStarvesHeldJobInvariant is the aging bound: a steady
// stream of small jobs whose estimates are too short (3 s estimated, 20 s
// real) cannot postpone the held job for ever. Once it would fit but for
// jobs that passed it, nothing passes it any more; it starts when those
// end.
func TestBackfillNeverStarvesHeldJobInvariant(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 64 * gib // 56 GiB schedulable
	s := newTestSched(t, clk, g)
	// pre1 (30 GiB, estimated 60 s) overruns: it really ends at 5 min.
	placed(t, s.Submit(newJob("pre1", "b1", 2, 30*gib, 60)))
	head := s.Submit(newJob("head", "b2", 2, 40*gib, 600))
	pending(t, head)
	const realRun = 20 * time.Second
	ends := map[string]time.Time{}
	var queued []Ticket
	pre1End := t0.Add(5 * time.Minute)
	var headAt, frozenSince time.Time
	passedAfterFreeze := 0
	for sec := 1; sec <= 30*60; sec++ {
		clk.Advance(time.Second)
		now := clk.Now()
		for id, end := range ends {
			if !now.Before(end) {
				s.Release(id, nil)
				delete(ends, id)
			}
		}
		if now.Equal(pre1End) {
			s.Release("pre1", nil)
		}
		queued = append(queued, s.Submit(newJob(fmt.Sprintf("s%04d", sec), fmt.Sprintf("agent%d", sec%7), 1, 8*gib, 3)))
		s.Tick(now)
		// Small jobs start whenever the scheduler places them (on submit,
		// on a release, on a tick); each really runs realRun.
		waiting := queued[:0]
		for _, tk := range queued {
			if _, ok := isPlaced(tk); !ok {
				waiting = append(waiting, tk)
				continue
			}
			ends[tk.JobID()] = now.Add(realRun)
			if !frozenSince.IsZero() {
				passedAfterFreeze++
			}
		}
		queued = waiting
		if _, ok := isPlaced(head); ok {
			headAt = now
			break
		}
		if !now.Before(pre1End) && frozenSince.IsZero() {
			// From here only small jobs that passed it stand in its way.
			frozenSince = now
		}
	}
	if headAt.IsZero() {
		t.Fatalf("head job still waits after 30 minutes behind a stream of small jobs: %q", reason(s, "head"))
	}
	if headAt.After(pre1End.Add(realRun + time.Second)) {
		t.Errorf("head job started %s after pre1 ended; the jobs in its way end within %s", headAt.Sub(pre1End), realRun)
	}
	if passedAfterFreeze > 0 {
		t.Errorf("%d small jobs passed the head job after only jobs that had passed it stood in its way", passedAfterFreeze)
	}
	t.Logf("head job placed %s after submission, %s after pre1 ended", headAt.Sub(t0), headAt.Sub(pre1End))
}

// TestBackfillDeterministicReasonsWithHolds: with a hold in place, jobs of
// the same shape but different durations are judged one by one (the
// shape memo would otherwise copy the first one's verdict).
func TestBackfillDeterministicReasonsWithHolds(t *testing.T) {
	s, _, _ := heldGX10(t)
	long := s.Submit(newJob("long", "a", 1, 12*gib, 3600))
	short := s.Submit(newJob("short", "a", 1, 12*gib, 60))
	pending(t, long)
	if _, ok := isPlaced(short); !ok {
		t.Fatalf("a 1-minute job of the same shape waits: %q", reason(s, "short"))
	}
}

// TestFreeMemoryCountsOnlyTheUnusedPartOfReservations: 2026-11-15 on gx10,
// 58 GiB available, two prechecks asked 20 GiB each and use 3 GiB each.
// Free for placement is what is available (after the owner's reserve)
// minus the part of each reservation not used yet: 58 − 8 − 2 × 17 =
// 16 GiB, so a 14 GiB job starts. (Measured by process group the two
// prechecks looked like 160 MiB each and the whole reservations counted
// again: "8.5 GiB free". The daemon now measures local jobs by their
// scope: TestLocalJobLiveUsageComesFromItsScope.)
func TestFreeMemoryCountsOnlyTheUnusedPartOfReservations(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	s := newTestSched(t, clk, g)
	local := withReq(func(r *proto.Requirements) { r.Prefer = proto.PreferLocal })
	placed(t, s.Submit(newJob("pre1", "b1", 3.4, 20*gib, 25*60, local)))
	placed(t, s.Submit(newJob("pre2", "b2", 3.4, 20*gib, 25*60, local)))
	clk.Advance(30 * time.Second)
	s.UpdateJobUsage("pre1", proto.Resources{CPUs: 2, Mem: 3 * gib})
	s.UpdateJobUsage("pre2", proto.Resources{CPUs: 2, Mem: 3 * gib})
	g.Metrics.Time = clk.Now()
	g.Metrics.MemAvailable = 58 * gib
	g.Metrics.CPUBusy = 4.0 / 20 // the two prechecks' own load
	s.UpdateNode(g)
	if free := s.Snapshot().Free["gx10"]; free.Mem != 16*gib {
		t.Errorf("free %s, want 16 GiB", fmtBytes(free.Mem))
	} else if free.CPUs < 18-2*3.4-1e-9 {
		t.Errorf("free %.1f CPUs: the prechecks' own load counted beside their CPUs", free.CPUs)
	}
	if pl := placed(t, s.Submit(newJob("j14", "c", 2, 14*gib, 600, local))); pl.Node != "gx10" {
		t.Fatalf("%+v", pl)
	}
}

// The four tests below are review 117's probes A-D (2026-11-15), red on
// 5b559777 and green since: an unknown start must not let anything pass the
// held job by "ending first", and "room the held job does not need" must
// count CPUs as the held job's own admission will.

var localOnly = withReq(func(r *proto.Requirements) { r.Prefer = proto.PreferLocal })

// TestBackfillUnknownStartLetsNoLongJobPassInvariant (probe A): non-kran
// memory stands in the held job's way, so when it can start is unknown. A
// 2-hour job must not pass it as if it ended first; when the load drops
// and pre1 ends, the held job starts.
func TestBackfillUnknownStartLetsNoLongJobPassInvariant(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 44 * gib // 44 − 8 reserve − 20 pre1 = 16 free
	s := newTestSched(t, clk, g)
	placed(t, s.Submit(newJob("pre1", "b1", 2, 20*gib, 25*60, localOnly)))
	head := s.Submit(newJob("head", "b2", 2, 40*gib, 600, localOnly))
	pending(t, head)
	clk.Advance(3 * time.Minute)
	s.Tick(clk.Now())
	pending(t, head)
	long := s.Submit(newJob("long", "c", 1, 16*gib, 2*3600, localOnly))
	if pl, ok := isPlaced(long); ok {
		t.Fatalf("a 2-hour job passed a held job whose start is unknown: %q", pl.Reason)
	}
	clk.Advance(time.Minute)
	s.Release("pre1", nil)
	g.Metrics.Time = clk.Now()
	g.Metrics.MemAvailable = 56 * gib // non-kran load dropped
	s.UpdateNode(g)
	s.Tick(clk.Now())
	if _, ok := isPlaced(head); !ok {
		t.Fatalf("the held job still waits once its room is free: %q", reason(s, "head"))
	}
}

// TestBackfillSpareRoomCPUWithoutOvercommitInvariant (probe B): an
// hour-long 16-CPU job must not pass into "room the held job does not
// need" that the held job then lacks: when A ends on time the held job
// (4 CPUs) starts.
func TestBackfillSpareRoomCPUWithoutOvercommitInvariant(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 60 * gib // 60 − 8 − 30 = 22 free
	s := newTestSched(t, clk, g)
	placed(t, s.Submit(newJob("A", "b1", 1, 30*gib, 10*60, localOnly)))
	head := s.Submit(newJob("head", "b2", 4, 40*gib, 600, localOnly))
	pending(t, head)
	clk.Advance(3 * time.Minute)
	s.Tick(clk.Now())
	pending(t, head)
	s.Submit(newJob("cpu", "c", 16, gib, 3600, localOnly))
	clk.Advance(7 * time.Minute)
	s.Release("A", nil)
	g.Metrics.Time = clk.Now()
	g.Metrics.MemAvailable = 90 * gib
	s.UpdateNode(g)
	s.Tick(clk.Now())
	if _, ok := isPlaced(head); !ok {
		t.Fatalf("the held job waits for CPUs a job that passed it took: %q", reason(s, "head"))
	}
}

// TestBackfillForeignJobsUnknownStartInvariant (probe C): devmac is held
// for a starving --remote job, and devmac's own jobs (another origin, ends
// unknown) are in its way. An hour-long job from here must not pass it
// there.
func TestBackfillForeignJobsUnknownStartInvariant(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 20 * gib
	d := devmac(t0)
	d.Allocated = proto.Resources{CPUs: 8, Mem: 60 * gib}
	d.AllocatedJobs = []string{"dm1", "dm2"}
	d.Running = 2
	d.Metrics.MemAvailable = 36 * gib
	s := newTestSched(t, clk, g, d)
	remote := withReq(func(r *proto.Requirements) { r.Prefer = proto.PreferRemote; r.OS = proto.OSAny })
	head := s.Submit(newJob("head", "b2", 2, 30*gib, 600, remote))
	pending(t, head)
	clk.Advance(3 * time.Minute)
	g.Metrics.Time, d.Metrics.Time = clk.Now(), clk.Now()
	s.UpdateNode(g)
	s.UpdateNode(d)
	s.Tick(clk.Now())
	pending(t, head)
	long := s.Submit(newJob("long", "c", 2, 12*gib, 3600, remote))
	if pl, ok := isPlaced(long); ok && pl.Node == "devmac" {
		t.Fatalf("an hour-long job passed a hold on devmac whose start is unknown: %q", pl.Reason)
	}
}

// TestBackfillOversizeJobNotStarvedInvariant (probe D): a job larger than
// any node holds the whole node and runs alone once it is empty; 10-second
// jobs arriving every 5 s must not keep it from ever starting.
func TestBackfillOversizeJobNotStarvedInvariant(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 100 * gib
	s := newTestSched(t, clk, g)
	placed(t, s.Submit(newJob("r0", "b1", 1, 4*gib, 60, localOnly)))
	big := s.Submit(newJob("big", "b2", 4, 200*gib, 600, localOnly))
	pending(t, big)
	ends := map[string]time.Time{"r0": t0.Add(60 * time.Second)}
	var q []Ticket
	for sec := 1; sec <= 30*60; sec++ {
		clk.Advance(time.Second)
		now := clk.Now()
		for id, e := range ends {
			if !now.Before(e) {
				s.Release(id, nil)
				delete(ends, id)
			}
		}
		if sec%5 == 0 {
			q = append(q, s.Submit(newJob(fmt.Sprintf("s%04d", sec), fmt.Sprintf("a%d", sec%5), 1, 2*gib, 10, localOnly)))
		}
		g.Metrics.Time = now
		s.UpdateNode(g)
		s.Tick(now)
		w := q[:0]
		for _, tk := range q {
			if _, ok := isPlaced(tk); ok {
				ends[tk.JobID()] = now.Add(20 * time.Second)
				continue
			}
			w = append(w, tk)
		}
		q = w
		if _, ok := isPlaced(big); ok {
			t.Logf("big started after %s", now.Sub(t0))
			return
		}
	}
	t.Fatalf("the job larger than any node never started in 30 min behind 10-s jobs every 5 s: %q", reason(s, "big"))
}
