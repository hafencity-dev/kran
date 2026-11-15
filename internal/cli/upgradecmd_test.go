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
		UpgradePeer: func(_ context.Context, name, bin, binVer, ver string, force bool) (*link.UpgradeResult, error) {
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
	u.UpgradePeer = func(_ context.Context, name, bin, binVer, ver string, force bool) (*link.UpgradeResult, error) {
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

// TestUpgradeOnePeerLeavesThisMachineAlone: -n upgrades (or takes back) one
// linked machine; nothing is fetched, installed or restarted here, and the
// peer is told this machine's binary is the running version (so it takes
// the version asked for from the cache when they differ).
func TestUpgradeOnePeerLeavesThisMachineAlone(t *testing.T) {
	var log []string
	u, out, _ := newTestUpgrader("0.1.12-5-gf7c1755", &log)
	u.UpgradePeer = func(_ context.Context, name, bin, binVer, ver string, force bool) (*link.UpgradeResult, error) {
		log = append(log, "peer:"+name+":"+binVer+"->"+ver)
		return &link.UpgradeResult{Name: name, From: "0.1.12-5-gf7c1755", To: ver, Installed: true, Restarted: true}, nil
	}
	if err := u.run(context.Background(), upgradeFlags{Version: "0.1.10-6-gc9403ef", Node: "devmac"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, " "); got != "peer:devmac:0.1.12-5-gf7c1755->0.1.10-6-gc9403ef" {
		t.Fatalf("log %q", got)
	}
	if !strings.Contains(out.String(), "devmac upgraded 0.1.12-5-gf7c1755 -> 0.1.10-6-gc9403ef") {
		t.Fatalf("%q", out.String())
	}
	log = nil
	if err := u.run(context.Background(), upgradeFlags{Version: "0.1.10-6-gc9403ef", Node: "nope"}); err == nil || len(log) != 0 {
		t.Fatalf("unknown machine: %v %v", err, log)
	}
}

// TestUpgradeTellsPeersTheInstalledVersion: after this machine installed the
// target, its binary is the target, and the peers are told so.
func TestUpgradeTellsPeersTheInstalledVersion(t *testing.T) {
	var log []string
	u, _, _ := newTestUpgrader("0.1.12-2-gd3852cf", &log)
	u.Peers = func() []string { return []string{"devmac"} }
	u.UpgradePeer = func(_ context.Context, name, bin, binVer, ver string, force bool) (*link.UpgradeResult, error) {
		log = append(log, "peer:"+binVer+"->"+ver)
		return &link.UpgradeResult{Name: name, To: ver}, nil
	}
	if err := u.run(context.Background(), upgradeFlags{Version: "0.1.12-5-gf7c1755"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, " "); got != "fetch:0.1.12-5-gf7c1755 install:/cache/kran->/bin/kran restart peer:0.1.12-5-gf7c1755->0.1.12-5-gf7c1755" {
		t.Fatalf("log %q", got)
	}
}

func TestUpgradeFlagCombinations(t *testing.T) {
	a, _, errb := testApp(nil)
	for args, want := range map[string]string{
		"upgrade -n devmac --local-only --version 0.1.10-6-gc9403ef": "cannot be combined",
		"upgrade -n devmac": "-n needs --version",
	} {
		errb.Reset()
		if code := a.Execute(context.Background(), strings.Fields(args)); code != 2 || !strings.Contains(errb.String(), want) {
			t.Errorf("%s: %d %q", args, code, errb.String())
		}
	}
}

// TestUpgradeRefusedRestartChangesNothing (review F5-2): when the restart
// cannot happen (the wait ran out), nothing was replaced, and the command
// fails saying so.
func TestUpgradeRefusedRestartChangesNothing(t *testing.T) {
	var log []string
	u, _, _ := newTestUpgrader("0.1.12-5-gf7c1755", &log)
	u.Prepare = func(context.Context, bool) (func(), error) {
		log = append(log, "prepare")
		return func() {}, errors.New("the daemon is still busy after 10m0s (1 jobs are starting); nothing was changed")
	}
	err := u.run(context.Background(), upgradeFlags{Version: "0.1.12-6-g072ef3a", Local: true})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(err.Error(), "still runs 0.1.12-5-gf7c1755") {
		t.Fatalf("%v", err)
	}
	if got := strings.Join(log, " "); got != "fetch:0.1.12-6-g072ef3a prepare" {
		t.Fatalf("log %q (nothing may be installed or restarted)", got)
	}
}

// TestUpgradeFailedRestartIsAFailure (review F5-2): a restart that fails
// after the binary was replaced ends the command non-zero, saying what is
// on disk, what runs, and the one command that finishes it.
func TestUpgradeFailedRestartIsAFailure(t *testing.T) {
	var log []string
	u, out, _ := newTestUpgrader("0.1.12-5-gf7c1755", &log)
	released := false
	u.Prepare = func(context.Context, bool) (func(), error) { return func() { released = true }, nil }
	u.Restart = func(context.Context, bool) error { return errors.New("systemctl: timeout") }
	err := u.run(context.Background(), upgradeFlags{Version: "0.1.12-6-g072ef3a", Local: true})
	if err == nil || !strings.Contains(err.Error(), "the binary on disk is now kran 0.1.12-6-g072ef3a while the daemon still runs 0.1.12-5-gf7c1755") ||
		!strings.Contains(err.Error(), "Finish it with: kran service restart") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(out.String(), "Local service is running the new version") || !released {
		t.Fatalf("out %q released %v", out.String(), released)
	}
}

// TestPeerFailureIsAFailure (review F5-1): with -n, a peer the upgrade could
// not bring to the version ends the command non-zero with the peer's own
// account; "already runs" is said only with the daemon's report.
func TestPeerFailureIsAFailure(t *testing.T) {
	var log []string
	u, out, _ := newTestUpgrader("0.1.12-6-g072ef3a", &log)
	u.UpgradePeer = func(_ context.Context, name, bin, binVer, ver string, force bool) (*link.UpgradeResult, error) {
		return nil, errors.New("could not provide kran 0.1.10-6-gc9403ef for darwin/arm64 (no SHA256SUMS); nothing was installed on devmac, its binary is still 0.1.12-6-g072ef3a")
	}
	err := u.run(context.Background(), upgradeFlags{Version: "0.1.10-6-gc9403ef", Node: "devmac"})
	if err == nil || !strings.Contains(err.Error(), "nothing was installed on devmac") || strings.Contains(out.String(), "already runs") {
		t.Fatalf("%v / %q", err, out.String())
	}
	u.UpgradePeer = func(_ context.Context, name, bin, binVer, ver string, force bool) (*link.UpgradeResult, error) {
		return &link.UpgradeResult{Name: name, From: ver, To: ver, Daemon: ver}, nil
	}
	out.Reset()
	if err := u.run(context.Background(), upgradeFlags{Version: "0.1.10-6-gc9403ef", Node: "devmac"}); err != nil ||
		!strings.Contains(out.String(), "devmac already runs 0.1.10-6-gc9403ef (its daemon reports 0.1.10-6-gc9403ef)") {
		t.Fatalf("%v / %q", err, out.String())
	}
}

// TestUpgradeVersionCheckedFirst (review F5-3): a version outside the
// grammar is refused before anything is fetched, here or for a peer.
func TestUpgradeVersionCheckedFirst(t *testing.T) {
	for _, f := range []upgradeFlags{{Version: "../x", Local: true}, {Version: "0.1.12-dirty", Node: "devmac"}, {Version: "0.1.12-5-gf7c1755/..", Local: true}} {
		var log []string
		u, _, _ := newTestUpgrader("0.1.12-5-gf7c1755", &log)
		err := u.run(context.Background(), f)
		if err == nil || !strings.Contains(err.Error(), "x.y.z-N-g<hex>") || len(log) != 0 {
			t.Errorf("%+v: %v, log %v", f, err, log)
		}
	}
}

// TestLocalUpgradeConfirmsTheDaemon (review F5-2): --local-only succeeds
// only when the restarted daemon reports the version asked for; the
// messages name what the daemon runs, not what this binary is.
func TestLocalUpgradeConfirmsTheDaemon(t *testing.T) {
	var log []string
	u, out, _ := newTestUpgrader("0.1.12-6-g072ef3a", &log)
	daemon := "0.1.12-6-g072ef3a"
	u.DaemonVersion = func(context.Context) string { return daemon }
	u.Confirm = func(_ context.Context, want string) (string, error) {
		return daemon, errors.New("it reports kran " + daemon + " 1m30s after the restart, not " + want)
	}
	// The way back, and the daemon keeps running the newer one.
	err := u.run(context.Background(), upgradeFlags{Version: "0.1.12-5-gf7c1755", Local: true})
	if err == nil || !strings.Contains(err.Error(), "the binary on disk is now kran 0.1.12-5-gf7c1755 while the daemon still runs 0.1.12-6-g072ef3a") ||
		!strings.Contains(err.Error(), "Finish it with: kran service restart") || strings.Contains(out.String(), "Local service is running the new version") {
		t.Fatalf("%v / %q", err, out.String())
	}
	u.Confirm = func(_ context.Context, want string) (string, error) { return want, nil }
	out.Reset()
	if err := u.run(context.Background(), upgradeFlags{Version: "0.1.12-5-gf7c1755", Local: true}); err != nil ||
		!strings.Contains(out.String(), "Local service is running the new version") {
		t.Fatalf("%v / %q", err, out.String())
	}
}

// TestBinaryPutInPlaceByHandIsFinished: the binary on disk is already the
// version asked for but the daemon runs another (a hand install): the
// upgrade restarts and confirms instead of saying it is installed and done.
// When the daemon runs it too, nothing here is touched.
func TestBinaryPutInPlaceByHandIsFinished(t *testing.T) {
	var log []string
	u, out, _ := newTestUpgrader("0.1.12-7-gc25ce13", &log)
	daemon := "0.1.12-5-gf7c1755"
	u.DaemonVersion = func(context.Context) string { return daemon }
	u.Confirm = func(_ context.Context, want string) (string, error) {
		log = append(log, "confirm:"+want)
		return want, nil
	}
	if err := u.run(context.Background(), upgradeFlags{Version: "0.1.12-7-gc25ce13", Local: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, " "); got != "restart confirm:0.1.12-7-gc25ce13" {
		t.Fatalf("log %q", got)
	}
	log, daemon = nil, "0.1.12-7-gc25ce13"
	out.Reset()
	if err := u.run(context.Background(), upgradeFlags{Version: "0.1.12-7-gc25ce13", Local: true}); err != nil ||
		len(log) != 0 || !strings.Contains(out.String(), "already installed here and its daemon runs it") {
		t.Fatalf("%v log %v out %q", err, log, out.String())
	}
}
