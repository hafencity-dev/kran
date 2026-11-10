package execenv

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/version"
)

func cachePath(t *testing.T, ver, arch string) string {
	t.Helper()
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	return paths.BinaryCache(ver, "linux", arch)
}

func put(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
}

func TestCheckLinuxRuntime(t *testing.T) {
	const dev = "0.1.10-6-gc9403ef"
	t.Run("present and executable", func(t *testing.T) {
		p := cachePath(t, dev, runtime.GOARCH)
		put(t, p, 0o755)
		if got := CheckLinuxRuntime(p, dev); got != nil {
			t.Fatalf("want ok, got %+v", got)
		}
	})
	t.Run("missing development build", func(t *testing.T) {
		p := cachePath(t, dev, runtime.GOARCH)
		got := CheckLinuxRuntime(p, dev)
		if got == nil || got.Code != proto.ProblemLinuxBinaryMissing || got.Path != p || got.Version != dev || got.Release {
			t.Fatalf("got %+v", got)
		}
		if !strings.Contains(got.Fix, "no published release asset") || !strings.Contains(got.Fix, p) {
			t.Errorf("a development build must not promise a download: %q", got.Fix)
		}
		c := LinuxRuntimeCause(got)
		if want := "its linux runtime for " + dev + " is missing at " + p; !strings.HasPrefix(c, want) {
			t.Errorf("cause %q does not start with %q", c, want)
		}
	})
	t.Run("missing release names the upgrade", func(t *testing.T) {
		p := cachePath(t, "0.1.11", runtime.GOARCH)
		got := CheckLinuxRuntime(p, "v0.1.11")
		if got == nil || !got.Release || got.Version != "0.1.11" || !strings.Contains(got.Fix, "kran upgrade") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("not executable", func(t *testing.T) {
		p := cachePath(t, dev, runtime.GOARCH)
		put(t, p, 0o644)
		got := CheckLinuxRuntime(p, dev)
		if got == nil || got.Code != proto.ProblemLinuxBinaryNotExecutable || !strings.Contains(got.Fix, "chmod 755 "+p) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("not a file", func(t *testing.T) {
		p := cachePath(t, dev, runtime.GOARCH)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := CheckLinuxRuntime(p, dev); got == nil || got.Code != proto.ProblemLinuxBinaryNotFile {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("wrong arch dir", func(t *testing.T) {
		other := "amd64"
		if runtime.GOARCH == "amd64" {
			other = "arm64"
		}
		want := cachePath(t, dev, runtime.GOARCH)
		put(t, paths.BinaryCache(dev, "linux", other), 0o755)
		got := CheckLinuxRuntime(want, dev)
		if got == nil || got.Code != proto.ProblemLinuxBinaryMissing {
			t.Fatalf("a binary for the other architecture must not count: %+v", got)
		}
		if !strings.Contains(got.Fix, "linux-"+other) || !strings.Contains(got.Fix, "linux-"+runtime.GOARCH) {
			t.Errorf("fix should say which architecture is there and which is needed: %q", got.Fix)
		}
	})
	t.Run("linux needs none", func(t *testing.T) {
		if got := CheckLinuxRuntime("", dev); got != nil {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestLinuxRuntimePathFollowsVersionAndCache(t *testing.T) {
	if runtime.GOOS == "linux" {
		if LinuxRuntimeNeeded() || LinuxRuntimePath() != "" {
			t.Fatal("a Linux node runs containers with its own binary")
		}
		return
	}
	old := version.Version
	defer func() { version.Version = old }()
	version.Version = "v0.1.10-6-gc9403ef"
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	if got, want := LinuxRuntimePath(), paths.BinaryCache("0.1.10-6-gc9403ef", "linux", runtime.GOARCH); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

// volumeManager is a manager that behaves like a macOS node (the sync-helper
// volume, hence the cache binary) on any host.
func volumeManager(t *testing.T, bin string) (*testEnv, *syncLog) {
	t.Helper()
	te := newTestEnv(t, func(o *Options, tun *tunables) {
		o.LinuxBinary = bin
		o.Container = "on"
		tun.forceVolume = true
	})
	return te, te.log
}

func TestContainerRuntimeReportsMissingLinuxBinary(t *testing.T) {
	old := version.Version
	defer func() { version.Version = old }()
	version.Version = "0.1.10-6-gc9403ef"
	bin := cachePath(t, version.Version, runtime.GOARCH)
	te, lg := volumeManager(t, bin)

	dk := &dockerEnv{ver: dockerVersion{APIVersion: "1.45"}}
	rt, ok := te.m.containerRuntime(context.Background(), dk, "")
	if !ok || rt.Available || rt.Problem == nil || rt.Problem.Code != proto.ProblemLinuxBinaryMissing {
		t.Fatalf("a missing binary must make the runtime unavailable with a problem: %+v", rt)
	}
	if !strings.Contains(rt.Reason, "its linux runtime for 0.1.10-6-gc9403ef is missing at "+bin) {
		t.Errorf("reason %q", rt.Reason)
	}

	// Docker down as well: the node's own defect is still named.
	rt, _ = te.m.containerRuntime(context.Background(), nil, "Docker is not reachable")
	if rt.Problem == nil || !strings.HasPrefix(rt.Reason, "Docker is not reachable; and its linux runtime") {
		t.Errorf("docker down: %+v", rt)
	}

	// One log line per state, not one per refresh.
	for i := 0; i < 3; i++ {
		te.m.containerRuntime(context.Background(), dk, "")
	}
	out := lg.String()
	if n := strings.Count(out, "cannot run container jobs"); n != 1 {
		t.Errorf("want one log line, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, bin) || !strings.Contains(out, "native jobs are unaffected") {
		t.Errorf("log line must name the path and that native jobs run:\n%s", out)
	}

	// Fixed from outside: available again, and the log says so.
	put(t, bin, 0o755)
	rt, _ = te.m.containerRuntime(context.Background(), dk, "")
	if !rt.Available || rt.Problem != nil || rt.Reason != "" {
		t.Fatalf("after the fix: %+v", rt)
	}
	if !strings.Contains(lg.String(), "linux runtime for 0.1.10-6-gc9403ef found at "+bin) {
		t.Errorf("recovery not logged:\n%s", lg.String())
	}
}

func TestContainerRuntimeOffReportsNothing(t *testing.T) {
	bin := cachePath(t, "dev", runtime.GOARCH)
	te := newTestEnv(t, func(o *Options, tun *tunables) {
		o.LinuxBinary, o.Container = bin, "off"
		tun.forceVolume = true
	})
	rt, _ := te.m.containerRuntime(context.Background(), nil, "off")
	if rt.Problem != nil {
		t.Fatalf("container jobs switched off: nothing to report, got %+v", rt.Problem)
	}
}
