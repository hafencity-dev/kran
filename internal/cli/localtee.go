package cli

import (
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// historyTee sends a local child's output to the daemon over the run
// session (stdout/stderr frames, client→daemon), so `kran logs` and the UI
// show local jobs too. It must never slow the child down: push only copies
// into a bounded queue, a goroutine of its own does the (possibly slow)
// socket writes, and when the daemon falls behind data is dropped and the
// history gets a truncation mark instead. The terminal never waits for it.
type historyTee struct {
	max int // bytes the queue may hold

	mu      sync.Mutex
	q       []teeChunk
	qBytes  int
	sess    client.RunSession // nil while disconnected: data waits (bounded)
	busy    bool              // a Send is in progress
	mark    bool              // data was dropped since the last mark
	markT   proto.FrameType   // stream of the dropped data
	last    byte              // last byte queued (marks start on their own line)
	idle    chan struct{}     // closed while the queue is empty and nothing is in flight
	wake    chan struct{}
	quit    chan struct{}
	stopped bool
	done    chan struct{}
}

type teeChunk struct {
	t proto.FrameType
	b []byte
}

// historyTruncated is what the history shows where output was dropped.
const historyTruncated = "[kran: output truncated in history]"

const (
	teeQueueMax  = 8 << 20  // ~ a second of a very chatty build
	teeChunkMax  = 64 << 10 // keeps each socket write short
	teeFlushWait = 2 * time.Second
)

func newHistoryTee(max int) *historyTee {
	t := &historyTee{max: max, idle: make(chan struct{}), wake: make(chan struct{}, 1),
		quit: make(chan struct{}), done: make(chan struct{})}
	close(t.idle)
	go t.run()
	return t
}

// push queues output of stream ft. It never blocks on the daemon.
func (t *historyTee) push(ft proto.FrameType, b []byte) {
	if len(b) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	if t.mark && !t.enqueueMarkLocked() {
		t.dropLocked(ft)
		return
	}
	if t.qBytes+len(b) > t.max {
		t.dropLocked(ft)
		return
	}
	t.enqueueLocked(ft, b)
}

func (t *historyTee) dropLocked(ft proto.FrameType) {
	if !t.mark {
		t.mark, t.markT = true, ft
	}
}

// enqueueMarkLocked queues the truncation mark if there is room for it.
func (t *historyTee) enqueueMarkLocked() bool {
	text := historyTruncated + "\n"
	if t.last != 0 && t.last != '\n' {
		text = "\n" + text
	}
	if t.qBytes+len(text) > t.max {
		return false
	}
	t.mark = false
	t.enqueueLocked(t.markT, []byte(text))
	return true
}

func (t *historyTee) enqueueLocked(ft proto.FrameType, b []byte) {
	for len(b) > 0 {
		n := min(len(b), teeChunkMax)
		t.q = append(t.q, teeChunk{ft, append([]byte(nil), b[:n]...)})
		t.qBytes += n
		b = b[n:]
	}
	t.last = t.q[len(t.q)-1].b[len(t.q[len(t.q)-1].b)-1]
	t.notIdleLocked()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *historyTee) notIdleLocked() {
	select {
	case <-t.idle:
		t.idle = make(chan struct{})
	default:
	}
}

func (t *historyTee) idleLocked() {
	if len(t.q) == 0 && !t.busy {
		select {
		case <-t.idle:
		default:
			close(t.idle)
		}
	}
}

// setSession switches to a (re)connected session; nil while disconnected.
// The caller sends reattach/started first: output must not precede them.
func (t *historyTee) setSession(s client.RunSession) {
	t.mu.Lock()
	t.sess = s
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *historyTee) run() {
	defer close(t.done)
	for {
		select {
		case <-t.quit:
			return
		case <-t.wake:
		}
		for {
			t.mu.Lock()
			s := t.sess
			if t.stopped || len(t.q) == 0 || s == nil {
				t.idleLocked()
				t.mu.Unlock()
				break
			}
			c := t.q[0]
			t.q[0] = teeChunk{}
			t.q = t.q[1:]
			t.qBytes -= len(c.b)
			t.busy = true
			t.mu.Unlock()
			err := s.Send(c.t, c.b)
			t.mu.Lock()
			t.busy = false
			if err != nil {
				// That chunk is lost; the rest waits for a reconnect.
				t.dropLocked(c.t)
				if t.sess == s {
					t.sess = nil
				}
			}
			t.mu.Unlock()
		}
	}
}

// flush waits up to timeout until everything queued (and a pending mark)
// reached the session. It reports whether it did.
func (t *historyTee) flush(timeout time.Duration) bool {
	tm := time.NewTimer(timeout)
	defer tm.Stop()
	for {
		t.mu.Lock()
		if t.mark {
			t.enqueueMarkLocked()
		}
		// A full queue had no room for the mark: it goes once the queue
		// drained (the next round), so the history ends with it.
		pending := t.mark
		idle := t.idle
		t.mu.Unlock()
		select {
		case t.wake <- struct{}{}:
		default:
		}
		select {
		case <-idle:
			if !pending {
				return true
			}
		case <-tm.C:
			return false
		}
	}
}

// stop ends the sender; queued output is dropped. It reports whether the
// history is incomplete (something was dropped or is still queued), so the
// caller can send the mark itself.
func (t *historyTee) stop() (incomplete bool) {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return false
	}
	t.stopped = true
	incomplete = t.mark || len(t.q) > 0 || t.busy
	t.q, t.qBytes = nil, 0
	t.mu.Unlock()
	// A Send stuck on a wedged daemon ends by its write deadline; the
	// sender exits then. Nobody needs to wait for that.
	close(t.quit)
	return incomplete
}
