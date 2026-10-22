package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestProbeGateBacksOffAndRecovers(t *testing.T) {
	t0 := time.Unix(1000, 0)
	g := newProbeGate(2*time.Second, time.Minute)
	if !g.allow(t0, false) {
		t.Fatal("first run must be allowed")
	}
	g.done(t0, false)
	if g.allow(t0.Add(time.Second), false) {
		t.Fatal("retried right after a failure")
	}
	if !g.allow(t0.Add(3*time.Second), false) {
		t.Fatal("not retried after the backoff")
	}
	g.done(t0.Add(3*time.Second), false)
	if g.allow(t0.Add(6*time.Second), false) {
		t.Fatal("second failure must double the wait")
	}
	for i := 0; i < 20; i++ {
		g.done(t0, false)
	}
	if !g.allow(t0.Add(61*time.Second), false) {
		t.Fatal("backoff must be capped")
	}
	g.done(t0.Add(61*time.Second), true)
	if !g.allow(t0.Add(61*time.Second), false) {
		t.Fatal("success must reset the backoff")
	}
	// Under stress the probe runs at most every stressedEvery.
	g.done(t0.Add(100*time.Second), true)
	if g.allow(t0.Add(110*time.Second), true) {
		t.Fatal("a stressed node must not fork the probe every sample")
	}
	if !g.allow(t0.Add(100*time.Second+stressedProbeEvery), true) {
		t.Fatal("a stressed node still probes now and then")
	}
}

// Under load nvidia-smi is not forked every 2 s; the last reading stands.
// A killed run backs off instead of being retried each sample.
func TestNvidiaPacedUnderStress(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"--query-gpu": fixtureNvidiaGB10}}
	p := newProberWith(Options{}, fr.run)
	now := time.Unix(5000, 0)
	p.now = func() time.Time { return now }
	p.nvidia.looked, p.nvidia.path = true, "/usr/bin/nvidia-smi"
	p.nvidia.probe(context.Background())
	var m proto.NodeMetrics
	p.nvidia.sample(context.Background(), &m, false)
	base := fr.count("--query-gpu")
	for i := 0; i < 5; i++ {
		now = now.Add(2 * time.Second)
		m = proto.NodeMetrics{}
		p.nvidia.sample(context.Background(), &m, true)
		if !near(m.GPUUtil, 0.06) {
			t.Fatalf("skipped sample lost the reading: %+v", m)
		}
	}
	if n := fr.count("--query-gpu") - base; n != 0 {
		t.Fatalf("nvidia-smi ran %d times in 10 s under stress", n)
	}

	fr.mu.Lock()
	fr.err = map[string]error{"--query-gpu": errors.New("/usr/bin/nvidia-smi: signal: killed")}
	fr.mu.Unlock()
	now = now.Add(time.Minute)
	p.nvidia.sample(context.Background(), &m, false)
	before := fr.count("--query-gpu")
	now = now.Add(2 * time.Second)
	p.nvidia.sample(context.Background(), &m, false)
	if fr.count("--query-gpu") != before {
		t.Fatal("a killed nvidia-smi was retried at the next sample")
	}
}

// One docker probe that times out does not make the container runtime
// disappear; only consecutive failures do, and that is logged once.
func TestDockerNeedsConsecutiveFailures(t *testing.T) {
	var logs []string
	fr := &fakeRunner{out: map[string]string{"info --format": "20 130595991552\n"}}
	p := newProberWith(Options{Logf: func(f string, a ...any) { logs = append(logs, f) }}, fr.run)
	now := time.Unix(5000, 0)
	p.now = func() time.Time { return now }
	d := p.docker
	d.vm = false
	d.lookup = func() string { return "/usr/bin/docker" }
	d.refresh(context.Background())
	if _, ok := d.runtimeMetrics(proto.NodeMetrics{Time: now}); !ok {
		t.Fatal("docker reachable")
	}
	fr.mu.Lock()
	fr.err = map[string]error{"info --format": errors.New("/usr/bin/docker: signal: killed")}
	fr.mu.Unlock()
	for i := 1; i <= dockerFailuresToDown; i++ {
		now = now.Add(dockerInfoEvery)
		d.refresh(context.Background())
		_, ok := d.runtimeMetrics(proto.NodeMetrics{Time: now})
		if i < dockerFailuresToDown && !ok {
			t.Fatalf("runtime gone after %d failed probe(s)", i)
		}
		if i == dockerFailuresToDown && ok {
			t.Fatalf("runtime still reported after %d failed probes", i)
		}
	}
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "container runtime unavailable") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("logged %d times: %v", n, logs)
	}
}

// Where the engine is a unix socket, capacity comes from the Engine API:
// no docker CLI process per probe.
func TestDockerInfoViaAPI(t *testing.T) {
	fr := &fakeRunner{out: map[string]string{"info --format": "1 1\n"}}
	p := newProberWith(Options{}, fr.run)
	d := p.docker
	d.vm = false
	d.lookup = func() string { return "/usr/bin/docker" }
	d.exists = func(string) bool { return true }
	var asked string
	d.api = func(ctx context.Context, endpoint string) (int, int64, error) {
		asked = endpoint
		return 20, 130595991552, nil
	}
	d.refresh(context.Background())
	if !strings.HasPrefix(asked, "unix://") {
		t.Fatalf("api asked %q", asked)
	}
	if fr.count("info") != 0 {
		t.Fatalf("docker CLI ran: %v", fr.calls)
	}
	if !d.info.Reachable || d.info.CPUs != 20 {
		t.Fatalf("info %+v", d.info)
	}
	// The context lookup (a CLI fork) is cached.
	d.infoAt = time.Time{}
	d.refresh(context.Background())
	if n := fr.count("context inspect"); n > 1 {
		t.Fatalf("docker context inspect ran %d times", n)
	}
}
