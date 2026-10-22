package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// kran run drops a session it believes broken (a torn write, a daemon that
// looked silent) and reattaches. While its process is alive that drop is
// not a cancel: the job keeps running for the reattach.
func TestLiveCLIDropIsNotACancel(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	var alive atomic.Bool
	alive.Store(true)
	a.clientAlive = func(int) bool { return alive.Load() }
	// The reattach below must never race the grace on a loaded machine.
	a.timing.CLIDropGrace = time.Minute
	b.start()
	a.start()
	c.waitOnline("a", "b")
	ws := workspaceDir(t, map[string]string{"f": "x"})
	script, want := counterScript(30, "0.05")
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	res := &cliResult{}
	var once sync.Once
	err := runCLIInto(ctx, a.sock(), shSpec(ws, script), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout && reachedLine(r, p, "3") {
				var err error
				once.Do(func() { err = errCLIDies })
				return err
			}
			return nil
		}}, res)
	if err != errCLIDies {
		t.Fatalf("err %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := runCLIInto(ctx, a.sock(), proto.JobSpec{}, cliOpts{reattach: &proto.Reattach{JobID: res.jobID, Offset: res.offset}}, res); err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 0 || res.out() != want {
		t.Fatalf("exit %+v out %q", res.exit, res.out())
	}

	// A CLI whose process is gone is a cancel right away.
	alive.Store(false)
	res, err = runCLI(ctx, a.sock(), shSpec(ws, "echo go; sleep 30"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout {
				return errCLIDies
			}
			return nil
		}})
	waitFor(t, 2*time.Second, "job canceled", func() bool {
		j, err := a.store.GetJob(context.Background(), res.jobID)
		return err == nil && j.State == proto.JobCanceled
	})
}

// Before the command started, kran run runs it locally after a drop and
// never reattaches: even a live CLI's drop cancels at once.
func TestLiveCLIDropBeforeStartCancels(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	a.clientAlive = func(int) bool { return true }
	gate := make(chan struct{})
	defer close(gate)
	b.exec.mu.Lock()
	b.exec.acquireGate = gate
	b.exec.mu.Unlock()
	b.start()
	a.start()
	c.waitOnline("a", "b")
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	var id string
	runCLI(ctx, a.sock(), shSpec(ws, "echo ran"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameDecision {
				var dc proto.Decision
				json.Unmarshal(p, &dc)
				id = dc.JobID
				return errCLIDies
			}
			return nil
		}})
	waitFor(t, time.Second, "job canceled", func() bool {
		j, err := a.store.GetJob(context.Background(), id)
		return err == nil && j.State == proto.JobCanceled
	})
}
