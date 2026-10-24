package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestIdleMeshKeepsJobLocalWhenSyncCostsMore(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)), devmac(t0))
	pl := placed(t, s.Submit(newJob("j1", "a", 8, 4*gib, 60)))
	if pl.Node != "gx10" || !pl.Local || pl.Runtime != proto.RuntimeNative {
		t.Fatalf("placement %+v", pl)
	}
	if !strings.HasPrefix(pl.Reason, "here: est. 1m; devmac (Linux container) would take 48s plus ~") {
		t.Errorf("reason %q", pl.Reason)
	}
	if pl.Alloc != (proto.Resources{CPUs: 8, Mem: 4 * gib}) {
		t.Errorf("alloc %+v", pl.Alloc)
	}
}

func TestShortJobsStayLocal(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 8*gib + 512*mib // nearly full here
	s := newTestSched(t, clk, g, devmac(t0))
	tk := s.Submit(newJob("j1", "a", 1, gib, 3))
	pending(t, tk)
	if r := reason(s, "j1"); r != "waiting: est. 3s, too short to send elsewhere; needs 1.0 GiB, gx10 has 512 MiB free" {
		t.Errorf("reason %q", r)
	}
	g.Metrics.MemAvailable = 20 * gib
	s.UpdateNode(g)
	pl := placed(t, tk)
	if pl.Node != "gx10" || pl.Reason != "here: est. 3s, too short to send elsewhere" {
		t.Errorf("%+v", pl)
	}
}

func TestOffloadsWhenOriginIsBusy(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 10 * gib // 2 GiB above the reserve
	s := newTestSched(t, clk, g, devmac(t0))
	pl := placed(t, s.Submit(newJob("j1", "a", 8, 4*gib, 60)))
	if pl.Node != "devmac" || pl.Local || pl.Runtime != proto.RuntimeContainer {
		t.Fatalf("placement %+v", pl)
	}
	want := "devmac (Linux container): 15.8 CPUs free, est. 1m5s; here: needs 4.0 GiB, gx10 has 2.0 GiB free"
	if pl.Reason != want {
		t.Errorf("reason\n got %q\nwant %q", pl.Reason, want)
	}
}

func TestOffloadsWhenFasterIncludingSync(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)), devmac(t0))
	// Long job, warm repo on devmac: 300 s here vs 240 s + 3 s there.
	s.mu.Lock()
	s.learnSyncCost("repo-a", "devmac", 3)
	s.mu.Unlock()
	pl := placed(t, s.Submit(newJob("j1", "a", 8, 4*gib, 300)))
	if pl.Node != "devmac" {
		t.Fatalf("%+v", pl)
	}
	if pl.Reason != "devmac (Linux container): 15.8 CPUs free, est. 4m3s vs 5m here" {
		t.Errorf("reason %q", pl.Reason)
	}
}

func TestWaitingReasonNamesBindingMemory(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 8*gib + fgib(1.2)
	d := devmac(t0)
	d.Metrics.Runtimes[proto.RuntimeContainer] = proto.RuntimeMetrics{MemAvailable: fgib(3.1)}
	s := newTestSched(t, clk, g, d)
	tk := s.Submit(newJob("j1", "a", 4, 6*gib, 120))
	pending(t, tk)
	want := "waiting: needs 6.0 GiB, devmac's Linux VM has 3.1 GiB free, gx10 has 1.2 GiB free"
	if r := reason(s, "j1"); r != want {
		t.Errorf("reason\n got %q\nwant %q", r, want)
	}
	select {
	case r := <-tk.Updates():
		if r != want {
			t.Errorf("update %q", r)
		}
	default:
		t.Error("no reason update delivered")
	}
	// With os=any the job may also run natively on macOS, where memory is free.
	tk2 := s.Submit(newJob("j2", "a", 4, 6*gib, 120, withReq(func(r *proto.Requirements) { r.OS = proto.OSAny })))
	if pl := placed(t, tk2); pl.Node != "devmac" || pl.Runtime != proto.RuntimeNative {
		t.Errorf("os=any: %+v", pl)
	}
}

func TestWaitingReasonCPU(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.CPUBusy = 0.9 // 18 cores busy outside kran: 16 beyond the reserve
	s := newTestSched(t, clk, g)
	pending(t, s.Submit(newJob("j1", "a", 8, 2*gib, 60)))
	if r := reason(s, "j1"); r != "waiting: needs 8 CPUs, gx10 has 2 free" {
		t.Errorf("reason %q", r)
	}
}

// A job that waited long logs why when it is placed: the queue entry
// with its reason is gone afterwards.
func TestPlacedAfterWaitLogsReason(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.CPUBusy = 0.9
	s := newTestSched(t, clk, g)
	var log []string
	s.opts.Logf = func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }
	tk := s.Submit(newJob("j1", "a", 8, 2*gib, 60, withQueuedAt(t0)))
	pending(t, tk)
	clk.Advance(2 * time.Minute)
	g = self(gx10(clk.t))
	s.UpdateNode(g)
	placed(t, tk)
	last := log[len(log)-1]
	if !strings.Contains(last, "placed j1") || !strings.Contains(last, "after waiting 2m: needs 8 CPUs, gx10 has 2 free") {
		t.Fatalf("log %q", last)
	}
}

func TestPlacedHereSaysWhyNotElsewhere(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)), devmac(t0))
	// More memory than devmac's Linux VM holds.
	if r := placed(t, s.Submit(newJob("big", "a", 4, 40*gib, 600))).Reason; !strings.Contains(r, "; devmac (Linux container) cannot hold 40.0 GiB") {
		t.Errorf("oversize reason %q", r)
	}
	// devmac's VM busy: say what it has free.
	for i := 0; i < 5; i++ {
		placed(t, s.Submit(newJob(fmt.Sprintf("f%d", i), "b", 1, 6*gib, 600, withReq(func(r *proto.Requirements) { r.Node = "devmac" }))))
	}
	if r := placed(t, s.Submit(newJob("j", "c", 4, 4*gib, 600))).Reason; !strings.Contains(r, "; needs 4.0 GiB, devmac's Linux VM has") {
		t.Errorf("busy reason %q", r)
	}
	d := devmac(clk.t)
	d.Status = proto.NodeDraining
	s.UpdateNode(d)
	if r := placed(t, s.Submit(newJob("k", "d", 4, 4*gib, 600))).Reason; !strings.HasSuffix(r, "; devmac: draining") {
		t.Errorf("draining reason %q", r)
	}
}

