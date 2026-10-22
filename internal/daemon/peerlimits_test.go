package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/hafencity-dev/kran/internal/proto"
)

// A peer must not make one decoder hold a huge window (a tiny frame of RLE
// blocks declaring 1 GiB would allocate it).
func TestZstdDecoderRefusesLargeWindow(t *testing.T) {
	var buf bytes.Buffer
	enc, _ := zstd.NewWriter(&buf, zstd.WithWindowSize(128<<20), zstd.WithSingleSegment(false), zstd.WithEncoderConcurrency(1))
	enc.Write(bytes.Repeat([]byte("a"), 1<<20))
	enc.Close()
	dec, err := newZstdDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	dec.Reset(&buf)
	if _, err := io.Copy(io.Discard, dec); err == nil {
		t.Fatal("128 MiB window accepted")
	}
	// What our own origin sends still decodes.
	buf.Reset()
	enc, _ = zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	enc.Write(bytes.Repeat([]byte("abc"), 4<<20))
	enc.Close()
	dec.Reset(&buf)
	if n, err := io.Copy(io.Discard, dec); err != nil || n != 12<<20 {
		t.Fatal(n, err)
	}
}

func TestDecodeManifestIsBounded(t *testing.T) {
	old := maxManifest
	maxManifest = 1 << 10
	defer func() { maxManifest = old }()
	big := `{"repo_id":"r","entries":[{"p":"` + strings.Repeat("x", 4<<10) + `"}]}`
	var m proto.Manifest
	if err := decodeManifest(strings.NewReader(big), &m); err == nil {
		t.Fatal("oversized manifest accepted")
	}
	b, _ := json.Marshal(proto.Manifest{RepoID: "r", Entries: []proto.ManifestEntry{{Path: "a"}}})
	if err := decodeManifest(bytes.NewReader(b), &m); err != nil || len(m.Entries) != 1 {
		t.Fatal(m, err)
	}
}

// Blobs are only taken for the files of the job's manifest, at their size.
func TestBlobPutOnlyForManifestFiles(t *testing.T) {
	c := newCluster(t, "a", "b")
	b := c.nodes["b"]
	b.start()
	ctx := context.Background()
	s := helloAs(t, c, "a", "b")
	if err := peerOneShot(ctx, s, proto.PeerAdmit, "job1", admitBody{Alloc: proto.Resources{CPUs: 1, Mem: 1 << 30}, Runtime: proto.RuntimeNative}, nil); err != nil {
		t.Fatal(err)
	}
	if err := peerOneShot(ctx, s, proto.PeerSlotAcquire, "job1", slotAcquireBody{RepoID: "r", Runtime: proto.RuntimeNative}, nil); err != nil {
		t.Fatal(err)
	}
	// No manifest yet: nothing is accepted.
	pc, err := peerCall(ctx, s, proto.PeerBlobPut, "job1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	h := strings.Repeat("ab", 32)
	pc.writeJSONFrame(pfBlob, blobHeader{Hash: h, Size: -1})
	if _, _, err := pc.readFrame(); err == nil {
		t.Fatal("blob outside any manifest acked instead of closing the stream")
	}
}

// The target names the blobs it is missing; the origin sends only files of
// this job's manifest (the scanner can open blobs of every repo).
func TestUploadOnlyManifestBlobs(t *testing.T) {
	m := &proto.Manifest{Entries: []proto.ManifestEntry{{Path: "a", Hash: strings.Repeat("a", 64), Size: 1}}}
	plan := proto.SyncPlan{Missing: []string{strings.Repeat("b", 64)}}
	if _, err := (&Daemon{}).uploadBlobs(context.Background(), nil, nil, "b", m, plan); err == nil {
		t.Fatal("uploaded a blob outside the manifest")
	}
}

// A peer reports live usage only for jobs placed on it.
func TestRemoteLiveOnlyFromTheJobsNode(t *testing.T) {
	sched := newFakeSched("a", alwaysLocal)
	d := &Daemon{self: "a", deps: Deps{Sched: sched}}
	d.jobs = newJobTable(d)
	d.jobs.active["j1"] = &jobEntry{d: d, id: "j1", job: proto.Job{Node: "b", State: proto.JobRunning}}
	d.applyRemoteLive("c", []allocInfo{{JobID: "j1", Origin: "a", Live: &proto.Resources{CPUs: 99}}})
	if _, ok := sched.usage["j1"]; ok {
		t.Fatal("usage of a job on b accepted from c")
	}
}

func TestPeerRequestLineBounded(t *testing.T) {
	c := newCluster(t, "a", "b")
	c.nodes["b"].start()
	s, err := c.net.Dial(context.Background(), "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	go func() {
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < 2*maxRequestLine>>20; i++ {
			if _, err := st.Write(chunk); err != nil {
				return
			}
		}
	}()
	var b [1]byte
	if _, err := st.Read(b[:]); err == nil {
		t.Fatal("expected the stream to be closed")
	}
}

// Streams a peer holds open are bounded per session.
func TestPeerStreamsBounded(t *testing.T) {
	c := newCluster(t, "a", "b")
	c.nodes["b"].start()
	s := helloAs(t, c, "a", "b")
	var open []io.Closer
	defer func() {
		for _, c := range open {
			c.Close()
		}
	}()
	for i := 0; i < maxPeerStreams; i++ {
		st, err := s.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, st)
		st.Write([]byte(`{"method":"metrics.subscribe"}` + "\n"))
		var resp [1]byte
		if _, err := st.Read(resp[:]); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}
	st, err := s.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Write([]byte(`{"method":"metrics.subscribe"}` + "\n"))
	var resp [64]byte
	if n, err := st.Read(resp[:]); err == nil {
		t.Fatalf("stream beyond the limit served: %q", resp[:n])
	}
}
