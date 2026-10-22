package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// A cancel that reaches the target while job.start is still in flight (an
// image build, a slow runtime) must neither release the slot under the
// starting job nor leave the job running once Start returns.
func TestCancelDuringStartLeavesNoOrphan(t *testing.T) {
	_, a, b := twoNodes(t)
	gate := make(chan struct{})
	b.exec.mu.Lock()
	b.exec.startGate = gate
	b.exec.mu.Unlock()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sent := false
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo started; sleep 30"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameDecision && !sent {
				sent = true
				go func() {
					waitFor(t, 5*time.Second, "Start entered on b", func() bool {
						n, _, _ := b.exec.counts()
						return n == 1
					})
					rs.SendJSON(proto.FrameSignal, proto.Signal{Name: "INT"})
				}()
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 130 || res.exit.State != proto.JobCanceled {
		t.Fatalf("exit %+v", res.exit)
	}
	// The slot must still be held while Start runs.
	time.Sleep(200 * time.Millisecond)
	if b.exec.heldSlots() != 1 {
		t.Fatalf("slot released while Start was still running (held %d)", b.exec.heldSlots())
	}
	close(gate)
	waitFor(t, 5*time.Second, "job gone on b", func() bool { return len(b.exec.Active()) == 0 })
	if p := b.exec.proc(res.jobID); p != nil {
		p.mu.Lock()
		lost := p.lost
		p.mu.Unlock()
		if lost {
			t.Fatal("the started job ran on until its lease expired instead of being canceled")
		}
	}
	waitFor(t, 3*time.Second, "slot and allocation released on b", func() bool {
		return b.exec.heldSlots() == 0 && len(b.daemon().target.status().Allocs) == 0
	})
	time.Sleep(300 * time.Millisecond)
	if _, acq, rel := b.exec.counts(); acq != rel {
		t.Fatalf("slot acquired %d times, released %d times", acq, rel)
	}
}

// Exit.BeforeStart (execenv: setup failed, command not found on the
// target, container did not start) means nothing of the command ran: the
// origin falls back to running it locally (SPEC §6.1), whatever the state.
func TestEndedBeforeStartFallsBack(t *testing.T) {
	_, a, b := twoNodes(t)
	b.exec.mu.Lock()
	b.exec.endBeforeStart = &proto.Exit{Code: proto.ExitNotFound, State: proto.JobFailed, BeforeStart: true,
		Reason: "command not found on b: mytool"}
	b.exec.mu.Unlock()
	ws := workspaceDir(t, map[string]string{"f": "local data"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "cat f"), cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 0 || !res.exit.Fallback || res.out() != "local data" {
		t.Fatalf("exit %+v out %q", res.exit, res.out())
	}
	if n := len(res.decisions); n != 2 || res.decisions[1].Mode != "local" {
		t.Fatalf("decisions %+v", res.decisions)
	}
	waitFor(t, 3*time.Second, "slot released on b", func() bool { return b.exec.heldSlots() == 0 })
}
