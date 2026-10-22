//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestE2EDiskFloor: a target below its free-disk floor takes no jobs and
// says why. Its state dir lives on a tiny tmpfs (/run/lock, a few MiB).
func TestE2EDiskFloor(t *testing.T) {
	small := smallFilesystem(t)
	c := newCluster(t, []string{"a", "b"}, map[string]nodeOpts{"b": {StateDir: small, Degraded: true}})
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	ns := c.waitNodeStatus("a", "b", 30*time.Second, proto.NodeDegraded)
	if !strings.Contains(ns.Reason, "disk") {
		t.Errorf("b degraded without a disk reason: %q", ns.Reason)
	}
	st := b.statusJSON()
	if self, _ := nodeView(&st, "b"); self.Status != proto.NodeDegraded || !strings.Contains(self.Reason, "floor") {
		t.Errorf("b's own view: %s %q", self.Status, self.Reason)
	}
	// Pinned to b: refused there; the job waits and says why (like a
	// drained node, the space may come back), Ctrl-C ends it.
	p := a.startKran(runOpts{dir: repo}, "run", "-n", "b", "--no-fallback", "--", "echo", "must not run")
	poll(15*time.Second, func() bool { return strings.Contains(p.stderr.String(), "floor") })
	p.signal(syscall.SIGINT)
	r := p.wait()
	if r.code != 130 || !strings.Contains(string(r.stderr), "disk:") || strings.Contains(string(r.stdout), "must not run") {
		t.Errorf("-n b with b below the disk floor: %s", r)
	}
	// Plain run: here, never on b.
	r = a.kran(runOpts{dir: repo}, "run", "--", "sh", "-c", `echo "on $KRAN_NODE"`)
	if r.code != 0 || string(r.stdout) != "on a\n" && string(r.stdout) != "on \n" {
		t.Errorf("plain run with b below the disk floor: %s", r)
	}
	if entries, _ := os.ReadDir(filepath.Join(small, "slots")); len(entries) > 0 {
		t.Errorf("b created slots below the disk floor")
	}
}

