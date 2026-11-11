package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// How much of the Docker VM's memory the host has already paid for (SPEC
// §6.3, container jobs in a VM). A Colima/Lima guest takes a host page when
// it first touches it and never hands it back (no free-page reporting), so
// the VM process's physical footprint only grows until the VM restarts. The
// scheduler counts container jobs against the host only for the unpaid
// rest; a reading that is missing, failed or stale is published as none,
// so the VM gets no credit (never the last reading: a restarted VM starts
// unpaid).
const (
	vmResidentEvery = 30 * time.Second
	vmResidentStale = 2 * time.Minute
)

var errNotColima = errors.New("the Docker endpoint is not Colima's; only a Colima VM's footprint is measured")

// refreshResident measures the VM's footprint at most every
// vmResidentEvery. On an overloaded node it forks nothing; the last reading
// stands in until it is stale.
func (d *dockerProbe) refreshResident(ctx context.Context, bin string, now time.Time, stressed bool) {
	d.mu.Lock()
	due := d.residentTried.IsZero() || now.Sub(d.residentTried) >= vmResidentEvery
	d.mu.Unlock()
	if !due || stressed {
		return
	}
	_, endpoint := d.resolve(ctx, bin, now)
	v, err := d.measureResident(ctx, endpoint)
	if ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	d.residentTried = now
	if err != nil {
		d.resident, d.residentAt = 0, time.Time{}
	} else {
		d.resident, d.residentAt = v, now
	}
	d.mu.Unlock()
	if err != nil {
		d.p.logOnce("vm-resident", "metrics: the Linux VM's footprint on this host is unknown, so container jobs count against the host's memory in full: %v", err)
	}
}

// measureResident reads the physical footprint of the Lima host agent
// behind a Colima endpoint, rounded down by one displayed unit.
func (d *dockerProbe) measureResident(ctx context.Context, endpoint string) (int64, error) {
	dir := colimaLimaDir(endpoint)
	if dir == "" {
		return 0, errNotColima
	}
	read := d.readFile
	if read == nil {
		read = os.ReadFile
	}
	raw, err := read(filepath.Join(dir, "ha.pid"))
	if err != nil {
		return 0, fmt.Errorf("Lima host agent: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("Lima host agent: bad pid file %s", filepath.Join(dir, "ha.pid"))
	}
	out, err := d.p.run(ctx, 5*time.Second, "/usr/bin/top", "-l", "1", "-pid", strconv.Itoa(pid), "-stats", "pid,mem")
	if err != nil {
		return 0, fmt.Errorf("footprint of pid %d: %w", pid, err)
	}
	v, ok := parseTopMem(string(out), pid)
	if !ok {
		return 0, fmt.Errorf("footprint of pid %d: no MEM figure in top's output", pid)
	}
	return v, nil
}

// colimaLimaDir maps a Colima Docker endpoint to its Lima instance
// directory: unix://~/.colima/<profile>/docker.sock (or ~/.colima/docker.sock
// for the default profile) → ~/.colima/_lima/colima[-<profile>]. "" for any
// other endpoint.
func colimaLimaDir(endpoint string) string {
	path, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok || filepath.Base(path) != "docker.sock" {
		return ""
	}
	dir := filepath.Dir(path)
	var base, profile string
	switch {
	case filepath.Base(dir) == ".colima":
		base, profile = dir, "default"
	case filepath.Base(filepath.Dir(dir)) == ".colima":
		base, profile = filepath.Dir(dir), filepath.Base(dir)
	default:
		return ""
	}
	inst := "colima"
	if profile != "default" {
		inst += "-" + profile
	}
	return filepath.Join(base, "_lima", inst)
}

// parseTopMem finds pid's row in `top -l 1 -pid <pid> -stats pid,mem` and
// returns its MEM (the physical footprint), rounded down by one displayed
// unit so that it never overstates: "40G" → 39 GiB, "2318M+" → 2317 MiB.
func parseTopMem(out string, pid int) (int64, bool) {
	want := strconv.Itoa(pid)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != want {
			continue
		}
		return parseTopSize(f[1])
	}
	return 0, false
}

func parseTopSize(s string) (int64, bool) {
	s = strings.TrimRight(s, "+-*")
	if s == "" {
		return 0, false
	}
	units := map[byte]float64{'B': 1, 'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40}
	unit, ok := units[s[len(s)-1]]
	if !ok {
		return 0, false
	}
	num := s[:len(s)-1]
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	step := 1.0 // one unit of the last displayed digit
	if _, frac, ok := strings.Cut(num, "."); ok {
		step = math.Pow(10, -float64(len(frac)))
	}
	if unit == 1 {
		step = 0 // bytes are exact
	}
	return int64(math.Max(0, n-step) * unit), true
}
