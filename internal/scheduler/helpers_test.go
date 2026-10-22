package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// gx10: 20-core GB10 (10 X925 + 10 A725), 128 GB unified, CUDA, Linux.
func gx10(now time.Time) proto.NodeState {
	return proto.NodeState{
		Info: proto.NodeInfo{
			Name: "gx10", OS: "linux", Arch: "arm64", CPUs: 20, PerfCPUs: 10, MemTotal: 128 * gib,
			DiskTotal: 3700 * gib, SingleCoreScore: 1000,
			GPUs: []proto.GPU{{Vendor: "nvidia", Model: "NVIDIA GB10", Unified: true}},
			Runtimes: []proto.Runtime{
				{Kind: proto.RuntimeNative, OS: "linux", Arch: "arm64", CPUs: 20, Mem: 128 * gib, Available: true,
					Toolchains: map[string]string{"go": "1.26.0", "node": "24.8.0"}},
				{Kind: proto.RuntimeContainer, OS: "linux", Arch: "arm64", CPUs: 20, Mem: 128 * gib, Available: true, Docker: true},
			},
		},
		Status:  proto.NodeOnline,
		Accepts: true,
		Reserve: proto.Resources{CPUs: 2, Mem: 8 * gib},
		Metrics: proto.NodeMetrics{Time: now, MemAvailable: 120 * gib, CPUBusy: 0.05, DiskFree: 2000 * gib},
	}
}

// devmac: 28-core M3 Ultra (20P+8E), 96 GB, Colima VM 16 CPU / 32 GB.
func devmac(now time.Time) proto.NodeState {
	return proto.NodeState{
		Info: proto.NodeInfo{
			Name: "devmac", OS: "darwin", Arch: "arm64", CPUs: 28, PerfCPUs: 20, MemTotal: 96 * gib,
			DiskTotal: 1800 * gib, SingleCoreScore: 1250,
			GPUs: []proto.GPU{{Vendor: "apple", Model: "Apple M3 Ultra", Unified: true}},
			Runtimes: []proto.Runtime{
				{Kind: proto.RuntimeNative, OS: "darwin", Arch: "arm64", CPUs: 28, Mem: 96 * gib, Available: true,
					Toolchains: map[string]string{"go": "1.26.0", "node": "22.1.0"}},
				{Kind: proto.RuntimeContainer, OS: "linux", Arch: "arm64", CPUs: 16, Mem: 32 * gib, Available: true, Docker: true},
			},
		},
		Status:  proto.NodeOnline,
		Accepts: true,
		Reserve: proto.Resources{CPUs: 2, Mem: 8 * gib},
		Link:    proto.LinkInfo{Connected: true, RTTMillis: 4, BandwidthBps: 100e6},
		Metrics: proto.NodeMetrics{Time: now, MemAvailable: 80 * gib, CPUBusy: 0.03, DiskFree: 900 * gib,
			Runtimes: map[proto.RuntimeKind]proto.RuntimeMetrics{proto.RuntimeContainer: {CPUBusy: 0.01, MemAvailable: 31 * gib}}},
	}
}

func self(n proto.NodeState) proto.NodeState { n.Self = true; return n }

type jobOpt func(*proto.Job)

func newJob(id, owner string, cpus float64, mem int64, dur float64, opts ...jobOpt) *proto.Job {
	j := &proto.Job{
		Spec: proto.JobSpec{
			ID:        id,
			Argv:      []string{"go", "test", "./..."},
			Workspace: proto.Workspace{RepoID: "repo-" + owner},
			Owner:     proto.Owner{Kind: proto.OwnerClaude, ID: owner, Name: "claude · " + owner},
			Req:       proto.Requirements{OS: proto.OSSame, Prefer: proto.PreferAuto},
		},
		Estimate:    proto.Estimate{CPUs: cpus, Mem: mem, Duration: dur, Source: "test"},
		Fingerprint: "fp-" + id,
		Origin:      "gx10",
	}
	for _, o := range opts {
		o(j)
	}
	return j
}

func withReq(f func(r *proto.Requirements)) jobOpt { return func(j *proto.Job) { f(&j.Spec.Req) } }
func withPrio(p proto.Priority) jobOpt             { return func(j *proto.Job) { j.Spec.Priority = p } }
func withTTY() jobOpt                              { return func(j *proto.Job) { j.Spec.TTY = &proto.TTY{Rows: 40, Cols: 120} } }
func withQueuedAt(t time.Time) jobOpt              { return func(j *proto.Job) { j.QueuedAt = t } }

func newTestSched(t *testing.T, clk *fakeClock, nodes ...proto.NodeState) *sched {
	t.Helper()
	s := New(Options{Origin: "gx10", Now: clk.Now, Logf: t.Logf}).(*sched)
	for _, n := range nodes {
		s.UpdateNode(n)
	}
	return s
}

// placed returns the placement of a resolved ticket or fails.
func placed(t *testing.T, tk Ticket) Placement {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	pl, err := tk.Wait(ctx)
	if err != nil {
		t.Fatalf("job %s not placed: %v", tk.JobID(), err)
	}
	return pl
}

func pending(t *testing.T, tk Ticket) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if pl, err := tk.Wait(ctx); err == nil {
		t.Fatalf("job %s unexpectedly placed on %s: %s", tk.JobID(), pl.Node, pl.Reason)
	}
}

func resolvedErr(t *testing.T, tk Ticket) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := tk.Wait(ctx)
	if err == nil || err == context.DeadlineExceeded {
		t.Fatalf("job %s: want a resolution error, got %v", tk.JobID(), err)
	}
	return err
}

func reason(s *sched, id string) string { return s.Snapshot().Reasons[id] }

func fgib(x float64) int64 { return int64(x * float64(gib)) }

// isPlaced reports a resolved placement without blocking.
func isPlaced(tk Ticket) (Placement, bool) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pl, err := tk.Wait(ctx)
	return pl, err == nil
}
