package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
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
	ctx, cancel := testContext(20 * time.Second)
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
	ctx, cancel := testContext(20 * time.Second)
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

// failingWrites is a stream whose writes fail while reads keep working
// (one direction of a dying connection breaks first).
type failingWrites struct{ netConn }

func (failingWrites) Write([]byte) (int, error) { return 0, errors.New("write broke") }

// pumpJob must not return while its reader can still append output: the
// caller reattaches at spool.outputBytes(), and output appended after that
// point would be delivered twice.
func TestPumpJobStopsReadingBeforeReturning(t *testing.T) {
	n := newCluster(t, "a").nodes["a"]
	d, err := New(Deps{Config: n.cfg, Dirs: n.dirs, Store: n.store, Sched: newFakeSched("a", alwaysLocal), Est: n.est,
		Prober: n.prober, Timing: n.timing, Logf: n.log.logf("a")})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := openSpool(filepath.Join(t.TempDir(), "spool"))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	e := d.jobs.newEntry(proto.Job{Spec: proto.JobSpec{ID: "0000000001"}}, sp)
	originSide, targetSide := net.Pipe()
	defer targetSide.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if proto.WriteFrame(targetSide, proto.FrameStdout, []byte("0123456789")) != nil {
				return
			}
		}
	}()
	pc := newPeerConn(failingWrites{originSide})
	last := d.now()
	res := d.pumpJob(context.Background(), e, "b", pc, &last)
	if res.err == nil {
		t.Fatalf("pump ended without the write error: %+v", res)
	}
	at := sp.outputBytes()
	time.Sleep(100 * time.Millisecond)
	if after := sp.outputBytes(); after != at {
		t.Fatalf("output kept arriving after pumpJob returned: %d → %d bytes", at, after)
	}
}

// slowFirstSet delays the first write of a key, like a busy sqlite.
type slowFirstSet struct {
	*memStore
	mu   sync.Mutex
	seen map[string]bool
}

func (s *slowFirstSet) Set(ctx context.Context, key, value string) error {
	s.mu.Lock()
	first := !s.seen[key]
	s.seen[key] = true
	s.mu.Unlock()
	if first {
		time.Sleep(100 * time.Millisecond)
	}
	return s.memStore.Set(ctx, key, value)
}

// The record of started jobs is what a restarted target uses to release
// their slots: the last state must win even when writes overlap.
func TestTargetJobRecordLastWriteWins(t *testing.T) {
	n := newCluster(t, "a").nodes["a"]
	st := &slowFirstSet{memStore: newMemStore(), seen: map[string]bool{}}
	d, err := New(Deps{Config: n.cfg, Dirs: n.dirs, Store: st, Sched: newFakeSched("a", alwaysLocal), Est: n.est,
		Prober: n.prober, Timing: n.timing, Logf: n.log.logf("a")})
	if err != nil {
		t.Fatal(err)
	}
	tg := d.target
	slot := proto.SlotRef{Node: "a", RepoID: "r", Index: 1, Path: "/x"}
	tg.mu.Lock()
	tg.jobs["0000000001"] = &tjob{ID: "0000000001", Origin: "o", started: true}
	tg.persistLocked()
	tg.jobs["0000000002"] = &tjob{ID: "0000000002", Origin: "o", started: true, Slot: &slot}
	tg.persistLocked()
	tg.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	v, _, _ := st.Get(context.Background(), targetJobsKey)
	var recs map[string]tjobRecord
	json.Unmarshal([]byte(v), &recs)
	if r, ok := recs["0000000002"]; !ok || r.Slot == nil || len(recs) != 2 {
		t.Fatalf("persisted record is stale: %s", v)
	}
}

// A job that finishes while its target daemon is down still holds its slot
// in execenv (released only with the persisted SlotRef): the restarted
// daemon must release it, not leave it to the reaper's 30 min safety net.
func TestTargetRestartReleasesSlotOfJobThatEndedMeanwhile(t *testing.T) {
	_, a, b := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	var once sync.Once
	restarted := make(chan struct{})
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo x; sleep 0.3; echo y"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout {
				once.Do(func() {
					go func() {
						defer close(restarted)
						b.stop()
						waitFor(t, 5*time.Second, "job ended on b", func() bool { return len(b.exec.Active()) == 0 })
						b.start()
					}()
				})
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	<-restarted
	if res.exit == nil || res.exit.Code != 0 || res.out() != "x\ny\n" {
		t.Fatalf("exit %+v out %q", res.exit, res.out())
	}
	waitFor(t, 5*time.Second, "slot released after restart", func() bool { return b.exec.heldSlots() == 0 })
}

// A job that was still syncing when its origin daemon restarted is run
// locally by its kran run (it had not started). The restarted daemon must
// not place and start it again while nobody is attached: that would run the
// command twice.
func TestRestartedOriginDoesNotRerunPrestartJobWithoutCLI(t *testing.T) {
	_, a, b := twoNodes(t)
	gate := make(chan struct{})
	b.exec.mu.Lock()
	b.exec.acquireGate = gate
	b.exec.mu.Unlock()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	var once sync.Once
	res, _ := runCLI(ctx, a.sock(), shSpec(ws, "echo ran"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameDecision {
				once.Do(func() { go a.stop() })
			}
			return nil
		}})
	waitFor(t, 10*time.Second, "origin stopped", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.cancel == nil
	})
	if res.jobID == "" {
		t.Fatal("no job id")
	}
	b.exec.mu.Lock()
	b.exec.acquireGate = nil
	b.exec.mu.Unlock()
	close(gate)
	a.start()
	waitFor(t, 15*time.Second, "job ended", func() bool {
		j, err := a.store.GetJob(context.Background(), res.jobID)
		return err == nil && j.State.Terminal()
	})
	if n, _, _ := b.exec.counts(); n != 0 {
		t.Fatalf("the job was started on b %d time(s) after the origin restart with no kran run attached", n)
	}
}

// Same for a job still in setup on the target: its kran run did not see it
// start and runs it locally, so the restarted origin must stop it before
// the command starts there too.
func TestRestartedOriginStopsJobStillInSetup(t *testing.T) {
	_, a, b := twoNodes(t)
	b.exec.mu.Lock()
	b.exec.setup = 1500 * time.Millisecond
	b.exec.mu.Unlock()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	marker := filepath.Join(shortDir(t), "ran")
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	var once sync.Once
	res, _ := runCLI(ctx, a.sock(), shSpec(ws, "touch "+marker), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			var ph proto.Phase
			if ft == proto.FramePhase && json.Unmarshal(p, &ph) == nil && ph.State == proto.JobSetup {
				once.Do(func() { go a.stop() })
			}
			return nil
		}})
	waitFor(t, 10*time.Second, "origin stopped", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.cancel == nil
	})
	a.start()
	waitFor(t, 10*time.Second, "job ended", func() bool {
		j, err := a.store.GetJob(context.Background(), res.jobID)
		return err == nil && j.State.Terminal()
	})
	time.Sleep(2 * time.Second) // past the setup
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran on b although its kran run had already run it locally")
	}
	waitFor(t, 5*time.Second, "b clean", func() bool { return len(b.exec.Active()) == 0 && b.exec.heldSlots() == 0 })
}
