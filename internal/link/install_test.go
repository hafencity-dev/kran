package link

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
			os.WriteFile(filepath.Join(dir, asset), tarball(t, "kran", "FROM-GH"), 0o644)
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
