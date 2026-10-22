package metrics

// Pure parsers for the text formats metrics reads. They live in an untagged
// file so their tests run on every OS against recorded fixtures.

import (
	"bufio"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// cpuTimes are the aggregate jiffies of the first /proc/stat line.
type cpuTimes struct {
	total, idle uint64
}

// parseProcStat reads the aggregate "cpu" line of /proc/stat.
func parseProcStat(s string) (cpuTimes, error) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t cpuTimes
		for i, v := range f[1:] {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("parse /proc/stat field %d: %w", i+1, err)
			}
			// guest and guest_nice (fields 9, 10) are already included in
			// user and nice; counting them twice would inflate busy time.
			if i >= 8 {
				continue
			}
			t.total += n
			if i == 3 || i == 4 { // idle, iowait
				t.idle += n
			}
		}
		return t, nil
	}
	return cpuTimes{}, errors.New("parse /proc/stat: no cpu line")
}

// busyBetween returns the busy fraction (0..1) between two samples.
func busyBetween(prev, cur cpuTimes) (float64, bool) {
	if cur.total <= prev.total {
		return 0, false
	}
	dt := float64(cur.total - prev.total)
	di := float64(0)
	if cur.idle > prev.idle {
		di = float64(cur.idle - prev.idle)
	}
	return clamp01(1 - di/dt), true
}

// parseMeminfo returns /proc/meminfo values in bytes keyed by field name.
func parseMeminfo(s string) map[string]int64 {
	out := make(map[string]int64, 16)
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && f[1] == "kB" {
			n *= 1024
		}
		out[strings.TrimSpace(k)] = n
	}
	return out
}

// parseLoadavg returns the 1-minute load average of /proc/loadavg.
func parseLoadavg(s string) (float64, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, errors.New("parse loadavg: empty")
	}
	return strconv.ParseFloat(f[0], 64)
}

// parsePSI returns "some avg10" of a /proc/pressure/* file.
func parsePSI(s string) (float64, error) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "some" {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "avg10="); ok {
				return strconv.ParseFloat(v, 64)
			}
		}
	}
	return 0, errors.New("parse psi: no some avg10")
}

// netCounters are cumulative bytes over the interfaces we count.
type netCounters struct {
	rx, tx uint64
}

// skipInterface reports interfaces whose traffic is local or duplicates a
// physical interface (bridges, veths, tunnels).
func skipInterface(name string) bool {
	for _, p := range []string{"lo", "veth", "docker", "br-", "virbr", "cni", "flannel", "tailscale", "utun", "gif", "stf", "awdl", "llw", "anpi", "bridge", "vmnet", "ap1"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func counterRate(prev, cur uint64, dt float64) float64 {
	if cur < prev { // counter reset (interface went away)
		return 0
	}
	return float64(cur-prev) / dt
}

// parseNetDev sums rx/tx bytes of /proc/net/dev.
func parseNetDev(s string) netCounters {
	var c netCounters
	for _, line := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if skipInterface(name) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		c.rx += rx
		c.tx += tx
	}
	return c
}

// parseNetstatIB sums Ibytes/Obytes of the <Link#n> rows of macOS
// `netstat -ib`. The Address column is empty for some interfaces, so the
// counters are read from the right.
func parseNetstatIB(s string) netCounters {
	var c netCounters
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || !strings.HasPrefix(f[2], "<Link#") {
			continue
		}
		name := f[0]
		if strings.HasSuffix(name, "*") || skipInterface(name) {
			continue
		}
		n := len(f)
		rx, err1 := strconv.ParseUint(f[n-5], 10, 64)
		tx, err2 := strconv.ParseUint(f[n-2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		c.rx += rx
		c.tx += tx
	}
	return c
}

// parseCPUList parses "0-3,8,10-11" into the CPU indices.
func parseCPUList(s string) ([]int, error) {
	var out []int
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("parse cpu list: empty")
	}
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("parse cpu list %q: %w", s, err)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("parse cpu list %q: bad range", s)
			}
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	return out, nil
}

// cpuInfoEntry is one processor block of /proc/cpuinfo.
type cpuInfoEntry struct {
	processor   int
	modelName   string
	implementer uint64
	part        uint64
}

