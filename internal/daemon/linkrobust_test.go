package daemon

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/transport"
)

// hintSession records the load hints the daemon gives a session.
type hintSession struct {
	transport.Session
	stressed atomic.Bool
}

func (h *hintSession) SetPeerStressed(on bool) {
	h.stressed.Store(on)
	if lh, ok := h.Session.(transport.LoadHinter); ok {
		lh.SetPeerStressed(on)
	}
}

type hintDialer struct {
	inner PeerDialer
	mu    sync.Mutex
	last  *hintSession
}

func (d *hintDialer) Dial(ctx context.Context, p PeerConfig) (transport.Session, error) {
	s, err := d.inner.Dial(ctx, p)
	if err != nil {
		return nil, err
	}
	h := &hintSession{Session: s}
	d.mu.Lock()
	d.last = h
	d.mu.Unlock()
	return h, nil
}

func (d *hintDialer) session() *hintSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

// A peer that reports heavy load (or this node being overloaded itself)
// makes the link tolerate longer silences; the hint goes away with the load.
func TestLoadHintReachesPeerSessions(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	hd := &hintDialer{inner: memDialer{c.net, "a"}}
	a.dialer = hd
	b.start()
	a.start()
	c.waitOnline("a", "b")
	waitFor(t, 2*time.Second, "outbound session", func() bool { return hd.session() != nil })
	if hd.session().stressed.Load() {
		t.Fatal("calm peer marked stressed")
	}

	b.prober.mu.Lock()
	b.prober.m.MemPressure = 45 // PSI: stalled on memory 45% of the time
	b.prober.mu.Unlock()
	waitFor(t, 3*time.Second, "stress hint for a loaded peer", func() bool { return hd.session().stressed.Load() })

	b.prober.mu.Lock()
	b.prober.m.MemPressure = 0
	b.prober.mu.Unlock()
	waitFor(t, 3*time.Second, "hint cleared", func() bool { return !hd.session().stressed.Load() })

	a.prober.mu.Lock()
	a.prober.m.Load1 = 64 // 16 CPUs: this node itself is overloaded
	a.prober.mu.Unlock()
	waitFor(t, 3*time.Second, "stress hint for an overloaded self", func() bool { return hd.session().stressed.Load() })
}

// failingDialer fails while down is set and counts attempts.
type failingDialer struct {
	inner    PeerDialer
	down     atomic.Bool
	attempts atomic.Int64
}

func (d *failingDialer) Dial(ctx context.Context, p PeerConfig) (transport.Session, error) {
	d.attempts.Add(1)
	if d.down.Load() {
		return nil, transport.ErrUnreachable
	}
	return d.inner.Dial(ctx, p)
}

// An unreachable peer is retried with exponential backoff, and the log says
// so on state changes only, plus a periodic "still unreachable" summary,
// never one line per attempt.
func TestReconnectBacksOffAndLogsStateChanges(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.timing.BackoffMin = 20 * time.Millisecond
	a.timing.BackoffMax = 400 * time.Millisecond
	a.timing.UnreachableLog = 700 * time.Millisecond
	fd := &failingDialer{inner: memDialer{c.net, "a"}}
	fd.down.Store(true)
	a.dialer = fd
	a.logs = &logCapture{}
	b.peers.set([]PeerConfig{{Name: "a", ID: "a-id", Addresses: []string{"mem"}, Direction: "inbound"}}) // b does not dial a
	b.start()
	a.start()
	time.Sleep(2 * time.Second)
	n := fd.attempts.Load()
	// Without backoff 2 s at a 20 ms floor would be ~100 attempts.
	if n < 3 || n > 20 {
		t.Fatalf("%d dial attempts in 2 s", n)
	}
	if got := a.logs.count("b is unreachable"); got != 1 {
		t.Fatalf("logged the same failure %d times:\n%s", got, a.logs.all())
	}
	if got := a.logs.count("still unreachable after"); got < 1 || got > 3 {
		t.Fatalf("%d summaries:\n%s", got, a.logs.all())
	}
	fd.down.Store(false)
	waitFor(t, 3*time.Second, "reconnected", func() bool { return a.daemon().peers.get("b").active() != nil })
	waitFor(t, time.Second, "recovery logged", func() bool { return a.logs.count("failed attempts") == 1 })
}

// When the peer dials in, or the network changes, the next outbound
// attempt starts at once with a fresh backoff instead of waiting it out.
func TestReconnectRetriesAtOnceOnInboundOrNetworkChange(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.timing.BackoffMin = 8 * time.Second // a natural retry comes after 4–8 s
	a.timing.BackoffMax = 30 * time.Second
	fd := &failingDialer{inner: memDialer{c.net, "a"}}
	fd.down.Store(true)
	a.dialer = fd
	var sig atomic.Value
	sig.Store("net-1")
	a.netSig = func() string { return sig.Load().(string) }
	b.peers.set([]PeerConfig{{Name: "a", ID: "a-id", Addresses: []string{"mem"}, Direction: "inbound"}})
	b.start()
	a.start()
	waitFor(t, 2*time.Second, "first attempt", func() bool { return fd.attempts.Load() >= 1 })
	time.Sleep(200 * time.Millisecond)
	before := fd.attempts.Load()

	// The network changes: retry now, not in 4–8 s.
	start := time.Now()
	sig.Store("net-2")
	waitFor(t, time.Second, "retry after a network change", func() bool { return fd.attempts.Load() > before })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("retried %s after the network changed", d)
	}
	if time.Sleep(100 * time.Millisecond); fd.attempts.Load() > before+1 {
		t.Fatalf("network change caused %d attempts", fd.attempts.Load()-before)
	}

	// b dials in: a retries its own direction at once too.
	before = fd.attempts.Load()
	fd.down.Store(false)
	start = time.Now()
	b.peers.set([]PeerConfig{{Name: "a", ID: "a-id", Addresses: []string{"mem"}, Direction: "both"}})
	waitFor(t, 1500*time.Millisecond, "outbound retried after b connected", func() bool {
		return fd.attempts.Load() > before && a.daemon().peers.get("b").outbound() != nil
	})
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("outbound came back %s after b connected", d)
	}
}

// Connections that close before sending anything (the bridge probing
// whether the daemon is up) are not worth a log line.
func TestPeerSocketProbeIsQuiet(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.logs = &logCapture{}
	a.start()
	for i := 0; i < 3; i++ {
		conn, err := net.Dial("unix", a.dirs.peerSocket())
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	// A broken first line is still worth a line.
	conn, err := net.Dial("unix", a.dirs.peerSocket())
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("kran-peer BAD NAME\n"))
	conn.Close()
	waitFor(t, 2*time.Second, "bad peer line logged", func() bool { return a.logs.count("invalid peer name") == 1 })
	if n := a.logs.count("EOF"); n != 0 {
		t.Fatalf("probe connections logged:\n%s", a.logs.all())
	}
}
