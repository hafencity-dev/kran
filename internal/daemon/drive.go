package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
)

type driveMode int

const (
	driveFresh driveMode = iota
	driveResubmit
	driveRestoreLocal
	driveRestoreRemote
)

func (m driveMode) String() string {
	return [...]string{"fresh", "queued again", "waiting for its kran run", "reattaching to the target"}[m]
}

var errStopping = errors.New("daemon stopping")

// submit registers a new job from a run session.
func (d *Daemon) submit(spec proto.JobSpec) (*jobEntry, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("empty command")
	}
	if spec.ID == "" || !validID(spec.ID) || d.jobs.get(spec.ID) != nil {
		spec.ID = d.ids.next(d.now())
	}
	if spec.SubmittedAt.IsZero() {
		spec.SubmittedAt = d.now().UTC()
	}
	if spec.Priority == "" {
		spec.Priority = proto.PriorityNormal
		if spec.TTY != nil {
			spec.Priority = proto.PriorityHigh
		}
	}
	if spec.Req.OS == "" {
		spec.Req.OS = proto.OSSame
	}
	if spec.Req.Prefer == "" {
		spec.Req.Prefer = proto.PreferAuto
	}
	fp := d.deps.Est.Fingerprint(&spec)
	est := d.deps.Est.Estimate(&spec, fp, d.nodeInfo().CPUs)
	job := proto.Job{
		Spec:        spec,
		State:       proto.JobQueued,
		Estimate:    est,
		Fingerprint: fp,
		QueuedAt:    d.now().UTC(),
		Origin:      d.self,
	}
	sp, err := openSpool(filepath.Join(d.deps.Dirs.spoolDir(), spec.ID))
	if err != nil {
		return nil, fmt.Errorf("create output spool: %w", err)
	}
	e := d.jobs.newEntry(job, sp)
	d.jobs.add(e)
	e.mu.Lock()
	e.persistLocked()
	d.bus.publish(proto.EventJob, cloneJob(e.job))
	e.mu.Unlock()
	return e, nil
}

func (d *Daemon) startDriver(e *jobEntry, mode driveMode) {
	d.goLoop(func(ctx context.Context) { d.drive(ctx, e, mode) })
}

// drive owns a job from submission to its terminal state.
func (d *Daemon) drive(ctx context.Context, e *jobEntry, mode driveMode) {
	defer func() {
		if r := recover(); r != nil {
			d.logf("job %s: driver panic: %v", e.id, r)
			e.finish(proto.Exit{Code: proto.ExitInfra, State: proto.JobInfraErr, Reason: fmt.Sprintf("kran failed: %v", r)})
		}
	}()
	switch mode {
	case driveRestoreLocal:
		d.restoreLocal(ctx, e)
		return
	case driveRestoreRemote:
		// Heartbeats must resume at once (the target's lease is short);
		// the CLI gets CLIReattachWait to come back.
		go d.cancelWithoutCLI(ctx, e)
		job := e.snapshot()
		p := scheduler.Placement{JobID: e.id, Node: job.Node, Runtime: job.Runtime, Alloc: job.Alloc}
		if err := d.followRemote(ctx, e, p, nil); err != nil {
			// Before running there is nothing to fall back to here: the
			// CLI that could run it locally is gone or reattaching.
			e.finish(proto.Exit{Code: proto.ExitInfra, State: proto.JobInfraErr, Reason: err.Error()})
		}
		return
	case driveResubmit:
		e.setState(proto.JobQueued, "queued again after a daemon restart")
		go d.cancelWithoutCLI(ctx, e)
	}

	j := e.snapshot()
	ticket := d.deps.Sched.Submit(&j)
	d.publishQueue()
	var lastFail *prestartError
	for {
		p, err := d.awaitPlacement(ctx, e, ticket)
		if err != nil {
			if errors.Is(err, errStopping) {
				return
			}
			d.deps.Sched.Cancel(e.id)
			d.finishCanceledBeforeStart(e)
			return
		}
		e.markPlaced(p)
		d.publishQueue()
		spec := e.snapshot().Spec
		if lastFail != nil {
			where := "running it here"
			if !p.Local {
				where = "trying " + p.Node
			}
			e.notice("fallback", fmt.Sprintf("%s failed before the job started (%v); %s", lastFail.node, lastFail.err, where))
		}
		if p.Local {
			if spec.Req.Prefer == proto.PreferRemote || (spec.NoFallback && (p.Fallback || lastFail != nil)) {
				reason := "no other node could take the job"
				if lastFail != nil {
					reason = fmt.Sprintf("%s failed before the job started: %v", lastFail.node, lastFail.err)
				}
				if p.Reason != "" && lastFail == nil {
					reason += " (" + p.Reason + ")"
				}
				e.finish(proto.Exit{Code: proto.ExitInfra, State: proto.JobInfraErr, Reason: reason + "; not running it locally because of --remote/--no-fallback"})
				return
			}
			d.runLocal(ctx, e, p)
			return
		}
		err = d.runRemote(ctx, e, p)
		if err == nil {
			return
		}
		var pe *prestartError
		if !errors.As(err, &pe) {
			d.logf("job %s: %v", e.id, err)
			return
		}
		if c, _, _ := e.canceled(); c {
			d.deps.Sched.Release(e.id, nil)
			e.unplace()
			d.finishCanceledBeforeStart(e)
			return
		}
		if ctx.Err() != nil {
			return
		}
		d.logf("job %s: %s failed before start: %v", e.id, p.Node, pe.err)
		d.peers.recordFailure(p.Node, pe.err)
		e.unplace()
		e.setState(proto.JobQueued, fmt.Sprintf("%s failed before start: %v", p.Node, pe.err))
		lastFail = pe
		ticket = d.deps.Sched.Reject(e.id, p.Node, pe.err)
	}
}

