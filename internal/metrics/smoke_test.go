package metrics

import (
	"context"
	"encoding/json"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestSmokeRealMachine probes the machine the tests run on and logs what it
// sees (run with -v to read the values). It asserts only invariants that
// hold on any real node.
func TestSmokeRealMachine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := newProberWith(Options{StateDir: t.TempDir(), Logf: t.Logf}, execRunner)
	start := time.Now()
	info, err := p.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Info took %v", time.Since(start).Round(time.Millisecond))
	b, _ := json.MarshalIndent(info, "", "  ")
	t.Logf("info: %s", b)
	if info.CPUs <= 0 || info.PerfCPUs <= 0 || info.PerfCPUs > info.CPUs || info.MemTotal <= 0 ||
		info.DiskTotal <= 0 || info.SingleCoreScore <= 0 || info.Name == "" || info.OSVersion == "" {
		t.Fatalf("implausible info: %+v", info)
	}
	if again, _ := p.Info(ctx); again.SingleCoreScore != info.SingleCoreScore {
		t.Fatal("score must be cached")
	}

	p.sampleOnce(ctx)
	time.Sleep(300 * time.Millisecond)
	p.sampleOnce(ctx)
	m := p.Sample()
	b, _ = json.MarshalIndent(m, "", "  ")
	t.Logf("sample: %s", b)
	if m.MemAvailable <= 0 || m.MemAvailable > info.MemTotal || m.DiskFree <= 0 || m.CPUBusy < 0 || m.CPUBusy > 1 {
		t.Fatalf("implausible sample: %+v", m)
	}

	pg := syscall.Getpgrp()
	u, err := p.JobUsage(UsageRef{PGID: pg})
	if err != nil {
		t.Fatalf("usage of own process group %d: %v", pg, err)
	}
	t.Logf("own process group %d: %.2f cores, %d MiB", pg, u.CPUs, u.Mem>>20)
	if u.Mem <= 0 {
		t.Fatal("own process group must use memory")
	}

	d := ProbeDocker(ctx)
	t.Logf("docker: %+v", d)
}

// TestLiveRunLoop runs the real sample loop for ~7 s (CPU busy from top on
// macOS, docker stats of the VM). Opt-in: KRAN_METRICS_LIVE=1.
func TestLiveRunLoop(t *testing.T) {
	if os.Getenv("KRAN_METRICS_LIVE") == "" {
		t.Skip("set KRAN_METRICS_LIVE=1 to run the live sample loop")
	}
	p := newProberWith(Options{StateDir: t.TempDir(), Logf: t.Logf}, execRunner)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	for i := 0; i < 4; i++ {
		time.Sleep(SampleInterval + 100*time.Millisecond)
		m := p.Sample()
		t.Logf("busy=%.2f load=%.1f avail=%dMiB rx=%.0fB/s gpu=%.2f runtimes=%+v", m.CPUBusy, m.Load1, m.MemAvailable>>20, m.NetRxBps, m.GPUUtil, m.Runtimes)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
