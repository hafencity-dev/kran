package link

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/paths"
)

func otherPlatform() (string, string) {
	if runtime.GOARCH == "arm64" {
		return runtime.GOOS, "amd64"
	}
	return runtime.GOOS, "arm64"
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func tarball(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestAcquireOrder(t *testing.T) {
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	goos, arch := otherPlatform()
	self := filepath.Join(t.TempDir(), "kran")
	os.WriteFile(self, []byte("self"), 0o755)
	o := &Options{Version: "1.2.3", SelfPath: self, SourceDir: t.TempDir()}
	ctx := context.Background()

	// same platform: ourselves
	if p, err := (binarySource{o, scripted{func(Cmd) (Output, error) { return Output{}, fmt.Errorf("must not run") }}}).acquire(ctx, runtime.GOOS, runtime.GOARCH); err != nil || p != self {
		t.Fatal(p, err)
	}

	// nothing available: error lists the three options
	none := scripted{func(c Cmd) (Output, error) { return Output{}, &ExitError{Code: 1, Stderr: "no " + c.Name} }}
	_, err := binarySource{o, none}.acquire(ctx, goos, arch)
	if err == nil || !strings.Contains(err.Error(), "make install") || !strings.Contains(err.Error(), "gh auth login") || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("%v", err)
	}

	// gh success
	asset := assetName("1.2.3", goos, arch)
	var ghCalls [][]string
	gh := scripted{func(c Cmd) (Output, error) {
		if c.Name != "gh" {
			return Output{}, &ExitError{Code: 1}
		}
		ghCalls = append(ghCalls, c.Args)
		if c.Args[0] == "release" {
			dir := c.Args[len(c.Args)-2]
			tgz := tarball(t, "kran", "FROM-GH")
			os.WriteFile(filepath.Join(dir, asset), tgz, 0o644)
			os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sha256hex(tgz)+"  "+asset+"\n"), 0o644)
		}
		return Output{}, nil
	}}
	p, err := binarySource{o, gh}.acquire(ctx, goos, arch)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "FROM-GH" || p != paths.BinaryCache("1.2.3", goos, arch) {
		t.Fatalf("%s %q", p, b)
	}
	if got := strings.Join(ghCalls[1], " "); !strings.Contains(got, "release download v1.2.3 -R hafencity-dev/kran -p "+asset) {
		t.Fatal(got)
	}

	// cache hit wins over everything (no commands run)
	if p2, err := (binarySource{o, scripted{func(Cmd) (Output, error) { return Output{}, fmt.Errorf("must not run") }}}).acquire(ctx, goos, arch); err != nil || p2 != p {
		t.Fatal(p2, err)
	}
}

