package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// UpgradeProtocol is the HTTP Upgrade token of run sessions (SPEC §8.2).
const UpgradeProtocol = "kran-run/1"

func (d *Daemon) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != UpgradeProtocol {
		writeAPIError(w, http.StatusUpgradeRequired, "run sessions need the header Upgrade: "+UpgradeProtocol, "use kran run")
		return
	}
	if d.stopping.Load() {
		writeAPIError(w, http.StatusServiceUnavailable, "the daemon is shutting down", "retry in a moment")
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "cannot upgrade: "+err.Error(), "")
		return
	}
	untrack := d.trackConn(conn)
	defer untrack()
	defer conn.Close()
	conn.SetDeadline(time.Time{})
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + UpgradeProtocol + "\r\n\r\n")
	if err := brw.Flush(); err != nil {
		return
	}
	pid, _ := strconv.Atoi(r.Header.Get("X-Kran-Client-Pid"))
	d.serveRun(conn, brw.Reader, pid)
}

// runConn serializes writes to the CLI.
type runConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (c *runConn) write(t proto.FrameType, p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return proto.WriteFrame(c.conn, t, p)
}

func (c *runConn) writeJSON(t proto.FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.write(t, b)
}

func (c *runConn) fail(msg, hint string) {
	c.writeJSON(proto.FrameError, proto.APIError{Error: msg, Hint: hint})
}

// serveRun drives one run session: the first frame is submit or reattach;
// afterwards the job's spool streams to the CLI while the CLI's frames
// (stdin, resize, signals, local started/exit) flow to the job.
func (d *Daemon) serveRun(conn net.Conn, br *bufio.Reader, clientPID int) {
	rc := &runConn{conn: conn}
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	t, p, err := proto.ReadFrame(br, nil)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}
	var e *jobEntry
	var offset int64
	reattach := false
	switch t {
	case proto.FrameSubmit:
		var spec proto.JobSpec
		if err := json.Unmarshal(p, &spec); err != nil {
			rc.fail("malformed submit: "+err.Error(), "upgrade kran so the CLI and the daemon match")
			return
		}
		e, err = d.submit(spec)
		if err != nil {
			rc.fail("cannot accept the job: "+err.Error(), "")
			return
		}
		e.attach()
		d.startDriver(e, driveFresh)
	case proto.FrameReattach:
		var ra proto.Reattach
		if err := json.Unmarshal(p, &ra); err != nil || !validID(ra.JobID) {
			rc.fail("malformed reattach", "")
			return
		}
		offset, reattach = ra.Offset, true
		e = d.jobs.get(ra.JobID)
		if e == nil {
			d.serveFinished(rc, ra)
			return
		}
		e.attach()
	default:
		rc.fail("expected submit or reattach, got "+t.String(), "")
		return
	}
	defer e.detach(clientPID)

	ctx, cancel := context.WithCancel(d.lifetime())
	defer cancel()

	// Spool → CLI.
	go func() {
		defer cancel()
		rd, err := e.spool.reader(offset, reattach)
		if err != nil {
			rc.fail("read job output: "+err.Error(), "")
			return
		}
		rd.hideLocalOutput = true
		defer rd.Close()
		if reattach {
			// Input sent on the lost session may be gone: ask for it again
			// (written after the reader's start, so this session sees it).
			e.cliReattached()
		}
		for {
			ft, fp, err := rd.Next(ctx)
			if err != nil {
				if err == io.EOF {
					// Finished without an exit frame (abandoned).
					rc.writeJSON(proto.FrameExit, exitOf(e.snapshot()))
				}
				return
			}
			if err := rc.write(ft, fp); err != nil {
				return
			}
			// A FrameError ends the session (SPEC §8.2): the CLI then runs
			// the command itself, so no exit frame may follow.
			if ft == proto.FrameExit || ft == proto.FrameError {
				return
			}
		}
	}()

	// Heartbeats to the CLI.
	go func() {
		tk := time.NewTicker(d.t.Heartbeat)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				if rc.write(proto.FrameHeartbeat, nil) != nil {
					cancel()
					return
				}
			}
		}
	}()

	// CLI → job.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer cancel()
		var buf []byte
		for {
			ft, fp, err := proto.ReadFrame(br, buf)
			if err != nil {
				return
			}
			buf = fp[:0]
			if !d.onCLIFrame(ctx, e, ft, fp) {
				return
			}
		}
	}()
	<-ctx.Done()
	// The session may end on our side first (a write to a CLI that just
	// quit fails). What that CLI sent before it quit, its exit above all,
	// is still buffered: read it before the session counts as gone, or
	// the job would be recorded as canceled.
	conn.SetReadDeadline(time.Now().Add(time.Second))
	<-readDone
}

