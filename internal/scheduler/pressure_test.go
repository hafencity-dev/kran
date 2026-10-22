package scheduler

import (
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

// overloaded is gx10 as it was during the incident: load 28-47 on 20
// cores but only 30% CPU busy (tasks waiting on reclaim and swap-in),
// memory PSI 40, swap full.
func overloaded(n proto.NodeState) proto.NodeState {
	n.Metrics.CPUBusy = 0.30
	n.Metrics.Load1 = 40
	n.Metrics.CPUPressure = 12
	n.Metrics.MemPressure = 40
	n.Metrics.IOPressure = 25
	n.Metrics.SwapTotal = 16 * gib
	n.Metrics.SwapUsed = 16*gib - 20*mib
	n.Metrics.MemAvailable = 30 * gib
	return n
}

func TestOverloadedOriginOffloads(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(overloaded(gx10(t0))), devmac(t0))
	s.mu.Lock()
	s.learnSyncCost("repo-a", "devmac", 15)
	s.mu.Unlock()
	// The job that stayed "here: est. 1m" while gx10 thrashed.
	pl := placed(t, s.Submit(newJob("j1", "a", 8, 4*gib, 60)))
	if pl.Node != "devmac" {
		t.Fatalf("placed on the overloaded origin: %+v", pl)
	}
	for _, want := range []string{" here (slowed ~", "memory pressure 40%", "full swap"} {
		if !strings.Contains(pl.Reason, want) {
			t.Errorf("reason %q lacks %q", pl.Reason, want)
		}
	}

	// The same origin without the pressure keeps the job.
	s2 := newTestSched(t, clk, self(gx10(t0)), devmac(t0))
	s2.mu.Lock()
	s2.learnSyncCost("repo-a", "devmac", 15)
	s2.mu.Unlock()
	if pl := placed(t, s2.Submit(newJob("j1", "a", 8, 4*gib, 60))); pl.Node != "gx10" {
		t.Errorf("healthy origin: %+v", pl)
	}
}

// A job that must stay on the origin still runs there, and its reason says
// why it will be slow.
func TestOverloadedOriginReasonWhenLocal(t *testing.T) {
	clk := &fakeClock{t0}
	s := newTestSched(t, clk, self(overloaded(gx10(t0))))
	pl := placed(t, s.Submit(newJob("j1", "a", 2, gib, 60)))
	if pl.Node != "gx10" || !strings.HasPrefix(pl.Reason, "here: est. 2m") || !strings.Contains(pl.Reason, "slowed ~") {
		t.Errorf("%+v", pl)
	}
}

// A peer under pressure loses jobs it would otherwise win: a long job that
// goes to the faster Mac (TestOffloadsWhenFasterIncludingSync) stays here
// while the Mac is swapping hard.
func TestPressureOnPeerCounts(t *testing.T) {
	clk := &fakeClock{t0}
	d := devmac(t0)
	d.Metrics.MemPressure = 95 // memorystatus level 5
	d.Metrics.Load1 = 42       // 1.5 per core
	s := newTestSched(t, clk, self(gx10(t0)), d)
	s.mu.Lock()
	s.learnSyncCost("repo-a", "devmac", 3)
	s.mu.Unlock()
	pl := placed(t, s.Submit(newJob("j1", "a", 8, 4*gib, 300)))
	if pl.Node != "gx10" {
		t.Fatalf("%+v", pl)
	}
	want := "here: est. 5m; devmac (Linux container) would take 11m32s (slowed ~2.9x by load 42 on 28 cores, memory pressure 95%) plus ~3s to sync"
	if pl.Reason != want {
		t.Errorf("reason\n got %q\nwant %q", pl.Reason, want)
	}
}

