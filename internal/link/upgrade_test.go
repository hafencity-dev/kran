package link

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/paths"
)

func TestLatestRelease(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	gh := scripted{func(c Cmd) (Output, error) {
		if c.Name == "gh" && c.Args[0] == "release" {
			return Output{Stdout: []byte("v0.4.2\n")}, nil
		}
		return Output{}, nil
	}}
	if v, err := LatestRelease(ctx, Options{Runner: gh}); err != nil || v != "0.4.2" {
		t.Fatal(v, err)
	}
	noGh := scripted{func(Cmd) (Output, error) { return Output{}, &ExitError{Code: 1} }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 404)
			return
		}
		fmt.Fprint(w, `{"tag_name":"v1.0.0"}`)
	}))
	defer srv.Close()
	if v, err := LatestRelease(ctx, Options{Runner: noGh, GitHubToken: "tok", APIBase: srv.URL, HTTPClient: srv.Client()}); err != nil || v != "1.0.0" {
		t.Fatal(v, err)
	}
	if _, err := LatestRelease(ctx, Options{Runner: noGh, GitHubToken: "bad", APIBase: srv.URL, HTTPClient: srv.Client()}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatal(err)
	}
	_, err := LatestRelease(ctx, Options{Runner: noGh})
	if err == nil || !strings.Contains(err.Error(), "no kran release has been published") || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatal(err)
	}
}

func TestFetchRelease(t *testing.T) {
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	goos, arch := otherPlatform()
	asset := assetName("2.0.0", goos, arch)
	tgz := tarball(t, "kran", "V2")
	good := func() string {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "x"), tgz, 0o644)
		s, _ := fileSHA256(filepath.Join(d, "x"))
		return s
	}()
	mk := func(sum string) scripted {
		return scripted{func(c Cmd) (Output, error) {
			if c.Name == "gh" && c.Args[0] == "release" {
				dir := c.Args[len(c.Args)-2]
				pat := c.Args[6]
				if pat == asset {
					os.WriteFile(filepath.Join(dir, asset), tgz, 0o644)
				} else {
					os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sum+"  "+asset+"\n"+"abc  other.tar.gz\n"), 0o644)
				}
			}
			return Output{}, nil
		}}
	}
	// o.Version is deliberately different: the version argument wins
	o := Options{Version: "9.9.9", Runner: mk("0000" + good[4:])}
	if _, err := FetchRelease(context.Background(), o, "v2.0.0", goos, arch); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(paths.BinaryCache("2.0.0", goos, arch)); err == nil {
		t.Fatal("corrupt download must not be cached")
	}
	o.Runner = mk(good)
	p, err := FetchRelease(context.Background(), o, "2.0.0", goos, arch)
	if err != nil || p != paths.BinaryCache("2.0.0", goos, arch) {
		t.Fatal(p, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "V2" {
		t.Fatalf("%q", b)
	}
	// cache hit: nothing runs
	o.Runner = scripted{func(Cmd) (Output, error) { return Output{}, fmt.Errorf("must not run") }}
	if p2, err := FetchRelease(context.Background(), o, "2.0.0", goos, arch); err != nil || p2 != p {
		t.Fatal(p2, err)
	}
	// A release without checksums.txt, or one that does not list the asset,
	// is refused: the binary is pushed to and run on every linked machine.
	asset21 := assetName("2.1.0", goos, arch)
	sums := func(content string) scripted {
		return scripted{func(c Cmd) (Output, error) {
			if c.Name == "gh" && c.Args[0] == "release" {
				dir := c.Args[len(c.Args)-2]
				if c.Args[6] == asset21 {
					os.WriteFile(filepath.Join(dir, asset21), tgz, 0o644)
				} else if content == "" {
					return Output{}, &ExitError{Code: 1, Stderr: "no asset"}
				} else {
					os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(content), 0o644)
				}
			}
			return Output{}, nil
		}}
	}
	o.Runner = sums("")
	if _, err := FetchRelease(context.Background(), o, "2.1.0", goos, arch); err == nil || !strings.Contains(err.Error(), "checksums.txt") {
		t.Fatalf("missing checksums.txt: %v", err)
	}
	o.Runner = sums(good + "  other.tar.gz\n")
	if _, err := FetchRelease(context.Background(), o, "2.1.0", goos, arch); err == nil || !strings.Contains(err.Error(), "checksums.txt") {
		t.Fatalf("asset not in checksums.txt: %v", err)
	}
	if _, err := os.Stat(paths.BinaryCache("2.1.0", goos, arch)); err == nil {
		t.Fatal("unverified download must not be cached")
	}
	// A version is a release tag, never a path or a flag.
	for _, v := range []string{"../../x", "1.2.3/../../x", "--repo=evil/kran", "1.2.3-a/b", ""} {
		if _, err := FetchRelease(context.Background(), o, v, goos, arch); err == nil || !strings.Contains(err.Error(), "not a release version") {
			t.Fatalf("%q: %v", v, err)
		}
	}
	// nothing available: helpful
	o.Runner = scripted{func(Cmd) (Output, error) { return Output{}, &ExitError{Code: 1} }}
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	if _, err := FetchRelease(context.Background(), o, "3.0.0", goos, arch); err == nil || !strings.Contains(err.Error(), "anonymous download failed") {
		t.Fatal(err)
	}
}

