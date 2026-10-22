package transport

import (
	"testing"
	"time"
)

// The RTT shown for a link is a smoothed value of the keepalive pings. The
// handshake ping (taken while dialing, often under load or while the far
// daemon starts) only stands in until the first keepalive sample.
func TestRTTIsSmoothedKeepaliveRTT(t *testing.T) {
	var r rttEstimate
	if r.value() != 0 {
		t.Fatal("no sample yet")
	}
	r.handshake(1907 * time.Millisecond)
	if r.value() != 1907*time.Millisecond {
		t.Fatalf("before keepalives: %s", r.value())
	}
	r.sample(5 * time.Millisecond)
	if r.value() != 5*time.Millisecond {
		t.Fatalf("the first keepalive replaces the handshake: %s", r.value())
	}
	r.sample(205 * time.Millisecond) // one slow ping
	if v := r.value(); v < 40*time.Millisecond || v > 80*time.Millisecond {
		t.Fatalf("one spike moved it to %s", v)
	}
	for i := 0; i < 20; i++ {
		r.sample(5 * time.Millisecond)
	}
	if v := r.value(); v > 6*time.Millisecond {
		t.Fatalf("did not settle: %s", v)
	}
}
