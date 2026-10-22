package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

const (
	dockerStatsEvery = 5 * time.Second  // docker stats is ~2 s of work in the VM; never more often
	dockerInfoEvery  = 60 * time.Second // capacity changes only when the VM is reconfigured
	dockerStatsFresh = 15 * time.Second
)

// dockerCandidates are tried when docker is not on PATH (launchd agents get
// a minimal PATH; Colima/Docker Desktop/Homebrew install elsewhere).
var dockerCandidates = []string{
	"/usr/local/bin/docker",
	"/opt/homebrew/bin/docker",
	"/usr/local/lokalaise/tools/bin/docker",
	"/Applications/Docker.app/Contents/Resources/bin/docker",
	"/usr/bin/docker",
}

// DockerBinary returns the docker CLI path, or "" when none is installed.
func DockerBinary() string {
	if p, err := exec.LookPath("docker"); err == nil {
		return p
	}
	for _, c := range dockerCandidates {
		if fileExists(c) {
			return c
		}
	}
	return ""
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// DockerInfo is the capacity of the Docker engine jobs would use (on macOS:
// the Linux VM of Colima/Docker Desktop).
type DockerInfo struct {
	Reachable bool
	Binary    string
	CPUs      int
	MemTotal  int64
	Reason    string // why not reachable, one plain sentence
}

// ProbeDocker asks the Docker engine for its CPUs and memory.
func ProbeDocker(ctx context.Context) DockerInfo {
	return probeDocker(ctx, execRunner, DockerBinary())
}

func probeDocker(ctx context.Context, run runner, bin string) DockerInfo {
	if bin == "" {
		return DockerInfo{Reason: "docker is not installed"}
	}
	out, err := run(ctx, 5*time.Second, bin, "info", "--format", "{{.NCPU}} {{.MemTotal}}")
	if err != nil {
		return DockerInfo{Binary: bin, Reason: "Docker not reachable: " + firstLine(err.Error())}
	}
	n, mem, err := parseDockerInfo(string(out))
	if err != nil {
		return DockerInfo{Binary: bin, Reason: "Docker not reachable: " + err.Error()}
	}
	return DockerInfo{Reachable: true, Binary: bin, CPUs: n, MemTotal: mem}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// dockerProbe tracks engine capacity and (on macOS, where containers run in
// a VM) aggregated container usage.
type dockerProbe struct {
	p      *prober
	vm     bool // containers run in a VM: live usage comes from docker stats
	lookup func() string

	mu       sync.Mutex
	info     DockerInfo
	infoAt   time.Time
	statsAt  time.Time
	cpus     float64 // Σ container cores
	memUsed  int64   // Σ container memory
	byName   map[string]dockerStat
	idByName map[string]string
}

func newDockerProbe(p *prober) *dockerProbe {
	return &dockerProbe{p: p, vm: runtime.GOOS == "darwin", lookup: DockerBinary, byName: map[string]dockerStat{}, idByName: map[string]string{}}
}

func (d *dockerProbe) loop(ctx context.Context) {
	t := time.NewTicker(dockerStatsEvery)
	defer t.Stop()
	for {
		d.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *dockerProbe) refresh(ctx context.Context) {
	now := d.p.now()
	d.mu.Lock()
	needInfo := d.infoAt.IsZero() || now.Sub(d.infoAt) >= dockerInfoEvery
	d.mu.Unlock()
	if needInfo {
		info := probeDocker(ctx, d.p.run, d.lookup())
		if ctx.Err() != nil {
			return
		}
		if !info.Reachable {
			d.p.logOnce("docker:"+info.Reason, "metrics: container runtime unavailable: %s", info.Reason)
		}
		d.mu.Lock()
		d.info, d.infoAt = info, now
		d.mu.Unlock()
	}
	d.mu.Lock()
	info := d.info
	d.mu.Unlock()
	if !info.Reachable || !d.vm {
		return
	}
	stats, err := d.stats(ctx, info.Binary)
	if err != nil {
		if ctx.Err() == nil {
			d.p.logOnce("docker-stats", "metrics: docker stats failed: %v", err)
		}
		return
	}
	d.store(stats, now)
}

func (d *dockerProbe) stats(ctx context.Context, bin string, names ...string) ([]dockerStat, error) {
	args := append([]string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}"}, names...)
	out, err := d.p.run(ctx, 10*time.Second, bin, args...)
	if err != nil {
		return nil, err
	}
	return parseDockerStats(string(out)), nil
}

func (d *dockerProbe) store(stats []dockerStat, now time.Time) {
	var cpus float64
	var mem int64
	by := make(map[string]dockerStat, len(stats))
	for _, s := range stats {
		cpus += s.cpus
		mem += s.mem
		by[s.name] = s
	}
	d.mu.Lock()
	d.cpus, d.memUsed, d.byName, d.statsAt = cpus, mem, by, now
	d.mu.Unlock()
}

// runtimeMetrics returns live availability of the container runtime. On
// Linux containers share the host, so host values apply.
func (d *dockerProbe) runtimeMetrics(host proto.NodeMetrics) (proto.RuntimeMetrics, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.info.Reachable {
		return proto.RuntimeMetrics{}, false
	}
	if !d.vm {
		return proto.RuntimeMetrics{CPUBusy: host.CPUBusy, MemAvailable: min(host.MemAvailable, d.info.MemTotal)}, true
	}
	rm := proto.RuntimeMetrics{MemAvailable: d.info.MemTotal}
	if !d.statsAt.IsZero() && host.Time.Sub(d.statsAt) <= dockerStatsFresh {
		rm.CPUBusy = clamp01(d.cpus / float64(d.info.CPUs))
		rm.MemAvailable = max(0, d.info.MemTotal-d.memUsed)
	}
	return rm, true
}

// containerUsage measures one container. On Linux it reads the container's
// cgroup directly (cheap, exact); otherwise it uses docker stats.
func (d *dockerProbe) containerUsage(ctx context.Context, name string, now time.Time) (proto.Resources, error) {
	d.mu.Lock()
	info := d.info
	d.mu.Unlock()
	bin := info.Binary
	if bin == "" {
		bin = d.lookup()
	}
	if bin == "" {
		return proto.Resources{}, errors.New("container usage: docker is not installed")
	}
	if runtime.GOOS == "linux" {
		if dir, err := d.containerCgroup(ctx, bin, name); err == nil {
			return d.p.cgroupUsage(dir, now)
		}
	}
	d.mu.Lock()
	s, ok := d.byName[name]
	fresh := ok && now.Sub(d.statsAt) <= 10*time.Second
	d.mu.Unlock()
	if fresh {
		return proto.Resources{CPUs: s.cpus, Mem: s.mem}, nil
	}
	stats, err := d.stats(ctx, bin, name)
	if err != nil {
		return proto.Resources{}, fmt.Errorf("container usage of %s: %w", name, err)
	}
	for _, s := range stats {
		if s.name == name {
			return proto.Resources{CPUs: s.cpus, Mem: s.mem}, nil
		}
	}
	return proto.Resources{}, fmt.Errorf("container usage of %s: not listed by docker stats", name)
}

func (d *dockerProbe) containerCgroup(ctx context.Context, bin, name string) (string, error) {
	d.mu.Lock()
	id := d.idByName[name]
	d.mu.Unlock()
	if id == "" {
		out, err := d.p.run(ctx, 5*time.Second, bin, "inspect", "--format", "{{.Id}}", name)
		if err != nil {
			return "", err
		}
		id = strings.TrimSpace(string(out))
		if id == "" {
			return "", errors.New("docker inspect: empty id")
		}
		d.mu.Lock()
		if len(d.idByName) > 512 {
			d.idByName = map[string]string{}
		}
		d.idByName[name] = id
		d.mu.Unlock()
	}
	for _, rel := range []string{
		"system.slice/docker-" + id + ".scope", // systemd cgroup driver
		"docker/" + id,                         // cgroupfs driver
	} {
		dir := filepath.Join(CgroupRoot, rel)
		if fileExists(filepath.Join(dir, "cpu.stat")) {
			return dir, nil
		}
	}
	return "", fmt.Errorf("no cgroup found for container %s", name)
}
