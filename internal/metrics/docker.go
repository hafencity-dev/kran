package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/dockercli"
	"github.com/hafencity-dev/kran/internal/proto"
)

const (
	dockerStatsEvery = 5 * time.Second  // docker stats is ~2 s of work in the VM; never more often
	dockerInfoEvery  = 60 * time.Second // capacity changes only when the VM is reconfigured
	dockerStatsFresh = 15 * time.Second
	dockerCtxEvery   = 10 * time.Minute // the CLI's context (endpoint) rarely changes
	// dockerFailuresToDown consecutive failed probes make a reachable
	// engine count as gone: one probe killed by its timeout on an
	// overloaded machine proves nothing.
	dockerFailuresToDown = 3
)

// DockerBinary returns the docker CLI path, or "" when none is installed.
// launchd agents get a minimal PATH, so well-known install locations of
// Docker Desktop, Colima, OrbStack and Homebrew are tried too.
func DockerBinary() string { return dockercli.FindHere() }

// dockerHostOverride returns the endpoint to pass with --host when the
// CLI's own choice (DOCKER_HOST, else its context) is a socket that does
// not exist but a well-known one does (a Colima or OrbStack socket without
// a docker context). "" otherwise.
func dockerHostOverride(ctx context.Context, run runner, bin string) string {
	if bin == "" || os.Getenv("DOCKER_HOST") != "" {
		return ""
	}
	ctxHost := ""
	if out, err := run(ctx, 5*time.Second, bin, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"); err == nil {
		ctxHost = strings.TrimSpace(string(out))
	}
	home, _ := os.UserHomeDir()
	return dockercli.Override("", ctxHost, home, os.Getenv("XDG_RUNTIME_DIR"), nil)
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
	Host      string // passed with --host; "" = the CLI's own choice
	CPUs      int
	MemTotal  int64
	Reason    string // why not reachable, one plain sentence
}

// ProbeDocker asks the Docker engine for its CPUs and memory.
func ProbeDocker(ctx context.Context) DockerInfo {
	bin := DockerBinary()
	return probeDocker(ctx, execRunner, bin, dockerHostOverride(ctx, execRunner, bin))
}

func probeDocker(ctx context.Context, run runner, bin, host string) DockerInfo {
	if bin == "" {
		return DockerInfo{Reason: "docker is not installed"}
	}
	out, err := run(ctx, 5*time.Second, bin, hostArgs(host, "info", "--format", "{{.NCPU}} {{.MemTotal}}")...)
	if err != nil {
		return DockerInfo{Binary: bin, Host: host, Reason: "Docker not reachable: " + firstLine(err.Error())}
	}
	n, mem, err := parseDockerInfo(string(out))
	if err != nil {
		return DockerInfo{Binary: bin, Host: host, Reason: "Docker not reachable: " + err.Error()}
	}
	return DockerInfo{Reachable: true, Binary: bin, Host: host, CPUs: n, MemTotal: mem}
}

// hostArgs prefixes a docker CLI command with --host when one is set.
func hostArgs(host string, args ...string) []string {
	if host == "" {
		return args
	}
	return append([]string{"--host", host}, args...)
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
	// api asks the Engine API on a unix socket for (NCPU, MemTotal); nil:
	// always use the CLI (tests).
	api func(ctx context.Context, endpoint string) (int, int64, error)
	// exists reports whether a socket exists (nil: the real filesystem).
	exists    func(string) bool
	statsGate *probeGate

	mu       sync.Mutex
	info     DockerInfo
	infoAt   time.Time
	failures int // consecutive failed info probes
	ctxHost  string
	ctxAt    time.Time
	statsAt  time.Time
	cpus     float64 // Σ container cores
	memUsed  int64   // Σ container memory
	byName   map[string]dockerStat
	idByName map[string]string
}

