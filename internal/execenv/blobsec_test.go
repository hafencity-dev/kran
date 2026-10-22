package execenv

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

// A size below zero used to mean "read to EOF": an endless stream from a
// peer filled blobs/tmp.
func TestPutBlobRejectsNegativeSize(t *testing.T) {
	f := newFixture(t)
	if err := f.bs.put(context.Background(), hashOf("x"), -1, strings.NewReader("x")); err == nil {
		t.Fatal("negative size accepted")
	}
	if ents, _ := os.ReadDir(f.l.blobTmp()); len(ents) != 0 {
		t.Fatalf("temp blobs left: %d", len(ents))
	}
}

func TestOpenBlobRejectsBadHash(t *testing.T) {
	m := &manager{blobs: newFixture(t).bs}
	for _, h := range []string{"../../../../etc/passwd", "", strings.Repeat("A", 64)} {
		if rc, _, err := m.openBlob(h); err == nil {
			rc.Close()
			t.Fatalf("opened %q", h)
		}
	}
}

// The apply side runs in a container jobs can tamper with: the host sends
// only blobs of the manifest it asked to apply.
func TestDriveApplyRejectsUnknownNeed(t *testing.T) {
	m := &proto.Manifest{Entries: []proto.ManifestEntry{{Path: "a", Hash: hashOf("a"), Size: 1}}}
	for _, need := range []string{"../../../../Users/u/.ssh/id_ed25519", hashOf("not in manifest")} {
		var reply bytes.Buffer
		writeAFJSON(&reply, afNeed, applyNeed{Needed: []string{need}})
		opened := false
		open := func(h string) (io.ReadCloser, int64, error) {
			opened = true
			return io.NopCloser(strings.NewReader("a")), 1, nil
		}
		if _, _, err := driveApply(context.Background(), io.Discard, &reply, applyReq{Op: "commit", Slot: "r/0", Manifest: m}, open); err == nil || opened {
			t.Fatalf("need %q: err=%v opened=%v", need, err, opened)
		}
	}
}