// smallFilesystem returns a fresh dir on a filesystem far below the disk
// floor (10 GiB), or skips.
func smallFilesystem(t *testing.T) string {
	if hostOS() != "linux" {
		t.Skip("needs a small writable tmpfs (/run/lock on Linux)")
	}
	dir, err := os.MkdirTemp("/run/lock", "kran-e2e-")
	if err != nil {
		t.Skipf("no small writable filesystem: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestE2EMemoryCap: a job above its memory cap is killed and the message
// says how to rerun it (SPEC §6.8).
func TestE2EMemoryCap(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("needs python3")
	}
	c := newCluster(t, []string{"a", "b"}, nil)
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	// --mem 1G → cap max(2×1 GiB, 2 GiB) = 2 GiB; the job touches 3 GiB.
	alloc := []string{"python3", "-c", "import sys; x = bytearray(3 << 30); x[::4096] = b'a' * len(x[::4096]); print('survived')"}
	r := a.kran(runOpts{dir: repo}, append([]string{"run", "-n", "b", "--mem", "1G", "--"}, alloc...)...)
	if r.code != 137 || strings.Contains(string(r.stdout), "survived") {
		t.Errorf("remote job over its cap: %s", r)
	}
	if !strings.Contains(string(r.stderr), "kran: job hit its 2 GiB memory cap on b") || !strings.Contains(string(r.stderr), "--mem") {
		t.Errorf("remote OOM message: %q", r.stderr)
	}
	if hostOS() == "linux" {
		r = a.kran(runOpts{dir: repo}, append([]string{"run", "--local", "--mem", "1G", "--"}, alloc...)...)
		if r.code != 137 || strings.Contains(string(r.stdout), "survived") {
			t.Errorf("local job over its cap: %s", r)
		}
		if !strings.Contains(string(r.stderr), "memory cap") {
			t.Errorf("local OOM message: %q", r.stderr)
		}
	}
	// Within the cap it runs normally.
	r = a.kran(runOpts{dir: repo}, "run", "-n", "b", "--mem", "1G", "--", "python3", "-c", "x = bytearray(256 << 20); print('ok')")
	if r.code != 0 || string(r.stdout) != "ok\n" {
		t.Errorf("job within its cap: %s", r)
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}

// TestE2EContainerPathParity: in the container runtime the job sees the
// origin's absolute paths (SPEC §6.7). Needs Docker: KRAN_TEST_DOCKER=1.
func TestE2EContainerPathParity(t *testing.T) {
	if os.Getenv("KRAN_TEST_DOCKER") != "1" {
		t.Skip("set KRAN_TEST_DOCKER=1 to run container tests")
	}
	c := newCluster(t, []string{"a", "b"}, map[string]nodeOpts{"b": {Container: "true"}})
	a, b := c.node("a"), c.node("b")
	// A Node pin the host cannot satisfy makes b's native runtime
	// ineligible, so the job goes to b's container runtime (mise installs
	// node there).
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{".node-version": "18.20.4\n", "sub/x.txt": "x\n"})
	waitContainerRuntime(t, a, "b")
	r := a.kran(runOpts{dir: filepath.Join(repo, "sub"), timeout: 10 * time.Minute}, "run", "--json-events", "-n", "b", "--",
		"sh", "-c", `pwd; cat "$PWD/x.txt"; test -f /.dockerenv && echo container; id -u`)
	if r.code != 0 {
		t.Fatalf("container job: %s", r)
	}
	if ev := event(r.events(), "decision"); ev == nil || ev["runtime"] != "container" {
		t.Fatalf("not placed in the container runtime: %v", ev)
	}
	want := filepath.Join(repo, "sub") + "\nx\ncontainer\n" + itoa(os.Getuid()) + "\n"
	if string(r.stdout) != want {
		t.Errorf("container view: got %q want %q", r.stdout, want)
	}

	// The job looks like the origin (SPEC §6.8): its user name and home,
	// the workspace's pinned node, and the origin's own go although the
	// workspace does not pin it.
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	r = a.kran(runOpts{dir: filepath.Join(repo, "sub"), timeout: 10 * time.Minute}, "run", "-n", "b", "--",
		"sh", "-c", `whoami; echo "$HOME"; touch "$HOME/.kran-e2e" && echo home-ok; node --version; command -v go >/dev/null && go version || echo no-go`)
	if r.code != 0 {
		t.Fatalf("container job: %s", r)
	}
	t.Logf("container job:\n%s", r.stdout)
	want = u.Username + "\n" + home + "\nhome-ok\nv18.20.4\n"
	if out := string(r.stdout); !strings.HasPrefix(out, want) || strings.Contains(string(r.stderr), "No version is set") {
		t.Errorf("container user/tools: got %q, want prefix %q; stderr %s", out, want, r.stderr)
	}
	if _, err := exec.LookPath("go"); err == nil && !strings.Contains(string(r.stdout), "go version go") {
		t.Errorf("the origin's go is missing in the container: %q", r.stdout)
	}
	// Docker from the job: a bind mount of the workspace reaches the
	// job's slot (a file only the job wrote), not the origin's directory
	// of the same path on the Docker host.
	r = a.kran(runOpts{dir: filepath.Join(repo, "sub"), timeout: 10 * time.Minute}, "run", "-n", "b", "--",
		"sh", "-c", `echo written-in-the-job > from-job && docker run --rm --user "$(id -u)" -v "$PWD":/w alpine:3.20 cat /w/from-job /w/x.txt`)
	if r.code != 0 || string(r.stdout) != "written-in-the-job\nx\n" {
		t.Errorf("docker bind mount from the job: %s", r)
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
	if out := dockerPS(t, "label=kran.job"); strings.TrimSpace(out) != "" {
		t.Errorf("containers left: %s", out)
	}
}

func waitContainerRuntime(t *testing.T, from *knode, node string) {
	t.Helper()
	if !poll(5*time.Minute, func() bool {
		st, err := from.state()
		if err != nil {
			return false
		}
		ns, _ := nodeView(st, node)
		for _, rt := range ns.Info.Runtimes {
			if rt.Kind == proto.RuntimeContainer && rt.Available {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("%s has no available container runtime", node)
	}
}
