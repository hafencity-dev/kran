//go:build e2e

package e2e

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// slowSetupRepo is a repo whose project setup runs for a while on the
// target, so a job sits in phase `setup` (before the user's command).
func slowSetupRepo(t *testing.T, c *cluster, secs string) string {
	return makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{
		".kran.toml": "[setup]\nrun = \"sleep " + secs + "\"\nkey = [\"setup.lock\"]\n",
		"setup.lock": "v1\n",
	})
}

// TestE2ETargetDiesBeforeStart: the target daemon dies while the job is
// still in setup. Back within the lease, the origin reattaches and the job
// runs there, once. Gone for good, nobody can tell whether the command
// started there before it died (SPEC §6.1 step 8): 125, and nothing runs
// here; the command must not run there later either.
func TestE2ETargetDiesBeforeStart(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a, b := c.node("a"), c.node("b")
	repo := slowSetupRepo(t, c, "4")
	marks := filepath.Join(c.base, "marks")
	for _, comesBack := range []bool{true, false} {
		os.Remove(marks)
		must(t, os.WriteFile(filepath.Join(repo, "setup.lock"), []byte(fmt.Sprint(comesBack)), 0o644)) // setup runs again
		p := a.startKran(runOpts{dir: repo, env: []string{"MARKS=" + marks}}, "run", "--json-events", "-n", "b", "--",
			"sh", "-c", `echo "${KRAN_NODE:-here}" >> "$MARKS"; echo "ran on ${KRAN_NODE:-here}"`)
		if ev := waitPhase(p, "setup", 60*time.Second); ev == nil {
			t.Fatalf("job never reached setup on b:\n%s", p.wait())
		}
		b.crash()
		if comesBack {
			time.Sleep(2 * time.Second)
			b.start()
			if r := p.wait(); r.code != 0 || string(r.stdout) != "ran on b\n" {
				t.Errorf("target back within the lease: %s", r)
			}
		} else {
			r := p.wait()
			if r.code != 125 || len(r.stdout) != 0 || !strings.Contains(string(r.stderr), "may have started on b") {
				t.Errorf("target gone for good: %s", r)
			}
			b.start()
		}
		c.waitMesh(60 * time.Second)
		time.Sleep(execLease + 8*time.Second)
		data, _ := os.ReadFile(marks)
		want := []string{"b"}
		if !comesBack {
			want = nil
		}
		if got := strings.Fields(string(data)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("target back %v: command ran on %v, want %v", comesBack, got, want)
		}
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}

// execLease is the supervisor's lease (SPEC §6.2).
const execLease = 15 * time.Second

func waitPhase(p *cliProc, state string, d time.Duration) map[string]any {
	var ev map[string]any
	poll(d, func() bool {
		for _, e := range parseEvents(p.stderr.Bytes()) {
			if e["kran"] == "phase" && e["state"] == state {
				ev = e
				return true
			}
		}
		return p.exited()
	})
	return ev
}

// TestE2EDrainAndNoFallback: a drained node takes nothing new (jobs
// pinned to it wait with a reason); --remote/--no-fallback never run
// locally and fail with 125 when the target dies before the command.
func TestE2EDrainAndNoFallback(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	marks := filepath.Join(c.base, "marks")
	cmd := []string{"sh", "-c", `echo "${KRAN_NODE:-here}" >> "$MARKS"`}
	env := []string{"MARKS=" + marks}

	// A running job on b survives the drain.
	long := a.startKran(runOpts{dir: repo}, "run", "-n", "b", "--", "sh", "-c", "echo started; sleep 4; echo finished")
	if !long.waitStdout("started", 60*time.Second) {
		t.Fatal(long.wait())
	}
	if r := a.kran(runOpts{}, "drain", "b"); r.code != 0 || !strings.Contains(string(r.stdout), "draining") {
		t.Fatalf("kran drain b: %s", r)
	}
	c.waitNodeStatus("a", "b", 10*time.Second, proto.NodeDraining)
	// Nothing new starts there: --remote waits with the reason.
	p := a.startKran(runOpts{dir: repo, env: env}, append([]string{"run", "--remote", "--"}, cmd...)...)
	poll(10*time.Second, func() bool { return strings.Contains(p.stderr.String(), "draining") })
	if !strings.Contains(p.stderr.String(), "kran: ") || !strings.Contains(p.stderr.String(), "draining") || p.exited() {
		t.Errorf("--remote with b draining should wait and say why: %q", p.stderr.String())
	}
	if r := long.wait(); r.code != 0 || !strings.Contains(string(r.stdout), "finished") {
		t.Errorf("running job did not survive the drain: %s", r)
	}
	if r := a.kran(runOpts{}, "resume", "b"); r.code != 0 {
		t.Fatalf("kran resume b: %s", r)
	}
	if r := p.wait(); r.code != 0 {
		t.Errorf("--remote after resume: %s", r)
	}

	// Target dies before the command starts: 125, nothing runs locally.
	repo2 := makeRepo(t, filepath.Join(c.base, "repo2"), map[string]string{
		".kran.toml": "[setup]\nrun = \"sleep 4\"\nkey = [\"k\"]\n", "k": "1",
	})
	p = a.startKran(runOpts{dir: repo2, env: env}, append([]string{"run", "--json-events", "--remote", "-n", "b", "--"}, cmd...)...)
	if waitPhase(p, "setup", 60*time.Second) == nil {
		t.Fatalf("no setup phase: %s", p.wait())
	}
	b.crash()
	r := p.wait()
	if r.code != 125 {
		t.Errorf("--remote, target died in setup: %s", r)
	}

	// No daemon at all.
	a.stop()
	r = a.kran(runOpts{dir: repo, env: env}, append([]string{"run", "--no-fallback", "--"}, cmd...)...)
	if r.code != 125 || !strings.Contains(string(r.stderr), "kran: ") {
		t.Errorf("--no-fallback without daemon: %s", r)
	}
	r = a.kran(runOpts{dir: repo, env: env}, append([]string{"run", "--"}, cmd...)...)
	if r.code != 0 || !strings.Contains(string(r.stderr), "kran: daemon not running") {
		t.Errorf("no daemon, plain run must run locally: %s", r)
	}
	b.start()
	a.start()
	c.waitMesh(60 * time.Second)
	time.Sleep(execLease + 5*time.Second)
	data, _ := os.ReadFile(marks)
	// --remote after resume on b, then the no-daemon run here.
	if got := strings.Fields(string(data)); strings.Join(got, ",") != "b,here" {
		t.Errorf("runs: %v, want [b here]", got)
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}

// TestE2EQueueReasonAndCancel: a node at its job limit queues with a
// reason; queued, syncing and running jobs can be canceled.
func TestE2EQueueReasonAndCancel(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, map[string]nodeOpts{"a": {MaxJobs: 1}, "b": {MaxJobs: 1}})
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})

	first := a.startKran(runOpts{dir: repo}, "run", "--json-events", "--local", "--", "sh", "-c", "echo started; sleep 8")
	firstID := first.jobID(decisionWait)
	if !first.waitStdout("started", 30*time.Second) {
		t.Fatalf("first job: %s", first.wait())
	}
	second := a.startKran(runOpts{dir: repo}, "run", "--local", "--", "echo", "second ran")
	var queued proto.Job
	poll(20*time.Second, func() bool {
		for _, j := range a.statusJSON().Jobs {
			if j.State == proto.JobQueued && j.Reason != "" {
				queued = j
				return true
			}
		}
		return false
	})
	if queued.Spec.ID == "" {
		t.Fatalf("second job never showed as queued with a reason")
	}
	if !strings.Contains(queued.Reason, "limit") {
		t.Errorf("queue reason %q does not say why", queued.Reason)
	}
	// The CLI tells the user after 2 s.
	poll(10*time.Second, func() bool { return strings.Contains(second.stderr.String(), "kran: ") })
	if !strings.Contains(second.stderr.String(), "kran: waiting") && !strings.Contains(second.stderr.String(), "limit") {
		t.Errorf("queued CLI stderr: %q", second.stderr.String())
	}
	// Cancel while queued.
	if r := a.kran(runOpts{}, "cancel", queued.Spec.ID); r.code != 0 {
		t.Fatalf("kran cancel: %s", r)
	}
	r := second.wait()
	if r.code != 143 || strings.Contains(string(r.stdout), "second ran") {
		t.Errorf("canceled queued job: %s", r)
	}
	if j, _ := a.job(queued.Spec.ID); j.State != proto.JobCanceled {
		t.Errorf("canceled queued job record: %s", j.State)
	}
	// Cancel while running (local job).
	if r := a.kran(runOpts{}, "cancel", firstID); r.code != 0 {
		t.Fatalf("kran cancel running: %s", r)
	}
	r = first.wait()
	if r.code != 143 {
		t.Errorf("canceled running local job: %s", r)
	}
	if j, _ := a.job(firstID); j.State != proto.JobCanceled {
		t.Errorf("canceled local job record: %s", j.State)
	}

	// Cancel while running (remote job): the process tree on b dies.
	p := a.startKran(runOpts{dir: repo}, "run", "--json-events", "-n", "b", "--", "sh", "-c", "echo started; sleep 60 & wait")
	id := p.jobID(decisionWait)
	if !p.waitStdout("started", 60*time.Second) {
		t.Fatalf("remote job: %s", p.wait())
	}
	check := jobProcsCheckable(t)
	if procs := processesWithJobID(id); check && len(procs) == 0 {
		t.Errorf("no processes with KRAN_JOB_ID=%s while it runs", id)
	}
	if r := a.kran(runOpts{}, "cancel", id[:9]); r.code != 0 {
		t.Fatalf("kran cancel <prefix>: %s", r)
	}
	r = p.wait()
	if r.code != 143 {
		t.Errorf("canceled remote job: %s", r)
	}
	if check && !poll(15*time.Second, func() bool { return len(processesWithJobID(id)) == 0 }) {
		t.Errorf("processes of canceled job survive: %v", processesWithJobID(id))
	}
	if j, _ := a.job(id); j.State != proto.JobCanceled {
		t.Errorf("canceled remote job record: %s", j.State)
	}

	// Cancel while syncing: the link to b stalls in the middle of the
	// upload of a large file.
	big := make([]byte, 48<<20)
	rand.Read(big)
	must(t, os.WriteFile(filepath.Join(repo, "big.bin"), big, 0o644))
	p = a.startKran(runOpts{dir: repo}, "run", "--json-events", "-n", "b", "--", "echo", "must not run")
	id = p.jobID(decisionWait)
	if id == "" {
		t.Fatalf("no decision: %s", p.wait())
	}
	a.proxy.StallAfter(4 << 20)
	b.proxy.StallAfter(4 << 20)
	a.waitJob(id, 30*time.Second, func(j proto.Job) bool { return j.State == proto.JobSyncing })
	time.Sleep(time.Second)
	if j, _ := a.job(id); j.State != proto.JobSyncing {
		t.Fatalf("job left syncing despite the stall: %s", j.State)
	}
	canceledAt := time.Now()
	if r := a.kran(runOpts{}, "cancel", id); r.code != 0 {
		t.Fatalf("kran cancel syncing: %s", r)
	}
	r = p.wait()
	if r.code != 143 || strings.Contains(string(r.stdout), "must not run") {
		t.Errorf("canceled syncing job: %s", r)
	}
	// The link is still stalled: the job ends at once all the same, it does
	// not wait for the link to be declared dead.
	if took := time.Since(canceledAt); took > 20*time.Second {
		t.Errorf("cancel while syncing over a stalled link took %v", took.Round(time.Second))
	}
	b.proxy.Stall(false)
	a.proxy.Stall(false)
	c.waitMesh(60 * time.Second)
	if j, _ := a.job(id); j.State != proto.JobCanceled {
		t.Errorf("canceled syncing job record: %s %q", j.State, j.Reason)
	}
	a.waitIdle(60 * time.Second)
	b.waitIdle(60 * time.Second)
}

