package execenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// fakeDocker is a docker CLI stand-in for the image lifecycle: "build"
// takes until the gate file exists, then records the image.
func fakeDocker(t *testing.T) (dk *dockerEnv, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := `#!/bin/sh
d="` + dir + `"
case "$1 $2" in
"image inspect") [ -f "$d/built" ] && { cat "$d/built"; exit 0; }; echo "No such image" >&2; exit 1 ;;
"image ls") cat "$d/images" 2>/dev/null; exit 0 ;;
"image rm") echo "$3" >> "$d/removed"; exit 0 ;;
"image prune") echo "$*" >> "$d/pruned"; echo "Total reclaimed space: 0B"; exit 0 ;;
esac
if [ "$1" = build ]; then
  echo x >> "$d/builds"
  while [ ! -f "$d/gate" ]; do sleep 0.02; done
  label=""
  for a in "$@"; do case "$a" in kran.dockerfile=*) label="${a#kran.dockerfile=}" ;; esac; done
  echo "$label" > "$d/built"
  exit 0
fi
echo "unexpected: $*" >&2; exit 1
`
	cli := filepath.Join(dir, "docker")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &dockerEnv{cli: cli, cliEnv: []string{"PATH=/usr/bin:/bin"}}, dir
}

// forgetImage drops what the manager learned from this machine's real
// Docker at construction.
func forgetImage(m *manager) {
	m.img.mu.Lock()
	m.img.ready, m.img.building, m.img.err, m.img.lastPruned = false, nil, nil, time.Time{}
	m.img.mu.Unlock()
}

func TestRunnerImageIsBuiltOnceOnFirstUse(t *testing.T) {
	te := newTestEnv(t)
	m := te.m
	dk, dir := fakeDocker(t)
	forgetImage(m)
	m.rtMu.Lock()
	m.runtimes = []proto.Runtime{{Kind: proto.RuntimeContainer, Available: true, FirstUse: true}}
	m.rtMu.Unlock()
	if m.imageBuilt(context.Background(), dk) {
		t.Fatal("no image yet")
	}

	// A job that stops waiting does not cancel the build.
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := m.ensureImage(short, dk, nil)
	cancel()
	if !errors.Is(err, ErrRuntimeUnavailable) || !strings.Contains(err.Error(), "still being prepared") {
		t.Fatalf("impatient job: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var phases []proto.Phase
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = m.ensureImage(context.Background(), dk, func(p proto.Phase) {
				mu.Lock()
				phases = append(phases, p)
				mu.Unlock()
			})
		}()
	}
	// All three wait behind the one build (each reports the phase first).
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		n := len(phases)
		mu.Unlock()
		if n == len(errs) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d jobs waiting for the build", n, len(errs))
		}
	}
	os.WriteFile(filepath.Join(dir, "gate"), nil, 0o600)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "builds")); string(b) != "x\n" {
		t.Fatalf("builds %q, want exactly one", b)
	}
	if len(phases) == 0 || phases[0].State != proto.JobSetup || phases[0].Message != "preparing the Linux runtime (first use, ~30 s)" {
		t.Fatalf("phases %+v", phases)
	}
	if rts := m.Runtimes(context.Background()); rts[0].FirstUse {
		t.Fatal("runtime still reports first use after the build")
	}
	if !m.imageBuilt(context.Background(), dk) {
		t.Fatal("image not remembered")
	}
	// Ready: no phase, no build.
	called := false
	if err := m.ensureImage(context.Background(), dk, func(proto.Phase) { called = true }); err != nil || called {
		t.Fatalf("second use: %v, phase %v", err, called)
	}
}

func TestPruneImagesRemovesOtherVersionsOnly(t *testing.T) {
	te := newTestEnv(t)
	m := te.m
	dk, dir := fakeDocker(t)
	forgetImage(m)
	m.rtMu.Lock()
	m.dk = dk
	m.rtMu.Unlock()
	cur := m.imageTag()
	list := strings.Join([]string{cur, "kran-test-runner:0.0.1", "kran-test-runner:<none>", "other:1", "kran-test-sync:1"}, "\n") + "\n"
	os.WriteFile(filepath.Join(dir, "images"), []byte(list), 0o600)
	if err := m.pruneImages(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "removed"))
	if string(b) != "kran-test-runner:0.0.1\n" {
		t.Fatalf("removed %q", b)
	}
	// Dangling images of this namespace (rebuilds of the same tag) go too.
	if b, _ := os.ReadFile(filepath.Join(dir, "pruned")); string(b) != "image prune --force --filter label=kran.ns=kran-test\n" {
		t.Fatalf("pruned %q", b)
	}
	// Rate-limited: the next reaper pass does not list again.
	os.Remove(filepath.Join(dir, "removed"))
	m.pruneImages(context.Background())
	if _, err := os.Stat(filepath.Join(dir, "removed")); err == nil {
		t.Fatal("pruned again within the hour")
	}
}

func TestPruneSummary(t *testing.T) {
	out := "Deleted Images:\nuntagged: kran-runner@sha256:ab\ndeleted: sha256:01\ndeleted: sha256:02\n\nTotal reclaimed space: 1.2GB\n"
	if got := pruneSummary(out); got != "2 layers or images, 1.2GB reclaimed" {
		t.Errorf("%q", got)
	}
	if got := pruneSummary("Total reclaimed space: 0B\n"); got != "" {
		t.Errorf("%q", got)
	}
}

func TestStaleRunnerImages(t *testing.T) {
	got := staleRunnerImages("kran-runner:1.2.0\nkran-runner:1.1.0\nkran-runner-x:1\nkran-runner:<none>\n\n", "kran-runner", "kran-runner:1.2.0")
	if !reflect.DeepEqual(got, []string{"kran-runner:1.1.0"}) {
		t.Fatalf("%v", got)
	}
}
