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
	// Also when this machine runs the version asked for: the running file
	// is checked against nothing, the cache copy against its SHA256SUMS.
	o.SelfVersion = want
	if p, err := src.acquire(context.Background(), runtime.GOOS, runtime.GOARCH); err != nil || p != bin {
		t.Fatalf("same version: %q %v, want the verified cache copy", p, err)
	}
	// A cache copy that does not match its SHA256SUMS is refused on the
	// peer path too (review R5-2).
	os.WriteFile(bin, []byte("TAMPERED"), 0o755)
	if p, err := src.acquire(context.Background(), runtime.GOOS, runtime.GOARCH); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered cache copy pushed: %q %v", p, err)
	}
	if len(*hits) != 0 || len(*cmds) != 0 {
		t.Fatalf("requests %v, commands %v", *hits, *cmds)
	}
}

// TestVersionGrammarInvariant: the version string is checked before any
// file is touched with it, on every path (here, a peer, the cache), and
// independent of any digest.
func TestVersionGrammarInvariant(t *testing.T) {
	for _, v := range []string{"1.4.0", "v1.4.0", "1.4.0-rc1", "1.4.0-rc.1", "0.1.12-5-gf7c1755", "1.4.0-rc1-3-gabc123"} {
		if err := ValidVersion(v); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{"", "0.1.12-dirty", "0.1.12-5-gf7c1755-dirty", "0.1.12-DIRTY", "../outside", "0.1.12-5-gf7c1755/../../x",
		"0.1.12/x", "0.1.12\\x", "0.1.12 x", "0.1.12\tx", "-0.1.12", "--version", "0.1.12-..", "0.1", "0.1.12-5-gXYZ", "0.1.12-5-g", "1.4.0-a-b-c"} {
		err := ValidVersion(v)
		if err == nil {
			t.Errorf("%q accepted", v)
		} else if !strings.Contains(err.Error(), "x.y.z-N-g<hex>") {
			t.Errorf("%q: the message does not give the grammar: %v", v, err)
		}
	}
	// The peer path refuses them before it builds a path (no cache file
	// planted beside them could make them pass).
	o, _, _ := devCacheEnv(t)
	for _, v := range []string{"../outside", "0.1.12-6-g072ef3a-dirty", "0.1.12-dirty"} {
		putCached(t, "x", "linux", "arm64", "X", "")
		o.Version, o.CacheOnly = v, true
		if _, err := (binarySource{o: &o, r: o.Runner}).acquire(context.Background(), "linux", "arm64"); err == nil || !strings.Contains(err.Error(), "not a kran version") {
			t.Errorf("peer path %q: %v", v, err)
		}
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

// TestPushBinaryChecksTheCopyOnThePeer: a copy whose digest on the peer is
// not the one sent never replaces the peer's binary.
func TestPushBinaryChecksTheCopyOnThePeer(t *testing.T) {
	w := newWorld(t)
	c := newSSH(&w.opts)
	defer c.exitMaster()
	dir := filepath.Join(w.remoteHome, "pushdir")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "kran"), []byte("OLD"), 0o755)
	src := filepath.Join(t.TempDir(), "kran")
	os.WriteFile(src, []byte("NEW"), 0o755)
	old := pushDigest
	t.Cleanup(func() { pushDigest = old })
	pushDigest = func(string) (string, error) { return sha256hex([]byte("NEW, as it should have arrived")), nil }
	if err := c.pushBinary(context.Background(), src, dir); err == nil {
		t.Fatal("a copy that does not match was installed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kran")); string(b) != "OLD" {
		t.Fatalf("the old binary was replaced: %q", b)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("left behind: %v", ents)
	}
	pushDigest = old
	if err := c.pushBinary(context.Background(), src, dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kran")); string(b) != "NEW" {
		t.Fatalf("the good copy was not installed: %q", b)
	}
}

// TestExactUpgradeFailsLoudly (review F5-1): with --version a peer either
// ends up with exactly that version, binary and daemon, or the upgrade
// fails and says what runs there: never "already runs" with a note.
func TestExactUpgradeFailsLoudly(t *testing.T) {
	w := newWorld(t)
	w.start(true)
	ctx := context.Background()
	if _, err := Link(ctx, w.opts); err != nil {
		t.Fatal(err)
	}
	const target = "1.2.4-1-gabc1234"
	sd := w.remoteEnv["KRAN_STATE_DIR"]
	// The new build is the test binary with a byte more (runs the same).
	newBin := filepath.Join(t.TempDir(), "kran")
	copyFile(t, w.self, newBin)
	f, _ := os.OpenFile(newBin, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\nbuild " + target)
	f.Close()
	oldSum, _ := fileSHA256(w.self)
	newSum, _ := fileSHA256(newBin)
	os.MkdirAll(sd, 0o700)
	os.WriteFile(filepath.Join(sd, "fake-versions"), []byte(oldSum+" 1.2.3\n"+newSum+" "+target+"\n"), 0o600)
	os.WriteFile(filepath.Join(sd, "daemon-version"), []byte("1.2.3\n"), 0o600)
	o := Options{Version: target, SelfPath: w.self, SelfVersion: "1.2.3", Runner: w.runner, CacheOnly: true, Exact: true}

	// 1. No verified copy here: a failure, nothing installed there.
	_, err := Upgrade(ctx, "devbox", o)
	if err == nil || !strings.Contains(err.Error(), "nothing was installed on") || !strings.Contains(err.Error(), "still 1.2.3") {
		t.Fatalf("no verified copy: %v", err)
	}

	// 2. A verified copy, but the daemon there stays on the old version.
	goos, goarch := runtime.GOOS, runtime.GOARCH
	bin := paths.BinaryCache(target, goos, goarch)
	os.MkdirAll(filepath.Dir(bin), 0o755)
	copyFile(t, newBin, bin)
	os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(bin)), SumsFile), []byte(newSum+"  "+goos+"-"+goarch+"/kran\n"), 0o644)
	os.WriteFile(filepath.Join(sd, "fake-daemon-stuck"), []byte("1.2.3"), 0o600)
	res, err := Upgrade(ctx, "devbox", o)
	if err == nil || !strings.Contains(err.Error(), "daemon reports 1.2.3 after the restart, not "+target) {
		t.Fatalf("stuck daemon: %+v %v", res, err)
	}

	// 3. The daemon follows: success, and the result says what runs there.
	os.Remove(filepath.Join(sd, "fake-daemon-stuck"))
	res, err = Upgrade(ctx, "devbox", o)
	if err != nil || res.Daemon != target || res.To != target {
		t.Fatalf("good upgrade: %+v %v", res, err)
	}
}