func TestCPUOvercommitFactor(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)))
	placed(t, s.Submit(newJob("a", "a", 10, gib, 60))) // 8 of 18 left
	placed(t, s.Submit(newJob("b", "a", 10, gib, 60))) // 10 <= 8*1.25
	pending(t, s.Submit(newJob("c", "a", 1, gib, 60))) // 18 allocated, none left
	if r := reason(s, "c"); r != "waiting: needs 1 CPU, gx10 has 0 free" {
		t.Errorf("reason %q", r)
	}
}

func TestMemoryNeverOvercommittedUnderBurst(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)), devmac(t0))
	var tickets []Ticket
	for i := 0; i < 60; i++ {
		tickets = append(tickets, s.Submit(newJob(fmt.Sprintf("j%02d", i), fmt.Sprintf("o%d", i%5), 1, 6*gib, 600)))
	}
	snap := s.Snapshot()
	// gx10: live 112 GiB / cap 120 → 18 jobs; devmac VM: 30.4 GiB → 5 jobs.
	if a := snap.Allocated["gx10"].Mem; a > 112*gib {
		t.Errorf("gx10 allocated %s", fmtBytes(a))
	}
	if snap.Running["gx10"] != 18 || snap.Running["devmac"] != 5 {
		t.Errorf("running %v", snap.Running)
	}
	if len(snap.Queue) != 60-23 {
		t.Errorf("queue %d", len(snap.Queue))
	}
	for _, id := range snap.Queue {
		if snap.Reasons[id] == "" {
			t.Errorf("%s has no reason", id)
		}
	}
	_ = tickets
}

func TestLiveNonKranMemoryReducesRoom(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	s := newTestSched(t, clk, g)
	placed(t, s.Submit(newJob("a", "a", 1, 50*gib, 600)))
	// Sample after the job started: it uses 20 GiB of its 50, and someone
	// else (an IDE, a browser) took 40 GiB.
	clk.Advance(30 * time.Second)
	s.UpdateJobUsage("a", proto.Resources{CPUs: 1, Mem: 20 * gib})
	clk.Advance(2 * time.Second)
	g.Metrics.Time = clk.Now()
	g.Metrics.MemAvailable = (128 - 20 - 40) * gib // 68 GiB
	s.UpdateNode(g)
	// free = min(120-50, 68-8-(50-20)) = 30 GiB
	if f := s.Snapshot().Free["gx10"].Mem; f != 30*gib {
		t.Errorf("free %s, want 30 GiB", fmtBytes(f))
	}
	pending(t, s.Submit(newJob("b", "a", 1, 31*gib, 600)))
	placed(t, s.Submit(newJob("c", "a", 1, 30*gib, 600)))
}

func TestJobLargerThanAnyNodeRunsAloneOnLargest(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)), devmac(t0))
	small := placed(t, s.Submit(newJob("small", "a", 1, gib, 600, withReq(func(r *proto.Requirements) { r.Node = "gx10" }))))
	tk := s.Submit(newJob("huge", "a", 8, 200*gib, 600, withReq(func(r *proto.Requirements) { r.OS = proto.OSAny })))
	pending(t, tk)
	want := "waiting: needs 200.0 GiB, more than any node has; will run alone on gx10 (120.0 GiB) once its 1 running jobs finish"
	if r := reason(s, "huge"); r != want {
		t.Errorf("reason\n got %q\nwant %q", r, want)
	}
	s.Release(small.JobID, nil)
	pl := placed(t, tk)
	if pl.Node != "gx10" || pl.Alloc.Mem != 120*gib {
		t.Errorf("%+v", pl)
	}
	// Nothing else fits next to it.
	pending(t, s.Submit(newJob("tiny", "b", 1, 512*mib, 3)))
}

func TestNoHeadOfLineBlocking(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)))
	placed(t, s.Submit(newJob("fill", "a", 1, 100*gib, 600)))
	big := s.Submit(newJob("big", "a", 1, 50*gib, 60))
	pending(t, big)
	// Same owner and another owner: both small jobs run past the big one.
	placed(t, s.Submit(newJob("small1", "a", 1, 4*gib, 60)))
	placed(t, s.Submit(newJob("small2", "b", 1, 4*gib, 60)))
	if q := s.Snapshot().Queue; len(q) != 1 || q[0] != "big" {
		t.Errorf("queue %v", q)
	}
}

func TestFairShareAcrossOwners(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)))
	placed(t, s.Submit(newJob("fill", "x", 18, gib, 600)))
	// Owner a floods 10 jobs, then b and c submit 2 each.
	for i := 0; i < 10; i++ {
		s.Submit(newJob(fmt.Sprintf("a%d", i), "a", 18, gib, 60))
	}
	for i := 0; i < 2; i++ {
		s.Submit(newJob(fmt.Sprintf("b%d", i), "b", 18, gib, 60))
		s.Submit(newJob(fmt.Sprintf("c%d", i), "c", 18, gib, 60))
	}
	q := s.Snapshot().Queue
	want := []string{"a0", "b0", "c0", "a1", "b1", "c1", "a2", "a3"}
	for i, id := range want {
		if q[i] != id {
			t.Fatalf("queue order %v, want prefix %v", q, want)
		}
	}
	// Dispatch one at a time: the order holds as jobs finish.
	var got []string
	prev := "fill"
	for i := 0; i < 6; i++ {
		s.Release(prev, nil)
		snap := s.Snapshot()
		for id, pl := range snap.Placements {
			if pl.Node == "gx10" {
				prev = id
			}
		}
		got = append(got, prev)
	}
	if strings.Join(got, ",") != "a0,b0,c0,a1,b1,c1" {
		t.Errorf("dispatch order %v", got)
	}
}

