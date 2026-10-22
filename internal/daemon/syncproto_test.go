package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

func syncManifest(n int, root string) *proto.Manifest {
	m := &proto.Manifest{RepoID: "r", Root: root, Keep: []string{"node_modules/"}}
	for i := 0; i < n; i++ {
		h := sha256.Sum256([]byte(fmt.Sprint(i)))
		m.Entries = append(m.Entries, proto.ManifestEntry{Path: fmt.Sprintf("d%03d/f%05d", i/10, i), Mode: 0o644,
			Size: int64(i), MTime: int64(i) * 1000, Hash: hex.EncodeToString(h[:])})
	}
	return m
}

// syncPeer admits job and acquires a slot for it on b as origin a.
func syncPeer(t *testing.T, s transport.Session, job string) slotAcquireReply {
	t.Helper()
	ctx := context.Background()
	if err := peerOneShot(ctx, s, proto.PeerAdmit, job, admitBody{Alloc: proto.Resources{CPUs: 1, Mem: 1 << 30}, Runtime: proto.RuntimeNative}, nil); err != nil {
		t.Fatal(err)
	}
	var acq slotAcquireReply
	if err := peerOneShot(ctx, s, proto.PeerSlotAcquire, job, slotAcquireBody{RepoID: "r", Runtime: proto.RuntimeNative}, &acq); err != nil {
		t.Fatal(err)
	}
	return acq
}

// Protocol 1.1: the origin sends the whole manifest once, then deltas or
// nothing; the target ends up with exactly the origin's manifest each
// time, and a target that lost its copy gets the whole manifest again.
func TestSyncManifestFormats(t *testing.T) {
	c := newCluster(t, "a", "b")
	b := c.nodes["b"]
	b.start()
	s := helloAs(t, c, "a", "b")
	od := &Daemon{t: testTiming(), manifests: newOriginManifests()}
	ctx := context.Background()
	got := func() *proto.Manifest {
		b.exec.mu.Lock()
		defer b.exec.mu.Unlock()
		return b.exec.diffed[len(b.exec.diffed)-1]
	}
	cached := func() int {
		tc := b.d.target.manifests
		tc.mu.Lock()
		defer tc.mu.Unlock()
		return tc.ll.Len()
	}

	acq := syncPeer(t, s, "job1")
	if acq.Sync != syncRevision || acq.Index != 0 || acq.RepoID != "r" {
		t.Fatalf("acquire answer %+v", acq)
	}
	m1 := syncManifest(300, "/w/one")
	if _, err := od.remoteDiff(ctx, s, "job1", "b", acq.Sync, m1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got(), m1) || cached() != 1 {
		t.Fatalf("full manifest: target got another manifest (cached %d)", cached())
	}

	// Same manifest again: nothing sent, the target reuses its copy.
	if _, err := od.remoteDiff(ctx, s, "job1", "b", acq.Sync, syncManifest(300, "/w/one")); err != nil {
		t.Fatal(err)
	}
	if got() != b.d.target.manifests.get(manifestKey("a", proto.ManifestHash(m1))) {
		t.Fatal("unchanged manifest was not served from the target's copy")
	}

	// Changed: a delta; the result equals the origin's manifest.
	m2 := syncManifest(300, "/w/one")
	m2.Entries = append(m2.Entries[:5], m2.Entries[7:]...)
	m2.Entries[40].MTime = 1
	m2.Entries = append(m2.Entries, proto.ManifestEntry{Path: "zz/new", Kind: proto.EntrySymlink, Mode: 0o777, Target: "../d000/f00000"})
	if _, err := od.remoteDiff(ctx, s, "job1", "b", acq.Sync, m2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got(), m2) || cached() != 2 {
		t.Fatalf("delta: target manifest differs (cached %d)", cached())
	}

	// The target restarted (lost its copies): same and delta fall back.
	b.d.target.manifests = newManifestCache(manifestCacheEntries)
	if _, err := od.remoteDiff(ctx, s, "job1", "b", acq.Sync, m2); err != nil {
		t.Fatalf("fallback after the target lost its copy: %v", err)
	}
	if !reflect.DeepEqual(got(), m2) {
		t.Fatal("fallback sent another manifest")
	}
	m3 := syncManifest(301, "/w/one")
	b.d.target.manifests = newManifestCache(manifestCacheEntries)
	if _, err := od.remoteDiff(ctx, s, "job1", "b", acq.Sync, m3); err != nil || !reflect.DeepEqual(got(), m3) {
		t.Fatalf("delta fallback: %v", err)
	}
}

// A 1.0 target (no "sync" in its acquire answer) gets zstd JSON, which a
// 1.1 target still reads (that is what a 1.0 origin sends).
func TestSyncManifestToOldTarget(t *testing.T) {
	c := newCluster(t, "a", "b")
	b := c.nodes["b"]
	b.start()
	s := helloAs(t, c, "a", "b")
	od := &Daemon{t: testTiming(), manifests: newOriginManifests()}
	syncPeer(t, s, "job1")
	m := syncManifest(50, "/w")
	for i := 0; i < 2; i++ {
		if _, err := od.remoteDiff(context.Background(), s, "job1", "b", 0, m); err != nil {
			t.Fatal(err)
		}
		b.exec.mu.Lock()
		last := b.exec.diffed[len(b.exec.diffed)-1]
		b.exec.mu.Unlock()
		if !reflect.DeepEqual(last, m) {
			t.Fatal("JSON manifest differs on the target")
		}
	}
	if n := b.d.target.manifests.ll.Len(); n != 0 {
		t.Fatalf("JSON manifests cached as delta bases: %d", n)
	}
}

// An origin can only refer to manifests it sent itself.
func TestSyncBaseScopedToOrigin(t *testing.T) {
	c := newCluster(t, "a", "b", "c")
	b := c.nodes["b"]
	b.start()
	sa := helloAs(t, c, "a", "b")
	sc := helloAs(t, c, "c", "b")
	ctx := context.Background()
	m := syncManifest(20, "/w")
	acq := syncPeer(t, sa, "job1")
	if _, err := (&Daemon{t: testTiming(), manifests: newOriginManifests()}).remoteDiff(ctx, sa, "job1", "b", acq.Sync, m); err != nil {
		t.Fatal(err)
	}
	syncPeer(t, sc, "job2")
	h := proto.ManifestHash(m)
	_, err := (&Daemon{t: testTiming()}).sendDiff(ctx, sc, "job2", &slotDiffBody{Format: proto.ManifestSame, Hash: h, Base: h}, nil)
	if peerCode(err) != codeManifestBase {
		t.Fatalf("c used a's manifest as its base: %v", err)
	}
}