// TestE2ECtrlC: SIGINT to the CLI reaches the job; a second one within
// 2 s kills it.
func TestE2ECtrlC(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, map[string]nodeOpts{"a": {MaxJobs: 1}})
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	trap := `trap 'echo got INT' INT; echo ready; while :; do sleep 0.2; done`

	for _, where := range [][]string{{"-n", "b"}, {"--local"}} {
		// One Ctrl-C: the job's own handling decides (here: exit 130).
		p := a.startKran(runOpts{dir: repo}, append(append([]string{"run"}, where...), "--", "sh", "-c", "echo ready; sleep 30")...)
		if !p.waitStdout("ready", 60*time.Second) {
			t.Fatalf("%v: %s", where, p.wait())
		}
		p.signal(syscall.SIGINT)
		if r := p.wait(); r.code != 130 {
			t.Errorf("%v single Ctrl-C: %s", where, r)
		}
		// A job that traps INT: the second Ctrl-C within 2 s kills it.
		p = a.startKran(runOpts{dir: repo}, append(append([]string{"run"}, where...), "--", "sh", "-c", trap)...)
		if !p.waitStdout("ready", 60*time.Second) {
			t.Fatalf("%v: %s", where, p.wait())
		}
		p.signal(syscall.SIGINT)
		if !p.waitStdout("got INT", 10*time.Second) {
			t.Errorf("%v: first INT not delivered: %q", where, p.stdout.String())
		}
		time.Sleep(300 * time.Millisecond)
		p.signal(syscall.SIGINT)
		if r := p.wait(); r.code != 137 {
			t.Errorf("%v double Ctrl-C: want 137, %s", where, r)
		}
	}
	// Ctrl-C while queued: 130 at once, the job never runs.
	hold := a.startKran(runOpts{dir: repo}, "run", "--local", "--", "sh", "-c", "echo ready; sleep 20")
	if !hold.waitStdout("ready", 30*time.Second) {
		t.Fatal(hold.wait())
	}
	q := a.startKran(runOpts{dir: repo}, "run", "--json-events", "--local", "--", "echo", "must not run")
	poll(10*time.Second, func() bool { return event(parseEvents(q.stderr.Bytes()), "notice") != nil })
	q.signal(syscall.SIGINT)
	if r := q.wait(); r.code != 130 || strings.Contains(string(r.stdout), "must not run") {
		t.Errorf("Ctrl-C while queued: %s", r)
	}
	hold.signal(syscall.SIGINT)
	hold.wait()
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}