func TestPriorityWeightsAndInteractive(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)))
	placed(t, s.Submit(newJob("fill", "x", 18, gib, 600)))
	for i := 0; i < 6; i++ {
		s.Submit(newJob(fmt.Sprintf("n%d", i), "bg", 18, gib, 60))
		s.Submit(newJob(fmt.Sprintf("h%d", i), "human", 18, gib, 60, withTTY())) // interactive → high
	}
	q := s.Snapshot().Queue
	// high (weight 4) vs normal (2): two high jobs per normal one.
	if strings.Join(q[:6], ",") != "h0,n0,h1,h2,n1,h3" {
		t.Errorf("queue %v", q)
	}
	// Low priority yields; SetPriority re-orders.
	s.Submit(newJob("l0", "batch", 18, gib, 60, withPrio(proto.PriorityLow)))
	s.SetPriority("n5", proto.PriorityHigh)
	q = s.Snapshot().Queue
	pos := map[string]int{}
	for i, id := range q {
		pos[id] = i
	}
	if pos["n5"] > pos["n1"] {
		t.Errorf("n5 boosted to high should come before n1: %v", q)
	}
	if pos["l0"] < pos["h1"] {
		t.Errorf("low priority jumped ahead: %v", q)
	}
}

func TestStarvationGuardHoldsNodeForOldJob(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)))
	// Small jobs keep the node busy; a 60 GiB job waits.
	var running []string
	for i := 0; i < 11; i++ { // 11 × 10 GiB of 112 GiB
		id := fmt.Sprintf("s%02d", i)
		placed(t, s.Submit(newJob(id, "small", 1, 10*gib, 30)))
		running = append(running, id)
	}
	big := s.Submit(newJob("big", "big", 1, 60*gib, 60))
	pending(t, big)
	next := 11
	// Every 10 s one small job ends and another owner submits a new one.
	for round := 0; round < 30; round++ {
		clk.Advance(10 * time.Second)
		s.Release(running[0], nil)
		running = running[1:]
		id := fmt.Sprintf("s%02d", next)
		next++
		tk := s.Submit(newJob(id, fmt.Sprintf("o%d", round), 1, 10*gib, 30))
		if _, ok := isPlaced(tk); ok {
			running = append(running, id)
		}
		if _, ok := isPlaced(big); ok {
			if clk.Now().Sub(t0) < 2*time.Minute {
				t.Fatalf("big placed before starvation boost at %s?", clk.Now().Sub(t0))
			}
			t.Logf("big placed after %s", clk.Now().Sub(t0))
			return
		}
		if clk.Now().Sub(t0) >= 2*time.Minute+10*time.Second && !strings.Contains(reason(s, "big"), "next in line on gx10") {
			t.Errorf("starving job reason %q", reason(s, "big"))
		}
	}
	t.Fatal("big job starved")
}

func TestRejectReplacesThenFallsBack(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 10 * gib
	p3 := devmac(t0)
	p3.Info.Name = "studio2"
	s := newTestSched(t, clk, g, devmac(t0), p3)
	pl := placed(t, s.Submit(newJob("j1", "a", 4, 4*gib, 60)))
	if pl.Node != "devmac" {
		t.Fatalf("%+v", pl)
	}
	tk := s.Reject("j1", "devmac", errors.New("slot.commit: disk full"))
	pl = placed(t, tk)
	if pl.Node != "studio2" || pl.Fallback {
		t.Fatalf("re-placement %+v", pl)
	}
	// Second failure: remote attempts used up → origin, once it has room.
	tk = s.Reject("j1", "studio2", errors.New("setup: pnpm install failed"))
	pending(t, tk)
	if r := reason(s, "j1"); !strings.HasPrefix(r, "waiting: needs 4.0 GiB, gx10 has 2.0 GiB free") {
		t.Errorf("reason %q", r)
	}
	g.Metrics.MemAvailable = 40 * gib
	s.UpdateNode(g)
	pl = placed(t, tk)
	if pl.Node != "gx10" || !pl.Fallback || !pl.Local {
		t.Fatalf("fallback %+v", pl)
	}
	if !strings.HasPrefix(pl.Reason, "here: fallback after devmac and studio2 failed to start it") {
		t.Errorf("reason %q", pl.Reason)
	}
}

func TestRejectWithRemoteOnlyFails(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)), devmac(t0))
	tk := s.Submit(newJob("j1", "a", 4, 4*gib, 60, withReq(func(r *proto.Requirements) { r.Prefer = proto.PreferRemote })))
	if pl := placed(t, tk); pl.Node != "devmac" {
		t.Fatalf("%+v", pl)
	}
	err := resolvedErr(t, s.Reject("j1", "devmac", errors.New("ssh: connection lost")))
	var ue *UnplaceableError
	if !errors.As(err, &ue) || !strings.Contains(ue.Reason, "rerun without --remote") {
		t.Errorf("err %v", err)
	}
	if len(s.Snapshot().Queue) != 0 {
		t.Error("job left in queue")
	}
}

func TestNoFallbackNeverRunsOnOrigin(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 10 * gib
	s := newTestSched(t, clk, g, devmac(t0))
	j := newJob("j1", "a", 4, 4*gib, 60)
	j.Spec.NoFallback = true
	placed(t, s.Submit(j))
	err := resolvedErr(t, s.Reject("j1", "devmac", errors.New("boom")))
	if !strings.Contains(err.Error(), "--no-fallback") {
		t.Errorf("err %v", err)
	}
}

