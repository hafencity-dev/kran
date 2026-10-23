package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// TestRestoreReleasesSlotOfJobThatEndedMeanwhile: a job that ends between
// the executor's re-index (which marks its slot busy again) and the
// daemon's restore is no longer active, but its slot must still be freed.
// A slot that an active job holds stays busy.
func TestRestoreReleasesSlotOfJobThatEndedMeanwhile(t *testing.T) {
	c := newCluster(t, "b")
	b := c.nodes["b"]
	ended := proto.SlotRef{Node: "b", RepoID: "r1", Index: 0, Path: "/slots/r1/0"}
	b.exec.mu.Lock()
	b.exec.slots["r1/0"] = true
	b.exec.mu.Unlock()
	raw, _ := json.Marshal(map[string]tjobRecord{"0endedjob1": {Origin: "a", Slot: &ended}})
	b.store.Set(context.Background(), targetJobsKey, string(raw))
	b.start()
	waitFor(t, 5*time.Second, "slot of the ended job released", func() bool { return b.exec.heldSlots() == 0 })
}

// TestLocalExitSurvivesQuickQuit: a CLI that sends a burst of local output,
// its exit and quits at once gets that exit recorded, even when the
// daemon's own writes to the gone CLI fail first (short heartbeats).
func TestLocalExitSurvivesQuickQuit(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.timing.Heartbeat = 5 * time.Millisecond
	a.start()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	chunk := bytes.Repeat([]byte("o"), 32<<10)
	for i := 0; i < 8; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cl, err := client.DialSocket(ctx, a.sock())
		if err != nil {
			t.Fatal(err)
		}
		rs, err := cl.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rs.SendJSON(proto.FrameSubmit, shSpec(ws, "true"))
		var id string
		for id == "" {
			ft, p, err := rs.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if ft == proto.FrameDecision {
				var d proto.Decision
				json.Unmarshal(p, &d)
				id = d.JobID
			}
		}
		rs.SendJSON(proto.FrameStarted, proto.Started{JobID: id, PID: 999999, PGID: 999999})
		for j := 0; j < 64; j++ {
			rs.Send(proto.FrameStdout, chunk)
		}
		rs.SendJSON(proto.FrameExit, proto.Exit{JobID: id, Code: 3, State: proto.JobFailed})
		rs.Close()
		cl.Close()
		waitFor(t, 10*time.Second, "job "+id+" recorded", func() bool {
			j, err := a.store.GetJob(context.Background(), id)
			return err == nil && j.State.Terminal()
		})
		if j, _ := a.store.GetJob(context.Background(), id); j.State != proto.JobFailed || j.ExitCode == nil || *j.ExitCode != 3 {
			t.Fatalf("run %d: recorded %s %v (%s), the CLI reported exit 3", i, j.State, j.ExitCode, j.Reason)
		}
		cancel()
	}
}

// A restored job's CLI that reattached and already left again (a local job
// reporting its exit and quitting) counts as back: waiting for it longer
// would record the job lost.
func TestWaitForCLISeesALeftReattach(t *testing.T) {
	d := &Daemon{t: Timing{CLIReattachWait: 10 * time.Second}}
	d.jobs = newJobTable(d)
	e := d.jobs.newEntry(proto.Job{Spec: proto.JobSpec{ID: "j1"}}, nil)
	e.restored = true
	e.attach()
	e.mu.Lock()
	e.attached-- // left without the detach side effects
	e.mu.Unlock()
	start := time.Now()
	if !d.waitForCLI(context.Background(), e) {
		t.Fatal("waitForCLI gave up on a CLI that reattached")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("waited %s", time.Since(start))
	}
}
