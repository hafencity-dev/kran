package scheduler

import (
	"math"
	"strings"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Contention beyond free CPUs: a node whose memory is thrashing or whose
// disks are saturated runs every job slower, even with cores idle (load 40
// on 20 cores at 30% CPU busy is tasks waiting on reclaim and swap-in).
// The placement score multiplies a job's expected run time on a node by
// slowdown = 1 / (1 - stall), where stall is the share of time a new job
// is expected to wait, combined from independent sources:
// stall = 1 - Π(1 - s_i).
//
// Linux reports PSI "some" avg10 (share of time at least one task was
// stalled). Memory stalls hit any job that allocates, so memory "some"
// counts fully; CPU and I/O "some" count half: with many cores or one busy
// disk, "some task waited" overstates what a new job waits. A full swap
// while memory is tight means every new allocation forces reclaim. macOS
// has no PSI: CPU contention comes from the run queue (load per core),
// memory from the kernel's memory status level.
const (
	psiCPUWeight = 0.5
	psiMemWeight = 1.0
	psiIOWeight  = 0.5
	// Swap at least swapFullShare used while memory is tight (memory
	// PSI ≥ 10 or under 10% available) adds swapFullStall.
	swapFullShare = 0.9
	swapFullStall = 0.2
	// macOS memory status: pressure (100 - memorystatus_level) above
	// macMemFloor counts, scaled to at most macMemStall.
	macMemFloor = 75.0
	macMemStall = 0.6
	// maxStall caps the slowdown at 5x: beyond that the metrics say little
	// more than "very slow".
	maxStall = 0.8
	// slowNote is the slowdown from which reasons mention it.
	slowNote = 1.2
)

// pressure is a node's (or VM's) contention and what causes it.
type pressure struct {
	slow   float64  // ≥ 1, multiplies run time
	causes []string // e.g. "memory pressure 40%", "full swap"
}

func noPressure() pressure { return pressure{slow: 1} }

// hostPressure derives the slowdown from a node's live metrics.
func hostPressure(m proto.NodeMetrics, info proto.NodeInfo) pressure {
	if m.Time.IsZero() {
		return noPressure()
	}
	var b stallBuilder
	psi := m.CPUPressure > 0 || m.MemPressure > 0 || m.IOPressure > 0
	switch {
	case info.OS == "linux" && psi:
		b.add(psiCPUWeight*m.CPUPressure/100, "CPU pressure", m.CPUPressure)
		b.add(psiMemWeight*m.MemPressure/100, "memory pressure", m.MemPressure)
		b.add(psiIOWeight*m.IOPressure/100, "I/O pressure", m.IOPressure)
	case info.OS == "darwin":
		b.addLoad(m.Load1, info.CPUs)
		if m.MemPressure > macMemFloor {
			b.add(macMemStall*(m.MemPressure-macMemFloor)/(100-macMemFloor), "memory pressure", m.MemPressure)
		}
	default:
		// No PSI (old kernel, other OS): the run queue is all there is.
		b.addLoad(m.Load1, info.CPUs)
	}
	// macOS grows swap on demand, so a "full" swap says nothing there.
	if info.OS != "darwin" && m.SwapTotal > 0 && float64(m.SwapUsed) >= swapFullShare*float64(m.SwapTotal) {
		tight := m.MemPressure >= 10 || (info.MemTotal > 0 && m.MemAvailable < info.MemTotal/10)
		if tight {
			b.add(swapFullStall, "full swap", 0)
		}
	}
	return b.result()
}

// vmPressure is the contention a job in the node's Linux VM sees: the
// host's (the VM's cores, memory and disk are the host's) plus the VM's
// own CPU saturation.
func vmPressure(host pressure, busy float64) pressure {
	b := stallBuilder{stall: stallOf(host.slow), causes: append([]string(nil), host.causes...)}
	if busy > 0.95 {
		// The VM reports its busy share only; a saturated VM queues jobs.
		b.add(0.25*(busy-0.95)/0.05, "a busy Linux VM", 0)
	}
	return b.result()
}

type stallBuilder struct {
	stall  float64
	causes []string
}

func (b *stallBuilder) add(s float64, cause string, pct float64) {
	if s <= 0.02 {
		return
	}
	s = math.Min(s, maxStall)
	b.stall = 1 - (1-b.stall)*(1-s)
	if s >= 0.05 {
		if pct > 0 {
			cause += " " + fmtNum(math.Round(pct)) + "%"
		}
		b.causes = append(b.causes, cause)
	}
}

// addLoad counts run-queue oversubscription: with load L on n cores a
// CPU-bound job gets n/L of a core.
func (b *stallBuilder) addLoad(load float64, cpus int) {
	if cpus <= 0 || load <= float64(cpus) {
		return
	}
	b.add(1-float64(cpus)/load, "load "+fmtNum(math.Round(load*10)/10)+" on "+fmtNum(float64(cpus))+" cores", 0)
}

func (b *stallBuilder) result() pressure {
	s := math.Min(b.stall, maxStall)
	return pressure{slow: 1 / (1 - s), causes: b.causes}
}

func stallOf(slow float64) float64 {
	if slow <= 1 {
		return 0
	}
	return 1 - 1/slow
}

// note is a reason fragment like "slowed ~2.6x by memory pressure 40%, full
// swap", or "" when the slowdown is small.
func (p pressure) note() string {
	if p.slow < slowNote {
		return ""
	}
	n := "slowed ~" + fmtNum(math.Round(p.slow*10)/10) + "x"
	if len(p.causes) > 0 {
		n += " by " + strings.Join(p.causes, ", ")
	}
	return n
}