func TestAcquireTokenAPI(t *testing.T) {
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	goos, arch := otherPlatform()
	asset := assetName("1.2.3", goos, arch)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		switch r.URL.Path {
		case "/a/1":
			fmt.Fprintf(w, "%s  %s\n", sha256hex(tarball(t, "kran", "FROM-API")), asset)
		case "/repos/hafencity-dev/kran/releases/tags/v1.2.3":
			fmt.Fprintf(w, `{"assets":[{"name":"checksums.txt","url":"%s/a/1"},{"name":%q,"url":"%s/a/2"}]}`, srv.URL, asset, srv.URL)
		case "/a/2":
			if r.Header.Get("Accept") != "application/octet-stream" {
				http.Error(w, "accept", 406)
				return
			}
			w.Write(tarball(t, "kran", "FROM-API"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	noGh := scripted{func(c Cmd) (Output, error) { return Output{}, &ExitError{Code: 1, Stderr: "gh: not logged in"} }}
	o := &Options{Version: "1.2.3", SelfPath: "/x/kran", SourceDir: t.TempDir(), GitHubToken: "tok", APIBase: srv.URL, HTTPClient: srv.Client()}
	p, err := binarySource{o, noGh}.acquire(context.Background(), goos, arch)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "FROM-API" {
		t.Fatalf("%q", b)
	}
	// bad token explains
	o2 := *o
	o2.GitHubToken = "wrong"
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	if _, err := (binarySource{&o2, noGh}).acquire(context.Background(), goos, arch); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("%v", err)
	}
}

func TestAcquireSourceBuild(t *testing.T) {
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	goos, arch := otherPlatform()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/hafencity-dev/kran\n"), 0o644)
	var got Cmd
	r := scripted{func(c Cmd) (Output, error) {
		if c.Name == "go" && c.Args[0] == "build" {
			got = c
			for i, a := range c.Args {
				if a == "-o" {
					os.WriteFile(c.Args[i+1], []byte("BUILT"), 0o755)
				}
			}
		}
		return Output{}, nil
	}}
	// development build: source wins (no gh/cache lookups first)
	o := &Options{Version: "dev", SelfPath: "/x/kran", SourceDir: root}
	p, err := binarySource{o, r}.acquire(context.Background(), goos, arch)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "BUILT" || got.Dir != root {
		t.Fatalf("%q %+v", b, got)
	}
	env := strings.Join(got.Env, " ")
	if !strings.Contains(env, "CGO_ENABLED=0") || !strings.Contains(env, "GOOS="+goos) || !strings.Contains(env, "GOARCH="+arch) {
		t.Fatal(env)
	}
	// without go: skipped with a reason
	noGo := scripted{func(c Cmd) (Output, error) { return Output{}, &ExitError{Code: 127} }}
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	if _, err := (binarySource{o, noGo}).acquire(context.Background(), goos, arch); err == nil || !strings.Contains(err.Error(), "go is not installed") {
		t.Fatalf("%v", err)
	}
}

func TestFindSourceRoot(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/hafencity-dev/kran\n\ngo 1.26\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	if got := findSourceRoot(filepath.Join(root, "bin", "kran")); got != root {
		t.Fatal(got)
	}
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "go.mod"), []byte("module example.com/x\n"), 0o644)
	if got := findSourceRoot(filepath.Join(other, "kran")); got == other {
		t.Fatal("matched foreign module")
	}
}

func TestExtractKranRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.tar.gz")
	os.WriteFile(p, []byte("not gzip"), 0o644)
	if err := extractKran(p, filepath.Join(t.TempDir(), "kran")); err == nil {
		t.Fatal("expected error")
	}
	os.WriteFile(p, tarball(t, "other", "x"), 0o644)
	if err := extractKran(p, filepath.Join(t.TempDir(), "kran")); err == nil || !strings.Contains(err.Error(), "no kran binary") {
		t.Fatal(err)
	}
}

func TestPushLinuxBinaryForMacRemote(t *testing.T) {
	w := newWorld(t)
	goos, arch := "linux", "amd64"
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		arch = "arm64"
	}
	cache := paths.BinaryCache("1.2.3", goos, arch)
	os.MkdirAll(filepath.Dir(cache), 0o755)
	os.WriteFile(cache, []byte("LINUX-KRAN"), 0o755)
	c := newSSH(&w.opts)
	defer c.exitMaster()
	info := &Info{Version: "1.2.3", CacheDir: filepath.Join(w.remoteHome, "Caches", "kran")}
	src := binarySource{o: &w.opts, r: w.runner}
	changed := false
	if note := pushLinuxBinary(context.Background(), c, src, info, arch, &changed); note != "" {
		t.Fatal(note)
	}
	dst := filepath.Join(info.CacheDir, "bin", "1.2.3", "linux-"+arch, "kran")
	if b, _ := os.ReadFile(dst); string(b) != "LINUX-KRAN" || !changed {
		t.Fatalf("%q %v", b, changed)
	}
	// unchanged content is not copied again
	changed = false
	n := w.runner.sawInstall()
	pushLinuxBinary(context.Background(), c, src, info, arch, &changed)
	if changed || w.runner.sawInstall() != n {
		t.Fatal("re-uploaded identical binary")
	}
}

// publicRelease serves a public release: assets at /<repo>/releases/download/v<ver>/<name>
// and the latest tag at the API path, all without credentials. It fails the
// test when a request carries an Authorization header.
func publicRelease(t *testing.T, ver string, files map[string][]byte, hits *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("anonymous request carried credentials: %s", r.URL.Path)
		}
		if hits != nil {
			*hits = append(*hits, r.URL.Path)
		}
		if r.URL.Path == "/repos/hafencity-dev/kran/releases/latest" {
			fmt.Fprintf(w, `{"tag_name":"v%s"}`, ver)
			return
		}
		prefix := "/hafencity-dev/kran/releases/download/v" + ver + "/"
		if strings.HasPrefix(r.URL.Path, prefix) {
			if b, ok := files[strings.TrimPrefix(r.URL.Path, prefix)]; ok {
				w.Write(b)
				return
			}
		}
		http.NotFound(w, r)
	}))
}

