package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
)

// breakSpool makes every further write to the spool's file fail, as a full
// disk would.
func breakSpool(t *testing.T, sp *spool) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	f, err := os.Open(sp.path)
	if err != nil {
		t.Error(err)
		return
	}
	f.Close()
	sp.f.Close()
	sp.f = f // closed: Write fails
}

// A full disk under the state dir must not cut the run session off: the
// decision, output and exit still reach the CLI.
func TestSpoolKeepsFramesWhenTheDiskIsFull(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.place = func(job *proto.Job, nodes map[string]proto.NodeState, rej map[string]bool) (scheduler.Placement, string, bool) {
		if e := a.daemon().jobs.get(job.Spec.ID); e != nil {
			breakSpool(t, e.spool)
		}
		return alwaysLocal(job, nodes, rej)
	}
	a.start()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(5 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo hi; exit 4"), cliOpts{})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if res.exit == nil || res.exit.Code != 4 || res.out() != "hi\n" {
		t.Fatalf("exit %+v out %q", res.exit, res.out())
	}
}

func TestSpoolMemoryTail(t *testing.T) {
	sp, err := openSpool(t.TempDir() + "/s")
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	sp.append(proto.FrameDecision, []byte(`{}`))
	sp.append(proto.FrameStdout, []byte("on disk "))
	breakSpool(t, sp)
	if err := sp.appendData(proto.FrameStdout, []byte("in memory")); err != nil {
		t.Fatal(err)
	}
	if err := sp.append(proto.FrameExit, []byte(`{"code":0}`)); err != nil {
		t.Fatal(err)
	}
	if sp.outputBytes() != 17 {
		t.Fatalf("out %d", sp.outputBytes())
	}
	for _, off := range []int64{0, 3, 8, 12} {
		rd, err := sp.reader(off, false)
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		var types []proto.FrameType
		for {
			ft, p, err := rd.Next(context.Background())
			if err != nil {
				break
			}
			types = append(types, ft)
			if isOutput(ft) {
				out.Write(p)
			}
		}
		rd.Close()
		if want := "on disk in memory"[off:]; out.String() != want || types[len(types)-1] != proto.FrameExit {
			t.Fatalf("offset %d: %q %v", off, out.String(), types)
		}
	}
}

// Signals and resizes for the target overtake queued input (Ctrl-C must not
// wait behind megabytes of stdin); input keeps its own order, EOF last.
func TestOutboxControlBeforeInput(t *testing.T) {
	o := newOutbox()
	o.put(proto.FrameStdin, []byte("a"))
	o.put(proto.FrameStdin, []byte("b"))
	o.put(proto.FrameStdinEOF, nil)
	o.put(proto.FrameSignal, []byte(`{"name":"INT"}`))
	o.put(proto.FrameResize, []byte(`{}`))
	var got []string
	for f, ok := o.next(true); ok; f, ok = o.next(true) {
		got = append(got, f.t.String()+":"+string(f.p))
	}
	want := `signal:{"name":"INT"} resize:{} stdin:a stdin:b stdin_eof:`
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
}
