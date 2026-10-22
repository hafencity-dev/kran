package metrics

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// nvidiaProbe queries nvidia-smi (fast: ~30 ms on a GB10) for static GPU
// info and per-sample utilization.
type nvidiaProbe struct {
	p *prober

	gate *probeGate

	mu      sync.Mutex
	path    string // "" = not found
	looked  bool
	present bool // GPUs were listed at the last probe
	util    float64
	memUsed int64 // last successful sample
}

func newNvidiaProbe(p *prober) *nvidiaProbe {
	return &nvidiaProbe{p: p, gate: newProbeGate(5*time.Second, 2*time.Minute)}
}

func (n *nvidiaProbe) bin() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.looked {
		n.looked = true
		if path, err := exec.LookPath("nvidia-smi"); err == nil {
			n.path = path
		} else {
			for _, c := range []string{"/usr/bin/nvidia-smi", "/usr/local/bin/nvidia-smi"} {
				if fileExists(c) {
					n.path = c
					break
				}
			}
		}
	}
	return n.path
}

func (n *nvidiaProbe) query(ctx context.Context) ([]gpuReading, error) {
	bin := n.bin()
	if bin == "" {
		return nil, nil
	}
	out, err := n.p.run(ctx, 5*time.Second, bin, "--query-gpu="+nvidiaQuery, "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return parseNvidiaCSV(string(out))
}

// probe returns the node's NVIDIA GPUs (nil when there are none).
func (n *nvidiaProbe) probe(ctx context.Context) []proto.GPU {
	rs, err := n.query(ctx)
	if err != nil {
		n.p.logOnce("nvidia-probe", "metrics: nvidia-smi failed, GPUs not reported: %v", err)
	}
	gpus := make([]proto.GPU, 0, len(rs))
	for _, r := range rs {
		gpus = append(gpus, proto.GPU{Vendor: "nvidia", Model: r.name, MemTotal: r.memTot, Unified: r.unified})
	}
	n.mu.Lock()
	n.present = len(gpus) > 0
	n.mu.Unlock()
	if len(gpus) == 0 {
		return nil
	}
	return gpus
}

// sample fills GPU utilization (mean over GPUs) and used memory. Paced by
// the gate: on an overloaded node, or after a failed run, the last reading
// stands in instead of forking nvidia-smi again.
func (n *nvidiaProbe) sample(ctx context.Context, m *proto.NodeMetrics, stressed bool) {
	n.mu.Lock()
	present := n.present
	n.mu.Unlock()
	if !present {
		return
	}
	now := n.p.now()
	if !n.gate.allow(now, stressed) {
		n.mu.Lock()
		m.GPUUtil, m.GPUMemUsed = n.util, n.memUsed
		n.mu.Unlock()
		return
	}
	rs, err := n.query(ctx)
	ok := err == nil && len(rs) > 0
	n.gate.done(now, ok)
	if !ok {
		n.p.logOnce("nvidia-sample", "metrics: nvidia-smi sampling failed: %v (retrying less often)", err)
		n.mu.Lock()
		n.util, n.memUsed = 0, 0
		n.mu.Unlock()
		return
	}
	var util float64
	var mem int64
	for _, r := range rs {
		util += r.util
		mem += r.memUsed
	}
	m.GPUUtil, m.GPUMemUsed = util/float64(len(rs)), mem
	n.mu.Lock()
	n.util, n.memUsed = m.GPUUtil, mem
	n.mu.Unlock()
}
