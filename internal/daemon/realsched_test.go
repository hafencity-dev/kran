package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// realCluster runs daemons with C's scheduler and estimator.
func realCluster(t *testing.T) (*cluster, *testNode, *testNode) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.real, b.real = true, true
	b.start()
	a.start()
	waitFor(t, 10*time.Second, "b online in a's real scheduler", func() bool {
		for _, n := range a.daemon().State().Nodes {
			if n.Info.Name == "b" && n.Status == proto.NodeOnline && n.Info.CPUs > 0 {
				return true
			}
		}
		return false
	})
	return c, a, b
}

func TestRealSchedulerPinnedRemoteAndLocal(t *testing.T) {
	_, a, b := realCluster(t)
	ws := workspaceDir(t, map[string]string{"f": "real\n"})
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()

	spec := shSpec(ws, "cat f; exit 5")
	spec.Req.Node = "b"
	res, err := runCLI(ctx, a.sock(), spec, cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Node != "b" || res.exit.Code != 5 || res.out() != "real\n" {
		t.Fatalf("pinned: exit %+v out %q notices %+v", res.exit, res.out(), res.notices)
	}
	// b's scheduler admitted it as a guest and freed it afterwards.
	waitFor(t, 3*time.Second, "b free", func() bool {
		return len(b.rsched.Snapshot().Jobs["b"]) == 0 && b.exec.heldSlots() == 0
	})

	res, err = runCLI(ctx, a.sock(), shSpec(ws, "echo here"), cliOpts{})
	if err != nil || res.exit.Code != 0 || res.out() != "here\n" {
		t.Fatalf("auto: %v %+v %q", err, res.exit, res.out())
	}
	if len(a.rsched.Snapshot().Placements) != 0 {
		t.Fatal("placements leaked")
	}
}

func TestRealSchedulerRemoteFailures(t *testing.T) {
	_, a, b := realCluster(t)
	b.exec.mu.Lock()
	b.exec.startErr = errors.New("runtime broken")
	b.exec.mu.Unlock()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()

	// --remote: no fallback, exit 125 with the scheduler's reason.
	spec := shSpec(ws, "echo should-not-run")
	spec.Req.Prefer = proto.PreferRemote
	res, err := runCLI(ctx, a.sock(), spec, cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != proto.ExitInfra || res.out() != "" || !strings.Contains(res.exit.Reason, "--remote") {
		t.Fatalf("remote: exit %+v out %q", res.exit, res.out())
	}

	// Pinned to b without --no-fallback: the session ends with FrameError
	// before start, which makes kran run execute the command itself.
	spec = shSpec(ws, "echo fell back")
	spec.Req.Node = "b"
	res, err = runCLI(ctx, a.sock(), spec, cliOpts{})
	if err == nil || !strings.Contains(err.Error(), "no other node can run it") || res.exit != nil {
		t.Fatalf("pinned: err %v exit %+v", err, res.exit)
	}
	j := storedJob(t, a, res.jobID)
	if j.State != proto.JobCanceled || !j.Fallback {
		t.Fatalf("job %+v", j)
	}
	waitFor(t, 3*time.Second, "b clean", func() bool {
		return len(b.rsched.Snapshot().Jobs["b"]) == 0 && b.exec.heldSlots() == 0
	})
}