// onCLIFrame applies one frame from the CLI; false ends the session.
func (d *Daemon) onCLIFrame(ctx context.Context, e *jobEntry, t proto.FrameType, p []byte) bool {
	switch t {
	case proto.FrameHeartbeat:
	case proto.FrameStdin, proto.FrameStdinEOF:
		// kran run keeps to its credit, so this waits only for a target
		// without flow control.
		for !e.cliInput(t, p) {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(50 * time.Millisecond):
			}
		}
	case proto.FrameStdinResume:
		var r proto.StdinResume
		if json.Unmarshal(p, &r) == nil {
			e.cliResume(r)
		}
	case proto.FrameStdout, proto.FrameStderr:
		e.appendLocalOutput(t, p)
	case proto.FrameResize:
		var tty proto.TTY
		if json.Unmarshal(p, &tty) == nil {
			e.mu.Lock()
			e.tty = &tty
			e.mu.Unlock()
			e.outbox.put(proto.FrameResize, p)
		}
	case proto.FrameSignal:
		var s proto.Signal
		if json.Unmarshal(p, &s) == nil {
			e.forwardSignal(s.Name)
		}
	case proto.FrameStarted:
		var s proto.Started
		if json.Unmarshal(p, &s) == nil && !d.deliverLocal(e, localEvent{started: &s}) {
			return false
		}
	case proto.FrameExit:
		var x proto.Exit
		if json.Unmarshal(p, &x) == nil {
			e.mu.Lock()
			e.exitReported = true
			e.mu.Unlock()
			if !d.deliverLocal(e, localEvent{exit: &x}) {
				return false
			}
		}
	}
	return true
}

// deliverLocal hands a local job's start or exit to its driver. It does not
// depend on the session: a CLI that reported its exit and quit at once must
// still have that exit recorded.
func (d *Daemon) deliverLocal(e *jobEntry, ev localEvent) bool {
	t := time.NewTimer(10 * time.Second)
	defer t.Stop()
	select {
	case e.localCh <- ev:
		return true
	case <-e.done:
	case <-d.lifetime().Done():
	case <-t.C:
		d.logf("job %s: nobody took the CLI's report from kran run", e.id)
	}
	return false
}

// localHistoryMax caps what the history keeps of one local job's output,
// like the spool's in-memory tail for remote jobs (spoolMemMax).
var localHistoryMax int64 = spoolMemMax

// appendLocalOutput records output the CLI of a local job sends (SPEC
// §8.2: stdout/stderr frames client→daemon). Anything else sending them
// (a remote job's CLI, a job that already reported its exit) is ignored.
func (e *jobEntry) appendLocalOutput(t proto.FrameType, p []byte) {
	e.mu.Lock()
	ok := e.job.Local && !e.exitReported && !e.finished && !e.localCut
	room := localHistoryMax - e.localOut
	if ok {
		if int64(len(p)) > room {
			p = p[:max(room, 0)]
			e.localCut = true
		}
		e.localOut += int64(len(p))
	}
	cut := e.localCut
	e.mu.Unlock()
	if !ok {
		return
	}
	if err := e.spool.appendData(t, p); err != nil {
		e.d.logf("job %s: %v", e.id, err)
	}
	if cut {
		e.spool.appendData(t, []byte("\n"+historyTruncatedMark+"\n"))
	}
}

// historyTruncatedMark is what the history shows where output was dropped
// (the CLI uses the same words when it could not keep up).
const historyTruncatedMark = "[kran: output truncated in history]"

// forwardSignal routes a signal from the CLI: to the target once the job
// stream exists, as a cancel while the job has not started, and not at all
// for local jobs (the child shares the CLI's terminal and got it already).
func (e *jobEntry) forwardSignal(name string) {
	e.mu.Lock()
	streaming := e.streaming
	local := e.job.Local && e.placed
	e.mu.Unlock()
	switch {
	case streaming:
		e.outbox.put(proto.FrameSignal, mustJSON(proto.Signal{Name: name}))
	case local:
	default:
		reason := "interrupted before it started"
		if name != "INT" {
			reason = "stopped by " + name + " before it started"
		}
		e.requestCancel(reason, name)
	}
}

// serveFinished answers a reattach to a job that already ended: the output
// from offset (if spooled) and its exit.
func (d *Daemon) serveFinished(rc *runConn, ra proto.Reattach) {
	job, ok := d.jobs.find(ra.JobID)
	if !ok {
		rc.fail("unknown job "+ra.JobID, "list jobs: kran jobs")
		return
	}
	if !job.State.Terminal() {
		rc.fail("job "+ra.JobID+" is not running on this daemon", "")
		return
	}
	path := filepath.Join(d.deps.Dirs.spoolDir(), ra.JobID)
	if _, err := os.Stat(path); err == nil {
		if sp, err := openSpool(path); err == nil {
			sp.finish()
			rd, err := sp.reader(ra.Offset, true)
			if err == nil {
				rd.hideLocalOutput = true
				ctx, cancel := context.WithTimeout(d.lifetime(), time.Minute)
				for {
					ft, fp, err := rd.Next(ctx)
					if err != nil {
						break
					}
					if rc.write(ft, fp) != nil {
						break
					}
					if ft == proto.FrameExit || ft == proto.FrameError {
						cancel()
						rd.Close()
						sp.close()
						return
					}
				}
				cancel()
				rd.Close()
			}
			sp.close()
		}
	}
	rc.writeJSON(proto.FrameExit, exitOf(job))
}

func exitOf(j proto.Job) proto.Exit {
	x := proto.Exit{JobID: j.Spec.ID, State: j.State, Signal: j.Signal, Reason: j.Reason, Usage: j.Usage, Node: j.Node, Fallback: j.Fallback}
	if j.ExitCode != nil {
		x.Code = *j.ExitCode
	} else if !j.State.Terminal() || j.State == proto.JobInfraErr || j.State == proto.JobLost {
		x.Code = proto.ExitInfra
	}
	return x
}
