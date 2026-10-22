package daemon

import (
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// bus fans events out to SSE subscribers. Publishing never blocks: a
// subscriber whose buffer is full is dropped (its channel closed) and must
// reconnect, receiving a fresh state event. The bus lock is a leaf lock:
// callers may publish while holding their own locks.
type bus struct {
	mu          sync.Mutex
	seq         uint64
	subs        map[*subscriber]struct{}
	lastMetrics map[string]time.Time
	now         func() time.Time
	bufSize     int
	metricsGap  time.Duration
}

type subscriber struct {
	ch      chan proto.Event
	dropped bool
}

func newBus(now func() time.Time) *bus {
	return &bus{
		subs:        map[*subscriber]struct{}{},
		lastMetrics: map[string]time.Time{},
		now:         now,
		bufSize:     256,
		metricsGap:  time.Second,
	}
}

func (b *bus) publish(t proto.EventType, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishLocked(t, data)
}

func (b *bus) publishLocked(t proto.EventType, data any) {
	b.seq++
	ev := proto.Event{Type: t, Seq: b.seq, Time: b.now().UTC(), Data: data}
	for s := range b.subs {
		select {
		case s.ch <- ev:
		default:
			s.dropped = true
			close(s.ch)
			delete(b.subs, s)
		}
	}
}

// publishMetrics publishes node.metrics at most once per second per node.
func (b *bus) publishMetrics(name string, m proto.NodeMetrics) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if last, ok := b.lastMetrics[name]; ok && now.Sub(last) < b.metricsGap {
		return
	}
	b.lastMetrics[name] = now
	b.publishLocked(proto.EventNodeMetrics, proto.NodeMetricsEvent{Name: name, Metrics: m})
}

// subscribe registers a subscriber and returns the sequence number of the
// last event it will not see. The caller then takes a state snapshot (which
// reflects at least everything up to that point) and sends it with that
// sequence number; deltas that overlap the snapshot are idempotent.
func (b *bus) subscribe() (*subscriber, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &subscriber{ch: make(chan proto.Event, b.bufSize)}
	b.subs[s] = struct{}{}
	return s, b.seq
}

func (b *bus) unsubscribe(s *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		close(s.ch)
	}
}

// closeAll ends every subscription (shutdown).
func (b *bus) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		delete(b.subs, s)
		close(s.ch)
	}
}

func (b *bus) subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
