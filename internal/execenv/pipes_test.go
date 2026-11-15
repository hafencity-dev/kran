package execenv

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestRuntimesReportPipeLimit: each Linux runtime carries
// fs.pipe-user-pages-soft of the kernel its jobs run under, for kran
// doctor: the host's on Linux, the Docker VM's (read through the sync
// helper with the slot check) when containers run in a VM.
func TestRuntimesReportPipeLimit(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$1 $3 $4\" = \"exec cat /proc/sys/fs/pipe-user-pages-soft\" ] && echo 16384\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	te := newTestEnv(t, func(o *Options, tun *tunables) { tun.forceVolume = true })
	m := te.m
	m.readVMPipePages(context.Background(), &dockerEnv{cli: fake})
	m.rtMu.Lock()
	m.runtimes = []proto.Runtime{
		{Kind: proto.RuntimeNative, OS: runtime.GOOS, Available: true},
		{Kind: proto.RuntimeContainer, OS: "linux", Available: true},
	}
	m.rtMu.Unlock()
	rts := m.Runtimes(context.Background())
	if rts[1].PipeUserPagesSoft != 16384 {
		t.Errorf("container runtime in a VM: %d, want the VM's 16384", rts[1].PipeUserPagesSoft)
	}
	if runtime.GOOS == "linux" {
		b, _ := os.ReadFile("/proc/sys/fs/pipe-user-pages-soft")
		if got := rts[0].PipeUserPagesSoft; got <= 0 || !strings.HasPrefix(string(b), itoa64(got)) {
			t.Errorf("native runtime: %d, the host has %s", got, b)
		}
	} else if rts[0].PipeUserPagesSoft != 0 {
		t.Errorf("macOS native runtime: %d", rts[0].PipeUserPagesSoft)
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