func parseCPUInfo(s string) []cpuInfoEntry {
	var out []cpuInfoEntry
	cur := cpuInfoEntry{processor: -1}
	flush := func() {
		if cur.processor >= 0 {
			out = append(out, cur)
		}
		cur = cpuInfoEntry{processor: -1}
	}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			flush()
			n, err := strconv.Atoi(v)
			if err == nil {
				cur.processor = n
			}
		case "model name":
			cur.modelName = v
		case "CPU implementer":
			cur.implementer, _ = strconv.ParseUint(v, 0, 64)
		case "CPU part":
			cur.part, _ = strconv.ParseUint(v, 0, 64)
		}
	}
	flush()
	return out
}

// armCoreName names an ARM core from its MIDR implementer and part number
// (the subset lscpu knows that kran nodes plausibly run on).
func armCoreName(implementer, part uint64) string {
	switch implementer {
	case 0x41:
		if n, ok := armParts[part]; ok {
			return n
		}
		return fmt.Sprintf("ARM core 0x%x", part)
	case 0x61:
		return "Apple"
	case 0xc0:
		switch part {
		case 0xac3:
			return "Ampere-1"
		case 0xac4:
			return "Ampere-1a"
		}
		return "Ampere"
	case 0x4e:
		if part == 0x004 {
			return "NVIDIA Carmel"
		}
		return "NVIDIA"
	case 0x51:
		return "Qualcomm"
	}
	return fmt.Sprintf("CPU 0x%x/0x%x", implementer, part)
}

var armParts = map[uint64]string{
	0xd03: "Cortex-A53", 0xd04: "Cortex-A35", 0xd05: "Cortex-A55", 0xd07: "Cortex-A57",
	0xd08: "Cortex-A72", 0xd09: "Cortex-A73", 0xd0a: "Cortex-A75", 0xd0b: "Cortex-A76",
	0xd0c: "Neoverse-N1", 0xd0d: "Cortex-A77", 0xd40: "Neoverse-V1", 0xd41: "Cortex-A78",
	0xd44: "Cortex-X1", 0xd46: "Cortex-A510", 0xd47: "Cortex-A710", 0xd48: "Cortex-X2",
	0xd49: "Neoverse-N2", 0xd4d: "Cortex-A715", 0xd4e: "Cortex-X3", 0xd4f: "Neoverse-V2",
	0xd80: "Cortex-A520", 0xd81: "Cortex-A720", 0xd82: "Cortex-X4", 0xd84: "Neoverse-V3",
	0xd85: "Cortex-X925", 0xd87: "Cortex-A725", 0xd8e: "Neoverse-N3",
}

// perfSet returns the CPUs within 15% of the fastest; a homogeneous machine
// returns all of them.
func perfSet(caps map[int]int64) []int {
	var top int64
	for _, v := range caps {
		top = max(top, v)
	}
	var out []int
	for c, v := range caps {
		if float64(v) >= 0.85*float64(top) {
			out = append(out, c)
		}
	}
	sort.Ints(out)
	return out
}

// cpuModel names the CPU like lscpu: x86 "model name", or ARM core names
// from MIDR parts, fastest cluster first ("Cortex-X925 ×10 + Cortex-A725 ×10").
func cpuModel(entries []cpuInfoEntry, caps map[int]int64) string {
	for _, e := range entries {
		if e.modelName != "" {
			return strings.Join(strings.Fields(e.modelName), " ")
		}
	}
	type cluster struct {
		name  string
		count int
		cap   int64
	}
	var order []*cluster
	by := map[string]*cluster{}
	for _, e := range entries {
		if e.implementer == 0 {
			continue
		}
		n := armCoreName(e.implementer, e.part)
		c := by[n]
		if c == nil {
			c = &cluster{name: n}
			by[n] = c
			order = append(order, c)
		}
		c.count++
		c.cap = max(c.cap, caps[e.processor])
	}
	if len(order) == 0 {
		return ""
	}
	if len(order) == 1 {
		return order[0].name
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].cap > order[j].cap })
	parts := make([]string, len(order))
	for i, c := range order {
		parts[i] = fmt.Sprintf("%s ×%d", c.name, c.count)
	}
	return strings.Join(parts, " + ")
}

