package execenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestEnospcScan(t *testing.T) {
	var e enospcScan
	if e.see([]byte("write /tmp/x: No space left")) {
		t.Fatal("matched half a message")
	}
	if !e.see([]byte(" on device\n")) {
		t.Fatal("missed a message split across chunks")
	}
	var f enospcScan
	if f.see([]byte(strings.Repeat("ok\n", 1000))) || !f.see([]byte("npm ERR! code ENOSPC")) {
		t.Fatal("ENOSPC")
	}
}

// A failed job whose output says a disk was full is marked disk short; a
// failure without it, and a success with it, are not.
func TestDiskShortExit(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, nil, proto.RuntimeNative)
	cases := []struct {
		script string
		short  bool
	}{
		{"echo 'write /tmp/a: no space left on device' >&2; exit 1", true},
		{"echo 'FAIL x'; exit 1", false},
		{"echo 'No space left on device (handled)'; exit 0", false},
	}
	for _, c := range cases {
		p := te.start(ref, sh(c.script))
		exit := waitExit(t, p, 30*time.Second)
		if exit.DiskShort != c.short {
			t.Errorf("%q: disk short %v, want %v (%+v)", c.script, exit.DiskShort, c.short, exit)
		}
		if c.short && (!strings.HasPrefix(exit.Reason, "disk short: ") || exit.Code != 1) {
			t.Errorf("%q: reason %q code %d", c.script, exit.Reason, exit.Code)
		}
	}
}

// The Docker disk (volume layout) is asked through the sync helper.
func TestDiskShortDockerDisk(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '     Avail 1B-blocks\\n%s 103865303040\\n' \"$FREE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &supervisor{spec: superSpec{Node: "devmac", DiskExec: []string{"exec", "kran-sync", "df"},
		Docker: &dockerStep{CLI: fake, Env: []string{"FREE=104857600"}}}}
	if r := s.diskShortReason(); !strings.Contains(r, "Docker disk on devmac had 100 MiB free") {
		t.Errorf("full Docker disk: %q", r)
	}
	s.spec.Docker.Env = []string{"FREE=53687091200"}
	if r := s.diskShortReason(); r != "" {
		t.Errorf("roomy Docker disk: %q", r)
	}
}
