package cli

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

	"github.com/hafencity-dev/kran/internal/link"
	"github.com/hafencity-dev/kran/internal/paths"
)

type noGh struct{}

func (noGh) Run(context.Context, link.Cmd) (link.Output, error) {
	return link.Output{}, fmt.Errorf("no gh in tests")
}

func relTarball(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "kran", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// fakeRelease serves release v<ver> with the given assets (name -> bytes)
// and a checksums.txt listing them; badSum names assets whose listed sum is
// wrong.
func fakeRelease(t *testing.T, ver string, assets map[string][]byte, badSum ...string) *httptest.Server {
	t.Helper()
	var sums strings.Builder
	for name, b := range assets {
		h := sha256.Sum256(b)
		s := hex.EncodeToString(h[:])
		for _, bad := range badSum {
			if bad == name {
				s = strings.Repeat("0", 64)
			}
		}
		fmt.Fprintf(&sums, "%s  %s\n", s, name)
	}
	prefix := "/hafencity-dev/kran/releases/download/v" + ver + "/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, prefix)
		switch {
		case !strings.HasPrefix(r.URL.Path, prefix):
			http.NotFound(w, r)
		case name == "checksums.txt":
			w.Write([]byte(sums.String()))
		case assets[name] != nil:
			w.Write(assets[name])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type upgradeRig struct {
	u    *upgrader
	out  *bytes.Buffer
	self string
	log  *[]string
}

// rig builds the real upgrader for a node of the given OS with a fake
// release server; Restart and the peers are stubbed.
func rig(t *testing.T, goos, enabled string, assets map[string][]byte, badSum ...string) upgradeRig {
	t.Helper()
	t.Setenv("KRAN_CACHE_DIR", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	srv := fakeRelease(t, "0.5.0", assets, badSum...)
	self := filepath.Join(t.TempDir(), "kran")
	if err := os.WriteFile(self, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	a := &App{Out: out, Err: &bytes.Buffer{}}
	lo := link.Options{Version: "0.4.0", SelfPath: self, DownloadBase: srv.URL, HTTPClient: srv.Client(), Runner: noGh{}}
	u := a.upgraderFor(self, upgradeFlags{}, lo, goos, enabled)
	u.Current = "0.4.0"
	u.Latest = func(context.Context) (string, error) { return "", fmt.Errorf("tests never ask GitHub") }
	var log []string
	u.Restart = func(context.Context, bool) error {
		b, _ := os.ReadFile(self)
		log = append(log, "restart:"+string(b))
		return nil
	}
	u.Peers = func() []string { return nil }
	return upgradeRig{u, out, self, &log}
}

func releaseAssets(t *testing.T, goos string) map[string][]byte {
	arch := runtime.GOARCH
	return map[string][]byte{
		fmt.Sprintf("kran_0.5.0_%s_%s.tar.gz", goos, arch): relTarball(t, "NEW-"+goos),
		fmt.Sprintf("kran_0.5.0_linux_%s.tar.gz", arch):    relTarball(t, "NEW-linux"),
	}
}

func selfContent(t *testing.T, r upgradeRig) string {
	b, err := os.ReadFile(r.self)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpgradeDarwinFetchesBothBinaries(t *testing.T) {
	r := rig(t, "darwin", "auto", releaseAssets(t, "darwin"))
	if err := r.u.run(context.Background(), upgradeFlags{Version: "0.5.0"}); err != nil {
		t.Fatal(err)
	}
	if got := selfContent(t, r); got != "NEW-darwin" {
		t.Fatalf("native binary: %q", got)
	}
	// The path the daemon's CheckLinuxRuntime expects for the new version.
	lp := paths.BinaryCache("0.5.0", "linux", runtime.GOARCH)
	b, err := os.ReadFile(lp)
	if err != nil || string(b) != "NEW-linux" {
		t.Fatalf("linux binary at %s: %q %v", lp, b, err)
	}
	if fi, _ := os.Stat(lp); fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("linux binary not executable: %v", fi.Mode())
	}
	if _, err := os.Stat(lp + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}
	// Both were in place before the restart.
	if len(*r.log) != 1 || (*r.log)[0] != "restart:NEW-darwin" {
		t.Fatalf("%v", *r.log)
	}
	o := r.out.String()
	if !strings.Contains(o, "Installed kran 0.5.0 at "+r.self) || !strings.Contains(o, "Linux runtime for container jobs: kran 0.5.0 at "+lp) {
		t.Fatalf("outcome of both binaries not printed:\n%s", o)
	}
}

func TestUpgradeLinuxAssetFailsBeforeNativeIsReplaced(t *testing.T) {
	linuxName := fmt.Sprintf("kran_0.5.0_linux_%s.tar.gz", runtime.GOARCH)
	cases := map[string]upgradeRig{}
	cases["wrong checksum"] = rig(t, "darwin", "true", releaseAssets(t, "darwin"), linuxName)
	missing := releaseAssets(t, "darwin")
	delete(missing, linuxName)
	cases["missing asset"] = rig(t, "darwin", "auto", missing)
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			err := r.u.run(context.Background(), upgradeFlags{Version: "0.5.0"})
			if err == nil || !strings.Contains(err.Error(), "Nothing was changed") || !strings.Contains(err.Error(), "linux runtime") {
				t.Fatalf("want a clear refusal, got %v", err)
			}
			if got := selfContent(t, r); got != "OLD" {
				t.Fatalf("native binary was replaced: %q", got)
			}
			if len(*r.log) != 0 {
				t.Fatalf("service restarted: %v", *r.log)
			}
			if _, err := os.Stat(paths.BinaryCache("0.5.0", "linux", runtime.GOARCH)); err == nil {
				t.Fatal("unverified linux binary was cached")
			}
		})
	}
}

func TestUpgradeContainerJobsOffSkipsLinux(t *testing.T) {
	assets := releaseAssets(t, "darwin")
	delete(assets, fmt.Sprintf("kran_0.5.0_linux_%s.tar.gz", runtime.GOARCH)) // would fail if fetched
	for _, off := range []string{"false", "off", " No "} {
		r := rig(t, "darwin", off, assets)
		if r.u.FetchLinux != nil {
			t.Fatalf("%q: fetch wired", off)
		}
		if err := r.u.run(context.Background(), upgradeFlags{Version: "0.5.0"}); err != nil {
			t.Fatalf("%q: %v", off, err)
		}
		if selfContent(t, r) != "NEW-darwin" || strings.Contains(r.out.String(), "Linux runtime") {
			t.Fatalf("%q: %s", off, r.out.String())
		}
	}
}

func TestUpgradeLinuxNodeFetchesNothingExtra(t *testing.T) {
	r := rig(t, "linux", "auto", map[string][]byte{
		fmt.Sprintf("kran_0.5.0_linux_%s.tar.gz", runtime.GOARCH): relTarball(t, "NEW-linux"),
	})
	if r.u.FetchLinux != nil {
		t.Fatal("a linux node needs no extra runtime")
	}
	if err := r.u.run(context.Background(), upgradeFlags{Version: "0.5.0"}); err != nil {
		t.Fatal(err)
	}
	if selfContent(t, r) != "NEW-linux" || strings.Contains(r.out.String(), "Linux runtime") {
		t.Fatal(r.out.String())
	}
}

// A node that already runs the target but lacks the linux binary (a hand
// replaced daemon) is repaired by running `kran upgrade`.
func TestUpgradeSameVersionRepairsMissingLinuxRuntime(t *testing.T) {
	r := rig(t, "darwin", "auto", releaseAssets(t, "darwin"))
	r.u.Current = "0.5.0"
	if err := r.u.run(context.Background(), upgradeFlags{Version: "0.5.0"}); err != nil {
		t.Fatal(err)
	}
	if selfContent(t, r) != "OLD" {
		t.Fatal("native binary must not be touched when the version matches")
	}
	if _, err := os.Stat(paths.BinaryCache("0.5.0", "linux", runtime.GOARCH)); err != nil {
		t.Fatal(err)
	}
}
