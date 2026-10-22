package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/hafencity-dev/kran/internal/execenv"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

// peerSession is the server-side view of one session: who is on the other
// end (known after hello for inbound sessions).
type peerSession struct {
	s       transport.Session
	mu      sync.Mutex
	remote  string
	inbound bool
}

func (ps *peerSession) name() string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.remote
}

// ServePeerConn serves a connection accepted on the peer socket (a `kran
// serve --stdio` bridge) or handed over by the in-memory network.
func (d *Daemon) ServePeerConn(c net.Conn) {
	if d.stopping.Load() {
		c.Close()
		return
	}
	s, err := transport.Server(c, d.deps.Session, "peer")
	if err != nil {
		d.logf("peer: %v", err)
		return
	}
	untrack := d.trackSession(s)
	defer untrack()
	ps := &peerSession{s: s, inbound: true}
	d.acceptStreams(ps)
}

// servePeerSession accepts the streams a peer opens on our outbound session.
func (d *Daemon) servePeerSession(s transport.Session, remote string) {
	d.acceptStreams(&peerSession{s: s, remote: remote})
}

func (d *Daemon) acceptStreams(ps *peerSession) {
	defer d.target.sessionClosed(ps.s)
	for {
		c, err := ps.s.Accept()
		if err != nil {
			return
		}
		go d.servePeerStream(ps, c)
	}
}

func (d *Daemon) servePeerStream(ps *peerSession, c net.Conn) {
	defer c.Close()
	pc := newPeerConn(c)
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	var req rawPeerRequest
	if err := pc.readJSON(&req); err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	if req.Method != proto.PeerHello && ps.name() == "" {
		pc.fail(codeHelloFirst, "send hello first")
		return
	}
	if req.JobID != "" && !validID(req.JobID) {
		pc.fail(codeBadRequest, "invalid job id")
		return
	}
	t := d.target
	switch req.Method {
	case proto.PeerHello:
		d.handleHello(ps, pc, req)
	case proto.PeerMetricsSubscribe:
		d.handleSubscribe(pc)
	case proto.PeerMeshPeers:
		pc.respond(d.peers.names())
	case proto.PeerAdmit:
		t.handleAdmit(ps, pc, req)
	case proto.PeerSlotAcquire:
		t.handleSlotAcquire(pc, req)
	case proto.PeerSlotDiff:
		t.handleSlotDiff(pc, req)
	case proto.PeerBlobPut:
		t.handleBlobPut(pc, req)
	case proto.PeerSlotCommit:
		t.handleSlotCommit(pc, req)
	case proto.PeerSlotRelease:
		t.handleSlotRelease(pc, req)
	case proto.PeerJobStart:
		t.handleJobStart(ps, pc, req)
	case proto.PeerJobAttach:
		t.handleJobAttach(ps, pc, req)
	case proto.PeerJobHeartbeat, proto.PeerJobSignal, proto.PeerJobResize, proto.PeerJobCancel:
		t.handleJobControl(pc, req)
	default:
		pc.fail(codeBadRequest, "unknown method %q (the other node runs a newer kran; run kran upgrade)", req.Method)
	}
}

func (d *Daemon) handleHello(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	var h proto.Hello
	if err := json.Unmarshal(req.Body, &h); err != nil || h.Node.Name == "" {
		pc.fail(codeBadRequest, "malformed hello")
		return
	}
	if !compatibleProtocol(h.Protocol) {
		pc.fail(codeVersion, "%s runs kran %s (protocol %s), %s runs kran %s (protocol %s); run kran upgrade",
			h.Node.Name, h.Node.Version, h.Protocol, d.self, d.nodeInfo().Version, protocolString())
		return
	}
	ps.mu.Lock()
	first := ps.remote == ""
	if first {
		ps.remote = h.Node.Name
	}
	ps.mu.Unlock()
	if err := pc.respond(d.helloBody()); err != nil {
		return
	}
	if first && ps.inbound {
		d.logf("peer: %s connected", h.Node.Name)
		d.peers.attachInbound(h.Node.Name, ps.s)
	}
}

