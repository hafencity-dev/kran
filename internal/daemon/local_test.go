package daemon

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// kran cancel between the local decision and the CLI's started frame must
// still stop the child once the CLI reports it.
func TestCancelBeforeLocalStartedKillsChild(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.start()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	cl, err := client.DialSocket(ctx, a.sock())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	rs, err := cl.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	rs.SendJSON(proto.FrameSubmit, shSpec(ws, "sleep 60"))
	var dc proto.Decision
	for {
		ft, p, err := rs.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if ft == proto.FrameDecision {
			jsonUnmarshal(p, &dc)
			break
		}
	}
	if err := cl.Cancel(ctx, dc.JobID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the daemon has seen the cancel
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	rs.SendJSON(proto.FrameStarted, proto.Started{JobID: dc.JobID, PID: cmd.Process.Pid, PGID: cmd.Process.Pid})
	select {
	case <-waitErr:
	case <-time.After(waitCeiling):
		t.Fatal("the canceled job's child kept running")
	}
}
