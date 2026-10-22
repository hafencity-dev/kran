package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// Exactly once (SPEC §6.1): a remote job runs on its target or here, never
// both. These tests hold each window between placement and the running
// phase open with hooks and check where the command ran.

// runs counts the command's runs: each appends one line to the marker.
func runs(t *testing.T, marker string) int {
	t.Helper()
	b, _ := os.ReadFile(marker)
	return len(strings.Fields(string(b)))
}

func onceScript(marker string, code int) string {
	return fmt.Sprintf("echo ran >> %s; exit %d", marker, code)
}

func isPhase(ft proto.FrameType, p []byte, st proto.JobState) bool {
	var ph proto.Phase
	return ft == proto.FramePhase && json.Unmarshal(p, &ph) == nil && ph.State == st
}

func waitStopped(t *testing.T, n *testNode) {
	t.Helper()
	waitFor(t, 10*time.Second, n.name+" stopped", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.cancel == nil
	})
}

// The origin stops gracefully while the job is in setup on its target and
// comes back; kran run reattaches (it does not run the job itself). The
// job must go on on the target and run exactly once, whether its setup
// ended while the origin was down (the command then waits for a heartbeat)
// or not.
func TestOriginStopDuringSetupReattachRunsOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		downTime time.Duration
	}{{"setup ends while down", 1200 * time.Millisecond}, {"setup still running", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			_, a, b := twoNodes(t)
			b.exec.mu.Lock()
			b.exec.setup = 800 * time.Millisecond
			b.exec.leaseTTL = 6 * time.Second // the target keeps the job while the origin is away
			b.exec.mu.Unlock()
			ws := workspaceDir(t, map[string]string{"f": "x"})
			marker := filepath.Join(shortDir(t), "ran")
			ctx, cancel := testContext(40 * time.Second)
			defer cancel()
			res := &cliResult{}
			var once sync.Once
			err := runCLIInto(ctx, a.sock(), shSpec(ws, onceScript(marker, 5)), cliOpts{
				hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
					if isPhase(ft, p, proto.JobSetup) {
						once.Do(func() { go a.stop() })
					}
					return nil
				}}, res)
			if err == nil {
				t.Fatalf("session survived the daemon stop: exit %+v", res.exit)
			}
			waitStopped(t, a)
			time.Sleep(tc.downTime)
			a.start()
			err = runCLIInto(ctx, a.sock(), proto.JobSpec{}, cliOpts{reattach: &proto.Reattach{JobID: res.jobID, Offset: res.offset}}, res)
			if err != nil {
				t.Fatalf("reattach: %v", err)
			}
			if res.exit == nil || res.exit.Code != 5 || res.exit.Node != "b" {
				t.Fatalf("exit %+v", res.exit)
			}
			if n := runs(t, marker); n != 1 {
				t.Fatalf("the command ran %d times", n)
			}
			waitFor(t, 5*time.Second, "b clean", func() bool { return len(b.exec.Active()) == 0 && b.exec.heldSlots() == 0 })
		})
	}
}

// job.start reaches the target but its answer does not come back in time
// (a slow start under load). The origin must not run the job elsewhere: it
// finds out from the target that the job started and follows it.
func TestStartNotAcknowledgedFollowsTheJob(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	a.timing.PrestartTimeout = 500 * time.Millisecond
	gate := make(chan struct{})
	b.exec.startGate = gate
	b.start()
	a.start()
	c.waitOnline("a", "b")
	go func() {
		waitFor(t, 10*time.Second, "Start entered on b", func() bool {
			n, _, _ := b.exec.counts()
			return n == 1
		})
		time.Sleep(a.timing.PrestartTimeout + 300*time.Millisecond) // the origin gave up waiting
		close(gate)
	}()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	marker := filepath.Join(shortDir(t), "ran")
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, onceScript(marker, 3)), cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := runs(t, marker); n != 1 {
		t.Fatalf("the command ran %d times (decisions %+v, exit %+v)", n, res.decisions, res.exit)
	}
	if res.exit == nil || res.exit.Code != 3 || res.exit.Node != "b" {
		t.Fatalf("exit %+v", res.exit)
	}
}

