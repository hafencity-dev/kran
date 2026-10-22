package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/transport"
)

func counterScript(n int, sleep string) (string, string) {
	script := fmt.Sprintf("i=0; while [ $i -lt %d ]; do echo $i; i=$((i+1)); sleep %s; done", n, sleep)
	var want strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&want, "%d\n", i)
	}
	return script, want.String()
}

// gatedCounterScript prints 0..n-1 like counterScript but stops after
// line at until the file gate exists: a disruption the test triggers at
// that line then always hits a running job, however slow the machine.
func gatedCounterScript(n, at int, gate string) (string, string) {
	script := fmt.Sprintf("i=0; while [ $i -lt %d ]; do echo $i; i=$((i+1)); if [ $i -eq %d ]; then while [ ! -f %s ]; do sleep 0.02; done; fi; sleep 0.01; done", n, at+1, gate)
	_, want := counterScript(n, "")
	return script, want
}

func twoNodes(t *testing.T) (*cluster, *testNode, *testNode) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	b.start()
	a.start()
	c.waitOnline("a", "b")
	return c, a, b
}

func TestFallbackAndCircuitBreaker(t *testing.T) {
	_, a, b := twoNodes(t)
	b.exec.mu.Lock()
	b.exec.startErr = errors.New("docker is gone")
	b.exec.mu.Unlock()
	ws := workspaceDir(t, map[string]string{"f": "local data"})
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	// The degraded window (BreakerCooldown) may pass while a loaded machine
	// finishes the third run: watch for it all along.
	var sawDegraded atomic.Bool
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go func() {
		for watchCtx.Err() == nil {
			if ns, ok := a.scheduler().node("b"); ok && ns.Status == proto.NodeDegraded && strings.Contains(ns.Reason, "failed before they started") {
				sawDegraded.Store(true)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	for i := 0; i < 3; i++ {
		res, err := runCLI(ctx, a.sock(), shSpec(ws, "cat f"), cliOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if res.exit == nil || res.exit.Code != 0 || res.out() != "local data" || !res.exit.Fallback {
			t.Fatalf("run %d: exit %+v out %q", i, res.exit, res.out())
		}
		if len(res.decisions) != 2 || res.decisions[0].Mode != "remote" || res.decisions[1].Mode != "local" {
			t.Fatalf("decisions %+v", res.decisions)
		}
		var fb *proto.Notice
		for i := range res.notices {
			if res.notices[i].Kind == "fallback" {
				fb = &res.notices[i]
			}
		}
		if fb == nil || !strings.Contains(fb.Message, "docker is gone") {
			t.Fatalf("no fallback notice: %+v", res.notices)
		}
		j := storedJob(t, a, res.jobID)
		if !j.Fallback || !j.Local || j.State != proto.JobSucceeded {
			t.Fatalf("job %+v", j)
		}
	}
	// Reservations and slots on b are released after each failure.
	waitFor(t, 3*time.Second, "b clean", func() bool {
		return b.exec.heldSlots() == 0 && len(b.daemon().target.status().Allocs) == 0
	})
	// Three failures in a row: b is degraded for a while.
	waitFor(t, 2*time.Second, "b degraded", sawDegraded.Load)
	if a.daemon().State().Stats.FallbacksToday != 3 {
		t.Fatalf("stats %+v", a.daemon().State().Stats)
	}
	// After the cool-down it is used again.
	b.exec.mu.Lock()
	b.exec.startErr = nil
	b.exec.mu.Unlock()
	waitFor(t, 6*time.Second, "b online again", func() bool {
		ns, _ := a.scheduler().node("b")
		return ns.Status == proto.NodeOnline
	})
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "cat f"), cliOpts{})
	if err != nil || res.exit.Node != "b" || res.exit.Fallback {
		t.Fatalf("after cool-down: %v %+v", err, res.exit)
	}
}

func TestNoFallbackFailsWith125(t *testing.T) {
	_, a, b := twoNodes(t)
	b.exec.mu.Lock()
	b.exec.startErr = errors.New("broken")
	b.exec.mu.Unlock()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	spec := shSpec(ws, "echo ran")
	spec.NoFallback = true
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), spec, cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit.Code != proto.ExitInfra || res.exit.State != proto.JobInfraErr || res.out() != "" {
		t.Fatalf("exit %+v out %q", res.exit, res.out())
	}
	if !strings.Contains(res.exit.Reason, "broken") {
		t.Fatalf("reason %q", res.exit.Reason)
	}
}

func TestCLIKilledMidSync(t *testing.T) {
	_, a, b := twoNodes(t)
	gate := make(chan struct{})
	b.exec.mu.Lock()
	b.exec.putBlobGate = gate
	b.exec.mu.Unlock()
	defer close(gate)
	ws := workspaceDir(t, map[string]string{"big": strings.Repeat("x", 1<<20), "small": "y"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo never"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FramePhase && strings.Contains(string(p), "sending") {
				return errCLIDies
			}
			return nil
		}})
	if !errors.Is(err, errCLIDies) {
		t.Fatalf("err %v exit %+v", err, res.exit)
	}
	waitFor(t, 5*time.Second, "job canceled at origin", func() bool {
		j, err := a.store.GetJob(context.Background(), res.jobID)
		return err == nil && j.State == proto.JobCanceled
	})
	waitFor(t, 5*time.Second, "reservation and slot released on b", func() bool {
		return b.exec.heldSlots() == 0 && len(b.daemon().target.status().Allocs) == 0
	})
	waitFor(t, time.Second, "origin allocation released", func() bool { return a.scheduler().allocCount() == 0 })
	if len(b.exec.Active()) != 0 {
		t.Fatal("job started on b although the CLI died during sync")
	}
}

