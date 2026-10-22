package execenv

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		if _, _, err := driveApply(context.Background(), io.Discard, &reply, applyReq{Op: "commit", Slot: "r/0", Manifest: m}, open, nil); err == nil || opened {
			t.Fatalf("need %q: err=%v opened=%v", need, err, opened)
		}
	}
}

// The forwarded env sits in jobs/<id>/spec.json for days; only the
// supervisor needs it, and only once.
func TestSpecFileRedactedAfterStart(t *testing.T) {
	te := newTestEnv(t)
	ref, err := te.m.AcquireSlot(context.Background(), testRepo, "wt", proto.RuntimeNative)
	if err != nil {
		t.Fatal(err)
	}
	p := te.start(ref, sh(`echo "secret=$MY_SECRET"`), withEnv("MY_SECRET", "hunter2-value"))
	waitExit(t, p, 30*time.Second)
	if out, _ := output(t, p); !strings.Contains(out, "secret=hunter2-value") {
		t.Fatalf("job did not get its env: %q", out)
	}
	b, err := os.ReadFile(filepath.Join(te.dir, "jobs", p.JobID(), fileSpec))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hunter2-value") {
		t.Fatalf("spec.json still holds the env: %s", b)
	}
}

func TestRedactDockerArgs(t *testing.T) {
	got := redactEnvArgs([]string{"run", "-e", "DOCKER_TOKEN=abc", "-e", "PLAIN", "--env=X=y", "--workdir", "/w", "img", "-e", "ARG=after-image"})
	want := []string{"run", "-e", "DOCKER_TOKEN", "-e", "PLAIN", "--env=X", "--workdir", "/w", "img", "-e", "ARG=after-image"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("%q", got)
	}
}
