//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2ELinkToleratesStallButNotOutage: a link that delivers nothing for a
// while (the far daemon starved on an overloaded machine, a congested
// network) stays up as long as the silence is shorter than the tolerance,
// and a job whose sync is caught in the stall still runs where it was
// placed instead of falling back. A real outage still ends the link soon
// after the tolerance. Timings shrunk with KRAN_KEEPALIVE: ping every
// 500 ms, dead after 6 s of silence.
func TestE2ELinkToleratesStallButNotOutage(t *testing.T) {
	const tolerance = 6 * time.Second
	env := []string{"KRAN_KEEPALIVE=500ms,6s,6s"}
	c := newCluster(t, []string{"a", "b"}, map[string]nodeOpts{"a": {Env: env}, "b": {Env: env}})
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	lost := func() int {
		return strings.Count(a.daemonLog(), "connection lost") + strings.Count(b.daemonLog(), "connection lost")
	}

	// A stall of about half the tolerance, with a job submitted into it.
	a.proxy.Stall(true)
	b.proxy.Stall(true)
	p := a.startKran(runOpts{dir: repo}, "run", "-n", "b", "--", "sh", "-c", `echo "on $KRAN_NODE: $(cat f)"`)
	time.Sleep(tolerance / 2)
	a.proxy.Stall(false)
	b.proxy.Stall(false)
	r := p.wait()
	if r.code != 0 || string(r.stdout) != "on b: x\n" || strings.Contains(string(r.stderr), "locally") {
		t.Errorf("job submitted into a short stall: %s", r)
	}
	time.Sleep(time.Second)
	if n := lost(); n != 0 {
		t.Fatalf("a %s stall ended the link (tolerance %s):\na: %s\nb: %s", tolerance/2, tolerance,
			a.tail("daemon.log", 20), b.tail("daemon.log", 20))
	}

	// An outage: nothing gets through any more.
	a.proxy.Stall(true)
	b.proxy.Stall(true)
	start := time.Now()
	if !poll(tolerance+15*time.Second, func() bool { return strings.Contains(a.daemonLog(), "connection lost: peer stopped answering") }) {
		t.Fatalf("link still up %s into an outage:\n%s", time.Since(start), a.tail("daemon.log", 20))
	}
	if d := time.Since(start); d < tolerance-time.Second {
		t.Errorf("declared dead after %s, before the %s tolerance", d, tolerance)
	}
	a.proxy.Stall(false)
	b.proxy.Stall(false)
	c.waitMesh(90 * time.Second)
	if r := a.kran(runOpts{dir: repo}, "run", "-n", "b", "--", "echo", "ok"); r.code != 0 || string(r.stdout) != "ok\n" {
		t.Errorf("after the outage healed: %s", r)
	}

	// The daemons logged to daemon.log only: nothing of it went to stderr
	// (which launchd and systemd would keep as a second copy).
	for _, n := range []*knode{a, b} {
		out, _ := os.ReadFile(filepath.Join(n.dir, "daemon.out"))
		if strings.Contains(string(out), "started on") || strings.Contains(string(out), "connected to") {
			t.Errorf("%s: daemon log lines on stderr:\n%s", n.name, out)
		}
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}