// TestBusyRefusalIsNotAFailure: a target that refuses admission because
// another origin filled it first is busy, not broken; losing that race
// again and again must not open its breaker or use up remote attempts.
func TestBusyRefusalIsNotAFailure(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 10 * gib
	p3 := devmac(t0)
	p3.Info.Name = "studio2"
	s := newTestSched(t, clk, g, devmac(t0), p3)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("j%d", i)
		pl := placed(t, s.Submit(newJob(id, "a", 1, 4*gib, 60, withReq(func(r *proto.Requirements) { r.Node = "devmac" }))))
		if pl.Node != "devmac" {
			t.Fatalf("%s on %s", id, pl.Node)
		}
		s.Reject(id, "devmac", &AdmitError{Code: "max_jobs", Reason: "devmac: at its limit of 1 jobs"})
		s.Cancel(id)
	}
	if u := s.Snapshot().Unavailable["devmac"]; u != "" {
		t.Errorf("busy refusals opened the breaker: %q", u)
	}
	// An auto job that lost the race moves on, but keeps both remote
	// attempts for real failures.
	pl := placed(t, s.Submit(newJob("k", "a", 4, 4*gib, 60)))
	if pl.Node != "devmac" {
		t.Fatalf("%+v", pl)
	}
	pl = placed(t, s.Reject("k", "devmac", &AdmitError{Code: "busy", Reason: "devmac: needs 4 CPUs"}))
	if pl.Node != "studio2" || pl.Fallback {
		t.Fatalf("after a busy refusal: %+v", pl)
	}
}

func TestBreakerOpensAfterRepeatedFailures(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 10 * gib
	s := newTestSched(t, clk, g, devmac(t0))
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("j%d", i)
		pl := placed(t, s.Submit(newJob(id, "a", 1, 4*gib, 60)))
		if pl.Node != "devmac" {
			t.Fatalf("%s on %s", id, pl.Node)
		}
		s.Reject(id, "devmac", errors.New("blob.put: broken pipe"))
		s.Cancel(id)
	}
	snap := s.Snapshot()
	if u := snap.Unavailable["devmac"]; u != "devmac: 3 failed starts in a row (last: blob.put: broken pipe), retrying in 30s" {
		t.Errorf("unavailable %q", u)
	}
	tk := s.Submit(newJob("k", "a", 1, 4*gib, 60))
	pending(t, tk)
	if r := reason(s, "k"); !strings.Contains(r, "devmac: 3 failed starts in a row") {
		t.Errorf("reason %q", r)
	}
	clk.Advance(31 * time.Second)
	s.Tick(clk.Now())
	if pl := placed(t, tk); pl.Node != "devmac" {
		t.Errorf("after cool-down: %+v", pl)
	}
	// Half-open probe fails again: cool-down doubles.
	s.Reject("k", "devmac", errors.New("again"))
	if u := s.Snapshot().Unavailable["devmac"]; !strings.HasSuffix(u, "retrying in 1m") {
		t.Errorf("unavailable %q", u)
	}
	// A job that ran resets the breaker.
	clk.Advance(2 * time.Minute)
	s.Cancel("k")
	pl := placed(t, s.Submit(newJob("ok", "a", 1, 4*gib, 60)))
	s.Release(pl.JobID, &proto.Usage{WallSeconds: 50, SyncSeconds: 2, SetupSeconds: 1})
	if u := s.Snapshot().Unavailable["devmac"]; u != "" {
		t.Errorf("breaker not reset: %q", u)
	}
	if c := s.syncCost["repo-a\x00devmac"]; c == nil || c.v != 3 {
		t.Errorf("sync cost not learned: %+v", c)
	}
}

func TestEligibilityReasons(t *testing.T) {
	clk := &fakeClock{t0}
	cases := []struct {
		name string
		mut  func(d *proto.NodeState)
		req  func(r *proto.Requirements)
		want string
	}{
		{"draining", func(d *proto.NodeState) { d.Status = proto.NodeDraining }, nil, "devmac: draining"},
		{"offline", func(d *proto.NodeState) { d.Status = proto.NodeOffline; d.Reason = "ssh: i/o timeout" }, nil, "devmac: offline (ssh: i/o timeout)"},
		{"no docker", func(d *proto.NodeState) {
			d.Info.Runtimes[1].Available = false
			d.Info.Runtimes[1].Reason = "Docker not reachable"
		}, nil, "devmac: no linux runtime (Docker not reachable)"},
		{"no container runtime", func(d *proto.NodeState) { d.Info.Runtimes = d.Info.Runtimes[:1] }, nil, "devmac: no linux runtime"},
		{"not accepting", func(d *proto.NodeState) { d.Accepts = false }, nil, "devmac: not accepting jobs (accept.enabled is off there)"},
		{"degraded", func(d *proto.NodeState) { d.Status = proto.NodeDegraded; d.Reason = "devmac: disk almost full" }, nil, "devmac: disk almost full"},
		{"disk floor", func(d *proto.NodeState) { d.Metrics.DiskFree = 4 * gib }, nil, "devmac: disk almost full (4.0 GiB free)"},
		{"toolchain", func(d *proto.NodeState) {}, func(r *proto.Requirements) {
			r.OS = proto.OSAny
			r.Toolchains = map[string]string{"node": "24", "go": "1.27"}
		}, ""}, // container runtime installs it: eligible
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := devmac(t0)
			d.Info.Runtimes = append([]proto.Runtime(nil), d.Info.Runtimes...)
			c.mut(&d)
			s := newTestSched(t, clk, self(gx10(t0)), d)
			tk := s.Submit(newJob("j", "a", 1, gib, 60, withReq(func(r *proto.Requirements) {
				r.Node = "devmac"
				if c.req != nil {
					c.req(r)
				}
			})))
			if c.want == "" {
				placed(t, tk)
				return
			}
			pending(t, tk)
			if r := reason(s, "j"); r != c.want {
				t.Errorf("reason\n got %q\nwant %q", r, c.want)
			}
		})
	}
}

