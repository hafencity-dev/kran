package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
)

// userHZ is the kernel's USER_HZ for /proc tick counters (100 on every
// Linux architecture kran targets).
const userHZ = 100

type linuxPlatform struct {
	p *prober

	mu       sync.Mutex
	havePrev bool
	prevCPU  cpuTimes
	prevNet  netCounters
	prevAt   time.Time
	perfCPUs []int // performance CPUs when the machine is hybrid
}

func newPlatform(p *prober) platform { return &linuxPlatform{p: p} }

func (l *linuxPlatform) staticInfo(ctx context.Context, info *proto.NodeInfo) {
	p := l.p
	info.OSVersion = l.osVersion()
	cpus := l.onlineCPUs()
	if len(cpus) > 0 {
		info.CPUs = len(cpus)
	}
	caps := l.capacities(cpus)
	perf := perfSet(caps)
	info.PerfCPUs = len(perf)
	if info.PerfCPUs == 0 {
		info.PerfCPUs = info.CPUs
	}
	l.mu.Lock()
	if len(perf) < len(cpus) {
		l.perfCPUs = perf
	} else {
		l.perfCPUs = nil
	}
	l.mu.Unlock()
	if s, err := p.readFile(p.procDir, "cpuinfo"); err == nil {
		info.CPUModel = cpuModel(parseCPUInfo(s), caps)
	}
	if s, err := p.readFile(p.procDir, "meminfo"); err == nil {
		info.MemTotal = parseMeminfo(s)["MemTotal"]
	} else {
		p.logOnce("meminfo", "metrics: cannot read meminfo: %v", err)
	}
}

func (l *linuxPlatform) osVersion() string {
	p := l.p
	name := ""
	if s, err := p.readFile(p.etcDir, "os-release"); err == nil {
		name = parseOSRelease(s)
	}
	var u unix.Utsname
	if err := unix.Uname(&u); err == nil {
		rel := unix.ByteSliceToString(u.Release[:])
		if name == "" {
			return "Linux " + rel
		}
		return name + " (kernel " + rel + ")"
	}
	if name == "" {
		return "Linux"
	}
	return name
}

func (l *linuxPlatform) onlineCPUs() []int {
	s, err := l.p.readFile(l.p.sysDir, "devices/system/cpu/online")
	if err != nil {
		return nil
	}
	cpus, err := parseCPUList(s)
	if err != nil {
		return nil
	}
	return cpus
}

// capacities returns a relative speed per CPU: cpu_capacity (arm64
// big.LITTLE, set from firmware) or else the max frequency.
func (l *linuxPlatform) capacities(cpus []int) map[int]int64 {
	out := map[int]int64{}
	for _, file := range []string{"cpu_capacity", "cpufreq/cpuinfo_max_freq"} {
		for _, c := range cpus {
			s, err := l.p.readFile(l.p.sysDir, fmt.Sprintf("devices/system/cpu/cpu%d/%s", c, file))
			if err != nil {
				continue
			}
			if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && v > 0 {
				out[c] = v
			}
		}
		if len(out) == len(cpus) && len(out) > 0 {
			return out
		}
		out = map[int]int64{}
	}
	return out
}