func (d *Daemon) handleSubscribe(pc *peerConn) {
	if err := pc.respond(nil); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(d.lifetime())
	defer cancel()
	go func() {
		io.Copy(io.Discard, pc.r) // returns when the subscriber goes away
		cancel()
	}()
	tk := time.NewTicker(d.t.MetricsEvery)
	defer tk.Stop()
	for {
		pc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := pc.writeJSON(d.target.status()); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}

// streamCtx is canceled when the origin closes the stream (it sends nothing
// more during the operation) or after timeout.
func (d *Daemon) streamCtx(pc *peerConn, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(d.lifetime(), timeout)
	go func() {
		var b [1]byte
		pc.r.Read(b[:])
		cancel()
	}()
	return ctx, cancel
}

func (t *targetSide) handleAdmit(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	var b admitBody
	if err := json.Unmarshal(req.Body, &b); err != nil || req.JobID == "" {
		pc.fail(codeBadRequest, "malformed admit")
		return
	}
	if err := t.admit(req.JobID, ps.name(), b, ps.s); err != nil {
		var ae *admitError
		if errors.As(err, &ae) {
			pc.fail(ae.code, "%s", ae.msg)
		} else {
			pc.fail(codeInternal, "%v", err)
		}
		return
	}
	t.d.publishSelf()
	pc.respond(nil)
}

// begin marks a prestart operation in flight (keeps the reservation alive).
func (t *targetSide) begin(id string) (*tjob, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j := t.jobs[id]
	if j == nil || j.ended {
		return nil, &peerError{Code: codeUnknownJob, Msg: fmt.Sprintf("%s holds no reservation for job %s (it expired or was released)", t.d.self, id)}
	}
	if j.started {
		return nil, &peerError{Code: codeJobStarted, Msg: fmt.Sprintf("job %s already started on %s", id, t.d.self)}
	}
	j.busy++
	return j, nil
}

func (t *targetSide) end(j *tjob) {
	t.mu.Lock()
	j.busy--
	j.reservedAt = t.d.now()
	t.mu.Unlock()
}

func failPeer(pc *peerConn, err error) {
	var pe *peerError
	if errors.As(err, &pe) {
		pc.fail(pe.Code, "%s", pe.Msg)
		return
	}
	pc.fail(codeInternal, "%v", err)
}

func (t *targetSide) handleSlotAcquire(pc *peerConn, req rawPeerRequest) {
	var b slotAcquireBody
	if err := json.Unmarshal(req.Body, &b); err != nil || b.RepoID == "" {
		pc.fail(codeBadRequest, "malformed slot.acquire")
		return
	}
	j, err := t.begin(req.JobID)
	if err != nil {
		failPeer(pc, err)
		return
	}
	defer t.end(j)
	t.mu.Lock()
	if j.Slot != nil {
		s := *j.Slot
		t.mu.Unlock()
		pc.respond(s)
		return
	}
	t.mu.Unlock()
	ctx, cancel := t.d.streamCtx(pc, t.d.t.SlotWait)
	defer cancel()
	slot, err := t.d.deps.Exec.AcquireSlot(ctx, b.RepoID, b.Hint, b.Runtime)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			pc.fail(codeNoSlot, "no free workspace slot on %s for %s", t.d.self, t.d.t.SlotWait)
			return
		}
		pc.fail(codeNoSlot, "acquire slot on %s: %v", t.d.self, err)
		return
	}
	t.mu.Lock()
	if t.jobs[j.ID] != j || j.Slot != nil {
		t.mu.Unlock()
		t.d.deps.Exec.ReleaseSlot(slot)
		pc.fail(codeUnknownJob, "reservation of job %s ended while waiting for a slot", j.ID)
		return
	}
	j.Slot = &slot
	t.mu.Unlock()
	if err := pc.respond(slot); err != nil {
		t.mu.Lock()
		t.releaseLocked(j, "origin went away during slot.acquire")
		t.mu.Unlock()
	}
}