func TestToolchainOnNativeRuntime(t *testing.T) {
	clk := &fakeClock{t0}
	d := devmac(t0)
	d.Info.Runtimes = d.Info.Runtimes[:1] // native only
	s := newTestSched(t, clk, self(gx10(t0)), d)
	req := func(r *proto.Requirements) {
		r.Node, r.OS = "devmac", proto.OSAny
		r.Toolchains = map[string]string{"go": "1.27.1", "node": "24"}
	}
	pending(t, s.Submit(newJob("j", "a", 1, gib, 60, withReq(req))))
	if r := reason(s, "j"); r != "devmac: missing toolchain node@24" {
		t.Errorf("reason %q", r)
	}
	d.Info.Runtimes[0].Toolchains = map[string]string{"go": "1.26.0", "node": "22.1.0", "mise": "2026.10.1"}
	s.UpdateNode(d)
	if pl := placed(t, s.Submit(newJob("k", "a", 1, gib, 60, withReq(req)))); pl.Node != "devmac" {
		t.Errorf("%+v", pl)
	}
}

func TestVersionSatisfies(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"24.8.0", "24", true},
		{"24.8.0", "24.8", true},
		{"24.8.0", "24.9", false},
		{"22.1.0", "24", false},
		{"v20.11.1", ">=20", true},
		{"18.0.0", ">=20", false},
		{"20.11.1", "^20.1", true},
		{"21.0.0", "^20.1", false},
		{"20.1.5", "~20.1.2", true},
		{"20.2.0", "~20.1", false},
		{"3.12.4", "3.12", true},
		{"3.11.9", "3.12.x", false},
		{"1.25rc1", "1.25", true},
		{"24.0.0", "lts", true},
		{"", "20", false},
	}
	for _, c := range cases {
		if got := versionSatisfies(c.have, c.want); got != c.ok {
			t.Errorf("versionSatisfies(%q, %q) = %v", c.have, c.want, got)
		}
	}
}

func TestGPUReasons(t *testing.T) {
	clk := &fakeClock{t0}
	gpu := func(r *proto.Requirements) { r.GPU = true }

	// From gx10: it is the only NVIDIA node; when it is full the reason says so.
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 18 * gib
	s := newTestSched(t, clk, g, devmac(t0))
	tk := s.Submit(newJob("vllm", "a", 4, 24*gib, 600, withReq(gpu)))
	pending(t, tk)
	if r := reason(s, "vllm"); r != "waiting: needs 24.0 GiB, gx10 has 10.0 GiB free (only gx10 has an NVIDIA GPU)" {
		t.Errorf("reason %q", r)
	}

	// From another Linux box: gx10 offline.
	box := proto.NodeState{Info: proto.NodeInfo{Name: "box", OS: "linux", Arch: "amd64", CPUs: 8, MemTotal: 32 * gib}, Status: proto.NodeOnline, Accepts: true, Self: true}
	g2 := gx10(t0)
	g2.Status = proto.NodeOffline
	s2 := New(Options{Origin: "box", Now: clk.Now}).(*sched)
	s2.UpdateNode(box)
	s2.UpdateNode(g2)
	s2.UpdateNode(devmac(t0))
	pending(t, s2.Submit(newJob("v", "a", 4, 24*gib, 600, withReq(gpu))))
	if r := reason(s2, "v"); r != "only gx10 has an NVIDIA GPU (gx10: offline)" {
		t.Errorf("reason %q", r)
	}
	g2.Status = proto.NodeOnline
	s2.UpdateNode(g2)
	pl := placed(t, s2.Submit(newJob("v2", "a", 4, 24*gib, 600, withReq(gpu))))
	if pl.Node != "gx10" || !strings.HasSuffix(pl.Reason, "(only node with an NVIDIA GPU)") {
		t.Errorf("%+v", pl)
	}

	// No GPU anywhere: fail fast.
	s3 := New(Options{Origin: "box", Now: clk.Now}).(*sched)
	s3.UpdateNode(box)
	err := resolvedErr(t, s3.Submit(newJob("v", "a", 1, gib, 60, withReq(gpu))))
	if err.Error() != "no node has an NVIDIA GPU" {
		t.Errorf("err %v", err)
	}
}

func TestValidation(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)), devmac(t0))
	cases := []struct {
		req  func(r *proto.Requirements)
		want string
	}{
		{func(r *proto.Requirements) { r.Node = "nope" }, `no machine named "nope" (known: devmac, gx10). Link it with: kran link nope`},
		{func(r *proto.Requirements) { r.Prefer = proto.PreferLocal; r.Node = "devmac" }, "--local and --node devmac conflict"},
		{func(r *proto.Requirements) { r.Prefer = proto.PreferRemote; r.Node = "gx10" }, "--remote and --node gx10 conflict: gx10 is this machine"},
		{func(r *proto.Requirements) {
			r.Prefer = proto.PreferRemote
			r.OriginOnly, r.OriginOnlyReason = true, "the command talks to localhost:5432"
		}, "the command talks to localhost:5432, but --remote was given"},
	}
	for i, c := range cases {
		err := resolvedErr(t, s.Submit(newJob(fmt.Sprintf("v%d", i), "a", 1, gib, 60, withReq(c.req))))
		if err.Error() != c.want {
			t.Errorf("got %q want %q", err, c.want)
		}
	}
	solo := newTestSched(t, clk, self(gx10(t0)))
	err := resolvedErr(t, solo.Submit(newJob("r", "a", 1, gib, 60, withReq(func(r *proto.Requirements) { r.Prefer = proto.PreferRemote }))))
	if !strings.Contains(err.Error(), "no other machine is linked") {
		t.Errorf("%v", err)
	}
	// Duplicate IDs are refused.
	placed(t, s.Submit(newJob("dup", "a", 1, gib, 60)))
	resolvedErr(t, s.Submit(newJob("dup", "a", 1, gib, 60)))
}