func TestFetchAnonymous(t *testing.T) {
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	goos, arch := otherPlatform()
	asset := assetName("4.0.0", goos, arch)
	tgz := tarball(t, "kran", "ANON")
	mustNotRun := scripted{func(c Cmd) (Output, error) { return Output{}, fmt.Errorf("must not run %s", c.Name) }}

	// success: no gh, no token
	srv := publicRelease(t, "4.0.0", map[string][]byte{asset: tgz, "checksums.txt": []byte(sha256hex(tgz) + "  " + asset + "\n")}, nil)
	defer srv.Close()
	o := Options{Runner: mustNotRun, DownloadBase: srv.URL, HTTPClient: srv.Client()}
	p, err := FetchRelease(context.Background(), o, "4.0.0", goos, arch)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "ANON" {
		t.Fatalf("%q", b)
	}

	// checksum mismatch is refused and not cached
	bad := publicRelease(t, "4.1.0", map[string][]byte{
		assetName("4.1.0", goos, arch): tgz,
		"checksums.txt":                []byte(strings.Repeat("0", 64) + "  " + assetName("4.1.0", goos, arch) + "\n"),
	}, nil)
	defer bad.Close()
	o.DownloadBase, o.HTTPClient = bad.URL, bad.Client()
	if _, err := FetchRelease(context.Background(), o, "4.1.0", goos, arch); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(paths.BinaryCache("4.1.0", goos, arch)); err == nil {
		t.Fatal("unverified download must not be cached")
	}
}

func TestFetchAnonymousFailureFallsBack(t *testing.T) {
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	goos, arch := otherPlatform()
	asset := assetName("5.0.0", goos, arch)
	tgz := tarball(t, "kran", "VIA-GH")
	empty := publicRelease(t, "5.0.0", nil, nil) // anonymous: everything 404s
	defer empty.Close()
	gh := scripted{func(c Cmd) (Output, error) {
		if c.Name == "gh" && c.Args[0] == "release" {
			dir := c.Args[len(c.Args)-2]
			if c.Args[6] == asset {
				os.WriteFile(filepath.Join(dir, asset), tgz, 0o644)
			} else {
				os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sha256hex(tgz)+"  "+asset+"\n"), 0o644)
			}
		}
		return Output{}, nil
	}}
	o := Options{Runner: gh, DownloadBase: empty.URL, HTTPClient: empty.Client()}
	p, err := FetchRelease(context.Background(), o, "5.0.0", goos, arch)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "VIA-GH" {
		t.Fatalf("%q", b)
	}

	// no gh, no token: the error names the anonymous failure and the fallbacks
	o = Options{Runner: scripted{func(Cmd) (Output, error) { return Output{}, &ExitError{Code: 1} }}, DownloadBase: empty.URL, HTTPClient: empty.Client()}
	_, err = FetchRelease(context.Background(), o, "5.1.0", goos, arch)
	if err == nil || !strings.Contains(err.Error(), "anonymous download failed") || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatal(err)
	}
}

func TestLatestReleaseAnonymous(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	var hits []string
	srv := publicRelease(t, "6.0.0", nil, &hits)
	defer srv.Close()
	mustNotRun := scripted{func(c Cmd) (Output, error) { return Output{}, fmt.Errorf("must not run %s", c.Name) }}
	v, err := LatestRelease(context.Background(), Options{Runner: mustNotRun, APIBase: srv.URL, HTTPClient: srv.Client()})
	if err != nil || v != "6.0.0" {
		t.Fatal(v, err)
	}
	// rate limited, no gh, no token: says so
	rl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "limit", 403) }))
	defer rl.Close()
	noGh := scripted{func(Cmd) (Output, error) { return Output{}, &ExitError{Code: 1} }}
	_, err = LatestRelease(context.Background(), Options{Runner: noGh, APIBase: rl.URL, HTTPClient: rl.Client()})
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatal(err)
	}
}
