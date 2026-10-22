package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
	"github.com/hafencity-dev/kran/internal/transport"
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

	var slot proto.SlotRef
	sctx, cancel := rpc(d.t.SlotWait + 10*time.Second)
	err = peerOneShot(sctx, sess, proto.PeerSlotAcquire, e.id,
		slotAcquireBody{RepoID: repoID, Hint: spec.Workspace.Root, Runtime: p.Runtime}, &slot)
	cancel()
	if err != nil {
		abandon()
		return fail(fmt.Errorf("workspace slot: %w", err))
	}

	plan, err := d.remoteDiff(pctx, sess, e.id, m)
	if err != nil {
		abandon()
		return fail(fmt.Errorf("sync: %w", err))
	}
	sent, err := d.uploadBlobs(pctx, sess, e, node, m, plan)
	if err != nil {
		abandon()
		return fail(fmt.Errorf("sync: %w", err))
	}
	e.phase(proto.Phase{State: proto.JobSyncing, Message: "updating the workspace on " + node, Progress: 1})
	cctx, cancel := rpc(d.t.PrestartTimeout)
	err = peerOneShot(cctx, sess, proto.PeerSlotCommit, e.id, nil, nil)
	cancel()
	if err != nil {
		abandon()
		return fail(fmt.Errorf("sync: %w", err))
	}
	syncSecs := d.now().Sub(syncStart).Seconds()
	e.update(func(j *proto.Job) {
		j.Usage.SyncBytes = sent
		j.Usage.SyncSeconds = syncSecs
	})

	spec.ID = e.id
	stctx, cancel := rpc(d.t.PrestartTimeout)
	pc, err := peerCall(stctx, sess, proto.PeerJobStart, e.id, jobStartBody{
		Spec: spec, Slot: slot, Runtime: p.Runtime, Alloc: p.Alloc, Origin: d.self,
	}, nil)
	cancel()
	if err != nil {
		abandon()
		return fail(fmt.Errorf("start: %w", err))
	}
	if c, _, _ := e.canceled(); c {
		// Canceled while starting: stop it right away.
		e.outbox.put(proto.FrameSignal, mustJSON(proto.Signal{Name: "TERM"}))
	}
	e.setState(proto.JobSetup, "")
	return d.followRemote(ctx, e, p, pc)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// remoteDiff sends the manifest (zstd JSON) and returns the target's plan.
func (d *Daemon) remoteDiff(ctx context.Context, sess transport.Session, id string, m *proto.Manifest) (proto.SyncPlan, error) {
	var plan proto.SyncPlan
	dctx, cancel := context.WithTimeout(ctx, d.t.PrestartTimeout)
	defer cancel()
	pc, err := peerCall(dctx, sess, proto.PeerSlotDiff, id, nil, nil)
	if err != nil {
		return plan, err
	}
	defer pc.Close()
	stop := context.AfterFunc(dctx, func() { pc.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	enc, err := zstd.NewWriter(frameWriter{pc}, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return plan, err
	}
	if err := json.NewEncoder(enc).Encode(m); err != nil {
		enc.Close()
		return plan, fmt.Errorf("send manifest: %w", ctxErr(dctx, err))
	}
	if err := enc.Close(); err != nil {
		return plan, fmt.Errorf("send manifest: %w", ctxErr(dctx, err))
	}
	if err := pc.writeFrame(pfEnd, nil); err != nil {
		return plan, fmt.Errorf("send manifest: %w", ctxErr(dctx, err))
	}
	if err := pc.readResponse(proto.PeerSlotDiff, &plan); err != nil {
		return plan, ctxErr(dctx, err)
	}
	return plan, nil
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
					cctx, c := context.WithTimeout(d.lifetime(), 5*time.Second)
					if s, serr := d.peers.session(node); serr == nil {
						peerOneShot(cctx, s, proto.PeerJobCancel, e.id, nil, nil)
					}
					c()
					return &prestartError{node: node, err: err}
				}
				state := proto.JobInfraErr
				if peerCode(err) == codeUnknownJob {
					state = proto.JobLost
				}
				e.finish(proto.Exit{Code: proto.ExitInfra, State: state, Node: node, Reason: err.Error()})
				return nil
			}
			e.update(func(j *proto.Job) { j.Reason = "" })
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
				if !running {
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
		pc, err := peerCall(cctx, sess, proto.PeerJobAttach, e.id, jobAttachBody{Offset: e.spool.outputBytes()}, nil)
		c2()
		cancel()
		if err == nil {
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

	// Target → origin.
	go func() {
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
			for _, f := range e.outbox.take() {
				if !send(f.t, f.p) {
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