func TestOriginOnlyAndLocal(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 10 * gib
	s := newTestSched(t, clk, g, devmac(t0))
	pending(t, s.Submit(newJob("l", "a", 1, 4*gib, 60, withReq(func(r *proto.Requirements) { r.Prefer = proto.PreferLocal }))))
	if r := reason(s, "l"); r != "waiting: needs 4.0 GiB, gx10 has 2.0 GiB free (--local)" {
		t.Errorf("reason %q", r)
	}
	pending(t, s.Submit(newJob("o", "a", 1, 4*gib, 60, withReq(func(r *proto.Requirements) {
		r.OriginOnly, r.OriginOnlyReason = true, "cwd is outside the workspace"
	}))))
	if r := reason(s, "o"); r != "waiting: needs 4.0 GiB, gx10 has 2.0 GiB free (runs only here: cwd is outside the workspace)" {
		t.Errorf("reason %q", r)
	}
	// Draining the origin does not stop --local jobs.
	g.Metrics.MemAvailable = 100 * gib
	g.Status = proto.NodeDraining
	s.UpdateNode(g)
	if pl := s.Snapshot().Placements["l"]; pl.Node != "gx10" || pl.Reason != "here (--local)" {
		t.Errorf("%+v", pl)
	}
	if pl := s.Snapshot().Placements["o"]; pl.Reason != "here: cwd is outside the workspace" {
		t.Errorf("%+v", pl)
	}
	// ...but auto jobs avoid a draining origin.
	if pl := placed(t, s.Submit(newJob("auto", "a", 1, 4*gib, 600))); pl.Node != "devmac" {
		t.Errorf("auto on draining origin: %+v", pl)
	}
}

func TestDeferForBetterNode(t *testing.T) {
	clk := &fakeClock{t0}
	d := devmac(t0)
	d.Link.BandwidthBps = 2e6 // slow link: cold sync ~41 s
	s := newTestSched(t, clk, self(gx10(t0)), d)
	// gx10 is full with a job that ends in ~3 s; a 60 s job would take
	// 48 s + ~41 s cold sync on devmac: waiting 5 s here is better.
	placed(t, s.Submit(newJob("fill", "x", 18, gib, 3)))
	tk := s.Submit(newJob("j", "a", 8, 4*gib, 60))
	pending(t, tk)
	if r := reason(s, "j"); r != "waiting: gx10 should free up in ~5s; est. 1m there vs 1m29s on devmac (Linux container)" {
		t.Errorf("reason %q", r)
	}
	// After maxDefer it takes what is free.
	clk.Advance(maxDefer)
	s.Tick(clk.Now())
	if pl := placed(t, tk); pl.Node != "devmac" {
		t.Errorf("%+v", pl)
	}
}

func TestCancelAndRelease(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)))
	placed(t, s.Submit(newJob("fill", "x", 18, gib, 600)))
	tk := s.Submit(newJob("q", "a", 1, gib, 60))
	tk2 := s.Submit(newJob("q2", "a", 1, gib, 60))
	s.Cancel("q")
	if err := resolvedErr(t, tk); !errors.Is(err, ErrCanceled) {
		t.Errorf("%v", err)
	}
	s.Release("q2", nil) // release of a queued job cancels it
	if err := resolvedErr(t, tk2); !errors.Is(err, ErrCanceled) {
		t.Errorf("%v", err)
	}
	s.Cancel("fill") // placed: no-op
	if _, ok := s.Snapshot().Placements["fill"]; !ok {
		t.Error("cancel removed a placed job")
	}
	s.Release("fill", &proto.Usage{WallSeconds: 1})
	snap := s.Snapshot()
	if len(snap.Placements) != 0 || snap.Allocated["gx10"].Mem != 0 || len(snap.Queue) != 0 {
		t.Errorf("leak: %+v", snap)
	}
	// Unknown IDs are harmless.
	s.Release("nope", nil)
	s.Cancel("nope")
	s.SetPriority("nope", proto.PriorityHigh)
	s.UpdateJobUsage("nope", proto.Resources{})
}

func TestTicketWaitHonorsContext(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)))
	placed(t, s.Submit(newJob("fill", "x", 18, gib, 600)))
	tk := s.Submit(newJob("q", "a", 1, gib, 60))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tk.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
	// Updates are coalesced: only the newest undelivered reason is kept.
	g := self(gx10(t0))
	for i := 0; i < 5; i++ {
		g.Metrics.CPUBusy = 0.1 * float64(i)
		s.UpdateNode(g)
	}
	n := 0
	for {
		select {
		case <-tk.Updates():
			n++
			continue
		default:
		}
		break
	}
	if n != 1 {
		t.Errorf("got %d buffered updates, want 1", n)
	}
	s.Release("fill", nil)
	placed(t, tk)
	if _, ok := <-tk.Updates(); ok {
		t.Error("updates channel not closed after placement")
	}
}

func TestAdmitForeignJobs(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)))
	j := newJob("f1", "remote", 4, 100*gib, 60)
	pl, err := s.Admit(j, proto.RuntimeContainer)
	if err != nil || pl.Node != "gx10" || pl.Alloc.Mem != 100*gib {
		t.Fatalf("%+v %v", pl, err)
	}
	if again, err := s.Admit(j, proto.RuntimeContainer); err != nil || again != pl {
		t.Errorf("idempotent admit: %+v %v", again, err)
	}
	_, err = s.Admit(newJob("f2", "remote", 4, 20*gib, 60), proto.RuntimeNative)
	var ae *AdmitError
	if !errors.As(err, &ae) || ae.Code != "busy" || ae.Reason != "gx10: needs 4 CPUs and 20.0 GiB, has 14 CPUs and 12.0 GiB free" {
		t.Errorf("%v", err)
	}
	// Local jobs see the foreign allocation.
	pending(t, s.Submit(newJob("local", "a", 1, 16*gib, 600)))
	s.Release("f1", nil)
	placed(t, s.Submit(newJob("local2", "a", 1, 16*gib, 600)))

	g := self(gx10(t0))
	g.Status = proto.NodeDraining
	s.UpdateNode(g)
	if _, err := s.Admit(newJob("f3", "remote", 1, gib, 60), proto.RuntimeNative); !errors.As(err, &ae) || ae.Code != "draining" {
		t.Errorf("%v", err)
	}
	g.Status = proto.NodeOnline
	g.Accepts = false
	s.UpdateNode(g)
	if _, err := s.Admit(newJob("f4", "remote", 1, gib, 60), proto.RuntimeNative); !errors.As(err, &ae) || ae.Code != "not_accepting" {
		t.Errorf("%v", err)
	}
}

