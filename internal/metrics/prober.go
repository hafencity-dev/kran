package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/version"
)

// SampleInterval is how often Run refreshes live metrics.
const SampleInterval = 2 * time.Second

// runner executes an external command with a timeout and returns stdout.
type runner func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			return stdout.Bytes(), fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return stdout.Bytes(), fmt.Errorf("%s: %w", name, err)
	}
	return stdout.Bytes(), nil
}

// platform is the OS-specific half of the prober.
type platform interface {
	// staticInfo fills OSVersion, CPUModel, CPUs, PerfCPUs, MemTotal and
	// built-in GPUs.
	staticInfo(ctx context.Context, info *proto.NodeInfo)
	// sample fills load, CPU, memory, swap, pressure and network fields.
	sample(ctx context.Context, m *proto.NodeMetrics, now time.Time)
	// start launches background helpers owned by Run (wg tracks them).
	start(ctx context.Context, wg *sync.WaitGroup)
	pgidUsage(ctx context.Context, pgid int, now time.Time) (proto.Resources, error)
	// pinBench restricts the calling (locked) thread to performance cores.
	pinBench()
}

type prober struct {
	opts    Options
	run     runner
	now     func() time.Time
	procDir string // "/proc" (tests: fixture dir)
	sysDir  string // "/sys"
	etcDir  string // "/etc"
	plat    platform
	docker  *dockerProbe
	nvidia  *nvidiaProbe
	rates   *rateTracker
	started time.Time

	infoMu    sync.Mutex
	info      proto.NodeInfo
	haveInfo  bool
	benchDone bool
	score     float64

	mu       sync.RWMutex
	last     proto.NodeMetrics
	stressed atomic.Bool // the last sample showed an overloaded node

	logMu  sync.Mutex
	logged map[string]bool
}

func newProber(opts Options) Prober {
	p := newProberWith(opts, execRunner)
	p.docker.api = dockerAPIInfo
	return p
}

func newProberWith(opts Options, run runner) *prober {
	p := &prober{
		opts:    opts,
		run:     run,
		now:     time.Now,
		procDir: "/proc",
		sysDir:  "/sys",
		etcDir:  "/etc",
		rates:   newRateTracker(),
		logged:  map[string]bool{},
	}
	p.started = p.now().UTC()
	p.docker = newDockerProbe(p)
	p.nvidia = newNvidiaProbe(p)
	p.plat = newPlatform(p)
	return p
}

func (p *prober) logf(format string, args ...any) {
	if p.opts.Logf != nil {
		p.opts.Logf(format, args...)
	}
}

// logOnce logs a degradation once per key so a missing tool does not spam
// the log every sample.
func (p *prober) logOnce(key, format string, args ...any) {
	p.logMu.Lock()
	seen := p.logged[key]
	p.logged[key] = true
	p.logMu.Unlock()
	if !seen {
		p.logf(format, args...)
	}
}

func (p *prober) readFile(dir, rel string) (string, error) {
	b, err := os.ReadFile(dir + "/" + rel)
	return string(b), err
}

func (p *prober) Info(ctx context.Context) (proto.NodeInfo, error) {
	p.infoMu.Lock()
	defer p.infoMu.Unlock()
	if !p.haveInfo {
		if err := p.probeLocked(ctx); err != nil {
			return proto.NodeInfo{}, err
		}
	}
	return cloneInfo(p.info), nil
}

func (p *prober) Refresh(ctx context.Context) error {
	p.infoMu.Lock()
	defer p.infoMu.Unlock()
	return p.probeLocked(ctx)
}

func (p *prober) probeLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info := proto.NodeInfo{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Version:   version.Version,
		CPUs:      runtime.NumCPU(),
		StartedAt: p.started,
	}
	if h, err := os.Hostname(); err == nil {
		info.Hostname = h
		info.Name = shortName(h)
	}
	p.plat.staticInfo(ctx, &info)
	if info.PerfCPUs <= 0 || info.PerfCPUs > info.CPUs {
		info.PerfCPUs = info.CPUs
	}
	info.GPUs = append(info.GPUs, p.nvidia.probe(ctx)...)
	if total, _, err := statfsNearest(p.opts.StateDir); err == nil {
		info.DiskTotal = total
	} else {
		p.logOnce("statfs-info", "metrics: disk size of %s unknown: %v", p.opts.StateDir, err)
	}
	if !p.benchDone {
		p.score = runBenchmark(p.plat.pinBench)
		p.benchDone = true
	}
	info.SingleCoreScore = p.score
	p.info = info
	p.haveInfo = true
	return nil
}

