package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
)

// prestartError: the remote placement failed before the command started;
// the job may be placed again or fall back to local (SPEC §6.1).
type prestartError struct {
	node string
	err  error
}

func (e *prestartError) Error() string { return fmt.Sprintf("%s: %v", e.node, e.err) }
func (e *prestartError) Unwrap() error { return e.err }

// runRemote runs phases a–d of SPEC §6.1 on the placed node and then
// follows the job. It returns nil once the job is terminal (or the daemon
// stops), or a *prestartError.
func (d *Daemon) runRemote(ctx context.Context, e *jobEntry, p scheduler.Placement) error {
	node := p.Node
	job := e.snapshot()
	spec := job.Spec
	fail := func(err error) error { return &prestartError{node: node, err: err} }

	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	go func() {
		select {
		case <-e.cancelCh:
			pcancel()
		case <-pctx.Done():
		}
	}()
	rpc := func(timeout time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(pctx, timeout)
	}

	sess, err := d.peers.session(node)
	if err != nil {
		return fail(err)
	}
	e.sendFrame(proto.FrameDecision, proto.Decision{
		JobID: e.id, Mode: "remote", Node: node, Runtime: p.Runtime, Reason: p.Reason, Estimate: job.Estimate,
		StdinLimit: e.stdinLimit(),
	})

	actx, cancel := rpc(30 * time.Second)
	err = peerOneShot(actx, sess, proto.PeerAdmit, e.id, admitBody{Origin: d.self, Alloc: p.Alloc, Runtime: p.Runtime,
		Duration: job.Estimate.Duration, RepoID: spec.Workspace.RepoID, Owner: spec.Owner, Argv: spec.Argv}, nil)
	cancel()
	if peerCode(err) == codeJobStarted {
		// Restarted before we persisted the start: it runs; follow it.
		e.mu.Lock()
		e.started = true
		e.mu.Unlock()
		return d.followRemote(ctx, e, p, nil)
	}
	if err != nil {
		return fail(fmt.Errorf("admission: %w", err))
	}
	abandon := func() {
		// Free the reservation and slot right away instead of waiting for
		// the target's TTL.
		cctx, c := context.WithTimeout(d.lifetime(), 5*time.Second)
		defer c()
		peerOneShot(cctx, sess, proto.PeerJobCancel, e.id, nil, nil)
	}

	e.setState(proto.JobSyncing, "syncing to "+node)
	e.phase(proto.Phase{State: proto.JobSyncing, Message: "scanning workspace"})
	if d.deps.Scanner == nil {
		abandon()
		return fail(errors.New("this node cannot scan workspaces"))
	}
	syncStart := d.now()
	m, err := d.deps.Scanner.Scan(pctx, spec.Workspace, spec.SyncInclude, spec.SyncExclude)
	if err != nil {
		abandon()
		return fail(fmt.Errorf("scan workspace: %w", err))
	}
	repoID := m.RepoID
	if repoID == "" {
		repoID = spec.Workspace.RepoID
	}

	// On macOS the first container job prepares the runtime while it
	// acquires a slot, on Linux when it starts: say why it takes a while.
	d.noteFirstUse(e, node, p.Runtime)
	var acq slotAcquireReply
	sctx, cancel := rpc(d.t.SlotWait + 10*time.Second)
	err = peerOneShot(sctx, sess, proto.PeerSlotAcquire, e.id,
		slotAcquireBody{RepoID: repoID, Hint: spec.Workspace.Root, Runtime: p.Runtime}, &acq)
	slot := acq.SlotRef
	cancel()
	if err != nil {
		abandon()
		return fail(fmt.Errorf("workspace slot: %w", err))
	}

	plan, err := d.remoteDiff(pctx, sess, e.id, node, acq.Sync, m)
	if err != nil {
		abandon()
		return fail(fmt.Errorf("sync: %w", err))
	}
	sent, err := d.uploadBlobs(pctx, sess, e, node, m, plan)
	if err != nil {
		abandon()
		return fail(fmt.Errorf("sync: %w", err))
	}
	if !plan.Committed { // a 1.1 target that had every blob already committed
		e.phase(proto.Phase{State: proto.JobSyncing, Message: "updating the workspace on " + node, Progress: 1})
		cctx, cancel := rpc(d.t.PrestartTimeout)
		err = peerOneShot(cctx, sess, proto.PeerSlotCommit, e.id, nil, nil)
		cancel()
		if err != nil {
			abandon()
			return fail(fmt.Errorf("sync: %w", err))
		}
	}
	syncSecs := d.now().Sub(syncStart).Seconds()
	e.update(func(j *proto.Job) {
		j.Usage.SyncBytes = sent
		j.Usage.SyncSeconds = syncSecs
	})

	spec.ID = e.id
	// kran run must still be there when the job starts: one that lost its
	// session before it saw the decision runs the command itself, and one
	// that reconnects reattaches (SPEC §6.1). The session's end cancels
	// the job if it does not come back.
	if err := e.waitAttached(pctx); err != nil {
		abandon()
		return fail(fmt.Errorf("start: %w", err))
	}
	d.noteFirstUse(e, node, p.Runtime)
	stctx, cancel := rpc(d.t.PrestartTimeout)
	var info jobStreamInfo
	pc, err := peerCall(stctx, sess, proto.PeerJobStart, e.id, jobStartBody{
		Spec: spec, Slot: slot, Runtime: p.Runtime, Alloc: p.Alloc, Origin: d.self,
	}, &info)
	if pc != nil {
		pc.stdinWindow = info.StdinWindow
	}
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil // the daemon stops; a restarted one settles it
		}
		if c, _, _ := e.canceled(); c {
			// Canceled while it started: stop it there, whatever it did.
			go abandon()
			return fail(fmt.Errorf("start: %w", err))
		}
		// The target may have started the job although its answer was
		// lost (a timeout under load, a dropped connection): ask it before
		// the job runs anywhere else.
		d.logf("job %s: job.start on %s: %v; asking %s whether the job started", e.id, node, err, node)
		ferr := d.followRemote(ctx, e, p, nil)
		var pe *prestartError
		if errors.As(ferr, &pe) {
			return fail(fmt.Errorf("start: %w", err))
		}
		return ferr
	}
	if c, _, _ := e.canceled(); c {
		// Canceled while starting: stop it right away.
		e.outbox.put(proto.FrameSignal, mustJSON(proto.Signal{Name: "TERM"}))
	}
	e.setState(proto.JobSetup, "")
	return d.followRemote(ctx, e, p, pc)
}