func TestAdmitIntoMacVM(t *testing.T) {
	clk := &fakeClock{t0}
	d := self(devmac(t0))
	s := New(Options{Origin: "devmac", Now: clk.Now}).(*sched)
	s.UpdateNode(d)
	if _, err := s.Admit(newJob("a", "x", 8, 20*gib, 60), proto.RuntimeContainer); err != nil {
		t.Fatal(err)
	}
	// The VM has 30.4 GiB schedulable: a second 20 GiB container job does
	// not fit although the host has plenty.
	if _, err := s.Admit(newJob("b", "x", 8, 20*gib, 60), proto.RuntimeContainer); err == nil {
		t.Error("VM overcommitted")
	}
	if _, err := s.Admit(newJob("c", "x", 8, 20*gib, 60), proto.RuntimeNative); err != nil {
		t.Errorf("native next to the VM: %v", err)
	}
}

func TestAdmitRespectsHoldOfStarvingLocalJob(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(gx10(t0)))
	placed(t, s.Submit(newJob("fill", "x", 1, 100*gib, 600)))
	pending(t, s.Submit(newJob("big", "a", 1, 50*gib, 60)))
	clk.Advance(3 * time.Minute)
	s.Tick(clk.Now())
	if _, err := s.Admit(newJob("f", "remote", 1, 10*gib, 60), proto.RuntimeNative); err == nil {
		t.Error("foreign job took memory held for a starving local job")
	}
}

func TestPeerReportDeduplication(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 9 * gib
	d := devmac(t0)
	s := newTestSched(t, clk, g, d)
	pl := placed(t, s.Submit(newJob("j1", "a", 4, 10*gib, 600, withReq(func(r *proto.Requirements) { r.OS = proto.OSAny }))))
	if pl.Node != "devmac" || pl.Runtime != proto.RuntimeNative {
		t.Fatalf("%+v", pl)
	}
	// devmac reports j1 plus 20 GiB of another origin's jobs.
	d.Allocated = proto.Resources{CPUs: 8, Mem: 30 * gib}
	d.AllocatedJobs = []string{"j1", "other-origin-job"}
	d.Running = 2
	s.UpdateNode(d)
	snap := s.Snapshot()
	if a := snap.Allocated["devmac"]; a.Mem != 30*gib || snap.Running["devmac"] != 2 {
		t.Errorf("with job IDs: %+v running %d", a, snap.Running["devmac"])
	}
	// Report from before j1 was admitted there: counts it on top.
	d.AllocatedJobs = []string{"other-origin-job"}
	d.Allocated = proto.Resources{CPUs: 4, Mem: 20 * gib}
	d.Running = 1
	s.UpdateNode(d)
	if a := s.Snapshot().Allocated["devmac"]; a.Mem != 30*gib {
		t.Errorf("not yet reported: %+v", a)
	}
	// Without IDs: the larger of report and own placements.
	d.AllocatedJobs = nil
	s.UpdateNode(d)
	if a := s.Snapshot().Allocated["devmac"]; a.Mem != 20*gib {
		t.Errorf("no IDs: %+v", a)
	}
}

func TestMaxJobs(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.MaxJobs = 2
	s := newTestSched(t, clk, g)
	placed(t, s.Submit(newJob("a", "a", 1, gib, 60)))
	placed(t, s.Submit(newJob("b", "a", 1, gib, 60)))
	pending(t, s.Submit(newJob("c", "a", 1, gib, 60)))
	if r := reason(s, "c"); r != "waiting: gx10 is at its limit of 2 jobs" {
		t.Errorf("reason %q", r)
	}
}

func TestSpeedFromEstimatorAndPrior(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 9 * gib
	calls := 0
	s := New(Options{Origin: "gx10", Now: clk.Now, Speed: func(fp, node string) float64 {
		calls++
		if fp == "fp-learned" {
			return 0.5
		}
		return 1 // unknown → benchmark prior
	}}).(*sched)
	s.UpdateNode(g)
	s.UpdateNode(devmac(t0))
	j := newJob("learned", "a", 4, 4*gib, 100)
	pl := placed(t, s.Submit(j))
	if !strings.Contains(pl.Reason, "est. 1m7s") { // 100 × 0.5 + ~16.5 s cold sync
		t.Errorf("%q", pl.Reason)
	}
	pl = placed(t, s.Submit(newJob("prior", "a", 4, 4*gib, 100)))
	if !strings.Contains(pl.Reason, "est. 1m37s") { // 100 × 0.8 + ~16.5 s
		t.Errorf("%q", pl.Reason)
	}
	if calls == 0 {
		t.Error("Speed not consulted")
	}
}

func TestSnapshotAndRemoveNode(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 10 * gib
	s := newTestSched(t, clk, g, devmac(t0))
	placed(t, s.Submit(newJob("r", "a", 4, 4*gib, 600)))
	snap := s.Snapshot()
	if snap.Running["devmac"] != 1 || snap.Jobs["devmac"][0] != "r" || snap.Free["gx10"].Mem != 2*gib {
		t.Errorf("%+v", snap)
	}
	s.RemoveNode("devmac")
	if _, ok := s.Snapshot().Running["devmac"]; ok {
		t.Error("removed node still in snapshot")
	}
	s.Release("r", nil) // allocation on a removed node: no panic
}