func shortName(host string) string {
	h, _, _ := strings.Cut(host, ".")
	return strings.ToLower(h)
}

func cloneInfo(in proto.NodeInfo) proto.NodeInfo {
	out := in
	out.GPUs = append([]proto.GPU(nil), in.GPUs...)
	if in.Labels != nil {
		out.Labels = make(map[string]string, len(in.Labels))
		for k, v := range in.Labels {
			out.Labels[k] = v
		}
	}
	return out
}

// Sample returns the latest snapshot; it never does I/O. Before Run's
// first sample it returns zero metrics (Time is zero).
func (p *prober) Sample() proto.NodeMetrics {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cloneMetrics(p.last)
}

func cloneMetrics(m proto.NodeMetrics) proto.NodeMetrics {
	if m.Runtimes != nil {
		rt := make(map[proto.RuntimeKind]proto.RuntimeMetrics, len(m.Runtimes))
		for k, v := range m.Runtimes {
			rt[k] = v
		}
		m.Runtimes = rt
	}
	return m
}

// Run samples immediately, then every SampleInterval, until ctx ends.
func (p *prober) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	p.plat.start(ctx, &wg)
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.docker.loop(ctx)
	}()
	p.sampleOnce(ctx)
	t := time.NewTicker(SampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.sampleOnce(ctx)
		}
	}
}

func (p *prober) sampleOnce(ctx context.Context) {
	now := p.now().UTC()
	m := proto.NodeMetrics{Time: now}
	p.plat.sample(ctx, &m, now)
	if _, free, err := statfsNearest(p.opts.StateDir); err == nil {
		m.DiskFree = free
	} else {
		p.logOnce("statfs", "metrics: free disk of %s unknown: %v", p.opts.StateDir, err)
	}
	stressed := Stressed(m, proto.NodeInfo{OS: runtime.GOOS, CPUs: runtime.NumCPU()})
	p.stressed.Store(stressed)
	p.nvidia.sample(ctx, &m, stressed)
	m.Runtimes = map[proto.RuntimeKind]proto.RuntimeMetrics{
		proto.RuntimeNative: {CPUBusy: m.CPUBusy, MemAvailable: m.MemAvailable},
	}
	if rm, ok := p.docker.runtimeMetrics(m); ok {
		m.Runtimes[proto.RuntimeContainer] = rm
	}
	p.mu.Lock()
	p.last = m
	p.mu.Unlock()
}

func (p *prober) JobUsage(ref UsageRef) (proto.Resources, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := p.now()
	switch {
	case ref.CgroupPath != "":
		return p.cgroupUsage(ref.CgroupPath, now)
	case ref.PGID > 0:
		return p.plat.pgidUsage(ctx, ref.PGID, now)
	case ref.Container != "":
		return p.docker.containerUsage(ctx, ref.Container, now)
	}
	return proto.Resources{}, errors.New("metrics: empty usage reference")
}

// statfsNearest statfs'es path or its nearest existing parent (the state
// dir may not exist yet on first start).
func statfsNearest(path string) (total, free int64, err error) {
	if path == "" {
		path = "/"
	}
	for {
		total, free, err = statfs(path)
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return total, free, err
		}
		parent := path[:max(strings.LastIndexByte(path, '/'), 0)]
		if parent == "" {
			parent = "/"
		}
		if parent == path {
			return 0, 0, err
		}
		path = parent
	}
}

// rateTracker turns cumulative CPU seconds into cores, per key.
type rateTracker struct {
	mu   sync.Mutex
	last map[string]rateEntry
}

type rateEntry struct {
	cpu float64
	at  time.Time
}

func newRateTracker() *rateTracker { return &rateTracker{last: map[string]rateEntry{}} }

// cores returns the CPU rate for key. With no earlier reading it averages
// over age (the job's lifetime), which is the best estimate available.
func (r *rateTracker) cores(key string, cpuSeconds float64, now time.Time, age float64) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.last[key]
	r.last[key] = rateEntry{cpu: cpuSeconds, at: now}
	// Forget keys of jobs that stopped being measured.
	if len(r.last) > 256 {
		for k, e := range r.last {
			if now.Sub(e.at) > 10*time.Minute {
				delete(r.last, k)
			}
		}
	}
	if ok {
		if dt := now.Sub(prev.at).Seconds(); dt >= 0.2 && cpuSeconds >= prev.cpu {
			return (cpuSeconds - prev.cpu) / dt
		}
	}
	if age > 0.5 {
		return cpuSeconds / age
	}
	return 0
}
