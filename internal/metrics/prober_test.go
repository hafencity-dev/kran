package metrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// fakeRunner answers commands by matching the joined argv against prefixes.
type fakeRunner struct {
	mu    sync.Mutex
	out   map[string]string
	err   map[string]error
	calls []string
}

func (f *fakeRunner) run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	for k, e := range f.err {
		if strings.Contains(cmd, k) {
			return nil, e
		}
	}
	for k, v := range f.out {
		if strings.Contains(cmd, k) {
			return []byte(v), nil
		}
	}
	return nil, errors.New("fake: unexpected command " + cmd)
}

func (f *fakeRunner) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func TestNvidiaGB10Unified(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"--query-gpu": fixtureNvidiaGB10}}
	p := newProberWith(Options{}, fr.run)
	p.nvidia.looked, p.nvidia.path = true, "/usr/bin/nvidia-smi"
	gpus := p.nvidia.probe(context.Background())
	if len(gpus) != 1 || gpus[0] != (proto.GPU{Vendor: "nvidia", Model: "NVIDIA GB10", Unified: true}) {
		t.Fatalf("gpus = %+v", gpus)
	}
	var m proto.NodeMetrics
	p.nvidia.sample(context.Background(), &m, false)
	if !near(m.GPUUtil, 0.06) || m.GPUMemUsed != 0 {
		t.Fatalf("metrics = %+v", m)
	}
}

func TestNvidiaAbsentOrFailing(t *testing.T) {
	var logs []string
	fr := &fakeRunner{err: map[string]error{"--query-gpu": errors.New("NVIDIA-SMI has failed")}}
	p := newProberWith(Options{Logf: func(f string, a ...any) { logs = append(logs, f) }}, fr.run)
	p.nvidia.looked, p.nvidia.path = true, "/usr/bin/nvidia-smi"
	if g := p.nvidia.probe(context.Background()); g != nil {
		t.Fatalf("gpus = %+v", g)
	}
	p.nvidia.probe(context.Background())
	if len(logs) != 1 {
		t.Fatalf("failure must be logged once, got %d", len(logs))
	}
	var m proto.NodeMetrics
	p.nvidia.sample(context.Background(), &m, false)
	if fr.count("--query-gpu") != 2 {
		t.Fatal("sample must not query when no GPU was found")
	}
	p2 := newProberWith(Options{}, fr.run)
	p2.nvidia.looked = true // no binary
	if g := p2.nvidia.probe(context.Background()); g != nil {
		t.Fatal("no binary means no GPUs")
	}
}

func TestDockerVMRuntimeMetrics(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{
		"info --format":         "16 33584394240\n",
		"stats --no-stream --f": fixtureDockerStats,
	}}
	p := newProberWith(Options{}, fr.run)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	d := p.docker
	d.vm = true
	d.lookup = func() string { return "/usr/local/bin/docker" }

	if _, ok := d.runtimeMetrics(proto.NodeMetrics{Time: now}); ok {
		t.Fatal("unprobed docker must not report a runtime")
	}
	d.refresh(context.Background())
	rm, ok := d.runtimeMetrics(proto.NodeMetrics{Time: now})
	if !ok {
		t.Fatal("docker reachable: want runtime metrics")
	}
	wantCPU := (0.002 + 0.3124 + 2.5) / 16
	wantMem := int64(33584394240) - (bytesOf(71.45*(1<<20)) + bytesOf(1.825*(1<<30)) + 512<<10)
	if !near(rm.CPUBusy, wantCPU) || rm.MemAvailable != wantMem {
		t.Fatalf("rm = %+v, want cpu %v mem %d", rm, wantCPU, wantMem)
	}
	// Stale stats: capacity only.
	rm, _ = d.runtimeMetrics(proto.NodeMetrics{Time: now.Add(time.Minute)})
	if rm.CPUBusy != 0 || rm.MemAvailable != 33584394240 {
		t.Fatalf("stale rm = %+v", rm)
	}
	// docker info is not repeated within dockerInfoEvery.
	d.refresh(context.Background())
	if n := fr.count("info --format"); n != 1 {
		t.Fatalf("docker info ran %d times", n)
	}
	// Container usage served from the fresh aggregate.
	u, err := d.containerUsage(context.Background(), "kran-job-abc", now)
	// (On Linux, docker inspect fails in the fake, so the cgroup lookup
	// falls back to the cached stats too.)
	if err != nil || !near(u.CPUs, 2.5) || u.Mem != 512<<10 {
		t.Fatalf("usage = %+v %v", u, err)
	}
}

