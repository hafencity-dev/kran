//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestE2EThreeNodesTwoOrigins: a and c both send jobs to b, which takes
// one at a time; every job runs exactly once and b never overbooks.
func TestE2EThreeNodesTwoOrigins(t *testing.T) {
	c := newCluster(t, []string{"a", "b", "c"}, map[string]nodeOpts{"b": {MaxJobs: 1}})
	b := c.node("b")
	var procs []*cliProc
	for _, origin := range []string{"a", "c"} {
		n := c.node(origin)
		repo := makeRepo(t, filepath.Join(c.base, "repo-"+origin), map[string]string{"f": origin})
		for i := 0; i < 3; i++ {
			procs = append(procs, n.startKran(runOpts{dir: repo}, "run", "--no-fallback", "-n", "b", "--",
				"sh", "-c", `echo "$KRAN_ORIGIN on $KRAN_NODE: $(cat f)"; sleep 1`))
		}
	}
	stop := make(chan struct{})
	var maxRunning int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			if st, err := b.state(); err == nil {
				for _, ns := range st.Nodes {
					if ns.Self && ns.Running > maxRunning {
						maxRunning = ns.Running
					}
				}
			}
		}
	}()
	counts := map[string]int{}
	for _, p := range procs {
		r := p.wait()
		if r.code != 0 {
			t.Errorf("%s", r)
		}
		counts[strings.TrimSpace(string(r.stdout))]++
	}
	close(stop)
	wg.Wait()
	if counts["a on b: a"] != 3 || counts["c on b: c"] != 3 {
		t.Errorf("outputs %v", counts)
	}
	if maxRunning > 1 {
		t.Errorf("b ran %d jobs at once with max_jobs = 1", maxRunning)
	}
	for _, name := range c.names {
		c.node(name).waitIdle(30 * time.Second)
	}
}

// TestE2ELinkHiccups: a dropped connection (reconnect) and a stall
// shorter than the lease both heal; the job's output stays complete.
func TestE2ELinkHiccups(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	script, want := counter(100, "0.1")
	p := a.startKran(runOpts{dir: repo}, "run", "-n", "b", "--", "sh", "-c", script)
	if !p.waitStdout("tick 10\n", 60*time.Second) {
		t.Fatal(p.wait())
	}
	a.proxy.Cut()
	b.proxy.Cut()
	if !p.waitStdout("tick 40\n", 60*time.Second) {
		t.Fatalf("no progress after a cut: %s", p.wait())
	}
	a.proxy.Stall(true)
	b.proxy.Stall(true)
	time.Sleep(6 * time.Second)
	a.proxy.Stall(false)
	b.proxy.Stall(false)
	r := p.wait()
	if r.code != 0 || string(r.stdout) != want {
		t.Errorf("after a cut and a 6 s stall: %s", r)
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}

// TestE2ECLIKilled: a CLI that dies without a word (SIGKILL) cancels its
// job; nothing keeps running, locally or remotely.
func TestE2ECLIKilled(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	for _, where := range [][]string{{"-n", "b"}, {"--local"}} {
		p := a.startKran(runOpts{dir: repo}, append(append([]string{"run", "--json-events"}, where...), "--",
			"sh", "-c", "echo started; (sleep 300 &) ; sleep 300")...)
		id := p.jobID(30 * time.Second)
		if !p.waitStdout("started", 60*time.Second) {
			t.Fatal(p.wait())
		}
		if len(processesWithJobID(id)) == 0 {
			t.Fatalf("%v: no job processes found", where)
		}
		p.signal(syscall.SIGKILL)
		p.wait()
		if !poll(30*time.Second, func() bool { return len(processesWithJobID(id)) == 0 }) {
			t.Errorf("%v: job processes survive their CLI: %v", where, processesWithJobID(id))
		}
		j := a.waitJob(id, 30*time.Second, func(j proto.Job) bool { return j.State.Terminal() })
		if j.State != proto.JobCanceled {
			t.Errorf("%v: job record %s %q", where, j.State, j.Reason)
		}
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}