// parseOSRelease returns PRETTY_NAME (or NAME VERSION) of /etc/os-release.
func parseOSRelease(s string) string {
	vals := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		vals[k] = strings.Trim(v, `"'`)
	}
	if v := vals["PRETTY_NAME"]; v != "" {
		return v
	}
	return strings.TrimSpace(vals["NAME"] + " " + vals["VERSION"])
}

// cgroupCPUUsec returns usage_usec of a cgroup v2 cpu.stat file.
func cgroupCPUUsec(s string) (uint64, error) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "usage_usec" {
			return strconv.ParseUint(f[1], 10, 64)
		}
	}
	return 0, errors.New("parse cpu.stat: no usage_usec")
}

// gpuReading is one row of nvidia-smi --query-gpu output.
type gpuReading struct {
	name    string
	memTot  int64 // bytes; 0 when not reported (unified memory)
	memUsed int64
	util    float64 // 0..1
	unified bool
}

// nvidiaQuery is the field list parseNvidiaCSV expects.
const nvidiaQuery = "name,memory.total,memory.used,utilization.gpu"

// parseNvidiaCSV parses `nvidia-smi --query-gpu=<nvidiaQuery>
// --format=csv,noheader,nounits`. The GB10 (and other unified-memory parts)
// reports "[N/A]" for memory: that is treated as unified, not an error.
func parseNvidiaCSV(s string) ([]gpuReading, error) {
	var out []gpuReading
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 4 {
			return nil, fmt.Errorf("parse nvidia-smi line %q: want 4 fields", line)
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		g := gpuReading{name: f[0]}
		if mib, ok := nvidiaNum(f[1]); ok && mib > 0 {
			g.memTot = int64(mib * (1 << 20))
		} else {
			g.unified = true
		}
		if mib, ok := nvidiaNum(f[2]); ok {
			g.memUsed = int64(mib * (1 << 20))
		}
		if u, ok := nvidiaNum(f[3]); ok {
			g.util = clamp01(u / 100)
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil, errors.New("parse nvidia-smi: no GPUs listed")
	}
	return out, nil
}

// nvidiaNum parses a numeric field; "[N/A]", "N/A" and "[Not Supported]"
// are absent values.
func nvidiaNum(s string) (float64, bool) {
	s = strings.TrimSuffix(strings.TrimSuffix(s, "MiB"), "%")
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v, err == nil
}

// parseVMStat returns page counts of macOS vm_stat keyed by label, plus the
// page size from its header.
func parseVMStat(s string) (pageSize int64, pages map[string]int64, err error) {
	pages = map[string]int64{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "page size of "); i >= 0 {
			f := strings.Fields(line[i+len("page size of "):])
			if len(f) > 0 {
				pageSize, _ = strconv.ParseInt(f[0], 10, 64)
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, perr := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
		if perr != nil {
			continue
		}
		pages[strings.Trim(strings.TrimSpace(k), `"`)] = n
	}
	if pageSize <= 0 {
		return 0, nil, errors.New("parse vm_stat: no page size")
	}
	return pageSize, pages, nil
}

// vmStatAvailable estimates reclaimable memory like Activity Monitor:
// free + inactive + speculative + purgeable pages.
func vmStatAvailable(pageSize int64, pages map[string]int64) int64 {
	n := pages["Pages free"] + pages["Pages inactive"] + pages["Pages speculative"] + pages["Pages purgeable"]
	return n * pageSize
}

// parseTopCPU returns the busy fraction from a macOS top line
// "CPU usage: 22.98% user, 14.90% sys, 62.11% idle".
func parseTopCPU(line string) (float64, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "CPU usage:")
	if !ok {
		return 0, false
	}
	for _, part := range strings.Split(rest, ",") {
		f := strings.Fields(part)
		if len(f) == 2 && f[1] == "idle" {
			v, err := strconv.ParseFloat(strings.TrimSuffix(f[0], "%"), 64)
			if err != nil {
				return 0, false
			}
			return clamp01(1 - v/100), true
		}
	}
	return 0, false
}

// parseClock parses ps time/etime values: "[[dd-]hh:]mm:ss[.ff]"; minutes
// may exceed 59 (macOS `time` prints "123:45.67").
func parseClock(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("parse clock: empty")
	}
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, fmt.Errorf("parse clock %q: %w", s, err)
		}
		days, s = float64(n), rest
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("parse clock %q: too many fields", s)
	}
	total := 0.0
	mult := 1.0
	for i := len(parts) - 1; i >= 0; i-- {
		v, err := strconv.ParseFloat(parts[i], 64)
		if err != nil {
			return 0, fmt.Errorf("parse clock %q: %w", s, err)
		}
		total += v * mult
		mult *= 60
	}
	return total + days*86400, nil
}