// TestExactUpgradeOfAMacNeedsItsLinuxRuntime: with --version, a Mac peer is
// changed only when the linux runtime of that version can go with it (its
// container jobs need it); without one, nothing is installed there.
func TestExactUpgradeOfAMacNeedsItsLinuxRuntime(t *testing.T) {
	w := newWorld(t)
	w.start(true)
	ctx := context.Background()
	if _, err := Link(ctx, w.opts); err != nil {
		t.Fatal(err)
	}
	const target = "1.2.4-1-gabc1234"
	sd := w.remoteEnv["KRAN_STATE_DIR"]
	newBin := filepath.Join(t.TempDir(), "kran")
	copyFile(t, w.self, newBin)
	f, _ := os.OpenFile(newBin, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\nbuild " + target)
	f.Close()
	oldSum, _ := fileSHA256(w.self)
	newSum, _ := fileSHA256(newBin)
	os.MkdirAll(sd, 0o700)
	os.WriteFile(filepath.Join(sd, "fake-versions"), []byte(oldSum+" 1.2.3\n"+newSum+" "+target+"\n"), 0o600)
	os.WriteFile(filepath.Join(sd, "daemon-version"), []byte("1.2.3\n"), 0o600)
	// The peer says it is a Mac (the commands still run here).
	fake := t.TempDir()
	os.WriteFile(filepath.Join(fake, "uname"), []byte("#!/bin/sh\necho 'Darwin arm64'\n"), 0o755)
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	// This machine's cache has the darwin binary of the version, not its linux one.
	dbin := paths.BinaryCache(target, "darwin", "arm64")
	os.MkdirAll(filepath.Dir(dbin), 0o755)
	copyFile(t, newBin, dbin)
	sums := filepath.Join(filepath.Dir(filepath.Dir(dbin)), SumsFile)
	os.WriteFile(sums, []byte(newSum+"  darwin-arm64/kran\n"), 0o644)
	remoteBin := filepath.Join(w.remoteHome, "bin", "kran")
	before, _ := fileSHA256(remoteBin)
	o := Options{Version: target, SelfPath: w.self, SelfVersion: "1.2.3", Runner: w.runner, CacheOnly: true, Exact: true}

	_, err := Upgrade(ctx, "devbox", o)
	if err == nil || !strings.Contains(err.Error(), "could not provide the linux runtime of kran "+target) || !strings.Contains(err.Error(), "nothing was installed on devbox") {
		t.Fatalf("no linux runtime: %v", err)
	}
	if after, _ := fileSHA256(remoteBin); after != before {
		t.Fatal("the Mac's binary was replaced although its linux runtime could not go with it")
	}

	// With the linux runtime too: both go, and the daemon reports the version.
	lbin := paths.BinaryCache(target, "linux", "arm64")
	os.MkdirAll(filepath.Dir(lbin), 0o755)
	copyFile(t, newBin, lbin)
	os.WriteFile(sums, []byte(newSum+"  darwin-arm64/kran\n"+newSum+"  linux-arm64/kran\n"), 0o644)

	// The copy of the linux runtime fails there: a failure, not a note.
	rdir := filepath.Join(w.remoteEnv["KRAN_CACHE_DIR"], "bin", target)
	os.MkdirAll(rdir, 0o755)
	os.Chmod(rdir, 0o555)
	_, err = Upgrade(ctx, "devbox", o)
	os.Chmod(rdir, 0o755)
	if err == nil || !strings.Contains(err.Error(), "could not copy the linux binary") || !strings.Contains(err.Error(), "its daemon was not restarted") {
		t.Fatalf("failed linux copy: %v", err)
	}

	res, err := Upgrade(ctx, "devbox", o)
	if err != nil || res.Daemon != target {
		t.Fatalf("Mac with both binaries: %+v %v", res, err)
	}
	if after, _ := fileSHA256(remoteBin); after != newSum {
		t.Fatal("the Mac's binary is not the version asked for")
	}
	if got, _ := fileSHA256(filepath.Join(rdir, "linux-arm64", "kran")); got != newSum {
		t.Fatal("the Mac's linux runtime is not the version asked for")
	}
}
