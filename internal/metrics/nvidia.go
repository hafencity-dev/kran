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

	mu      sync.Mutex
	path    string // "" = not found
	looked  bool
	present bool // GPUs were listed at the last probe
}

func newNvidiaProbe(p *prober) *nvidiaProbe { return &nvidiaProbe{p: p} }

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

// sample fills GPU utilization (mean over GPUs) and used memory.
func (n *nvidiaProbe) sample(ctx context.Context, m *proto.NodeMetrics) {
	n.mu.Lock()
	present := n.present
	n.mu.Unlock()
	if !present {
		return
	}
	rs, err := n.query(ctx)
	if err != nil || len(rs) == 0 {
		n.p.logOnce("nvidia-sample", "metrics: nvidia-smi sampling failed: %v", err)
		return
	}
	var util float64
	for _, r := range rs {
		util += r.util
		m.GPUMemUsed += r.memUsed
	}
	m.GPUUtil = util / float64(len(rs))
}
