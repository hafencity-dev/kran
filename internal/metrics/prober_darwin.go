package metrics

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
)

// On macOS everything comes from sysctl except CPU busy (host_statistics
// needs cgo; a long-running `top -l 0` streams it cheaply instead) and
// network counters (`netstat -ib`).

const topStale = 10 * time.Second

type darwinPlatform struct {
	p *prober

	mu       sync.Mutex
	ncpu     int
	memTotal int64
	busy     float64
	busyAt   time.Time
	havePrev bool
	prevNet  netCounters
	prevAt   time.Time
}

func newPlatform(p *prober) platform { return &darwinPlatform{p: p} }

func (d *darwinPlatform) staticInfo(ctx context.Context, info *proto.NodeInfo) {
	p := d.p
	if n, err := unix.SysctlUint32("hw.ncpu"); err == nil && n > 0 {
		info.CPUs = int(n)
	}
	if n, err := unix.SysctlUint32("hw.perflevel0.logicalcpu"); err == nil && n > 0 {
		info.PerfCPUs = int(n)
	}
	if n, err := unix.SysctlUint64("hw.memsize"); err == nil {
		info.MemTotal = int64(n)
	} else {
		p.logOnce("memsize", "metrics: hw.memsize unavailable: %v", err)
	}
	if s, err := unix.Sysctl("machdep.cpu.brand_string"); err == nil {
		info.CPUModel = strings.TrimSpace(s)
	}
	ver := "macOS"
	if s, err := unix.Sysctl("kern.osproductversion"); err == nil {
		ver += " " + s
	}
	if s, err := unix.Sysctl("kern.osrelease"); err == nil {
		ver += " (Darwin " + s + ")"
	}
	info.OSVersion = ver
	if runtime.GOARCH == "arm64" {
		model := info.CPUModel
		if model == "" {
			model = "Apple Silicon"
		}
		info.GPUs = append(info.GPUs, proto.GPU{Vendor: "apple", Model: model, Unified: true})
	}
	d.mu.Lock()
	d.ncpu, d.memTotal = info.CPUs, info.MemTotal
	d.mu.Unlock()
}

func (d *darwinPlatform) totals() (int, int64) {
	d.mu.Lock()
	n, m := d.ncpu, d.memTotal
	d.mu.Unlock()
	if n == 0 {
		if v, err := unix.SysctlUint32("hw.ncpu"); err == nil {
			n = int(v)
		}
		if v, err := unix.SysctlUint64("hw.memsize"); err == nil {
			m = int64(v)
		}
		d.mu.Lock()
		d.ncpu, d.memTotal = n, m
		d.mu.Unlock()
	}
	return n, m
}

