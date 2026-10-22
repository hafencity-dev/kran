package scheduler

import (
	"context"
	"sync"
)

// ticket resolves once. Reason updates are coalesced into a one-slot
// channel so the scheduler never blocks on a slow reader; the channel is
// closed when the ticket resolves.
type ticket struct {
	id      string
	done    chan struct{}
	updates chan string

	mu       sync.Mutex
	resolved bool
	last     string
	pl       Placement
	err      error
}

func newTicket(id string) *ticket {
	return &ticket{id: id, done: make(chan struct{}), updates: make(chan string, 1)}
}

func (t *ticket) JobID() string          { return t.id }
func (t *ticket) Updates() <-chan string { return t.updates }

func (t *ticket) Wait(ctx context.Context) (Placement, error) {
	select {
	case <-t.done:
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.pl, t.err
	case <-ctx.Done():
		// A resolved ticket wins over a simultaneously canceled context so
		// a placement is never silently dropped.
		select {
		case <-t.done:
			t.mu.Lock()
			defer t.mu.Unlock()
			return t.pl, t.err
		default:
		}
		return Placement{}, ctx.Err()
	}
}

// push delivers a new reason, replacing an undelivered older one.
func (t *ticket) push(reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resolved || reason == t.last {
		return
	}
	t.last = reason
	select {
	case <-t.updates: // drop the stale one
	default:
	}
	t.updates <- reason
}

func (t *ticket) resolve(pl Placement, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resolved {
		return
	}
	t.resolved = true
	t.pl, t.err = pl, err
	close(t.updates)
	close(t.done)
}

// resolvedTicket is returned for calls that fail immediately.
func resolvedTicket(id string, pl Placement, err error) *ticket {
	t := newTicket(id)
	t.resolve(pl, err)
	return t
}