func newZstdDecoder() (*zstd.Decoder, error) {
	return zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(1<<30))
}

func (t *targetSide) handleSlotDiff(pc *peerConn, req rawPeerRequest) {
	j, err := t.begin(req.JobID)
	if err != nil {
		failPeer(pc, err)
		return
	}
	defer t.end(j)
	t.mu.Lock()
	slot := j.Slot
	t.mu.Unlock()
	if slot == nil {
		pc.fail(codeBadRequest, "slot.diff before slot.acquire")
		return
	}
	if err := pc.respond(nil); err != nil {
		return
	}
	fr := &frameReader{c: pc}
	dec, err := newZstdDecoder()
	if err != nil {
		pc.fail(codeInternal, "zstd: %v", err)
		return
	}
	defer dec.Close()
	if err := dec.Reset(fr); err != nil {
		pc.fail(codeInternal, "zstd: %v", err)
		return
	}
	var m proto.Manifest
	if err := json.NewDecoder(dec).Decode(&m); err != nil {
		fr.drain()
		pc.fail(codeBadRequest, "read manifest: %v", err)
		return
	}
	if err := fr.drain(); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(t.d.lifetime(), t.d.t.PrestartTimeout)
	defer cancel()
	plan, err := t.d.deps.Exec.Diff(ctx, *slot, &m)
	if err != nil {
		pc.fail(codeInternal, "diff on %s: %v", t.d.self, err)
		return
	}
	t.mu.Lock()
	j.manifest = &m
	t.mu.Unlock()
	pc.respond(plan)
}

func (t *targetSide) handleBlobPut(pc *peerConn, req rawPeerRequest) {
	var j *tjob
	if req.JobID != "" {
		var err error
		if j, err = t.begin(req.JobID); err != nil {
			failPeer(pc, err)
			return
		}
		defer t.end(j)
	}
	if err := pc.respond(nil); err != nil {
		return
	}
	dec, err := newZstdDecoder()
	if err != nil {
		return
	}
	defer dec.Close()
	ctx, cancel := context.WithCancel(t.d.lifetime())
	defer cancel()
	for {
		ty, p, err := pc.readFrame()
		if err != nil {
			return
		}
		switch ty {
		case pfDone:
			return
		case pfBlob:
			var h blobHeader
			if err := json.Unmarshal(p, &h); err != nil {
				return
			}
			res := t.putBlob(ctx, pc, dec, h)
			ack := blobAck{Hash: h.Hash}
			if res != nil {
				ack.Error = res.Error()
			}
			if err := pc.writeJSONFrame(pfAck, ack); err != nil {
				return
			}
			if j != nil {
				t.mu.Lock()
				j.reservedAt = t.d.now()
				t.mu.Unlock()
			}
		default:
			return
		}
	}
}

var errBlobConsumed = errors.New("blob consumer finished")

