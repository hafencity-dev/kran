package daemon

import (
	"context"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

func helloAs(t *testing.T, c *cluster, name, target string) transport.Session {
	t.Helper()
	s, err := c.net.Dial(context.Background(), name, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := peerOneShot(context.Background(), s, proto.PeerHello, "", proto.Hello{Node: proto.NodeInfo{Name: name}, Protocol: protocolString()}, nil); err != nil {
		t.Fatalf("hello as %s: %v", name, err)
	}
	return s
}

// Job IDs are visible to every peer (metrics.subscribe lists guests), so a
// job's reservation, slot and process answer only to the origin that
// admitted it.
func TestPeerCannotTouchAnotherOriginsJob(t *testing.T) {
	c := newCluster(t, "a", "b", "c")
	b := c.nodes["b"]
	b.start()
	ctx := context.Background()
	sa := helloAs(t, c, "a", "b")
	if err := peerOneShot(ctx, sa, proto.PeerAdmit, "job1", admitBody{Alloc: proto.Resources{CPUs: 1, Mem: 1 << 30}, Runtime: proto.RuntimeNative}, nil); err != nil {
		t.Fatal(err)
	}
	var slot proto.SlotRef
	if err := peerOneShot(ctx, sa, proto.PeerSlotAcquire, "job1", slotAcquireBody{RepoID: "r", Runtime: proto.RuntimeNative}, &slot); err != nil {
		t.Fatal(err)
	}
	sc := helloAs(t, c, "c", "b")
	if err := peerOneShot(ctx, sc, proto.PeerAdmit, "job1", admitBody{Alloc: proto.Resources{CPUs: 1, Mem: 1 << 30}, Runtime: proto.RuntimeNative}, nil); err == nil {
		t.Fatal("admit of another origin's job id accepted")
	}
	for _, m := range []string{proto.PeerSlotAcquire, proto.PeerSlotCommit, proto.PeerJobCancel, proto.PeerJobHeartbeat} {
		if err := peerOneShot(ctx, sc, m, "job1", slotAcquireBody{RepoID: "r", Runtime: proto.RuntimeNative}, nil); peerCode(err) != codeUnknownJob {
			t.Errorf("%s by another origin: %v", m, err)
		}
	}
	peerOneShot(ctx, sc, proto.PeerSlotRelease, "job1", nil, nil)
	if b.exec.heldSlots() != 1 || len(b.daemon().target.status().Allocs) != 1 {
		t.Fatal("another origin released the reservation")
	}
}

func TestPeerHelloMustBeLinked(t *testing.T) {
	c := newCluster(t, "b")
	c.nodes["b"].start()
	s, err := c.net.Dial(context.Background(), "x", "b")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = peerOneShot(context.Background(), s, proto.PeerHello, "", proto.Hello{Node: proto.NodeInfo{Name: "x"}, Protocol: protocolString()}, nil)
	if err == nil {
		t.Fatal("hello from a node that is not in peers.toml accepted")
	}
	if err := peerOneShot(context.Background(), s, proto.PeerAdmit, "j1", admitBody{}, nil); peerCode(err) != codeHelloFirst {
		t.Fatalf("after a refused hello: %v", err)
	}
}

func TestBlobPutNeedsReservation(t *testing.T) {
	c := newCluster(t, "a", "b")
	c.nodes["b"].start()
	s := helloAs(t, c, "a", "b")
	if err := peerOneShot(context.Background(), s, proto.PeerBlobPut, "", nil, nil); peerCode(err) != codeBadRequest {
		t.Fatalf("blob.put without a job: %v", err)
	}
}
