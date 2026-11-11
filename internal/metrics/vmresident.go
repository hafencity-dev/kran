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

var errNoCmprs = errors.New("top shows no readable compressed-memory (cmprs) figure")

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
	r, err := d.measureResident(ctx, endpoint)
	if ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	d.residentTried = now
	var msg string
	if err != nil {
		d.resident, d.residentAt, d.residentPid, d.residentDir = 0, time.Time{}, 0, ""
		// One log line per change of reason, not per tick.
		if msg = err.Error(); msg == d.residentWhy {
			msg = ""
		} else {
			d.residentWhy = err.Error()
		}
	} else {
		d.resident, d.residentAt, d.residentPid, d.residentDir = r.resident, now, r.pid, r.dir
		d.residentWhy = ""
	}
	d.mu.Unlock()
	if msg != "" {
		d.p.logf("metrics: the Linux VM's footprint on this host is unknown, so container jobs count against the host's memory in full: %s", msg)
	}
}

// measureResident reads the physical footprint of the Lima host agent
// behind a Colima endpoint, rounded down by one displayed unit.
func (d *dockerProbe) measureResident(ctx context.Context, endpoint string) (reading, error) {
	dir := colimaLimaDir(endpoint)
	if dir == "" {
		return reading{}, errNotColima
	}
	read := d.readFile
	if read == nil {
		read = os.ReadFile
	}
	raw, err := read(filepath.Join(dir, "ha.pid"))
	if err != nil {
		return reading{}, fmt.Errorf("Lima host agent: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return reading{}, fmt.Errorf("Lima host agent: bad pid file %s", filepath.Join(dir, "ha.pid"))
	}
	out, err := d.p.run(ctx, 5*time.Second, "/usr/bin/top", "-l", "1", "-pid", strconv.Itoa(pid), "-stats", "pid,mem,cmprs")
	if err != nil {
		return reading{}, fmt.Errorf("footprint of pid %d: %w", pid, err)
	}
	row, ok := parseTopRow(string(out), pid)
	if !ok {
		return reading{}, fmt.Errorf("footprint of pid %d: no MEM figure in top's output", pid)
	}
	if !row.cmprsOK {
		// MEM counts pages macOS compressed or swapped; without the
		// compressed part the resident share is unknown: no credit.
		return reading{}, fmt.Errorf("%w (pid %d): the VM gets no credit", errNoCmprs, pid)
	}
	return reading{resident: max(0, row.mem-row.cmprs), pid: pid, dir: dir}, nil
}

// reading is one valid measurement of the VM and the process it came from.
type reading struct {
	resident int64
	pid      int
	dir      string // the Lima instance directory holding ha.pid
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

// topRow is the pid row of `top -stats pid,mem,cmprs`: mem rounded down and
// cmprs rounded up by one displayed unit, so that mem-cmprs never overstates
// what is resident. cmprsOK is false when the column is absent, "N/A" or
// unparsable.
type topRow struct {
	mem, cmprs int64
	cmprsOK    bool
}

// parseTopRow finds pid's row. Columns are located by the header line when
// there is one (PID MEM CMPRS), else by position.
func parseTopRow(out string, pid int) (topRow, bool) {
	want := strconv.Itoa(pid)
	memCol, cmprsCol := 1, 2
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "PID" {
			memCol, cmprsCol = -1, -1
			for i, h := range f {
				switch strings.ToUpper(strings.TrimRight(h, "+-*")) {
				case "MEM":
					memCol = i
				case "CMPRS":
					cmprsCol = i
				}
			}
			continue
		}
		if len(f) < 2 || f[0] != want || memCol < 0 || memCol >= len(f) {
			continue
		}
		mem, _, ok := parseTopSizeBounds(f[memCol])
		if !ok {
			return topRow{}, false
		}
		row := topRow{mem: mem}
		if cmprsCol >= 0 && cmprsCol < len(f) {
			if _, hi, ok := parseTopSizeBounds(f[cmprsCol]); ok {
				row.cmprs, row.cmprsOK = hi, true
			}
		}
		return row, true
	}
	return topRow{}, false
}

// parseTopSize returns the lower bound of a top size such as "40G" or
// "2318M+": one displayed unit below the figure.
func parseTopSize(s string) (int64, bool) {
	lo, _, ok := parseTopSizeBounds(s)
	return lo, ok
}

// parseTopSizeBounds returns the range a rounded top figure stands for,
// widened by one unit of the last displayed digit on each side: "40G" is
// [39, 41] GiB. Use lo for what must not be overstated, hi for what must not
// be understated.
func parseTopSizeBounds(s string) (lo, hi int64, ok bool) {
	s = strings.TrimRight(s, "+-*")
	if s == "" {
		return 0, 0, false
	}
	units := map[byte]float64{'B': 1, 'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40}
	unit, found := units[s[len(s)-1]]
	if !found {
		return 0, 0, false
	}
	num := s[:len(s)-1]
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, 0, false
	}
	step := 1.0 // one unit of the last displayed digit
	if _, frac, has := strings.Cut(num, "."); has {
		step = math.Pow(10, -float64(len(frac)))
	}
	if unit == 1 {
		step = 0 // bytes are exact
	}
	return int64(math.Max(0, n-step) * unit), int64((n + step) * unit), true
}