// putBlob feeds one blob's frames through zstd into execenv.PutBlob.
func (t *targetSide) putBlob(ctx context.Context, pc *peerConn, dec *zstd.Decoder, h blobHeader) error {
	pr, pw := io.Pipe()
	resCh := make(chan error, 1)
	go func() {
		var err error
		if rerr := dec.Reset(pr); rerr != nil {
			err = rerr
		} else {
			err = t.d.deps.Exec.PutBlob(ctx, h.Hash, h.Size, dec)
			if err == nil {
				// Surface a truncated or corrupt stream even if PutBlob
				// stopped reading at the expected size.
				var b [1]byte
				if n, rerr := dec.Read(b[:]); n > 0 || (rerr != nil && rerr != io.EOF) {
					if rerr == nil {
						rerr = errors.New("blob longer than announced")
					}
					err = rerr
				}
			}
		}
		pr.CloseWithError(errBlobConsumed)
		resCh <- err
	}()
	fr := &frameReader{c: pc}
	buf := make([]byte, 64<<10)
	var readErr error
	for {
		n, err := fr.Read(buf)
		if n > 0 {
			pw.Write(buf[:n]) // fails once the consumer is done; keep draining
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	if readErr != nil {
		pw.CloseWithError(readErr)
	} else {
		pw.Close()
	}
	err := <-resCh
	if readErr != nil {
		return readErr
	}
	return err
}

func (t *targetSide) handleSlotCommit(pc *peerConn, req rawPeerRequest) {
	j, err := t.begin(req.JobID)
	if err != nil {
		failPeer(pc, err)
		return
	}
	defer t.end(j)
	t.mu.Lock()
	slot, m := j.Slot, j.manifest
	t.mu.Unlock()
	if slot == nil || m == nil {
		pc.fail(codeBadRequest, "slot.commit before slot.diff")
		return
	}
	ctx, cancel := t.d.streamCtx(pc, t.d.t.PrestartTimeout)
	defer cancel()
	if err := t.d.deps.Exec.Commit(ctx, *slot, m); err != nil {
		pc.fail(codeInternal, "commit on %s: %v", t.d.self, err)
		return
	}
	pc.respond(nil)
}

func (t *targetSide) handleSlotRelease(pc *peerConn, req rawPeerRequest) {
	t.mu.Lock()
	if j := t.jobs[req.JobID]; j != nil && !j.started {
		t.releaseLocked(j, "released by origin")
	}
	t.mu.Unlock()
	t.d.publishSelf()
	pc.respond(nil)
}

func (t *targetSide) handleJobStart(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	var b jobStartBody
	if err := json.Unmarshal(req.Body, &b); err != nil {
		pc.fail(codeBadRequest, "malformed job.start")
		return
	}
	j, err := t.begin(req.JobID)
	if err != nil {
		failPeer(pc, err)
		return
	}
	t.mu.Lock()
	slot := j.Slot
	t.mu.Unlock()
	if slot == nil {
		t.end(j)
		pc.fail(codeBadRequest, "job.start before slot.commit")
		return
	}
	b.Spec.ID = j.ID
	ctx, cancel := context.WithTimeout(t.d.lifetime(), t.d.t.PrestartTimeout)
	proc, err := t.d.deps.Exec.Start(ctx, execenv.StartRequest{
		Spec:    b.Spec,
		Slot:    *slot,
		Runtime: j.Runtime,
		Alloc:   j.Alloc,
		Nice:    t.d.cfg.Accept.Nice,
		Origin:  ps.name(),
		OnPhase: j.setPhase,
	})
	cancel()
	if err != nil {
		t.end(j)
		pc.fail(codeInternal, "start on %s: %v", t.d.self, err)
		return
	}
	j.setProc(proc)
	t.mu.Lock()
	j.busy--
	j.started = true
	t.persistLocked()
	t.startWatchLocked(j)
	t.mu.Unlock()
	t.d.logf("target: started job %s from %s", j.ID, j.Origin)
	t.d.publishSelf()
	if err := pc.respond(nil); err != nil {
		return // the origin reattaches
	}
	t.streamJob(pc, j, proc, 0)
}

func (t *targetSide) handleJobAttach(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	var b jobAttachBody
	json.Unmarshal(req.Body, &b)
	if t.d.deps.Exec == nil {
		pc.fail(codeUnknownJob, "%s runs no jobs", t.d.self)
		return
	}
	t.mu.Lock()
	j := t.jobs[req.JobID]
	if j == nil {
		// Not in memory (e.g. finished long ago or lost on restart without
		// a record): execenv may still know it.
		j = &tjob{ID: req.JobID, Origin: ps.name(), started: true, phaseCh: make(chan struct{}), reservedAt: t.d.now()}
	}
	t.mu.Unlock()
	if !j.started {
		pc.fail(codeUnknownJob, "job %s has not started on %s", req.JobID, t.d.self)
		return
	}
	ctx, cancel := context.WithTimeout(t.d.lifetime(), 30*time.Second)
	proc, err := t.ensureProc(ctx, j)
	cancel()
	if err != nil {
		pc.fail(codeUnknownJob, "%s does not know job %s (it may have restarted and lost it): %v", t.d.self, req.JobID, err)
		return
	}
	t.mu.Lock()
	if t.jobs[j.ID] == nil {
		t.jobs[j.ID] = j
		t.startWatchLocked(j)
	}
	t.mu.Unlock()
	if err := pc.respond(nil); err != nil {
		return
	}
	t.streamJob(pc, j, proc, b.Offset)
}

func (t *targetSide) handleJobControl(pc *peerConn, req rawPeerRequest) {
	t.mu.Lock()
	j := t.jobs[req.JobID]
	t.mu.Unlock()
	if j == nil {
		pc.fail(codeUnknownJob, "%s has no job %s", t.d.self, req.JobID)
		return
	}
	if !j.started {
		if req.Method == proto.PeerJobCancel {
			t.mu.Lock()
			t.releaseLocked(j, "canceled by origin")
			t.mu.Unlock()
			t.d.publishSelf()
		}
		pc.respond(nil)
		return
	}
	ctx, cancel := context.WithTimeout(t.d.lifetime(), 10*time.Second)
	proc, err := t.ensureProc(ctx, j)
	cancel()
	if err != nil {
		pc.fail(codeUnknownJob, "%v", err)
		return
	}
	switch req.Method {
	case proto.PeerJobHeartbeat:
		err = proc.Renew()
	case proto.PeerJobSignal:
		var b jobSignalBody
		json.Unmarshal(req.Body, &b)
		err = proc.Signal(b.Signal)
	case proto.PeerJobResize:
		var tty proto.TTY
		json.Unmarshal(req.Body, &tty)
		err = proc.Resize(tty)
	case proto.PeerJobCancel:
		err = t.cancelProc(j, proc)
	}
	if err != nil {
		pc.fail(codeInternal, "%v", err)
		return
	}
	pc.respond(nil)
}

// cancelProc sends TERM and, if the job is still there after the grace
// period, KILL.
func (t *targetSide) cancelProc(j *tjob, proc execenv.Process) error {
	err := proc.Signal("TERM")
	t.d.goLoop(func(ctx context.Context) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.d.t.KillGrace):
		}
		t.mu.Lock()
		ended := j.ended
		t.mu.Unlock()
		if !ended {
			proc.Signal("KILL")
		}
	})
	return err
}

