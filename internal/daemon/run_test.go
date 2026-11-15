package daemon

import (
	"context"
	"encoding/json"
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
	// No --mem and no kran.slice above local jobs (no systemd here): the
	// node's default cap from the estimate (SPEC §6.8; the fake books 1 GiB).
	if env.MemMax != 8<<30 || env.Slice != "" || env.Env["KRAN_JOB_ID"] != res.jobID {
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

// A file that changes while it is sent (a log being written) makes the
// sync scan again instead of failing the remote start.
func TestRemoteSyncRescansChangedFile(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	b.start()
	a.start()
	c.waitOnline("a", "b")
	ws := workspaceDir(t, map[string]string{"in.txt": "payload\n"})
	a.scanner.mu.Lock()
	a.scanner.changeOpens = 1
	a.scanner.mu.Unlock()
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "cat in.txt"), cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 0 || res.exit.Node != "b" || res.exit.Fallback || res.out() != "payload\n" {
		t.Fatalf("exit %+v out %q", res.exit, res.out())
	}
	a.scanner.mu.Lock()
	scans := a.scanner.scans
	a.scanner.mu.Unlock()
	if scans != 2 {
		t.Fatalf("%d scans, want 2 (one again after the change)", scans)
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
			var n proto.Notice
			if ft == proto.FrameNotice && json.Unmarshal(p, &n) == nil && n.Message != "" {
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
	if w := res.waitNotices(); len(w) == 0 || w[0].Kind != "queued" || !strings.Contains(w[0].Message, "not online") {
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

// TestLocalEnvelopeCapsOnlyAnExplicitMem: --mem caps a local job (twice
// what was asked, at least 2 GiB); without it the envelope carries no cap.
func TestLocalEnvelopeCapsOnlyAnExplicitMem(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.start()
	ws := workspaceDir(t, map[string]string{"f.txt": "hi"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	spec := shSpec(ws, "true")
	spec.Req.Mem = 3 << 30
	res, err := runCLI(ctx, a.sock(), spec, cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.decisions) != 1 || res.decisions[0].Envelope == nil {
		t.Fatalf("decisions %+v", res.decisions)
	}
	// (The fake scheduler books 1 GiB whatever was asked: twice that, at
	// least 2 GiB.)
	if env := res.decisions[0].Envelope; env.MemMax < 2<<30 {
		t.Fatalf("--mem: envelope %+v, want a cap", env)
	}
}

type fakeSlice struct{}

func (fakeSlice) Name() string    { return "kran.slice" }
func (fakeSlice) Available() bool { return true }
func (fakeSlice) Refresh()        {}

// TestLocalEnvelopeNoCapUnderKranSlice: with kran.slice above local jobs
// (it bounds them all), a job without --mem gets no cap of its own; with
// --mem it does.
func TestLocalEnvelopeNoCapUnderKranSlice(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.slice = fakeSlice{}
	a.start()
	ws := workspaceDir(t, map[string]string{"f.txt": "hi"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	for _, mem := range []int64{0, 3 << 30} {
		spec := shSpec(ws, "true")
		spec.Req.Mem = mem
		res, err := runCLI(ctx, a.sock(), spec, cliOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.decisions) != 1 || res.decisions[0].Envelope == nil {
			t.Fatalf("decisions %+v", res.decisions)
		}
		env := res.decisions[0].Envelope
		if env.Slice != "kran.slice" || (mem == 0) != (env.MemMax == 0) {
			t.Errorf("--mem %d: envelope %+v", mem, env)
		}
	}
}

// TestJobIsNamedBeforeAnything: the first frame a kran run gets is a notice
// with the job's ID, before any wait reason or decision, so a kran run that
// loses this daemon at any moment after its submit reattaches to the job
// instead of running it itself next to the queue (SPEC §6.1 step 8).
func TestJobIsNamedBeforeAnything(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.start()
	ws := workspaceDir(t, map[string]string{"f.txt": "hi"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	var first proto.FrameType
	var firstNotice proto.Notice
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "true"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if first == 0 {
				first = ft
				json.Unmarshal(p, &firstNotice)
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if first != proto.FrameNotice || firstNotice.Kind != "queued" || firstNotice.JobID == "" || firstNotice.JobID != res.jobID || firstNotice.Message != "" {
		t.Fatalf("first frame %v %+v (job %s)", first, firstNotice, res.jobID)
	}
}

// TestHoldKeepsNewJobsOutOfTheQueue: while a restart waits (kran service
// restart, kran upgrade), a new job is accepted and named, told why it
// waits, kept out of the scheduler and marked Held (the restart does not
// wait for it); when the hold ends it runs as usual.
func TestHoldKeepsNewJobsOutOfTheQueue(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.start()
	ws := workspaceDir(t, map[string]string{"f.txt": "hi"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	cl, err := client.DialSocket(ctx, a.sock())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	h, ok := cl.(interface {
		HoldNew(context.Context, time.Duration, string) error
		ReleaseHold(context.Context) error
	})
	if !ok {
		t.Fatal("the client cannot hold")
	}
	if err := h.HoldNew(ctx, time.Minute, "a restart of kran is pending"); err != nil {
		t.Fatal(err)
	}
	sawHeld := false
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "true"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			var n proto.Notice
			if ft != proto.FrameNotice || json.Unmarshal(p, &n) != nil || !strings.Contains(n.Message, "a restart of kran is pending") {
				return nil
			}
			if q := a.scheduler().Snapshot().Queue; len(q) != 0 {
				t.Errorf("a held job reached the scheduler: %v", q)
			}
			if j, ok := a.daemon().jobs.find(n.JobID); !ok || !j.Held || j.State != proto.JobQueued {
				t.Errorf("held job %+v", j)
			}
			sawHeld = true
			return h.ReleaseHold(ctx)
		}})
	if err != nil {
		t.Fatal(err)
	}
	if !sawHeld || res.exit == nil || res.exit.Code != 0 {
		t.Fatalf("held %v exit %+v", sawHeld, res.exit)
	}
	if j, _ := a.daemon().jobs.find(res.jobID); j.Held {
		t.Errorf("the job is still marked held after it ran: %+v", j)
	}
}
