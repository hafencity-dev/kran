package metrics

import (
	"sync"
	"time"
)

// stressedProbeEvery is how often a probe that forks a helper process
// (nvidia-smi, the docker CLI) still runs while the node is overloaded:
// there the fork is likely to be killed by its timeout and only adds load,
// so the last reading stands in between.
const stressedProbeEvery = 30 * time.Second

// probeGate paces a probe that forks a helper: after a failure (on a loaded
// machine typically a timeout kill) the next try waits twice as long as the
// last, up to max; a success resets that; under stress the probe runs at
// most every stressedProbeEvery.
type probeGate struct {
	mu       sync.Mutex
	min, max time.Duration
	wait     time.Duration // current backoff (0 = none)
	next     time.Time     // no run before this
	lastRun  time.Time
}

func newProbeGate(min, max time.Duration) *probeGate { return &probeGate{min: min, max: max} }

func (g *probeGate) allow(now time.Time, stressed bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.next) {
		return false
	}
	return !stressed || g.lastRun.IsZero() || now.Sub(g.lastRun) >= stressedProbeEvery
}

func (g *probeGate) done(now time.Time, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastRun = now
	if ok {
		g.wait, g.next = 0, time.Time{}
		return
	}
	g.wait = min(max(2*g.wait, g.min), g.max)
	g.next = now.Add(g.wait)
}