func (d *darwinPlatform) sample(ctx context.Context, m *proto.NodeMetrics, now time.Time) {
	p := d.p
	ncpu, total := d.totals()
	if raw, err := unix.SysctlRaw("vm.loadavg"); err == nil {
		m.Load1 = decodeLoadavg(raw)
	}
	if avail, err := sysctlAvailable(); err == nil {
		m.MemAvailable = min(avail, total)
	} else if avail, err2 := d.vmStatAvailable(ctx); err2 == nil {
		m.MemAvailable = min(avail, total)
	} else {
		p.logOnce("mem", "metrics: available memory unknown: %v; %v", err, err2)
	}
	m.MemUsed = max(0, total-m.MemAvailable)
	if lvl, err := unix.SysctlUint32("kern.memorystatus_level"); err == nil && lvl <= 100 {
		m.MemPressure = float64(100 - lvl)
	}
	if raw, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(raw) >= 24 {
		m.SwapTotal = int64(binary.LittleEndian.Uint64(raw[0:]))
		m.SwapUsed = int64(binary.LittleEndian.Uint64(raw[16:]))
	}
	var net netCounters
	out, netErr := p.run(ctx, 2*time.Second, "/usr/sbin/netstat", "-ib")
	if netErr == nil {
		net = parseNetstatIB(string(out))
	} else {
		p.logOnce("netstat", "metrics: netstat failed, network rates not reported: %v", netErr)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.busyAt.IsZero() && now.Sub(d.busyAt) <= topStale {
		m.CPUBusy = d.busy
	} else if ncpu > 0 {
		// top not running (yet): the load average is a usable stand-in.
		m.CPUBusy = clamp01(m.Load1 / float64(ncpu))
	}
	if netErr == nil {
		if d.havePrev {
			if dt := now.Sub(d.prevAt).Seconds(); dt > 0 {
				m.NetRxBps = counterRate(d.prevNet.rx, net.rx, dt)
				m.NetTxBps = counterRate(d.prevNet.tx, net.tx, dt)
			}
		}
		d.prevNet, d.prevAt, d.havePrev = net, now, true
	}
}

// decodeLoadavg decodes struct loadavg { fixpt_t ldavg[3]; long fscale; }.
func decodeLoadavg(raw []byte) float64 {
	if len(raw) < 24 {
		return 0
	}
	l1 := binary.LittleEndian.Uint32(raw[0:])
	scale := binary.LittleEndian.Uint64(raw[16:])
	if scale == 0 {
		return 0
	}
	return float64(l1) / float64(scale)
}

// sysctlAvailable is free + file-backed + purgeable pages: what can be
// handed to a new process without swapping.
func sysctlAvailable() (int64, error) {
	var sum int64
	for _, name := range []string{"vm.page_free_count", "vm.page_pageable_external_count", "vm.page_purgeable_count"} {
		v, err := unix.SysctlUint32(name)
		if err != nil {
			return 0, fmt.Errorf("sysctl %s: %w", name, err)
		}
		sum += int64(v)
	}
	return sum * int64(os.Getpagesize()), nil
}

func (d *darwinPlatform) vmStatAvailable(ctx context.Context) (int64, error) {
	out, err := d.p.run(ctx, 2*time.Second, "/usr/bin/vm_stat")
	if err != nil {
		return 0, err
	}
	ps, pages, err := parseVMStat(string(out))
	if err != nil {
		return 0, err
	}
	return vmStatAvailable(ps, pages), nil
}

func (d *darwinPlatform) start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.topLoop(ctx)
	}()
}

// topLoop keeps one `top -l 0 -s 2 -n 0` running and records its CPU usage
// line (the first one covers time since boot and is skipped).
func (d *darwinPlatform) topLoop(ctx context.Context) {
	for {
		err := d.runTop(ctx)
		if ctx.Err() != nil {
			return
		}
		d.p.logOnce("top", "metrics: top exited (%v); CPU busy falls back to load average", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (d *darwinPlatform) runTop(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/top", "-l", "0", "-s", "2", "-n", "0")
	cmd.WaitDelay = time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	first := true
	for sc.Scan() {
		v, ok := parseTopCPU(sc.Text())
		if !ok {
			continue
		}
		if first {
			first = false
			continue
		}
		d.mu.Lock()
		d.busy, d.busyAt = v, d.p.now()
		d.mu.Unlock()
	}
	err = cmd.Wait()
	if err == nil {
		err = errors.New("top ended")
	}
	return err
}

// pgidUsage sums a process group via ps (no cgo on macOS for proc_pidinfo).
func (d *darwinPlatform) pgidUsage(ctx context.Context, pgid int, now time.Time) (proto.Resources, error) {
	out, err := d.p.run(ctx, 3*time.Second, "/bin/ps", "-A", "-o", "pgid=,rss=,time=,etime=")
	if err != nil {
		return proto.Resources{}, fmt.Errorf("process group %d: %w", pgid, err)
	}
	rss, cpu, age, n := psGroupUsage(string(out), pgid)
	if n == 0 {
		return proto.Resources{}, fmt.Errorf("process group %d: no processes", pgid)
	}
	return proto.Resources{CPUs: d.p.rates.cores(fmt.Sprintf("pg:%d", pgid), cpu, now, age), Mem: rss}, nil
}

// pinBench is a no-op: macOS offers no affinity API; the benchmark keeps
// its fastest slice, which lands on a P-core.
func (d *darwinPlatform) pinBench() {}

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
		return time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
	}
	return time.Time{}
}