// noteFirstUse reports the setup phase while a node builds its container
// runtime for the first time (proto.Runtime.FirstUse).
func (d *Daemon) noteFirstUse(e *jobEntry, node string, rt proto.RuntimeKind) {
	if rt != proto.RuntimeContainer {
		return
	}
	p := d.peers.get(node)
	if p == nil {
		return
	}
	if r := runtimeInfo(p.nodeState().Info, rt); r != nil && r.FirstUse {
		e.phase(proto.Phase{State: proto.JobSetup, Message: "preparing the Linux runtime on " + node + " (first use, ~30 s)"})
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// followRemote streams the started job until it exits, reattaching after
// connection loss within the lease window. pc may be nil (attach first).
// It returns a *prestartError only if the connection was lost for good
// before the command started running.
func (d *Daemon) followRemote(ctx context.Context, e *jobEntry, p scheduler.Placement, pc *peerConn) error {
	node := p.Node
	e.mu.Lock()
	e.streaming = true
	e.mu.Unlock()
	lastContact := d.now()
	settles := 0
	for {
		if pc == nil {
			var err error
			pc, err = d.reattachRemote(ctx, e, node, lastContact.Add(d.t.LeaseTTL+d.t.LeaseGrace))
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				e.mu.Lock()
				running := e.started
				e.mu.Unlock()
				if !running {
					// Only the target can tell whether the command started
					// (its running phase may be lost on the way): fall back
					// only when it confirms the job never will (SPEC §6.1).
					switch d.settleOnTarget(node, e.id) {
					case settleReleased:
						return &prestartError{node: node, err: err}
					case settleStarted:
						if settles++; settles < 3 {
							lastContact = d.now()
							continue // it started after all: follow it
						}
					}
					if c, _, _ := e.canceled(); c {
						d.finishCanceledBeforeStart(e)
						return nil
					}
					e.finish(proto.Exit{Code: proto.ExitInfra, State: proto.JobInfraErr, Node: node,
						Reason: fmt.Sprintf("%v; the job may have started on %s before contact was lost, so kran did not run it here too (check: kran logs %s)", err, node, e.id)})
					return nil
				}
				state := proto.JobInfraErr
				if peerCode(err) == codeUnknownJob {
					state = proto.JobLost
				}
				e.finish(proto.Exit{Code: proto.ExitInfra, State: state, Node: node, Reason: err.Error()})
				return nil
			}
			e.update(func(j *proto.Job) { j.Reason = j.PlaceReason }) // reconnected
		}
		res := d.pumpJob(ctx, e, node, pc, &lastContact)
		pc.Close()
		pc = nil
		switch {
		case res.exit != nil:
			x := *res.exit
			canceled, reason, _ := e.canceled()
			if canceled {
				x.State = proto.JobCanceled
				x.Reason = reason
			}
			if !canceled && x.BeforeStart && x.State != proto.JobCanceled {
				// The target says nothing of the command ran (setup failed,
				// command not found there, runtime not ready): a local run
				// may still work (SPEC §6.1).
				return &prestartError{node: node, err: errors.New(firstNonEmpty(x.Reason, "the job did not start"))}
			}
			if x.State == proto.JobInfraErr || x.State == proto.JobLost {
				e.mu.Lock()
				running := e.started
				e.mu.Unlock()
				// Fall back only when the target confirms that nothing of
				// the command ran: a running phase can be missed across a
				// reconnect, and running the job twice is worse than 125.
				if !running && x.BeforeStart {
					// Setup failed on the target: the command never ran.
					return &prestartError{node: node, err: errors.New(firstNonEmpty(x.Reason, "setup failed"))}
				}
				x.Code = proto.ExitInfra
			}
			e.finish(x)
			return nil
		case ctx.Err() != nil:
			return nil
		default:
			d.logf("job %s: lost the stream to %s: %v; reattaching", e.id, node, res.err)
			e.update(func(j *proto.Job) { j.Reason = "lost connection to " + node + "; reconnecting" })
			e.notice("warn", fmt.Sprintf("lost connection to %s; reconnecting", node))
		}
	}
}

type settleResult int

const (
	settleUnknown  settleResult = iota // no answer, or one that proves nothing
	settleReleased                     // the target confirms the job never started and now cannot
	settleStarted                      // the job started there (it is being stopped)
)

// settleOnTarget cancels a job on its target and reports what the target
// says about it (job.cancel answers with a jobCancelResult).
func (d *Daemon) settleOnTarget(node, id string) settleResult {
	ctx, cancel := context.WithTimeout(d.lifetime(), d.t.LeaseTTL)
	defer cancel()
	s, err := d.peers.waitSession(ctx, node)
	if err != nil {
		return settleUnknown
	}
	var res jobCancelResult
	if err := peerOneShot(ctx, s, proto.PeerJobCancel, id, nil, &res); err != nil {
		return settleUnknown
	}
	switch {
	case res.Released:
		return settleReleased
	case res.Started:
		return settleStarted
	}
	return settleUnknown // a target without the answer (older kran)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// reattachRemote reopens the job stream at the current output offset,
// retrying until the deadline (after which the target's lease has expired
// and the job is gone).
func (d *Daemon) reattachRemote(ctx context.Context, e *jobEntry, node string, deadline time.Time) (*peerConn, error) {
	lostFor := func() string {
		return (d.t.LeaseTTL + d.t.LeaseGrace).Round(time.Second).String()
	}
	var lastErr error
	for {
		if !d.now().Before(deadline) {
			msg := fmt.Sprintf("lost contact with %s for %s; the job was stopped there (its lease expired); rerun it", node, lostFor())
			if lastErr != nil {
				msg += fmt.Sprintf(" (last error: %v)", lastErr)
			}
			return nil, errors.New(msg)
		}
		actx, cancel := context.WithDeadline(ctx, deadline)
		sess, err := d.peers.waitSession(actx, node)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		cctx, c2 := context.WithTimeout(actx, 10*time.Second)
		var info jobStreamInfo
		pc, err := peerCall(cctx, sess, proto.PeerJobAttach, e.id, jobAttachBody{Offset: e.spool.outputBytes()}, &info)
		c2()
		cancel()
		if err == nil {
			pc.stdinWindow = info.StdinWindow
			d.logf("job %s: reattached to %s at offset %d", e.id, node, e.spool.outputBytes())
			return pc, nil
		}
		if peerCode(err) == codeUnknownJob {
			return nil, &peerError{Code: codeUnknownJob, Msg: fmt.Sprintf("%s no longer knows the job; it probably restarted and lost it: %v", node, err)}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

type pumpResult struct {
	exit *proto.Exit
	err  error
}

// pumpJob moves frames on one job stream until it ends.
func (d *Daemon) pumpJob(ctx context.Context, e *jobEntry, node string, pc *peerConn, lastContact *time.Time) pumpResult {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan pumpResult, 2)
	silent := make(chan struct{})
	readerDone := make(chan struct{})
	// With flow control, input waits for the target's first report on this
	// stream (it says from where kran run must send).
	var inputReady atomic.Bool
	inputReady.Store(!e.targetStream(pc.stdinWindow))

	// Target → origin.
	go func() {
		defer close(readerDone)
		for {
			pc.SetReadDeadline(time.Now().Add(d.t.StreamSilence))
			t, p, err := pc.readFrame()
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					close(silent)
					err = fmt.Errorf("%s sent nothing for %s", node, d.t.StreamSilence)
				}
				results <- pumpResult{err: err}
				return
			}
			now := d.now()
			e.mu.Lock()
			*lastContact = now
			e.mu.Unlock()
			switch t {
			case proto.FrameStdout, proto.FrameStderr:
				if err := e.spool.appendData(t, p); err != nil {
					d.logf("job %s: %v", e.id, err)
				}
			case proto.FramePhase:
				var ph proto.Phase
				if json.Unmarshal(p, &ph) != nil {
					continue
				}
				d.onRemotePhase(e, node, ph)
				e.phase(ph)
			case pfStdinCredit:
				var r stdinReport
				if json.Unmarshal(p, &r) != nil {
					continue
				}
				first := !inputReady.Load()
				e.targetReport(r, first, node)
				if first {
					inputReady.Store(true)
					e.outbox.wake()
				}
			case proto.FrameExit:
				var x proto.Exit
				if err := json.Unmarshal(p, &x); err != nil {
					results <- pumpResult{err: fmt.Errorf("bad exit frame: %w", err)}
					return
				}
				results <- pumpResult{exit: &x}
				return
			}
		}
	}()

	// Origin → target: heartbeats every Heartbeat, queued stdin/resize/
	// signals, and the cancel sequence (TERM, then KILL after KillGrace).
	go func() {
		tk := time.NewTicker(d.t.Heartbeat)
		defer tk.Stop()
		cancelCh := e.cancelCh
		var killAt <-chan time.Time
		send := func(t proto.FrameType, p []byte) bool {
			pc.SetWriteDeadline(time.Now().Add(d.t.StreamSilence))
			if err := pc.writeFrame(t, p); err != nil {
				results <- pumpResult{err: fmt.Errorf("write to %s: %w", node, err)}
				return false
			}
			return true
		}
		if !send(proto.FrameHeartbeat, nil) {
			return
		}
		for {
			// One frame at a time: a signal queued while input is being
			// written goes out next.
			for f, ok := e.outbox.next(inputReady.Load()); ok; f, ok = e.outbox.next(inputReady.Load()) {
				if !send(f.t, f.p) {
					e.outbox.unget(f)
					return
				}
			}
			select {
			case <-pctx.Done():
				return
			case <-tk.C:
				if !send(proto.FrameHeartbeat, nil) {
					return
				}
			case <-e.outbox.notify:
			case <-cancelCh:
				cancelCh = nil
				_, _, sig := e.canceled()
				if sig == "" || sig == "INT" {
					sig = "TERM"
				}
				if !send(proto.FrameSignal, mustJSON(proto.Signal{Name: sig})) {
					return
				}
				killAt = time.After(d.t.KillGrace)
			case <-killAt:
				killAt = nil
				if !send(proto.FrameSignal, mustJSON(proto.Signal{Name: "KILL"})) {
					return
				}
			}
		}
	}()

	var res pumpResult
	select {
	case res = <-results:
	case <-ctx.Done():
		res = pumpResult{err: ctx.Err()}
	}
	// The caller reattaches at the spool's output offset: nothing may be
	// appended from this stream once we return.
	pc.Close()
	<-readerDone
	select {
	case <-silent:
		// Silence on a stream means the session is most likely half-open;
		// drop it so the peer manager redials at once.
		if s, err := d.peers.session(node); err == nil {
			s.Close()
		}
	default:
	}
	return res
}

func (d *Daemon) onRemotePhase(e *jobEntry, node string, ph proto.Phase) {
	switch ph.State {
	case proto.JobRunning:
		e.mu.Lock()
		first := !e.started
		e.started = true
		e.mu.Unlock()
		if first {
			e.setState(proto.JobRunning, "")
			d.peers.recordSuccess(node)
		}
	case proto.JobSetup:
		e.setState(proto.JobSetup, firstNonEmpty(ph.Message, "setup on "+node))
	}
}
