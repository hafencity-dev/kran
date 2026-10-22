package transport

import (
	"errors"
	"testing"
	"time"
)

// Any byte from the peer proves it is alive, not only ping replies: a link
// busy with a blob upload whose pongs queue behind data is not dead.
func TestLivenessCountsInboundData(t *testing.T) {
	t0 := time.Unix(1000, 0)
	l := newLiveness(time.Second, t0)
	if s := l.observe(t0.Add(time.Second), 0); s != time.Second {
		t.Fatalf("silent %s after one quiet second", s)
	}
	if s := l.observe(t0.Add(2*time.Second), 0); s != 2*time.Second {
		t.Fatalf("silent %s after two quiet seconds", s)
	}
	if s := l.observe(t0.Add(3*time.Second), 7); s != 0 {
		t.Fatalf("silent %s right after data arrived", s)
	}
	if s := l.observe(t0.Add(4*time.Second), 7); s != time.Second {
		t.Fatalf("silent %s one second after data", s)
	}
}

// A daemon that was not scheduled for a while (CPU starvation, swapped
// out) must not blame the peer for the time it was not reading itself: a
// late tick counts as one interval at most.
func TestLivenessIgnoresOwnPause(t *testing.T) {
	t0 := time.Unix(1000, 0)
	l := newLiveness(time.Second, t0)
	if s := l.observe(t0.Add(40*time.Second), 0); s != time.Second {
		t.Fatalf("a 40 s pause of our own counted as %s of peer silence", s)
	}
}

func TestToleranceAdapts(t *testing.T) {
	ka := KeepaliveConfig{Interval: time.Second, DeadAfter: 30 * time.Second, MaxDeadAfter: 120 * time.Second}
	if got := tolerance(ka, false, 5*time.Millisecond); got != 30*time.Second {
		t.Errorf("calm link: %s", got)
	}
	if got := tolerance(ka, true, 5*time.Millisecond); got != 120*time.Second {
		t.Errorf("stressed peer: %s", got)
	}
	if got := tolerance(ka, false, 3*time.Second); got != 60*time.Second {
		t.Errorf("slow link (rtt 3 s): %s", got)
	}
	if got := tolerance(ka, false, time.Minute); got != 120*time.Second {
		t.Errorf("never beyond the maximum: %s", got)
	}
}

func TestKeepaliveDefaults(t *testing.T) {
	c := SessionConfig{}.withDefaults()
	ka := c.Keepalive
	if ka.DeadAfter < 30*time.Second || ka.MaxDeadAfter < ka.DeadAfter || ka.MaxDeadAfter > 3*time.Minute {
		t.Fatalf("defaults %+v", ka)
	}
	// yamux's own timeouts close the whole session; they must never fire
	// before our liveness check would.
	if c.WriteTimeout < ka.MaxDeadAfter {
		t.Fatalf("write timeout %s below the link tolerance %s", c.WriteTimeout, ka.MaxDeadAfter)
	}
	// Old-style configs (tests) keep their meaning.
	old := SessionConfig{Keepalive: KeepaliveConfig{Interval: 100 * time.Millisecond, Timeout: 100 * time.Millisecond, Misses: 3}}.withDefaults()
	if old.Keepalive.DeadAfter != 400*time.Millisecond || old.Keepalive.MaxDeadAfter < old.Keepalive.DeadAfter {
		t.Fatalf("legacy config %+v", old.Keepalive)
	}
}

// A peer known to be overloaded gets the long tolerance: a stall longer
// than the calm tolerance but shorter than the maximum leaves the session
// up; a real outage still ends it.
func TestKeepaliveStressedPeerSurvivesLongStall(t *testing.T) {
	cfg := SessionConfig{Keepalive: KeepaliveConfig{Interval: 40 * time.Millisecond, DeadAfter: 300 * time.Millisecond, MaxDeadAfter: 1500 * time.Millisecond}}
	c, s, link, err := Pipe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer link.Cut()
	go echoServer(s)
	for _, x := range []Session{c, s} {
		x.(LoadHinter).SetPeerStressed(true)
	}
	roundTrip(t, c, "x")
	link.Stall(true)
	time.Sleep(800 * time.Millisecond)
	link.Stall(false)
	roundTrip(t, c, "after the stall")
	if c.Err() != nil || s.Err() != nil {
		t.Fatalf("session died in a stall within the tolerance: %v / %v", c.Err(), s.Err())
	}
	link.Stall(true)
	start := time.Now()
	waitDone(t, c, 4*time.Second)
	if d := time.Since(start); d < 1200*time.Millisecond {
		t.Errorf("declared dead after %s, before the stressed tolerance", d)
	}
	if !errors.Is(c.Err(), ErrKeepalive) {
		t.Fatalf("err = %v, want ErrKeepalive", c.Err())
	}
}

// Without the hint the calm tolerance applies.
func TestKeepaliveCalmPeerDiesAtDeadAfter(t *testing.T) {
	cfg := SessionConfig{Keepalive: KeepaliveConfig{Interval: 40 * time.Millisecond, DeadAfter: 300 * time.Millisecond, MaxDeadAfter: 3 * time.Second}}
	c, s, link, err := Pipe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer link.Cut()
	go echoServer(s)
	roundTrip(t, c, "x")
	link.Stall(true)
	waitDone(t, c, 2*time.Second)
	if !errors.Is(c.Err(), ErrKeepalive) {
		t.Fatalf("err = %v, want ErrKeepalive", c.Err())
	}
}

// The RTT shown after connecting comes from a warm ping, not from the
// first one that waited for the far end's bridge to start.
func TestHandshakeRTTIgnoresColdPing(t *testing.T) {
	c, s, link, err := Pipe(SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Cut()
	go echoServer(s)
	link.Stall(true)
	go func() {
		time.Sleep(300 * time.Millisecond)
		link.Stall(false)
	}()
	if err := handshakePing(c.(*session), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if r := c.RTT(); r <= 0 || r > 100*time.Millisecond {
		t.Fatalf("rtt after connecting %s; the cold ping took 300 ms", r)
	}
}