// waitForCLI waits up to CLIReattachWait for a run session to reattach to
// a restored job. It returns true at once for fresh jobs.
func (d *Daemon) waitForCLI(ctx context.Context, e *jobEntry) bool {
	deadline := time.NewTimer(d.t.CLIReattachWait)
	defer deadline.Stop()
	for {
		n, ch := e.attachedCount()
		if n > 0 || !e.restored {
			return true
		}
		select {
		case <-ch:
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		case <-e.done:
			return false
		}
	}
}

// cancelWithoutCLI cancels a restored job whose CLI does not come back:
// nobody would see its result.
func (d *Daemon) cancelWithoutCLI(ctx context.Context, e *jobEntry) {
	if !d.waitForCLI(ctx, e) && ctx.Err() == nil {
		e.requestCancel("its kran run did not reconnect after the daemon restarted", "TERM")
	}
}

func (d *Daemon) awaitPlacement(ctx context.Context, e *jobEntry, t scheduler.Ticket) (scheduler.Placement, error) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type res struct {
		p   scheduler.Placement
		err error
	}
	ch := make(chan res, 1)
	go func() {
		p, err := t.Wait(wctx)
		ch <- res{p, err}
	}()
	lastReason := ""
	for {
		select {
		case r := <-ch:
			if r.err != nil {
				if ctx.Err() != nil {
					return r.p, errStopping
				}
				if c, _, _ := e.canceled(); c {
					return r.p, context.Canceled
				}
				return r.p, r.err
			}
			// A cancel that raced the placement wins.
			if c, _, _ := e.canceled(); c {
				d.deps.Sched.Release(e.id, nil)
				return r.p, context.Canceled
			}
			return r.p, nil
		case reason, ok := <-t.Updates():
			if !ok {
				continue
			}
			if reason != "" && reason != lastReason {
				lastReason = reason
				e.update(func(j *proto.Job) { j.Reason = reason })
				e.notice("queued", reason)
				d.publishQueue()
			}
		case <-e.cancelCh:
			cancel()
			r := <-ch
			if r.err == nil {
				// Placed just before the cancel landed.
				d.deps.Sched.Release(e.id, nil)
			}
			return r.p, context.Canceled
		case <-ctx.Done():
			cancel()
			<-ch
			return scheduler.Placement{}, errStopping
		}
	}
}

func (d *Daemon) finishCanceledBeforeStart(e *jobEntry) {
	_, reason, sig := e.canceled()
	if reason == "" {
		reason = "canceled before it started"
	}
	code, name := 128+15, "TERM"
	if sig == "INT" {
		code, name = 128+2, "INT"
	}
	e.finish(proto.Exit{Code: code, Signal: name, State: proto.JobCanceled, Reason: reason})
}