func newDockerProbe(p *prober) *dockerProbe {
	return &dockerProbe{p: p, vm: runtime.GOOS == "darwin", lookup: DockerBinary, statsGate: newProbeGate(dockerStatsEvery, 2*time.Minute),
		byName: map[string]dockerStat{}, idByName: map[string]string{}}
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
	stressed := d.p.stressed.Load()
	d.mu.Lock()
	age := now.Sub(d.infoAt)
	needInfo := d.infoAt.IsZero() || age >= dockerInfoEvery ||
		(d.failures > 0 && age >= dockerStatsEvery) // confirm a failure soon
	if needInfo && stressed && d.info.Reachable && d.failures == 0 && age < 5*time.Minute {
		needInfo = false // the engine was fine a moment ago; do not add load
	}
	d.mu.Unlock()
	if needInfo {
		bin := d.lookup()
		info := d.probe(ctx, bin, now)
		if ctx.Err() != nil {
			return
		}
		d.mu.Lock()
		if info.Reachable {
			if d.failures >= dockerFailuresToDown {
				d.p.logf("metrics: container runtime reachable again")
			}
			d.failures = 0
			d.info = info
		} else {
			d.failures++
			if !d.info.Reachable || d.failures >= dockerFailuresToDown || bin == "" {
				d.p.logOnce("docker:"+info.Reason, "metrics: container runtime unavailable: %s", info.Reason)
				d.info = info
			}
		}
		d.infoAt = now
		d.mu.Unlock()
	}
	d.mu.Lock()
	info := d.info
	d.mu.Unlock()
	if !info.Reachable || !d.vm || !d.statsGate.allow(now, stressed) {
		return
	}
	stats, err := d.stats(ctx, info.Binary, info.Host)
	if ctx.Err() != nil {
		return
	}
	d.statsGate.done(now, err == nil)
	if err != nil {
		d.p.logOnce("docker-stats", "metrics: docker stats failed: %v (retrying less often)", err)
		return
	}
	d.store(stats, now)
}

// probe asks the engine for its capacity: over the Engine API when the
// endpoint is a unix socket (no process to fork), else with the CLI.
func (d *dockerProbe) probe(ctx context.Context, bin string, now time.Time) DockerInfo {
	if bin == "" {
		return DockerInfo{Reason: "docker is not installed"}
	}
	override, endpoint := d.resolve(ctx, bin, now)
	if d.api != nil && strings.HasPrefix(endpoint, "unix://") {
		n, mem, err := d.api(ctx, endpoint)
		if err != nil {
			return DockerInfo{Binary: bin, Host: override, Reason: "Docker not reachable: " + firstLine(err.Error())}
		}
		return DockerInfo{Reachable: true, Binary: bin, Host: override, CPUs: n, MemTotal: mem}
	}
	return probeDocker(ctx, d.p.run, bin, override)
}

// resolve returns the CLI's --host override and the endpoint it talks to.
// The CLI's context is asked (a fork) at most every dockerCtxEvery.
func (d *dockerProbe) resolve(ctx context.Context, bin string, now time.Time) (override, endpoint string) {
	env := os.Getenv("DOCKER_HOST")
	d.mu.Lock()
	stale := env == "" && (d.ctxAt.IsZero() || now.Sub(d.ctxAt) >= dockerCtxEvery)
	d.mu.Unlock()
	if stale {
		host := ""
		if out, err := d.p.run(ctx, 5*time.Second, bin, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"); err == nil {
			host = strings.TrimSpace(string(out))
		}
		d.mu.Lock()
		d.ctxHost, d.ctxAt = host, now
		d.mu.Unlock()
	}
	d.mu.Lock()
	ctxHost := d.ctxHost
	d.mu.Unlock()
	home, _ := os.UserHomeDir()
	rt := os.Getenv("XDG_RUNTIME_DIR")
	endpoint = dockercli.Endpoint(env, ctxHost, home, rt, d.exists)
	if env == "" {
		override = dockercli.Override("", ctxHost, home, rt, d.exists)
	}
	return override, endpoint
}

// dockerAPIInfo asks the Engine API on a unix socket for NCPU and MemTotal.
func dockerAPIInfo(ctx context.Context, endpoint string) (int, int64, error) {
	sock := strings.TrimPrefix(endpoint, "unix://")
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/info", nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", sock, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, 0, fmt.Errorf("%s: HTTP %d", sock, resp.StatusCode)
	}
	var v struct {
		NCPU     int
		MemTotal int64
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return 0, 0, fmt.Errorf("%s: %w", sock, err)
	}
	if v.NCPU <= 0 || v.MemTotal <= 0 {
		return 0, 0, fmt.Errorf("%s: info without NCPU/MemTotal", sock)
	}
	return v.NCPU, v.MemTotal, nil
}

func (d *dockerProbe) stats(ctx context.Context, bin, host string, names ...string) ([]dockerStat, error) {
	args := append([]string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}"}, names...)
	out, err := d.p.run(ctx, 10*time.Second, bin, hostArgs(host, args...)...)
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
		if dir, err := d.containerCgroup(ctx, bin, info.Host, name); err == nil {
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
	stats, err := d.stats(ctx, bin, info.Host, name)
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

func (d *dockerProbe) containerCgroup(ctx context.Context, bin, host, name string) (string, error) {
	d.mu.Lock()
	id := d.idByName[name]
	d.mu.Unlock()
	if id == "" {
		out, err := d.p.run(ctx, 5*time.Second, bin, hostArgs(host, "inspect", "--format", "{{.Id}}", name)...)
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