// psGroupUsage sums `ps -A -o pgid=,rss=,time=,etime=` rows of one process
// group: RSS bytes, CPU seconds and the group's age (oldest member).
func psGroupUsage(s string, pgid int) (rss int64, cpu, age float64, n int) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		g, err := strconv.Atoi(f[0])
		if err != nil || g != pgid {
			continue
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		t, err1 := parseClock(f[2])
		e, err2 := parseClock(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		rss += kb * 1024
		cpu += t
		age = max(age, e)
		n++
	}
	return rss, cpu, age, n
}

// parseSize parses docker's human sizes: "71.45MiB", "1.825GiB", "0B",
// "512kB", "1.2MB" (binary and decimal units).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i <= 0 {
		if i < 0 {
			v, err := strconv.ParseFloat(s, 64)
			return int64(v), err
		}
		return 0, fmt.Errorf("parse size %q", s)
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	mult, ok := sizeUnits[strings.ToLower(strings.TrimSpace(s[i:]))]
	if !ok {
		return 0, fmt.Errorf("parse size %q: unknown unit", s)
	}
	return int64(v * mult), nil
}

var sizeUnits = map[string]float64{
	"b":   1,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
	"k": 1 << 10, "m": 1 << 20, "g": 1 << 30,
}

// dockerStat is one container row of `docker stats --no-stream --format
// '{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}'`.
type dockerStat struct {
	name string
	cpus float64 // cores (100% = one core)
	mem  int64
}

func parseDockerStats(s string) []dockerStat {
	var out []dockerStat
	for _, line := range strings.Split(s, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) < 3 {
			continue
		}
		st := dockerStat{name: f[0]}
		if v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(f[1]), "%"), 64); err == nil {
			st.cpus = v / 100
		}
		used, _, _ := strings.Cut(f[2], "/")
		if v, err := parseSize(used); err == nil {
			st.mem = v
		}
		out = append(out, st)
	}
	return out
}

// parseDockerInfo parses `docker info --format '{{.NCPU}} {{.MemTotal}}'`.
func parseDockerInfo(s string) (int, int64, error) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("parse docker info %q: want \"NCPU MemTotal\"", strings.TrimSpace(s))
	}
	n, err1 := strconv.Atoi(f[0])
	m, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil || n <= 0 || m <= 0 {
		return 0, 0, fmt.Errorf("parse docker info %q: bad numbers", strings.TrimSpace(s))
	}
	return n, m, nil
}

// procStatGroup is what we need from /proc/<pid>/stat.
type procStatGroup struct {
	pgrp      int
	ticks     uint64 // utime + stime
	startTick uint64
	rssPages  int64
}

// parseProcPIDStat parses /proc/<pid>/stat; comm may contain spaces and
// parentheses, so fields are counted after the last ')'.
func parseProcPIDStat(s string) (procStatGroup, error) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procStatGroup{}, errors.New("parse stat: no comm")
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); field N is f[N-3].
	if len(f) < 22 {
		return procStatGroup{}, errors.New("parse stat: short")
	}
	pg, err := strconv.Atoi(f[2])
	if err != nil {
		return procStatGroup{}, err
	}
	ut, _ := strconv.ParseUint(f[11], 10, 64)
	st, _ := strconv.ParseUint(f[12], 10, 64)
	start, _ := strconv.ParseUint(f[19], 10, 64)
	rss, _ := strconv.ParseInt(f[21], 10, 64)
	return procStatGroup{pgrp: pg, ticks: ut + st, startTick: start, rssPages: rss}, nil
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