func TestHostPressureCalibration(t *testing.T) {
	linux := gx10(t0)
	mac := devmac(t0)
	cases := []struct {
		name   string
		node   proto.NodeState
		mut    func(m *proto.NodeMetrics)
		lo, hi float64
	}{
		{"idle linux", linux, func(m *proto.NodeMetrics) {}, 1, 1},
		{"no metrics yet", linux, func(m *proto.NodeMetrics) { *m = proto.NodeMetrics{} }, 1, 1},
		{"incident", overloaded(linux), func(m *proto.NodeMetrics) {}, 2.2, 3.2},
		{"memory PSI 40 alone", linux, func(m *proto.NodeMetrics) { m.MemPressure = 40 }, 1.6, 1.7},
		{"light PSI noise", linux, func(m *proto.NodeMetrics) { m.CPUPressure, m.MemPressure, m.IOPressure = 2, 0.5, 3 }, 1, 1.05},
		// Swap used long ago by idle processes while memory is plentiful
		// is not contention.
		{"full swap, memory fine", linux, func(m *proto.NodeMetrics) { m.SwapTotal, m.SwapUsed = 8*gib, 8*gib }, 1, 1},
		{"full swap, memory tight", linux, func(m *proto.NodeMetrics) {
			m.SwapTotal, m.SwapUsed, m.MemAvailable, m.MemPressure = 8*gib, 8*gib, 6*gib, 3
		}, 1.25, 1.35},
		// Load counts on Linux only without PSI (it includes tasks in D state).
		{"linux load with PSI", linux, func(m *proto.NodeMetrics) { m.Load1, m.CPUPressure = 40, 1 }, 1, 1},
		{"linux load without PSI", linux, func(m *proto.NodeMetrics) { m.Load1 = 40 }, 1.9, 2.1},
		{"mac busy run queue", mac, func(m *proto.NodeMetrics) { m.Load1 = 56 }, 1.9, 2.1},
		{"mac normal memory use", mac, func(m *proto.NodeMetrics) { m.MemPressure = 60 }, 1, 1},
		{"mac memory pressure", mac, func(m *proto.NodeMetrics) { m.MemPressure = 95 }, 1.8, 2},
		// macOS swap grows on demand: "full" is normal.
		{"mac swap", mac, func(m *proto.NodeMetrics) { m.SwapTotal, m.SwapUsed, m.MemAvailable = 4*gib, 4*gib, gib }, 1, 1},
		{"everything at once is capped", linux, func(m *proto.NodeMetrics) {
			m.CPUPressure, m.MemPressure, m.IOPressure, m.SwapTotal, m.SwapUsed = 100, 100, 100, gib, gib
		}, 5, 5},
	}
	for _, c := range cases {
		n := c.node
		c.mut(&n.Metrics)
		if c.name != "no metrics yet" && n.Metrics.Time.IsZero() {
			n.Metrics.Time = t0
		}
		p := hostPressure(n.Metrics, n.Info)
		if p.slow < c.lo-1e-9 || p.slow > c.hi+1e-9 {
			t.Errorf("%s: slowdown %.2f, want %.2f..%.2f (%v)", c.name, p.slow, c.lo, c.hi, p.causes)
		}
	}
	if n := hostPressure(overloaded(linux).Metrics, linux.Info).note(); n != "slowed ~2.5x by CPU pressure 12%, memory pressure 40%, I/O pressure 25%, full swap" {
		t.Errorf("note %q", n)
	}
}

// The Linux VM of a Mac runs on the host's cores and memory: the host's
// contention slows container jobs too, and so does a saturated VM.
func TestVMPressure(t *testing.T) {
	host := pressure{slow: 2, causes: []string{"load 56 on 28 cores"}}
	if p := vmPressure(host, 0.2); p.slow != 2 || len(p.causes) != 1 {
		t.Errorf("%+v", p)
	}
	if p := vmPressure(noPressure(), 1); p.slow < 1.3 || p.causes[0] != "a busy Linux VM" {
		t.Errorf("saturated VM: %+v", p)
	}
	if p := vmPressure(noPressure(), 0.5); p.slow != 1 {
		t.Errorf("idle: %+v", p)
	}
}
