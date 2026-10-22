package daemon

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
)

func TestLocalRun(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.start()
	ws := workspaceDir(t, map[string]string{"f.txt": "hi"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "cat f.txt; echo oops >&2; exit 3"), cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 3 || res.exit.State != proto.JobFailed {
		t.Fatalf("exit = %+v", res.exit)
	}
	// The CLI's copy of the output is recorded, not echoed back to it.
	if res.out() != "hi" || res.errOut() != "oops\n" {
		t.Fatalf("out %q err %q", res.out(), res.errOut())
	}
	cl, err := client.DialSocket(ctx, a.sock())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	var log strings.Builder
	if err := cl.Log(ctx, res.jobID, false, &log); err != nil || log.String() != "hioops\n" {
		t.Fatalf("history of the local job: %q %v", log.String(), err)
	}
	if len(res.decisions) != 1 || res.decisions[0].Mode != "local" || res.decisions[0].Envelope == nil {
		t.Fatalf("decisions %+v", res.decisions)
	}
	env := res.decisions[0].Envelope
	if env.MemMax < 2<<30 || env.Env["KRAN_JOB_ID"] != res.jobID {
		t.Fatalf("envelope %+v", env)
	}
	j := storedJob(t, a, res.jobID)
	if j.State != proto.JobFailed || j.ExitCode == nil || *j.ExitCode != 3 || !j.Local || j.Origin != "a" {
		t.Fatalf("stored job %+v", j)
	}
	waitFor(t, 2*time.Second, "estimate observed", func() bool { return a.est.count() == 1 })
	st := a.daemon().State()
	if st.Stats.JobsToday != 1 || st.Self != "a" {
		t.Fatalf("stats %+v", st.Stats)
	}
	if a.scheduler().allocCount() != 0 {
		t.Fatal("allocation leaked")
	}
}

func TestRemoteRun(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	b.start()
	a.start()
	c.waitOnline("a", "b")
	ws := workspaceDir(t, map[string]string{"data/in.txt": "payload\n", "run.sh": "echo script"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "cat data/in.txt; sh run.sh; echo to-stderr >&2; exit 7"), cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 7 || res.exit.Node != "b" {
		t.Fatalf("exit = %+v", res.exit)
	}
	if res.out() != "payload\nscript\n" || res.errOut() != "to-stderr\n" {
		t.Fatalf("out %q err %q", res.out(), res.errOut())
	}
	if len(res.decisions) != 1 || res.decisions[0].Mode != "remote" || res.decisions[0].Node != "b" {
		t.Fatalf("decisions %+v", res.decisions)
	}
	j := storedJob(t, a, res.jobID)
	if j.State != proto.JobFailed || j.Node != "b" || j.Local || j.Usage.SyncBytes == 0 {
		t.Fatalf("stored job %+v", j)
	}
	waitFor(t, 3*time.Second, "slot released on b", func() bool { return b.exec.heldSlots() == 0 })
	waitFor(t, 3*time.Second, "allocation released on b", func() bool { return len(b.daemon().target.status().Allocs) == 0 })
	if a.scheduler().allocCount() != 0 {
		t.Fatal("allocation leaked at origin")
	}

	// A second run reuses the uploaded blobs.
	puts := b.exec.putBlobs
	res, err = runCLI(ctx, a.sock(), shSpec(ws, "cat data/in.txt"), cliOpts{})
	if err != nil || res.exit.Code != 0 || res.out() != "payload\n" {
		t.Fatalf("second run: %v %+v %q", err, res.exit, res.out())
	}
	if b.exec.putBlobs != puts {
		t.Fatalf("blobs uploaded again: %d → %d", puts, b.exec.putBlobs)
	}
}

func TestRemoteStdinAndSignal(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	b.start()
	a.start()
	c.waitOnline("a", "b")
	ws := workspaceDir(t, map[string]string{"x": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()

	spec := shSpec(ws, "tr a-z A-Z")
	spec.Stdin = true
	res, err := runCLI(ctx, a.sock(), spec, cliOpts{stdin: []byte("hello stdin\n")})
	if err != nil {
		t.Fatal(err)
	}
	if res.out() != "HELLO STDIN\n" || res.exit.Code != 0 {
		t.Fatalf("out %q exit %+v", res.out(), res.exit)
	}

	sent := false
	sawLive := false
	res, err = runCLI(ctx, a.sock(), shSpec(ws, "echo ready; sleep 30"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout && !sent {
				// Live usage of the remote job reaches the origin.
				waitFor(t, 3*time.Second, "live usage", func() bool {
					j, ok := a.daemon().jobs.find(r.jobID)
					return ok && j.Live != nil && j.Live.Mem == 64<<20
				})
				sawLive = true
				sent = true
				return rs.SendJSON(proto.FrameSignal, proto.Signal{Name: "INT"})
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if !sawLive || res.exit.Code != 130 || res.exit.Signal != "INT" {
		t.Fatalf("exit %+v", res.exit)
	}
}

func TestCtrlCWhileQueued(t *testing.T) {
	c := newCluster(t, "a", "b")
	a := c.nodes["a"]
	a.place = preferRemote("b") // b never starts: the job waits
	a.start()
	ws := workspaceDir(t, map[string]string{"x": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "true"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameNotice {
				return rs.SendJSON(proto.FrameSignal, proto.Signal{Name: "INT"})
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 130 || res.exit.State != proto.JobCanceled {
		t.Fatalf("exit %+v", res.exit)
	}
	if k := res.noticeKinds(); len(k) == 0 || k[0] != "queued" || !strings.Contains(res.notices[0].Message, "not online") {
		t.Fatalf("notices %+v", res.notices)
	}
	if n := len(a.scheduler().Snapshot().Queue); n != 0 {
		t.Fatalf("queue not empty: %d", n)
	}
}

// The first container job on a node waits for its runtime image: the CLI
// is told why (setup phase) before the slot is acquired.
func TestFirstContainerJobSaysTheRuntimeIsBeingPrepared(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	b.exec.runtimes = []proto.Runtime{
		{Kind: proto.RuntimeNative, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: 16, Available: true},
		{Kind: proto.RuntimeContainer, OS: "linux", Arch: runtime.GOARCH, CPUs: 8, Available: true, FirstUse: true},
	}
	a.place = func(job *proto.Job, nodes map[string]proto.NodeState, rejected map[string]bool) (scheduler.Placement, string, bool) {
		if n, ok := nodes["b"]; !ok || n.Status != proto.NodeOnline {
			return scheduler.Placement{}, "waiting for b", false
		}
		return scheduler.Placement{Node: "b", Runtime: proto.RuntimeContainer, Reason: "test"}, "", true
	}
	b.start()
	a.start()
	c.waitOnline("a", "b")
	waitFor(t, 2*time.Second, "b's runtimes known at a", func() bool {
		p := a.daemon().peers.get("b")
		r := runtimeInfo(p.nodeState().Info, proto.RuntimeContainer)
		return r != nil && r.FirstUse
	})
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo ok"), cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 0 {
		t.Fatalf("exit %+v", res.exit)
	}
	found := false
	for _, ph := range res.phases {
		if ph.State == proto.JobSetup && ph.Message == "preparing the Linux runtime on b (first use, ~30 s)" {
			found = true
		}
	}
	if !found {
		t.Fatalf("phases %+v", res.phases)
	}
}

// A local job's history is capped like a remote job's in-memory tail; the
// cut is marked.
func TestLocalOutputHistoryIsCapped(t *testing.T) {
	old := localHistoryMax
	localHistoryMax = 10
	defer func() { localHistoryMax = old }()
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.start()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo 0123456789abcdef; echo more >&2"), cliOpts{})
	if err != nil || res.exit == nil || res.exit.Code != 0 {
		t.Fatalf("%v %+v", err, res.exit)
	}
	cl, err := client.DialSocket(ctx, a.sock())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	var log strings.Builder
	if err := cl.Log(ctx, res.jobID, false, &log); err != nil {
		t.Fatal(err)
	}
	if want := "0123456789\n[kran: output truncated in history]\n"; log.String() != want {
		t.Fatalf("history %q, want %q", log.String(), want)
	}
}

// Output frames from the CLI of a remote job are not the job's output.
func TestOutputFramesOfARemoteJobsCLIAreIgnored(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	b.start()
	a.start()
	c.waitOnline("a", "b")
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo remote"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameDecision {
				rs.Send(proto.FrameStdout, []byte("forged\n"))
			}
			return nil
		}})
	if err != nil || res.exit == nil || res.out() != "remote\n" {
		t.Fatalf("%v %+v %q", err, res.exit, res.out())
	}
	cl, err := client.DialSocket(ctx, a.sock())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	var log strings.Builder
	if err := cl.Log(ctx, res.jobID, false, &log); err != nil || log.String() != "remote\n" {
		t.Fatalf("history %q %v", log.String(), err)
	}
}