// --- local placement ---

func (d *Daemon) localEnvelope(e *jobEntry, p scheduler.Placement) *proto.LocalEnvelope {
	env := &proto.LocalEnvelope{
		CPUs: p.Alloc.CPUs,
		Env: map[string]string{
			"KRAN_JOB_ID": e.id,
			"KRAN_NODE":   d.self,
		},
	}
	// SPEC §6.8: cap = 2 × estimate, at least 2 GiB, at most what is free.
	limit := 2 * p.Alloc.Mem
	if limit < 2<<30 {
		limit = 2 << 30
	}
	if avail := d.deps.Prober.Sample().MemAvailable; avail > 0 && limit > avail {
		limit = avail
	}
	env.MemMax = limit
	if s := d.deps.Slice; s != nil && s.Available() {
		env.Slice = s.Name()
		s.Refresh()
	}
	return env
}

func (d *Daemon) runLocal(ctx context.Context, e *jobEntry, p scheduler.Placement) {
	d.target.addLocal(e.id, p.Alloc, true)
	job := e.snapshot()
	env := d.localEnvelope(e, p)
	e.sendFrame(proto.FrameDecision, proto.Decision{
		JobID: e.id, Mode: "local", Node: d.self, Runtime: proto.RuntimeNative,
		Reason: p.Reason, Envelope: env, Estimate: job.Estimate,
	})
	d.publishSelf()
	d.waitLocal(ctx, e)
}

// waitLocal follows a job the CLI runs itself: started → exit. A CLI that
// disappears without an exit gets its process tree killed.
func (d *Daemon) waitLocal(ctx context.Context, e *jobEntry) {
	killed := false
	var killDeadline <-chan time.Time
	kill := func() {
		if killed {
			return
		}
		killed = true
		e.mu.Lock()
		pid, pgid, scope := e.pid, e.pgid, e.scope
		e.mu.Unlock()
		if pid > 0 || pgid > 0 || scope != "" {
			go d.deps.KillLocal(pid, pgid, scope)
		}
		killDeadline = time.After(d.t.KillGrace + 5*time.Second)
	}
	cancelCh := e.cancelCh
	for {
		select {
		case ev := <-e.localCh:
			if ev.started != nil {
				e.mu.Lock()
				e.pid, e.pgid, e.scope = ev.started.PID, ev.started.PGID, ev.started.Scope
				e.started = true
				e.mu.Unlock()
				e.setState(proto.JobRunning, "")
				if c, _, _ := e.canceled(); c {
					kill()
				}
			}
			if ev.exit != nil {
				x := *ev.exit
				x.Node = d.self
				if c, reason, _ := e.canceled(); c {
					x.State = proto.JobCanceled
					x.Reason = reason
				}
				e.finish(x)
				return
			}
		case <-cancelCh:
			cancelCh = nil
			kill()
			if n, _ := e.attachedCount(); n == 0 {
				// Nobody will report an exit.
				d.finishCanceledLocal(e)
				return
			}
		case <-killDeadline:
			d.finishCanceledLocal(e)
			return
		case <-ctx.Done():
			return
		}
	}
}

func (d *Daemon) finishCanceledLocal(e *jobEntry) {
	_, reason, _ := e.canceled()
	e.finish(proto.Exit{Code: 128 + 15, Signal: "TERM", State: proto.JobCanceled, Reason: reason})
}

// restoreLocal waits for the CLI of a local job to reattach after a daemon
// restart; without it the result is unknown.
func (d *Daemon) restoreLocal(ctx context.Context, e *jobEntry) {
	job := e.snapshot()
	d.target.addLocal(e.id, job.Alloc, false)
	if !d.waitForCLI(ctx, e) {
		if ctx.Err() != nil {
			return
		}
		e.finish(proto.Exit{Code: proto.ExitInfra, State: proto.JobLost,
			Reason: "the daemon restarted while the job ran locally and its kran run did not reconnect; the result is unknown"})
		return
	}
	d.waitLocal(ctx, e)
}
