package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/link"
)

func newTestUpgrader(cur string, log *[]string) (*upgrader, *bytes.Buffer, *bytes.Buffer) {
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	u := &upgrader{
		Self: "/bin/kran", Current: cur, Out: out, Err: errb,
		Latest: func(context.Context) (string, error) { *log = append(*log, "latest"); return "0.5.0", nil },
		Fetch: func(_ context.Context, v string) (string, error) {
			*log = append(*log, "fetch:"+v)
			return "/cache/kran", nil
		},
		Install: func(src, dst string) error {
			*log = append(*log, "install:"+src+"->"+dst)
			return nil
		},
		Restart: func(_ context.Context, force bool) error {
			*log = append(*log, "restart")
			return nil
		},
		Peers: func() []string { return []string{"devmac", "box"} },
		UpgradePeer: func(_ context.Context, name, bin, ver string, force bool) (*link.UpgradeResult, error) {
			*log = append(*log, "peer:"+name+":"+ver)
			if name == "box" {
				return nil, errors.New("ssh timeout")
			}
			return &link.UpgradeResult{Name: name, From: "0.4.0", To: ver, Installed: true, Restarted: true}, nil
		},
	}
	return u, out, errb
}

func TestUpgradeOrder(t *testing.T) {
	var log []string
	u, out, errb := newTestUpgrader("0.4.0", &log)
	err := u.run(context.Background(), upgradeFlags{})
	if err == nil || !strings.Contains(err.Error(), "box") {
		t.Fatalf("a failing peer must make the command fail: %v", err)
	}
	want := "latest fetch:0.5.0 install:/cache/kran->/bin/kran restart peer:devmac:0.5.0 peer:box:0.5.0"
	if strings.Join(log, " ") != want {
		t.Fatalf("order:\n got %v\nwant %s", log, want)
	}
	if !strings.Contains(out.String(), "devmac upgraded 0.4.0 -> 0.5.0") || !strings.Contains(errb.String(), "box: ssh timeout") {
		t.Fatalf("%q %q", out.String(), errb.String())
	}
}

func TestUpgradeCheckAndSameVersion(t *testing.T) {
	var log []string
	u, out, _ := newTestUpgrader("0.5.0", &log)
	if err := u.run(context.Background(), upgradeFlags{Check: true}); err != nil || !strings.Contains(out.String(), "latest release") || len(log) != 1 {
		t.Fatal(err, out.String(), log)
	}
	log = nil
	out.Reset()
	u.UpgradePeer = func(_ context.Context, name, bin, ver string, force bool) (*link.UpgradeResult, error) {
		return &link.UpgradeResult{Name: name, To: ver}, nil
	}
	if err := u.run(context.Background(), upgradeFlags{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(log, " "), "fetch") || !strings.Contains(out.String(), "devmac already runs 0.5.0") {
		t.Fatalf("%v %q", log, out.String())
	}
}

func TestUpgradeDevBuildNeedsExplicitVersion(t *testing.T) {
	var log []string
	u, _, _ := newTestUpgrader("dev", &log)
	err := u.run(context.Background(), upgradeFlags{})
	if err == nil || !strings.Contains(err.Error(), "--version 0.5.0") {
		t.Fatal(err)
	}
	for _, l := range log {
		if strings.HasPrefix(l, "fetch") || strings.HasPrefix(l, "install") {
			t.Fatal("dev build must not be replaced implicitly")
		}
	}
}

func TestUpgradeLocalOnly(t *testing.T) {
	var log []string
	u, _, _ := newTestUpgrader("0.4.0", &log)
	if err := u.run(context.Background(), upgradeFlags{Local: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(log, " "), "peer:") {
		t.Fatal("peers touched")
	}
}

func TestInstallBinaryAtomicAndSymlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new")
	os.WriteFile(src, []byte("NEW"), 0o644)
	real := filepath.Join(dir, "real-kran")
	os.WriteFile(real, []byte("OLD"), 0o755)
	link := filepath.Join(dir, "kran")
	os.Symlink(real, link)
	if err := installBinary(src, link); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(real)
	st, _ := os.Stat(real)
	if string(b) != "NEW" || st.Mode().Perm() != 0o755 {
		t.Fatalf("%q %v", b, st.Mode())
	}
	if l, _ := os.Readlink(link); l != real {
		t.Fatal("symlink must stay a symlink")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 3 {
		t.Fatalf("leftovers: %v", ents)
	}
}
