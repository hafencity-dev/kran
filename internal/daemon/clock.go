package daemon

import (
	"context"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

// Clock offset of a peer, measured over its live session: the peer's time
// is compared with the midpoint of our request and its answer, and of
// several samples the one with the shortest round trip wins (its error is at
// most rtt/2). The age of a metrics sample is no measurement: it lags by
// seconds.

type clockBody struct {
	Time time.Time `json:"time"`
}

type clockSample struct {
	offset time.Duration // peer clock minus ours
	rtt    time.Duration
}

const clockSamples = 5

// measureClock takes clockSamples samples on s; ok is false when the peer
// does not answer (an older kran).
func measureClock(ctx context.Context, s transport.Session) (best clockSample, ok bool) {
	for i := 0; i < clockSamples; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return best, ok
			case <-time.After(100 * time.Millisecond):
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var body clockBody
		t0 := time.Now()
		err := peerOneShot(cctx, s, proto.PeerClock, "", nil, &body)
		t1 := time.Now()
		cancel()
		if err != nil || body.Time.IsZero() {
			continue
		}
		rtt := t1.Sub(t0)
		// Wall clocks: Round(0) drops the monotonic reading.
		mid := t0.Round(0).Add(rtt / 2)
		if !ok || rtt < best.rtt {
			best, ok = clockSample{offset: body.Time.Sub(mid), rtt: rtt}, true
		}
	}
	return best, ok
}

// maybeMeasureClock starts a measurement when the last one is ClockEvery old.
func (p *peer) maybeMeasureClock() {
	d := p.m.d
	p.mu.Lock()
	s := p.activeLocked()
	due := s != nil && !p.clockBusy && (p.clockAt.IsZero() || d.now().Sub(p.clockAt) >= d.t.ClockEvery)
	if due {
		p.clockBusy = true
	}
	p.mu.Unlock()
	if !due {
		return
	}
	go func() {
		best, ok := measureClock(p.ctx, s)
		p.mu.Lock()
		p.clockBusy = false
		p.clockAt = d.now()
		if ok {
			p.clock = best
		}
		p.mu.Unlock()
	}()
}
