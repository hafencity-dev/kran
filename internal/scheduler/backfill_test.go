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
