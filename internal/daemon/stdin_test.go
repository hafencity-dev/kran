package daemon

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// bigInput is n bytes of numbered lines, so a gap or a repeat shows.
func bigInput(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "line %08d\n", i)
	}
	return b.Bytes()
}

// Input far beyond what the target may queue reaches a slow job complete
// and in order: the origin forwards only what the target has room for.
func TestRemoteStdinFlowControl(t *testing.T) {
	_, a, _ := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	input := bigInput(1 << 20) // 4× the target's queue in tests
	spec := shSpec(ws, "sleep 1; cat")
	spec.Stdin = true
	res, err := runCLI(ctx, a.sock(), spec, cliOpts{stdin: input})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 0 {
		t.Fatalf("exit %+v", res.exit)
	}
	if out := res.out(); out != string(input) {
		t.Fatalf("job read %d of %d input bytes (first difference at %d)", len(out), len(input), firstDiff(out, string(input)))
	}
}

func firstDiff(a, b string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// A cut while input is on its way: the new stream says what the target
// holds and kran run sends the rest again, so the job reads every byte once.
func TestRemoteStdinSurvivesCut(t *testing.T) {
	c, a, _ := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(60 * time.Second)
	defer cancel()
	input := bigInput(512 << 10)
	spec := shSpec(ws, "while IFS= read -r l; do echo \"$l\"; done")
	spec.Stdin = true
	var once sync.Once
	res, err := runCLI(ctx, a.sock(), spec, cliOpts{stdin: input,
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout {
				once.Do(func() { c.net.Cut("a") })
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 0 {
		t.Fatalf("exit %+v", res.exit)
	}
	if out := res.out(); out != string(input) {
		t.Fatalf("job read %d of %d input bytes (first difference at %d)", len(out), len(input), firstDiff(out, string(input)))
	}
}

// Ctrl-C reaches a job that never reads the input piling up for it.
func TestRemoteSignalNotBehindStdin(t *testing.T) {
	_, a, _ := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	spec := shSpec(ws, "echo up; sleep 30")
	spec.Stdin = true
	var sentAt time.Time
	res, err := runCLI(ctx, a.sock(), spec, cliOpts{stdin: bigInput(4 << 20),
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout && sentAt.IsZero() {
				time.Sleep(300 * time.Millisecond) // the input backs up
				sentAt = time.Now()
				return rs.SendJSON(proto.FrameSignal, proto.Signal{Name: "INT"})
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 130 {
		t.Fatalf("exit %+v", res.exit)
	}
	if el := time.Since(sentAt); el > 2*time.Second {
		t.Fatalf("Ctrl-C took %s", el)
	}
}

// The origin daemon restarts while input is on its way: the reattaching
// kran run sends again from what the target holds.
func TestRemoteStdinSurvivesOriginRestart(t *testing.T) {
	_, a, _ := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(60 * time.Second)
	defer cancel()
	input := bigInput(512 << 10)
	spec := shSpec(ws, "while IFS= read -r l; do echo \"$l\"; done")
	spec.Stdin = true
	res := &cliResult{}
	var once sync.Once
	err := runCLIInto(ctx, a.sock(), spec, cliOpts{stdin: input,
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout {
				once.Do(func() { go a.stop() })
			}
			return nil
		}}, res)
	if err == nil {
		t.Fatal("session survived the daemon stop")
	}
	waitFor(t, 10*time.Second, "origin stopped", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.cancel == nil
	})
	a.start()
	err = runCLIInto(ctx, a.sock(), proto.JobSpec{}, cliOpts{stdin: input,
		reattach: &proto.Reattach{JobID: res.jobID, Offset: res.offset}}, res)
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 0 {
		t.Fatalf("exit %+v", res.exit)
	}
	if out := res.out(); out != string(input) {
		t.Fatalf("job read %d of %d input bytes (first difference at %d)", len(out), len(input), firstDiff(out, string(input)))
	}
}

// The target daemon restarts while input is on its way: nobody can tell how
// much the job read, so its input is closed, loudly, never silently cut.
func TestRemoteStdinTargetRestartClosesInputLoudly(t *testing.T) {
	_, a, b := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(60 * time.Second)
	defer cancel()
	input := bigInput(512 << 10)
	spec := shSpec(ws, "while IFS= read -r l; do echo \"$l\"; done; echo END")
	spec.Stdin = true
	var once sync.Once
	res, err := runCLI(ctx, a.sock(), spec, cliOpts{stdin: input,
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout {
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
	if res.exit == nil || res.exit.Code != 0 || !strings.HasSuffix(res.out(), "END\n") {
		t.Fatalf("exit %+v, out ends %q", res.exit, res.out()[max(0, len(res.out())-40):])
	}
	warned := false
	for _, n := range res.notices {
		if n.Kind == "warn" && strings.Contains(n.Message, "input was closed early") {
			warned = true
		}
	}
	if len(res.out()) < len(input)+4 && !warned {
		t.Fatalf("input was cut (%d of %d bytes) without a warning; notices %+v", len(res.out())-4, len(input), res.notices)
	}
}

// Two Resume requests can be outstanding (kran run reattached while the
// target stream was renewed): the answer to the older one must not count
// as "kran run lost the input".
func TestStaleResumeAnswerIsIgnored(t *testing.T) {
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
	e := d.jobs.newEntry(proto.Job{Spec: proto.JobSpec{ID: "0000000001", Stdin: true}}, sp)
	e.stdinLimit()
	e.targetStream(1 << 20)
	e.cliInput(proto.FrameStdin, make([]byte, 100))
	e.cliReattached()                                                               // asks kran run for input from 100
	e.targetReport(stdinReport{Received: 60, Consumed: 50, Known: true}, true, "b") // and then from 60
	e.cliResume(proto.StdinResume{Offset: 100})                                     // the answer to the first request
	e.in.mu.Lock()
	closed, waiting := e.in.closed, e.in.awaitResume
	e.in.mu.Unlock()
	if closed || !waiting {
		t.Fatalf("closed %v, waiting %v after a stale answer", closed, waiting)
	}
	e.cliResume(proto.StdinResume{Offset: 60})
	e.cliInput(proto.FrameStdin, make([]byte, 10))
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	if e.in.closed || e.in.awaitResume || e.in.next != 70 {
		t.Fatalf("closed %v waiting %v next %d", e.in.closed, e.in.awaitResume, e.in.next)
	}
}