func TestUpgrade(t *testing.T) {
	w := newWorld(t)
	w.start(true)
	ctx := context.Background()
	if _, err := Link(ctx, w.opts); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(w.remoteEnv["KRAN_STATE_DIR"], "service.log")

	// same binary: nothing to install, no restart
	o := Options{Version: "1.2.3", SelfPath: w.self, Runner: w.runner}
	res, err := Upgrade(ctx, "devbox", o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || res.Restarted || res.Name != "devbox" {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(logf); err == nil {
		t.Fatal("restarted without a new binary")
	}

	// new binary: installed in place (dir taken from the saved peer) and service restarted
	newSelf := filepath.Join(t.TempDir(), "kran")
	copyFile(t, w.self, newSelf)
	f, _ := os.OpenFile(newSelf, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\nnew build")
	f.Close()
	o.SelfPath = newSelf
	res, err = Upgrade(ctx, "devbox", o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Installed || !res.Restarted || res.From == "" && res.To == "" {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(logf); strings.TrimSpace(string(b)) != "restart" {
		t.Fatalf("restart log %q", b)
	}
	got, _ := os.ReadFile(filepath.Join(w.remoteHome, "bin", "kran"))
	want, _ := os.ReadFile(newSelf)
	if string(got) != string(want) {
		t.Fatal("remote binary not updated")
	}

	// --force passthrough, service not installed tolerated
	newer := filepath.Join(t.TempDir(), "kran")
	copyFile(t, newSelf, newer)
	f, _ = os.OpenFile(newer, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("!")
	f.Close()
	o.SelfPath, o.Force = newer, true
	res, err = Upgrade(ctx, "devbox", o)
	if err != nil || !res.Restarted {
		t.Fatal(res, err)
	}
	if b, _ := os.ReadFile(logf); !strings.HasSuffix(strings.TrimSpace(string(b)), "restart --force") {
		t.Fatalf("%q", b)
	}
	os.WriteFile(filepath.Join(w.remoteEnv["KRAN_STATE_DIR"], "no-service"), nil, 0o600)
	f, _ = os.OpenFile(newer, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("!")
	f.Close()
	res, err = Upgrade(ctx, "devbox", o)
	if err != nil || res.Restarted || !res.Installed || len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "not installed") {
		t.Fatalf("%+v %v", res, err)
	}

	// keys untouched, unknown peer rejected
	if _, err := Upgrade(ctx, "nope", o); err == nil || !strings.Contains(err.Error(), "no linked machine") {
		t.Fatal(err)
	}
}

// Re-linking with a newer binary restarts the remote service, so its daemon
// does not keep running the old build.
func TestRelinkRestartsAfterUpdate(t *testing.T) {
	w := newWorld(t)
	w.start(true)
	ctx := context.Background()
	if _, err := Link(ctx, w.opts); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(w.remoteEnv["KRAN_STATE_DIR"], "service.log")
	if _, err := Link(ctx, w.opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logf); err == nil {
		t.Fatal("restarted without a new binary")
	}
	newSelf := filepath.Join(t.TempDir(), "kran")
	copyFile(t, w.self, newSelf)
	f, _ := os.OpenFile(newSelf, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\nnew build")
	f.Close()
	w.opts.SelfPath = newSelf
	if _, err := Link(ctx, w.opts); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(logf); strings.TrimSpace(string(b)) != "restart" {
		t.Fatalf("restart log %q", b)
	}
}