func TestDockerUnreachable(t *testing.T) {
	var logs []string
	fr := &fakeRunner{err: map[string]error{"info": errors.New("exit status 1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock")}}
	p := newProberWith(Options{Logf: func(f string, a ...any) { logs = append(logs, f) }}, fr.run)
	p.docker.lookup = func() string { return "/usr/bin/docker" }
	p.docker.refresh(context.Background())
	if _, ok := p.docker.runtimeMetrics(proto.NodeMetrics{}); ok {
		t.Fatal("unreachable docker must not report a runtime")
	}
	if !strings.HasPrefix(p.docker.info.Reason, "Docker not reachable") || len(logs) != 1 {
		t.Fatalf("reason %q logs %v", p.docker.info.Reason, logs)
	}
	if fr.count("stats") != 0 {
		t.Fatal("stats must not run when docker is unreachable")
	}
	if info := probeDocker(context.Background(), fr.run, "", ""); info.Reachable || info.Reason != "docker is not installed" {
		t.Fatalf("info = %+v", info)
	}
}

func TestDockerLinuxUsesHost(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"info --format": "20 130595991552\n"}}
	p := newProberWith(Options{}, fr.run)
	p.docker.vm = false
	p.docker.lookup = func() string { return "/usr/bin/docker" }
	p.docker.refresh(context.Background())
	rm, ok := p.docker.runtimeMetrics(proto.NodeMetrics{CPUBusy: 0.4, MemAvailable: 50 << 30})
	if !ok || rm.CPUBusy != 0.4 || rm.MemAvailable != 50<<30 {
		t.Fatalf("rm = %+v", rm)
	}
	if fr.count("stats") != 0 {
		t.Fatal("Linux must not poll docker stats")
	}
}

func TestRateTracker(t *testing.T) {
	r := newRateTracker()
	t0 := time.Unix(1000, 0)
	if got := r.cores("a", 10, t0, 5); !near(got, 2) {
		t.Fatalf("first reading averages over age: %v", got)
	}
	if got := r.cores("a", 16, t0.Add(2*time.Second), 0); !near(got, 3) {
		t.Fatalf("delta rate = %v", got)
	}
	if got := r.cores("b", 1, t0, 0); got != 0 {
		t.Fatalf("no age, no history: %v", got)
	}
}

func TestSampleNeverBlocksAndRunExits(t *testing.T) {
	fr := &fakeRunner{err: map[string]error{"": errors.New("no tools in test")}}
	p := newProberWith(Options{StateDir: t.TempDir()}, fr.run)
	p.docker.lookup = func() string { return "" }
	if m := p.Sample(); !m.Time.IsZero() {
		t.Fatal("Sample before Run must be the zero snapshot")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for p.Sample().Time.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("Run did not sample immediately")
		}
		time.Sleep(10 * time.Millisecond)
	}
	m := p.Sample()
	m.Runtimes[proto.RuntimeNative] = proto.RuntimeMetrics{CPUBusy: 99}
	if p.Sample().Runtimes[proto.RuntimeNative].CPUBusy == 99 {
		t.Fatal("Sample must return a copy")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}

func TestJobUsageEmptyRef(t *testing.T) {
	p := newProberWith(Options{}, (&fakeRunner{}).run)
	if _, err := p.JobUsage(UsageRef{}); err == nil {
		t.Fatal("want error")
	}
}

func TestBenchmarkDeterministicWorkload(t *testing.T) {
	a, b := newBenchState(), newBenchState()
	if benchUnit(a, 7) != benchUnit(b, 7) || a.tab != b.tab {
		t.Fatal("workload must be deterministic")
	}
	if r := benchRate(2, 5*time.Millisecond); r <= 0 {
		t.Fatalf("rate = %v", r)
	}
	if got := benchScaleRate(benchCalib); got != 1000 {
		t.Fatalf("scale = %v", got)
	}
}

func TestStatfsNearest(t *testing.T) {
	dir := t.TempDir()
	total, free, err := statfsNearest(dir + "/does/not/exist/yet")
	if err != nil || total <= 0 || free <= 0 || free > total {
		t.Fatalf("total=%d free=%d err=%v", total, free, err)
	}
}

func TestCgroupUsage(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.current", "1073741824\n")
	write("memory.peak", "2147483648\n")
	write("cpu.stat", "usage_usec 4000000\nuser_usec 3000000\nsystem_usec 1000000\n")
	u, err := CgroupStats(dir)
	if err != nil || u.MemCurrent != 1<<30 || u.MemPeak != 2<<30 || !near(u.CPUSeconds, 4) {
		t.Fatalf("stats = %+v %v", u, err)
	}
	p := newProberWith(Options{}, (&fakeRunner{}).run)
	now := time.Now()
	if _, err := p.JobUsage(UsageRef{CgroupPath: dir}); err != nil {
		t.Fatal(err)
	}
	write("cpu.stat", "usage_usec 10000000\n")
	p.now = func() time.Time { return now.Add(3 * time.Second) }
	r, err := p.cgroupUsage(dir, now.Add(3*time.Second))
	if err != nil || r.Mem != 1<<30 || r.CPUs <= 0 {
		t.Fatalf("usage = %+v %v", r, err)
	}
	if _, err := CgroupStats(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("want error for missing group")
	}
}