// TestRestoreCountsRunningJobs: a job that kept running across a daemon
// restart holds its room again, even when it no longer fits and its node
// is not known yet (a target restores before its first self update).
func TestRestoreCountsRunningJobs(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk)
	s.Restore(newJob("r1", "a", 12, 10*gib, 300), Placement{Node: "devmac", Runtime: proto.RuntimeContainer, Alloc: proto.Resources{CPUs: 12, Mem: 10 * gib}})
	s.UpdateNode(self(gx10(t0)))
	s.UpdateNode(devmac(t0))
	s.Restore(newJob("r2", "a", 30, 200*gib, 300), Placement{Node: "gx10", Runtime: proto.RuntimeNative, Alloc: proto.Resources{CPUs: 30, Mem: 200 * gib}})
	snap := s.Snapshot()
	if snap.Running["devmac"] != 1 || snap.Allocated["devmac"].CPUs != 12 || snap.Running["gx10"] != 1 || snap.Allocated["gx10"].Mem != 200*gib {
		t.Fatalf("running %v allocated %v", snap.Running, snap.Allocated)
	}
	// devmac's Linux VM (16 CPUs) has 4 left: a 6-CPU job waits.
	tk := s.Submit(newJob("j", "b", 6, 2*gib, 600, withReq(func(r *proto.Requirements) { r.Node = "devmac" })))
	pending(t, tk)
	s.Release("r1", nil)
	if pl := placed(t, tk); pl.Node != "devmac" {
		t.Fatalf("placed %+v", pl)
	}
	s.Release("r2", nil)
	if snap := s.Snapshot(); snap.Running["gx10"] != 0 {
		t.Fatalf("running %v after release", snap.Running)
	}
}

// TestRelieveBusyOrigin: a check that devmac runs a little slower stays on
// a calm origin and goes to devmac while the origin is busy (CPUs taken by
// other load, or I/O contention), within a slowdown that grows with how
// busy it is; it never moves when its files matter, when it is short or
// pinned, when devmac would be much slower, or when it would eat devmac's
// spare room.
func TestRelieveBusyOrigin(t *testing.T) {
	slowMac := func(now time.Time) proto.NodeState {
		d := devmac(now)
		d.Info.SingleCoreScore = 900 // ~11% slower than gx10 per job
		return d
	}
	check := func(id string, dur float64, opts ...jobOpt) *proto.Job {
		j := newJob(id, id, 2, 2*gib, dur, opts...)
		j.Estimate.ResultOnly = true
		return j
	}
	busyCPU := func(n proto.NodeState) proto.NodeState { n.Metrics.CPUBusy = 0.85; return n } // other load: 17 of 20 cores
	busyIO := func(n proto.NodeState) proto.NodeState { n.Metrics.IOPressure = 20; return n }
	run := func(t *testing.T, g proto.NodeState, d proto.NodeState, j *proto.Job) Placement {
		t.Helper()
		s := newTestSched(t, &fakeClock{t0}, self(g), d)
		return placed(t, s.Submit(j))
	}
	for _, tc := range []struct {
		name   string
		g      proto.NodeState
		d      proto.NodeState
		job    *proto.Job
		want   string
		reason string
	}{
		{"calm origin keeps it", self(gx10(t0)), slowMac(t0), check("a", 300), "gx10", ""},
		{"busy CPUs relieve", busyCPU(gx10(t0)), slowMac(t0), check("b", 300), "devmac", "gx10 is busy (83% of its CPUs in use): relieving it is worth up to"},
		{"I/O pressure relieves", busyIO(gx10(t0)), slowMac(t0), check("c", 300), "devmac", "gx10 is busy (I/O pressure 20%)"},
		{"files matter", busyCPU(gx10(t0)), slowMac(t0), func() *proto.Job { j := check("d", 300); j.Estimate.ResultOnly = false; return j }(), "gx10", ""},
		{"too short", busyCPU(gx10(t0)), slowMac(t0), check("e", 20), "gx10", ""},
		{"local requested", busyCPU(gx10(t0)), slowMac(t0), check("f", 300, withReq(func(r *proto.Requirements) { r.Prefer = proto.PreferLocal })), "gx10", ""},
		{"origin only", busyCPU(gx10(t0)), slowMac(t0), check("g", 300, withReq(func(r *proto.Requirements) { r.OriginOnly = true })), "gx10", ""},
		{"devmac much slower", busyCPU(gx10(t0)), func() proto.NodeState { d := devmac(t0); d.Info.SingleCoreScore = 600; return d }(), check("h", 300), "gx10", ""},
		{"no spare room on devmac", busyCPU(gx10(t0)), slowMac(t0), func() *proto.Job { j := check("i", 300); j.Estimate.Mem = 20 * gib; return j }(), "gx10", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pl := run(t, tc.g, tc.d, tc.job)
			if pl.Node != tc.want || !strings.Contains(pl.Reason, tc.reason) {
				t.Fatalf("placed on %s: %s", pl.Node, pl.Reason)
			}
			if tc.want == "gx10" && strings.Contains(pl.Reason, "relieving") {
				t.Fatalf("reason %q", pl.Reason)
			}
		})
	}
}

// TestRelieveLeavesRoomForPinnedJobs: relieving checks fill at most half of
// devmac's Linux VM; a pinned job like the land gate's walls run (12 CPUs,
// 10 GiB) still starts at once.
func TestRelieveLeavesRoomForPinnedJobs(t *testing.T) {
	g := self(gx10(t0))
	g.Metrics.CPUBusy = 0.85
	d := devmac(t0)
	d.Info.SingleCoreScore = 900
	s := newTestSched(t, &fakeClock{t0}, g, d)
	moved := 0
	for i := 0; i < 6; i++ {
		j := newJob(fmt.Sprintf("c%d", i), fmt.Sprintf("o%d", i), 2, 2*gib, 300)
		j.Estimate.ResultOnly = true
		pl := placed(t, s.Submit(j))
		if !strings.Contains(pl.Reason, "relieving") {
			break // the first one kept here: devmac's spare room is used up
		}
		moved++
	}
	if moved == 0 || moved > 4 {
		t.Fatalf("%d checks moved to devmac to relieve gx10, want 1..4 (half of a 16-CPU VM)", moved)
	}
	walls := newJob("walls", "gate", 12, 10*gib, 300, withReq(func(r *proto.Requirements) { r.Node = "devmac"; r.Prefer = proto.PreferRemote }))
	if pl := placed(t, s.Submit(walls)); pl.Node != "devmac" {
		t.Fatalf("walls placed %+v", pl)
	}
}