// kran run loses its session (the daemon stays up) while the job is
// synced to its target. It reattaches; the job must not be canceled for
// it, and it runs once.
func TestCLISessionDropBeforeStartReattaches(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	a.clientAlive = func(int) bool { return true } // the dropped kran run lives on
	gate := make(chan struct{})
	b.exec.putBlobGate = gate
	b.start()
	a.start()
	c.waitOnline("a", "b")
	ws := workspaceDir(t, map[string]string{"big": strings.Repeat("x", 1<<20), "small": "y"})
	marker := filepath.Join(shortDir(t), "ran")
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	res := &cliResult{}
	err := runCLIInto(ctx, a.sock(), shSpec(ws, onceScript(marker, 4)), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FramePhase && strings.Contains(string(p), "sending") {
				return errCLIDies
			}
			return nil
		}}, res)
	if err != errCLIDies {
		t.Fatalf("err %v exit %+v", err, res.exit)
	}
	time.Sleep(300 * time.Millisecond)
	close(gate)
	err = runCLIInto(ctx, a.sock(), proto.JobSpec{}, cliOpts{reattach: &proto.Reattach{JobID: res.jobID, Offset: res.offset}}, res)
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 4 || res.exit.Node != "b" {
		t.Fatalf("exit %+v", res.exit)
	}
	if n := runs(t, marker); n != 1 {
		t.Fatalf("the command ran %d times", n)
	}
}

// The origin loses the target for good while the job is in setup there.
// The setup may have ended and the command started before the link went
// (the running phase lost on the way), so the origin cannot tell: it must
// not run the job here as well, but end it with 125.
func TestTargetLostDuringSetupIsNotRunHere(t *testing.T) {
	c, a, b := twoNodes(t)
	b.exec.mu.Lock()
	b.exec.setup = 3 * time.Second
	b.exec.mu.Unlock()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	marker := filepath.Join(shortDir(t), "ran")
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	var once sync.Once
	res, err := runCLI(ctx, a.sock(), shSpec(ws, onceScript(marker, 4)), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if isPhase(ft, p, proto.JobSetup) {
				once.Do(func() { c.net.SetDown("b", true) })
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.decisions {
		if d.Mode == "local" {
			t.Fatalf("ran the job here although it may have started on b: %+v", res.decisions)
		}
	}
	if res.exit == nil || res.exit.Code != proto.ExitInfra || !strings.Contains(res.exit.Reason, "may have started") {
		t.Fatalf("exit %+v", res.exit)
	}
	if n := runs(t, marker); n != 0 {
		t.Fatalf("the command ran %d times", n)
	}
}

// The target daemon restarts while the job is in setup there: the origin
// reattaches and the job runs once, on the target.
func TestTargetRestartDuringSetupRunsOnce(t *testing.T) {
	_, a, b := twoNodes(t)
	b.exec.mu.Lock()
	b.exec.setup = 800 * time.Millisecond
	b.exec.mu.Unlock()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	marker := filepath.Join(shortDir(t), "ran")
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	var once sync.Once
	res, err := runCLI(ctx, a.sock(), shSpec(ws, onceScript(marker, 6)), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if isPhase(ft, p, proto.JobSetup) {
				once.Do(func() {
					go func() {
						b.stop()
						b.start()
					}()
				})
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 6 || res.exit.Node != "b" {
		t.Fatalf("exit %+v decisions %+v", res.exit, res.decisions)
	}
	if n := runs(t, marker); n != 1 {
		t.Fatalf("the command ran %d times", n)
	}
}

// The origin stops while job.start is still in flight on the target (its
// answer never arrives). The restarted origin must settle that with the
// target before placing the job again: the start goes through there, so
// the job is followed, not run a second time.
func TestOriginStopWhileStartInFlight(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	gate := make(chan struct{})
	b.exec.startGate = gate
	b.start()
	a.start()
	c.waitOnline("a", "b")
	ws := workspaceDir(t, map[string]string{"f": "x"})
	marker := filepath.Join(shortDir(t), "ran")
	ctx, cancel := testContext(40 * time.Second)
	defer cancel()
	res := &cliResult{}
	var once sync.Once
	err := runCLIInto(ctx, a.sock(), shSpec(ws, onceScript(marker, 7)), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameDecision {
				once.Do(func() {
					go func() {
						waitFor(t, 10*time.Second, "Start entered on b", func() bool {
							n, _, _ := b.exec.counts()
							return n == 1
						})
						a.stop()
					}()
				})
			}
			return nil
		}}, res)
	if err == nil {
		t.Fatalf("session survived the daemon stop: exit %+v", res.exit)
	}
	waitStopped(t, a)
	a.start()
	time.AfterFunc(300*time.Millisecond, func() { close(gate) })
	err = runCLIInto(ctx, a.sock(), proto.JobSpec{}, cliOpts{reattach: &proto.Reattach{JobID: res.jobID, Offset: res.offset}}, res)
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := runs(t, marker); n != 1 {
		t.Fatalf("the command ran %d times (exit %+v)", n, res.exit)
	}
	if res.exit == nil || res.exit.Code != 7 || res.exit.Node != "b" {
		t.Fatalf("exit %+v", res.exit)
	}
	if n, _, _ := b.exec.counts(); n != 1 {
		t.Fatalf("job.start reached b %d times", n)
	}
}