// streamJob serves the job stream: output from offset, phases, heartbeats
// and the exit go to the origin; heartbeats (lease renewals), stdin,
// resizes and signals come back.
func (t *targetSide) streamJob(pc *peerConn, j *tjob, proc execenv.Process, offset int64) {
	d := t.d
	proc.Renew()
	ctx, cancel := context.WithCancel(d.lifetime())
	defer cancel()
	stop := context.AfterFunc(ctx, func() { pc.Close() })
	defer stop()

	out := proc.Output(offset)
	var outOnce sync.Once
	closeOut := func() { outOnce.Do(func() { out.Close() }) }
	defer closeOut()
	context.AfterFunc(ctx, closeOut)

	// Output and exit.
	go func() {
		defer cancel()
		br := bufio.NewReaderSize(out, 64<<10)
		var buf []byte
		for {
			ft, p, err := proto.ReadFrame(br, buf)
			if err == io.EOF {
				break
			}
			if err != nil {
				return
			}
			buf = p[:0]
			if err := pc.writeFrame(ft, p); err != nil {
				return
			}
		}
		ex, err := proc.Wait(ctx)
		if err != nil {
			return
		}
		if ex.Node == "" {
			ex.Node = d.self
		}
		if err := pc.writeJSONFrame(proto.FrameExit, ex); err != nil {
			return
		}
		// Give the origin a moment to read the exit and close the stream.
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}()

	// Heartbeats and phases.
	go func() {
		tk := time.NewTicker(d.t.Heartbeat)
		defer tk.Stop()
		var last proto.Phase
		for {
			ph, ch := j.currentPhase()
			if ph != last && ph.State != "" {
				if pc.writeJSONFrame(proto.FramePhase, ph) != nil {
					cancel()
					return
				}
				last = ph
			}
			select {
			case <-ctx.Done():
				return
			case <-ch:
			case <-tk.C:
				if pc.writeFrame(proto.FrameHeartbeat, nil) != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Stdin goes through its own goroutine: a job that does not read its
	// stdin must never block heartbeat (lease) processing.
	stdin := newByteQueue(64 << 20)
	defer stdin.close()
	go func() {
		w := proc.Stdin()
		if w == nil {
			return
		}
		for {
			b, eof, ok := stdin.pop(ctx)
			if !ok {
				return
			}
			if eof {
				w.Close()
				return
			}
			if _, err := w.Write(b); err != nil {
				return
			}
		}
	}()

	for {
		pc.SetReadDeadline(time.Now().Add(d.t.StreamSilence))
		ft, p, err := pc.readFrame()
		if err != nil {
			return
		}
		switch ft {
		case proto.FrameHeartbeat:
			if err := proc.Renew(); err != nil {
				d.logf("target: renew lease of %s: %v", j.ID, err)
			}
		case proto.FrameStdin:
			if !stdin.push(p) {
				d.logf("target: job %s: stdin backlog over 64 MiB, dropping input", j.ID)
			}
		case proto.FrameStdinEOF:
			stdin.pushEOF()
		case proto.FrameResize:
			var tty proto.TTY
			if json.Unmarshal(p, &tty) == nil {
				proc.Resize(tty)
			}
		case proto.FrameSignal:
			var s proto.Signal
			if json.Unmarshal(p, &s) == nil {
				if err := proc.Signal(s.Name); err != nil {
					d.logf("target: signal %s to %s: %v", s.Name, j.ID, err)
				}
			}
		}
	}
}

// byteQueue is a bounded FIFO of byte chunks with an EOF marker.
type byteQueue struct {
	mu     sync.Mutex
	chunks [][]byte
	size   int
	max    int
	eof    bool
	closed bool
	notify chan struct{}
}

func newByteQueue(max int) *byteQueue {
	return &byteQueue{max: max, notify: make(chan struct{}, 1)}
}

func (q *byteQueue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *byteQueue) push(b []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.eof || q.closed || q.size+len(b) > q.max {
		return false
	}
	q.chunks = append(q.chunks, append([]byte(nil), b...))
	q.size += len(b)
	q.wake()
	return true
}

func (q *byteQueue) pushEOF() {
	q.mu.Lock()
	q.eof = true
	q.mu.Unlock()
	q.wake()
}

func (q *byteQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wake()
}

// pop returns the next chunk, or eof once drained; ok is false when the
// queue was closed or ctx ended.
func (q *byteQueue) pop(ctx context.Context) (b []byte, eof bool, ok bool) {
	for {
		q.mu.Lock()
		if len(q.chunks) > 0 {
			b = q.chunks[0]
			q.chunks = q.chunks[1:]
			q.size -= len(b)
			q.mu.Unlock()
			return b, false, true
		}
		if q.eof {
			q.mu.Unlock()
			return nil, true, true
		}
		if q.closed {
			q.mu.Unlock()
			return nil, false, false
		}
		q.mu.Unlock()
		select {
		case <-q.notify:
		case <-ctx.Done():
			return nil, false, false
		}
	}
}
