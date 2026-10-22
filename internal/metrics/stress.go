package metrics

import "github.com/hafencity-dev/kran/internal/proto"

// Stressed reports whether a node is so overloaded that its processes,
// the kran daemon included, may answer late or have forks killed by
// timeouts: the run queue far beyond the cores, memory or I/O stalls, or
// swap exhausted. Peers then tolerate longer silences on the link, and the
// prober stops spawning helper processes that would only add to the load.
func Stressed(m proto.NodeMetrics, info proto.NodeInfo) bool {
	if m.Time.IsZero() {
		return false
	}
	if info.CPUs > 0 && m.Load1 >= 1.5*float64(info.CPUs) {
		return true
	}
	if m.CPUPressure >= 80 || m.IOPressure >= 40 {
		return true
	}
	// Linux reports PSI (share of time stalled on memory); macOS reports the
	// memorystatus level as 0..100, where 60–70 is everyday use.
	memLimit := 20.0
	if info.OS == "darwin" {
		memLimit = 90
	}
	if m.MemPressure >= memLimit {
		return true
	}
	return m.SwapTotal > 0 && m.SwapUsed >= m.SwapTotal*9/10 && m.MemAvailable < 512<<20
}