func TestTargetDaemonRestartMidRun(t *testing.T) {
	_, a, b := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	script, want := counterScript(40, "0.05")
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	restarted := make(chan struct{})
	var once sync.Once
	res, err := runCLI(ctx, a.sock(), shSpec(ws, script), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout && reachedLine(r, p, "5") {
				once.Do(func() {
					go func() {
						b.stop()
						b.start()
						close(restarted)
					}()
				})
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	<-restarted
	if res.out() != want {
		t.Fatalf("output after target restart:\n%q\nwant\n%q", res.out(), want)
	}
	if res.exit.Code != 0 || res.exit.Node != "b" {
		t.Fatalf("exit %+v", res.exit)
	}
	waitFor(t, 5*time.Second, "slot released after restart", func() bool { return b.exec.heldSlots() == 0 })
}

func TestShortNetworkCut(t *testing.T) {
	c, a, _ := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	script, want := counterScript(40, "0.05")
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	var once sync.Once
	res, err := runCLI(ctx, a.sock(), shSpec(ws, script), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout && reachedLine(r, p, "5") {
				once.Do(func() { c.net.Cut("a") })
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.out() != want || res.exit.Code != 0 {
		t.Fatalf("out %q exit %+v", res.out(), res.exit)
	}
}

func TestShortStallSurvives(t *testing.T) {
	c, a, _ := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	script, want := counterScript(30, "0.05")
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	var once sync.Once
	res, err := runCLI(ctx, a.sock(), shSpec(ws, script), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout && reachedLine(r, p, "3") {
				once.Do(func() {
					// Shorter than keepalive and lease: nothing may break.
					c.net.SetDown("a", true)
					time.AfterFunc(150*time.Millisecond, func() { c.net.SetDown("a", false) })
				})
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.out() != want || res.exit.Code != 0 {
		t.Fatalf("out %q exit %+v", res.out(), res.exit)
	}
}

func TestLongOutageLosesJob(t *testing.T) {
	c, a, b := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	var once sync.Once
	start := time.Now()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo started; sleep 60"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout {
				once.Do(func() { c.net.SetDown("b", true) })
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit.Code != proto.ExitInfra || !strings.Contains(res.exit.Reason, "lost contact with b") {
		t.Fatalf("exit %+v", res.exit)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("took %s to give up", el)
	}
	// The target's lease killed the job: no orphan.
	waitFor(t, 5*time.Second, "job gone on b", func() bool { return len(b.exec.Active()) == 0 })
	j := storedJob(t, a, res.jobID)
	if j.State != proto.JobInfraErr {
		t.Fatalf("state %s", j.State)
	}
}

func TestOriginDaemonRestartReattach(t *testing.T) {
	_, a, b := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	gate := filepath.Join(t.TempDir(), "gate")
	script, want := gatedCounterScript(50, 5, gate)
	ctx, cancel := testContext(40 * time.Second)
	defer cancel()
	res := &cliResult{}
	var once sync.Once
	err := runCLIInto(ctx, a.sock(), shSpec(ws, script), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout && reachedLine(r, p, "5") {
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
	os.WriteFile(gate, nil, 0o600)
	// The CLI reconnects and resumes at its offset.
	err = runCLIInto(ctx, a.sock(), proto.JobSpec{}, cliOpts{reattach: &proto.Reattach{JobID: res.jobID, Offset: res.offset}}, res)
	if err != nil {
		t.Fatal(err)
	}
	if res.out() != want {
		t.Fatalf("output across origin restart:\n%q\nwant\n%q", res.out(), want)
	}
	if res.exit == nil || res.exit.Code != 0 {
		t.Fatalf("exit %+v", res.exit)
	}
	j := storedJob(t, a, res.jobID)
	if j.State != proto.JobSucceeded || j.Node != "b" {
		t.Fatalf("job %+v", j)
	}
	waitFor(t, 5*time.Second, "b clean", func() bool { return b.exec.heldSlots() == 0 })
}

func TestOriginRestartWithoutCLICancels(t *testing.T) {
	_, a, b := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(40 * time.Second)
	defer cancel()
	var once sync.Once
	res, _ := runCLI(ctx, a.sock(), shSpec(ws, "echo go; sleep 60"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout {
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
	// Nobody reattaches: after CLIReattachWait the job is canceled on b.
	waitFor(t, 15*time.Second, "job canceled", func() bool {
		j, err := a.store.GetJob(context.Background(), res.jobID)
		return err == nil && j.State == proto.JobCanceled
	})
	waitFor(t, 5*time.Second, "b clean", func() bool { return len(b.exec.Active()) == 0 && b.exec.heldSlots() == 0 })
}

func TestReattachAfterFinish(t *testing.T) {
	_, a, _ := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo hello world; exit 4"), cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := runCLI(ctx, a.sock(), proto.JobSpec{}, cliOpts{reattach: &proto.Reattach{JobID: res.jobID, Offset: 6}})
	if err != nil {
		t.Fatal(err)
	}
	if again.out() != "world\n" || again.exit == nil || again.exit.Code != 4 {
		t.Fatalf("out %q exit %+v", again.out(), again.exit)
	}
	// Logs via the API.
	cl, err := client.DialSocket(ctx, a.sock())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	var sb strings.Builder
	if err := cl.Log(ctx, res.jobID, false, &sb); err != nil || sb.String() != "hello world\n" {
		t.Fatalf("log %q %v", sb.String(), err)
	}
	sb.Reset()
	if err := cl.Log(ctx, res.jobID, true, &sb); err != nil || sb.String() != "hello world\n" {
		t.Fatalf("follow log %q %v", sb.String(), err)
	}
}

func TestLocalCLIDisconnectKillsTree(t *testing.T) {
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
	// The "CLI" starts the child in its own group, reports it, then dies.
	cmd := exec.Command("sh", "-c", "sleep 60 & sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	rs.SendJSON(proto.FrameStarted, proto.Started{JobID: dc.JobID, PID: cmd.Process.Pid, PGID: cmd.Process.Pid})
	time.Sleep(200 * time.Millisecond)
	rs.Close()
	select {
	case <-waitErr:
	case <-time.After(waitCeiling):
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatal("child survived its CLI")
	}
	gone := false
	for i := 0; i < 50 && !gone; i++ {
		gone = syscall.Kill(-cmd.Process.Pid, 0) != nil
		time.Sleep(50 * time.Millisecond)
	}
	if !gone {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatal("process group still alive")
	}
	waitFor(t, 3*time.Second, "job canceled", func() bool {
		j, err := a.store.GetJob(context.Background(), dc.JobID)
		return err == nil && j.State == proto.JobCanceled
	})
}

func TestTwoOriginsRaceForOneTarget(t *testing.T) {
	c := newCluster(t, "a", "b", "c")
	a, b, cc := c.nodes["a"], c.nodes["b"], c.nodes["c"]
	// b can fit exactly one 1 GiB job: 9 GiB total − 8 GiB reserve.
	b.prober.info.MemTotal = 9 << 30
	b.prober.m.MemAvailable = 9 << 30
	a.place = preferRemote("b")
	cc.place = preferRemote("b")
	b.start()
	a.start()
	cc.start()
	c.waitOnline("a", "b")
	c.waitOnline("c", "b")
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make([]*cliResult, 2)
	for i, n := range []*testNode{a, cc} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := runCLI(ctx, n.sock(), shSpec(ws, "sleep 0.5; echo done"), cliOpts{})
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}()
	}
	// b must never hold more than one allocation.
	stop := make(chan struct{})
	maxAllocs := 0
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if d := b.daemon(); d != nil {
				if n := len(d.target.status().Allocs); n > maxAllocs {
					maxAllocs = n
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	wg.Wait()
	close(stop)
	if t.Failed() {
		return
	}
	if maxAllocs > 1 {
		t.Fatalf("b was overbooked: %d allocations at once", maxAllocs)
	}
	nodes := map[string]int{}
	for _, r := range results {
		if r.exit == nil || r.exit.Code != 0 || r.out() != "done\n" {
			t.Fatalf("exit %+v out %q", r.exit, r.out())
		}
		nodes[r.exit.Node]++
	}
	if nodes["b"] != 1 {
		t.Fatalf("placements %v: exactly one job should have run on b", nodes)
	}
}

func TestIncompatiblePeer(t *testing.T) {
	c := newCluster(t, "a", "b")
	a := c.nodes["a"]
	// b speaks protocol 2.0.
	c.net.Listen("b", func(conn netConn) {
		s, err := transport.Server(conn, testSessionConfig(), "mem")
		if err != nil {
			return
		}
		for {
			st, err := s.Accept()
			if err != nil {
				return
			}
			pc := newPeerConn(st)
			var req rawPeerRequest
			if pc.readJSON(&req) == nil {
				pc.respond(proto.Hello{Node: proto.NodeInfo{Name: "b", Version: "2.0.0"}, Protocol: "2.0"})
			}
			st.Close()
		}
	})
	a.start()
	waitFor(t, 5*time.Second, "b offline with upgrade hint", func() bool {
		ns, ok := a.scheduler().node("b")
		return ok && ns.Status == proto.NodeOffline && strings.Contains(ns.Reason, "upgrade")
	})
	st := a.daemon().State()
	for _, n := range st.Nodes {
		if n.Info.Name == "b" && n.Status != proto.NodeOffline {
			t.Fatalf("b %s", n.Status)
		}
	}
}

func TestIncompatibleOriginRefused(t *testing.T) {
	c := newCluster(t, "b")
	b := c.nodes["b"]
	b.start()
	s, err := c.net.Dial(context.Background(), "x", "b")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = peerOneShot(context.Background(), s, proto.PeerHello, "", proto.Hello{Node: proto.NodeInfo{Name: "x"}, Protocol: "9.1"}, nil)
	if peerCode(err) != codeVersion || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("err %v", err)
	}
	// Without hello nothing else is served.
	err = peerOneShot(context.Background(), s, proto.PeerAdmit, "j1", admitBody{}, nil)
	if peerCode(err) != codeHelloFirst {
		t.Fatalf("err %v", err)
	}
}

func TestReservationExpires(t *testing.T) {
	c := newCluster(t, "a", "b")
	b := c.nodes["b"]
	b.timing.ReservationTTL = 600 * time.Millisecond
	b.start()
	s, err := c.net.Dial(context.Background(), "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := peerOneShot(ctx, s, proto.PeerHello, "", proto.Hello{Node: proto.NodeInfo{Name: "a"}, Protocol: protocolString()}, nil); err != nil {
		t.Fatal(err)
	}
	if err := peerOneShot(ctx, s, proto.PeerAdmit, "job1", admitBody{Alloc: proto.Resources{CPUs: 1, Mem: 1 << 30}, Runtime: proto.RuntimeNative}, nil); err != nil {
		t.Fatal(err)
	}
	var slot proto.SlotRef
	if err := peerOneShot(ctx, s, proto.PeerSlotAcquire, "job1", slotAcquireBody{RepoID: "r", Runtime: proto.RuntimeNative}, &slot); err != nil {
		t.Fatal(err)
	}
	if b.exec.heldSlots() != 1 {
		t.Fatal("slot not held")
	}
	// No job.start follows: the reservation and the slot go away.
	waitFor(t, 3*time.Second, "reservation expired", func() bool {
		return len(b.daemon().target.status().Allocs) == 0 && b.exec.heldSlots() == 0
	})
	err = peerOneShot(ctx, s, proto.PeerSlotCommit, "job1", nil, nil)
	if peerCode(err) != codeUnknownJob {
		t.Fatalf("commit after expiry: %v", err)
	}
}

func TestAdmissionIsAtomic(t *testing.T) {
	c := newCluster(t, "b")
	b := c.nodes["b"]
	b.prober.info.MemTotal = 12 << 30 // 4 GiB schedulable
	b.start()
	tg := b.daemon().target
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tg.admit(fmt.Sprintf("j%d", i), "o", admitBody{Alloc: proto.Resources{CPUs: 1, Mem: 1 << 30}, Runtime: proto.RuntimeNative}, nil) == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 4 {
		t.Fatalf("%d admitted, want 4", ok)
	}
	err := tg.admit("big", "o", admitBody{Alloc: proto.Resources{CPUs: 1, Mem: 1 << 30}, Runtime: proto.RuntimeNative}, nil)
	var ae *admitError
	if !errors.As(err, &ae) || ae.code != "busy" || !strings.Contains(ae.msg, "needs 1.0 GiB") {
		t.Fatalf("err %v", err)
	}
}

func TestGracefulShutdownIsFast(t *testing.T) {
	_, a, b := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	cl, err := client.DialSocket(ctx, a.sock())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	evs, err := cl.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range evs {
		}
	}()
	go runCLI(ctx, a.sock(), shSpec(ws, "sleep 30"), cliOpts{})
	waitFor(t, 5*time.Second, "job running on b", func() bool { return len(b.exec.Active()) == 1 })
	start := time.Now()
	a.stop() // fails the test itself if > 5 s
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("shutdown took %s", el)
	}
	// The remote job keeps running (detached supervisor): stays active in
	// the store for reattach.
	if len(b.exec.Active()) != 1 {
		t.Fatal("remote job died with the origin daemon")
	}
	jobs, _ := a.store.ActiveJobs(context.Background())
	if len(jobs) != 1 {
		t.Fatalf("active jobs persisted: %d", len(jobs))
	}
}

func TestQueueNoticesThenPlacement(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	var mu sync.Mutex
	open := false
	a.place = func(job *proto.Job, nodes map[string]proto.NodeState, rej map[string]bool) (scheduler.Placement, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if !open {
			return scheduler.Placement{}, "waiting: needs 6 GiB, a has 3.1 GiB free", false
		}
		return scheduler.Placement{Reason: "fits now"}, "", true
	}
	a.start()
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo ok"), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameNotice {
				mu.Lock()
				open = true
				mu.Unlock()
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit.Code != 0 || len(res.notices) == 0 || !strings.Contains(res.notices[0].Message, "needs 6 GiB") {
		t.Fatalf("exit %+v notices %+v", res.exit, res.notices)
	}
}