func (l *linuxPlatform) sample(ctx context.Context, m *proto.NodeMetrics, now time.Time) {
	p := l.p
	if s, err := p.readFile(p.procDir, "loadavg"); err == nil {
		m.Load1, _ = parseLoadavg(s)
	}
	if s, err := p.readFile(p.procDir, "meminfo"); err == nil {
		mi := parseMeminfo(s)
		avail, ok := mi["MemAvailable"]
		if !ok { // kernels before 3.14
			avail = mi["MemFree"] + mi["Buffers"] + mi["Cached"]
		}
		m.MemAvailable = avail
		m.MemUsed = max(0, mi["MemTotal"]-avail)
		m.SwapTotal = mi["SwapTotal"]
		m.SwapUsed = max(0, mi["SwapTotal"]-mi["SwapFree"])
	} else {
		p.logOnce("meminfo-sample", "metrics: cannot read meminfo: %v", err)
	}
	for _, psi := range []struct {
		file string
		dst  *float64
	}{{"pressure/cpu", &m.CPUPressure}, {"pressure/memory", &m.MemPressure}, {"pressure/io", &m.IOPressure}} {
		if s, err := p.readFile(p.procDir, psi.file); err == nil {
			*psi.dst, _ = parsePSI(s)
		}
	}
	var cpu cpuTimes
	var cpuOK bool
	if s, err := p.readFile(p.procDir, "stat"); err == nil {
		cpu, err = parseProcStat(s)
		cpuOK = err == nil
	}
	var net netCounters
	s, netErr := p.readFile(p.procDir, "net/dev")
	if netErr == nil {
		net = parseNetDev(s)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.havePrev {
		if cpuOK {
			if busy, ok := busyBetween(l.prevCPU, cpu); ok {
				m.CPUBusy = busy
			}
		}
		if dt := now.Sub(l.prevAt).Seconds(); dt > 0 && netErr == nil {
			m.NetRxBps = counterRate(l.prevNet.rx, net.rx, dt)
			m.NetTxBps = counterRate(l.prevNet.tx, net.tx, dt)
		}
	}
	l.prevCPU, l.prevNet, l.prevAt, l.havePrev = cpu, net, now, cpuOK
}

func (l *linuxPlatform) start(ctx context.Context, wg *sync.WaitGroup) {}

// pgidUsage sums a process group from /proc (procps `ps -g` selects
// sessions, not groups, so ps is not used on Linux).
func (l *linuxPlatform) pgidUsage(ctx context.Context, pgid int, now time.Time) (proto.Resources, error) {
	p := l.p
	ents, err := os.ReadDir(p.procDir)
	if err != nil {
		return proto.Resources{}, fmt.Errorf("process group %d: %w", pgid, err)
	}
	up, err := p.readFile(p.procDir, "uptime")
	if err != nil {
		return proto.Resources{}, fmt.Errorf("process group %d: %w", pgid, err)
	}
	uptime, _ := parseLoadavg(up) // first field, same format
	page := int64(os.Getpagesize())
	var rss int64
	var ticks uint64
	var oldest = uint64(1<<63 - 1)
	n := 0
	for _, e := range ents {
		name := e.Name()
		if name[0] < '0' || name[0] > '9' {
			continue
		}
		s, err := p.readFile(p.procDir, name+"/stat")
		if err != nil {
			continue // exited meanwhile
		}
		st, err := parseProcPIDStat(s)
		if err != nil || st.pgrp != pgid {
			continue
		}
		rss += st.rssPages * page
		ticks += st.ticks
		oldest = min(oldest, st.startTick)
		n++
	}
	if n == 0 {
		return proto.Resources{}, fmt.Errorf("process group %d: no processes", pgid)
	}
	cpu := float64(ticks) / userHZ
	age := uptime - float64(oldest)/userHZ
	return proto.Resources{CPUs: p.rates.cores(fmt.Sprintf("pg:%d", pgid), cpu, now, age), Mem: rss}, nil
}

// pinBench limits the benchmark thread to the performance cores so a
// hybrid machine reports its fast cores.
func (l *linuxPlatform) pinBench() {
	l.mu.Lock()
	perf := l.perfCPUs
	l.mu.Unlock()
	if len(perf) == 0 {
		return
	}
	var set unix.CPUSet
	for _, c := range perf {
		set.Set(c)
	}
	if err := unix.SchedSetaffinity(0, &set); err != nil {
		l.p.logOnce("affinity", "metrics: benchmark runs unpinned: %v", err)
	}
}

func statfs(path string) (total, free int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return 0, 0, fmt.Errorf("statfs %s: %w", path, os.ErrNotExist)
		}
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	bs := int64(st.Bsize)
	return int64(st.Blocks) * bs, int64(st.Bavail) * bs, nil
}

func ctimeOf(fi os.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
	}
	return time.Time{}
}
