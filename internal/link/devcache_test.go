package link

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/hafencity-dev/kran/internal/paths"
)

// devCacheEnv gives the test its own binary cache, a release server that
// counts requests and a runner that records every command (gh, go build).
func devCacheEnv(t *testing.T) (Options, *[]string, *[]string) {
	t.Helper()
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	var mu sync.Mutex
	hits, cmds := &[]string{}, &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*hits = append(*hits, r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	src := t.TempDir() // a kran source checkout it must never build from
	os.WriteFile(filepath.Join(src, "go.mod"), []byte("module "+modulePath+"\n"), 0o644)
	o := Options{APIBase: srv.URL, DownloadBase: srv.URL, SourceDir: src,
		Runner: scripted{func(c Cmd) (Output, error) {
			mu.Lock()
			*cmds = append(*cmds, c.Name+" "+strings.Join(c.Args, " "))
			mu.Unlock()
			return Output{}, &ExitError{Code: 1}
		}}}
	return o, hits, cmds
}

func putCached(t *testing.T, ver, goos, goarch, content, sums string) string {
	t.Helper()
	bin := paths.BinaryCache(ver, goos, goarch)
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte(content), 0o755)
	if sums != "" {
		os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(bin)), SumsFile), []byte(sums), 0o644)
	}
	return bin
}

// TestDevVersionOnlyFromVerifiedCacheInvariant: kran upgrade --version of a
// build between releases (0.1.12-5-gf7c1755) takes it from the binary cache
// only, checked against the SHA256SUMS written at build time; it never
// downloads it and never builds it.
func TestDevVersionOnlyFromVerifiedCacheInvariant(t *testing.T) {
	const ver = "0.1.12-5-gf7c1755"
	goos, arch := "linux", "arm64"
	o, hits, cmds := devCacheEnv(t)
	fetch := func() (string, error) { return FetchRelease(context.Background(), o, ver, goos, arch) }

	if _, err := fetch(); err == nil || !strings.Contains(err.Error(), "not in the binary cache") || !strings.Contains(err.Error(), "make cache") {
		t.Fatalf("missing: %v", err)
	}
	bin := putCached(t, ver, goos, arch, "KRAN-0.1.12-5", "")
	if _, err := fetch(); err == nil || !strings.Contains(err.Error(), "has no SHA256SUMS") {
		t.Fatalf("no digest file: %v", err)
	}
	putCached(t, ver, goos, arch, "KRAN-0.1.12-5", sha256hex([]byte("something else"))+"  linux-arm64/kran\n")
	if _, err := fetch(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("digest mismatch: %v", err)
	}
	putCached(t, ver, goos, arch, "KRAN-0.1.12-5", sha256hex([]byte("KRAN-0.1.12-5"))+"  darwin-arm64/kran\n")
	if _, err := fetch(); err == nil || !strings.Contains(err.Error(), "names no linux-arm64/kran") {
		t.Fatalf("other platform only: %v", err)
	}
	putCached(t, ver, goos, arch, "KRAN-0.1.12-5", sha256hex([]byte("KRAN-0.1.12-5"))+"  linux-arm64/kran\n")
	if p, err := fetch(); err != nil || p != bin {
		t.Fatalf("verified: %q %v", p, err)
	}
	if len(*hits) != 0 || len(*cmds) != 0 {
		t.Fatalf("a development version was looked for elsewhere: requests %v, commands %v", *hits, *cmds)
	}
	for _, v := range []string{"0.1.12-5-gf7c1755-dirty", "0.1.12-5-g/../x", "0.1.12-5-g..", "0.1.12--5-gabc"} {
		if _, err := FetchRelease(context.Background(), o, v, goos, arch); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

// TestUpgradeTakesTheVersionAskedNotTheRunningOne: a peer upgraded (or taken
// back) to another version than this machine runs gets that version from the
// cache, never this machine's own binary, and never a fresh build.
func TestUpgradeTakesTheVersionAskedNotTheRunningOne(t *testing.T) {
	const want, running = "0.1.10-6-gc9403ef", "0.1.12-5-gf7c1755"
	o, hits, cmds := devCacheEnv(t)
	self := filepath.Join(t.TempDir(), "kran")
	os.WriteFile(self, []byte("RUNNING"), 0o755)
	bin := putCached(t, want, runtime.GOOS, runtime.GOARCH, "OLD", sha256hex([]byte("OLD"))+"  "+runtime.GOOS+"-"+runtime.GOARCH+"/kran\n")
	o.Version, o.SelfPath, o.SelfVersion, o.CacheOnly = want, self, running, true
	src := binarySource{o: &o, r: o.Runner}
	if p, err := src.acquire(context.Background(), runtime.GOOS, runtime.GOARCH); err != nil || p != bin {
		t.Fatalf("got %q %v, want the cached %s", p, err, bin)
	}
	o.SelfVersion = want // the running binary is the one asked for: it may go
	if p, err := src.acquire(context.Background(), runtime.GOOS, runtime.GOARCH); err != nil || p != self {
		t.Fatalf("same version: %q %v", p, err)
	}
	if len(*hits) != 0 || len(*cmds) != 0 {
		t.Fatalf("requests %v, commands %v", *hits, *cmds)
	}
}

// TestExactVersionInstallsAnOlderOne: --version installs exactly that
// version on a peer, also an older one (the way back); without it an older
// release never replaces a newer one.
func TestExactVersionInstallsAnOlderOne(t *testing.T) {
	for _, c := range []struct {
		remote, want string
		exact, need  bool
	}{
		{"0.1.12", "0.1.11", false, false},
		{"0.1.12", "0.1.11", true, true},
		{"0.1.11", "0.1.12", false, true},
		{"0.1.12-5-gf7c1755", "0.1.10-6-gc9403ef", true, true},
		{"0.1.10-6-gc9403ef", "0.1.10-6-gc9403ef", true, false},
	} {
		if got := needsInstall(c.remote, c.want, c.exact); got != c.need {
			t.Errorf("%s -> %s exact %v: install %v, want %v", c.remote, c.want, c.exact, got, c.need)
		}
	}
}