// TestE2EReattachAfterOriginRestart: the origin daemon dies mid-run; the
// CLI reconnects and the output continues complete.
func TestE2EReattachAfterOriginRestart(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	for _, kill := range []struct {
		name string
		fn   func()
	}{{"graceful", a.stop}, {"crash", a.crash}} {
		script, want := counter(60, "0.1")
		p := a.startKran(runOpts{dir: repo}, "run", "-n", "b", "--", "sh", "-c", script+"; exit 3")
		if !p.waitStdout("tick 10\n", 60*time.Second) {
			t.Fatalf("%s: %s", kill.name, p.wait())
		}
		kill.fn()
		time.Sleep(time.Second)
		a.start()
		r := p.wait()
		if r.code != 3 || string(r.stdout) != want {
			t.Errorf("%s restart of the origin: %s", kill.name, r)
		}
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}

// runningOn is how many jobs n's daemon counts on node.
func runningOn(n *knode, node string) int {
	st, err := n.state()
	if err != nil {
		return -1
	}
	for _, ns := range st.Nodes {
		if ns.Info.Name == node {
			return ns.Running
		}
	}
	return -1
}

// TestE2ETargetRestartMidRun: the target daemon dies mid-run; the
// supervisor keeps the job alive and the origin reattaches.
func TestE2ETargetRestartMidRun(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	for _, kill := range []struct {
		name string
		fn   func()
	}{{"graceful", b.stop}, {"crash", b.crash}} {
		script, want := counter(60, "0.1")
		p := a.startKran(runOpts{dir: repo}, "run", "-n", "b", "--", "sh", "-c", script)
		if !p.waitStdout("tick 10\n", 60*time.Second) {
			t.Fatalf("%s: %s", kill.name, p.wait())
		}
		kill.fn()
		time.Sleep(time.Second)
		b.start()
		// The restarted target counts the job it kept running.
		if !poll(20*time.Second, func() bool { return runningOn(b, "b") == 1 || p.exited() }) || p.exited() {
			t.Errorf("%s: after the restart b counts %d jobs of its own, want 1", kill.name, runningOn(b, "b"))
		}
		r := p.wait()
		if r.code != 0 || string(r.stdout) != want {
			t.Errorf("%s restart of the target: %s", kill.name, r)
		}
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}

// TestE2ELeaseExpiryAfterNetworkCut: the link dies for good mid-run; the
// target kills the job after the lease and the CLI exits 125.
func TestE2ELeaseExpiryAfterNetworkCut(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	p := a.startKran(runOpts{dir: repo}, "run", "--json-events", "-n", "b", "--", "sh", "-c", "echo started; sleep 300 & wait")
	id := p.jobID(decisionWait)
	if !p.waitStdout("started", 60*time.Second) {
		t.Fatalf("%s", p.wait())
	}
	cut := time.Now()
	a.proxy.SetDown(true)
	b.proxy.SetDown(true)
	r := p.wait()
	if r.code != 125 {
		t.Errorf("CLI after a permanent cut: %s", r)
	}
	if d := time.Since(cut); d > 45*time.Second {
		t.Errorf("CLI took %v to give up", d)
	}
	if jobProcsCheckable(t) && !poll(execLease+15*time.Second-time.Since(cut), func() bool { return len(processesWithJobID(id)) == 0 }) {
		t.Errorf("job processes survive the lease: %v", processesWithJobID(id))
	}
	a.proxy.SetDown(false)
	b.proxy.SetDown(false)
	c.waitMesh(90 * time.Second)
	j := a.waitJob(id, 10*time.Second, func(j proto.Job) bool { return j.State.Terminal() })
	if j.State != proto.JobInfraErr && j.State != proto.JobLost {
		t.Errorf("job record after cut: %s %q", j.State, j.Reason)
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
	// The mesh still works.
	if r := a.kran(runOpts{dir: repo}, "run", "-n", "b", "--", "echo", "ok"); r.code != 0 || string(r.stdout) != "ok\n" {
		t.Errorf("after the cut healed: %s", r)
	}
}

// TestE2ELongQueueStaysQueued: a job that waits longer than the CLI's
// 10 s silence window keeps waiting for its turn (daemon heartbeats keep
// the session alive) instead of being run locally behind the daemon's back.
func TestE2ELongQueueStaysQueued(t *testing.T) {
	c := newCluster(t, []string{"a"}, map[string]nodeOpts{"a": {MaxJobs: 1}})
	a := c.node("a")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	hold := a.startKran(runOpts{dir: repo}, "run", "--local", "--", "sh", "-c", "echo ready; sleep 16")
	if !hold.waitStdout("ready", 30*time.Second) {
		t.Fatal(hold.wait())
	}
	start := time.Now()
	r := a.kran(runOpts{dir: repo}, "run", "--local", "--", "sh", "-c", `echo "job=$KRAN_JOB_ID"`)
	if r.code != 0 || strings.Contains(string(r.stderr), "running locally") || string(r.stdout) == "job=\n" {
		t.Errorf("queued job did not wait for its turn: %s", r)
	}
	if d := time.Since(start); d < 12*time.Second {
		t.Errorf("queued job ran after %v, before the first one ended: %s", d, r)
	}
	hold.wait()
	a.waitIdle(10 * time.Second)
}

// TestE2EOriginRestartBeforeStart: the origin daemon restarts while a job
// waits; its CLI reattaches (the queued notice gave it the job ID) and the
// job runs once, where it was pinned, not next to the daemon's accounting.
func TestE2EOriginRestartBeforeStart(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, map[string]nodeOpts{"b": {MaxJobs: 1}})
	a, b := c.node("a"), c.node("b")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	marks := filepath.Join(c.base, "marks")
	env := []string{"MARKS=" + marks}
	hold := a.startKran(runOpts{dir: repo, env: env}, "run", "-n", "b", "--", "sh", "-c", `echo hold >> "$MARKS"; echo ready; sleep 6`)
	if !hold.waitStdout("ready", 60*time.Second) {
		t.Fatal(hold.wait())
	}
	waiting := a.startKran(runOpts{dir: repo, env: env}, "run", "--json-events", "-n", "b", "--", "sh", "-c", `echo "${KRAN_NODE:-here}" >> "$MARKS"`)
	poll(10*time.Second, func() bool { st, _ := a.state(); return st != nil && len(st.Queue) == 1 })
	if !poll(10*time.Second, func() bool {
		for _, ev := range parseEvents(waiting.stderr.Bytes()) {
			if id, _ := ev["job_id"].(string); ev["kran"] == "notice" && ev["kind"] == "queued" && id != "" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("no queued notice with the job ID: %s", waiting.wait())
	}
	a.restart()
	if r := waiting.wait(); r.code != 0 {
		t.Errorf("waiting job: %s", r)
	}
	if r := hold.wait(); r.code != 0 {
		t.Errorf("running job across the restart: %s", r)
	}
	time.Sleep(25 * time.Second) // CLI reattach window + placement
	data, _ := os.ReadFile(marks)
	if got := strings.Fields(string(data)); strings.Join(got, ",") != "hold,b" {
		t.Errorf("runs %v, want [hold b]: the waiting job must run exactly once, on b", got)
	}
	if event(waiting.wait().events(), "fallback") != nil {
		t.Errorf("waiting job ran locally: %s", waiting.wait())
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}

// TestE2EStdinIntactAfterFallback: input is forwarded only once the
// command runs remotely; a fallback before that (the setup fails on the
// target: nothing ran there) gets all of it.
func TestE2EStdinIntactAfterFallback(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a := c.node("a")
	repo := slowSetupRepo(t, c, "2; exit 3")
	in := strings.Repeat("0123456789abcdef", 8<<10) // 128 KiB
	p := a.startKran(runOpts{dir: repo, stdin: strings.NewReader(in)}, "run", "--json-events", "-i", "-n", "b", "--",
		"sh", "-c", `wc -c | tr -d ' '`)
	if waitPhase(p, "setup", 60*time.Second) == nil {
		t.Fatalf("no setup phase: %s", p.wait())
	}
	r := p.wait()
	if r.code != 0 || strings.TrimSpace(string(r.stdout)) != "131072" || event(r.events(), "fallback") == nil {
		t.Errorf("fallback after a failed remote start must get all of stdin: %s", r)
	}
	// And a normal remote run still gets everything.
	must(t, os.WriteFile(filepath.Join(repo, ".kran.toml"), nil, 0o644))
	r = a.kran(runOpts{dir: repo, stdin: strings.NewReader(in)}, "run", "-i", "-n", "b", "--", "sh", "-c", `wc -c | tr -d ' '`)
	if r.code != 0 || strings.TrimSpace(string(r.stdout)) != "131072" {
		t.Errorf("remote stdin: %s", r)
	}
}

// TestE2ELocalJobEndsWhileDaemonDown: a local job that finishes while its
// daemon restarts still gets its real result recorded.
func TestE2ELocalJobEndsWhileDaemonDown(t *testing.T) {
	c := newCluster(t, []string{"a"}, nil)
	a := c.node("a")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	p := a.startKran(runOpts{dir: repo}, "run", "--json-events", "--local", "--", "sh", "-c", "echo started; sleep 1; exit 3")
	id := p.jobID(decisionWait)
	if !p.waitStdout("started", 30*time.Second) {
		t.Fatal(p.wait())
	}
	a.crash()
	time.Sleep(3 * time.Second)
	a.start()
	if r := p.wait(); r.code != 3 {
		t.Errorf("local job across a daemon crash: %s", r)
	}
	j := a.waitJob(id, 40*time.Second, func(j proto.Job) bool { return j.State.Terminal() })
	if j.State != proto.JobFailed || j.ExitCode == nil || *j.ExitCode != 3 {
		t.Errorf("record %s exit %v (%s), want failed 3", j.State, j.ExitCode, j.Reason)
	}
}

// TestE2ETargetRestartDuringSetup: the target daemon restarts while the
// job is in setup; the command starts after the reattach and the origin
// must learn that (stdin flows, the result is the command's).
func TestE2ETargetRestartDuringSetup(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a, b := c.node("a"), c.node("b")
	repo := slowSetupRepo(t, c, "3")
	in := strings.Repeat("x", 1000)
	p := a.startKran(runOpts{dir: repo, stdin: strings.NewReader(in), timeout: 90 * time.Second}, "run", "--json-events", "-i", "-n", "b", "--",
		"sh", "-c", `wc -c | tr -d ' '; exit 4`)
	if waitPhase(p, "setup", 60*time.Second) == nil {
		t.Fatalf("no setup phase: %s", p.wait())
	}
	b.crash()
	time.Sleep(time.Second)
	b.start()
	r := p.wait()
	if r.code != 4 || strings.TrimSpace(string(r.stdout)) != "1000" {
		t.Errorf("job across a target restart during setup: %s", r)
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}

// TestE2EOriginRestartDuringSetup: the origin daemon restarts while the
// job is in setup on the target; the job runs exactly once and its CLI
// reports the command's result.
func TestE2EOriginRestartDuringSetup(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a, b := c.node("a"), c.node("b")
	repo := slowSetupRepo(t, c, "4")
	marks := filepath.Join(c.base, "marks")
	for _, kill := range []struct {
		name string
		fn   func()
	}{{"graceful", a.stop}, {"crash", a.crash}} {
		os.Remove(marks)
		must(t, os.WriteFile(filepath.Join(repo, "setup.lock"), []byte(kill.name), 0o644)) // setup runs again
		p := a.startKran(runOpts{dir: repo, env: []string{"MARKS=" + marks}}, "run", "--json-events", "-n", "b", "--",
			"sh", "-c", `echo "${KRAN_NODE:-here}" >> "$MARKS"; exit 5`)
		if waitPhase(p, "setup", 60*time.Second) == nil {
			t.Fatalf("no setup phase: %s", p.wait())
		}
		kill.fn()
		time.Sleep(500 * time.Millisecond)
		a.start()
		r := p.wait()
		time.Sleep(execLease + 5*time.Second)
		data, _ := os.ReadFile(marks)
		if got := strings.Fields(string(data)); len(got) != 1 || r.code != 5 {
			t.Errorf("%s restart during setup: ran on %v; %s", kill.name, got, r)
		}
	}
	a.waitIdle(30 * time.Second)
	b.waitIdle(30 * time.Second)
}
