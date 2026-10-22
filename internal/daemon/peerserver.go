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
	// bound is the name the peer's key was linked as, from the forced
	// command ("" for links made by an older kran).
	bound string
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
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	bound, rc, err := transport.ReadPeerName(c)
	if err != nil {
		// A connection that closes without a byte is `kran serve --stdio`
		// checking that the daemon is up before it bridges: not news.
		if !errors.Is(err, io.EOF) {
			d.logf("peer: %v", err)
		}
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})
	s, err := transport.Server(rc, d.deps.Session, "peer")
	if err != nil {
		d.logf("peer: %v", err)
		return
	}
	untrack := d.trackSession(s)
	defer untrack()
	ps := &peerSession{s: s, inbound: true, bound: bound}
	d.acceptStreams(ps)
}

// servePeerSession accepts the streams a peer opens on our outbound session.
func (d *Daemon) servePeerSession(s transport.Session, remote string) {
	d.acceptStreams(&peerSession{s: s, remote: remote})
}

func (d *Daemon) acceptStreams(ps *peerSession) {
	defer d.target.sessionClosed(ps.s)
	slots := make(chan struct{}, maxPeerStreams)
	for {
		c, err := ps.s.Accept()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close() // the peer holds too many streams open
			continue
		}
		go func() {
			defer func() { <-slots }()
			d.servePeerStream(ps, c)
		}()
	}
}

func (d *Daemon) servePeerStream(ps *peerSession, c net.Conn) {
	defer c.Close()
	pc := newPeerConn(c)
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	var req rawPeerRequest
	if err := pc.readJSONLimit(&req, maxRequestLine); err != nil {
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
	case proto.PeerClock:
		pc.respond(clockBody{Time: time.Now()})
	case proto.PeerAdmit:
		t.handleAdmit(ps, pc, req)
	case proto.PeerSlotAcquire:
		t.handleSlotAcquire(ps, pc, req)
	case proto.PeerSlotDiff:
		t.handleSlotDiff(ps, pc, req)
	case proto.PeerBlobPut:
		t.handleBlobPut(ps, pc, req)
	case proto.PeerSlotCommit:
		t.handleSlotCommit(ps, pc, req)
	case proto.PeerSlotRelease:
		t.handleSlotRelease(ps, pc, req)
	case proto.PeerJobStart:
		t.handleJobStart(ps, pc, req)
	case proto.PeerJobAttach:
		t.handleJobAttach(ps, pc, req)
	case proto.PeerJobHeartbeat, proto.PeerJobSignal, proto.PeerJobResize, proto.PeerJobCancel:
		t.handleJobControl(ps, pc, req)
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
	// Only linked nodes get served, under the name they are linked as (the
	// same name an outbound session to them carries), so jobs stay bound
	// to their origin whichever session it uses.
	name := h.Node.Name
	if ps.inbound && ps.bound != "" { // the key's linked name, whatever hello says
		if name = d.peers.linkedName(ps.bound, ""); name == "" {
			pc.fail(codeNotLinked, "%s is not linked with %s any more; run kran link", ps.bound, d.self)
			return
		}
	} else if ps.inbound {
		if name = d.peers.linkedName(h.Node.Name, h.Node.ID); name == "" {
			pc.fail(codeNotLinked, "%s is not linked with %s; run kran link", h.Node.Name, d.self)
			return
		}
	}
	ps.mu.Lock()
	first := ps.remote == ""
	if first {
		ps.remote = name
	}
	ps.mu.Unlock()
	if err := pc.respond(d.helloBody()); err != nil {
		return
	}
	if first && ps.inbound {
		d.logf("peer: %s connected", name)
		d.peers.attachInbound(name, ps.s)
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
// usesSlot marks operations that work in the job's slot (all but blob.put,
// which writes only the shared blob store): a cancel releases the slot only
// once none of them is in flight.
// Only the origin that admitted the job may act on it.
func (t *targetSide) begin(id, origin string, usesSlot bool) (*tjob, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j := t.jobs[id]
	if j == nil || j.ended || j.Origin != origin {
		return nil, &peerError{Code: codeUnknownJob, Msg: fmt.Sprintf("%s holds no reservation for job %s (it expired or was released)", t.d.self, id)}
	}
	if j.started {
		return nil, &peerError{Code: codeJobStarted, Msg: fmt.Sprintf("job %s already started on %s", id, t.d.self)}
	}
	if j.canceled {
		return nil, &peerError{Code: codeUnknownJob, Msg: fmt.Sprintf("job %s was canceled on %s", id, t.d.self)}
	}
	j.busy++
	if usesSlot {
		j.slotOps++
	}
	return j, nil
}

func (t *targetSide) end(j *tjob, usesSlot bool) {
	t.mu.Lock()
	j.busy--
	if usesSlot {
		j.slotOps--
	}
	j.reservedAt = t.d.now()
	if j.canceled && j.slotOps == 0 && !j.started && t.jobs[j.ID] == j {
		t.releaseLocked(j, "canceled by origin")
	}
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

func (t *targetSide) handleSlotAcquire(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	var b slotAcquireBody
	if err := json.Unmarshal(req.Body, &b); err != nil || b.RepoID == "" {
		pc.fail(codeBadRequest, "malformed slot.acquire")
		return
	}
	j, err := t.begin(req.JobID, ps.name(), true)
	if err != nil {
		failPeer(pc, err)
		return
	}
	defer t.end(j, true)
	t.mu.Lock()
	if j.Slot != nil {
		s := *j.Slot
		t.mu.Unlock()
		pc.respond(slotAcquireReply{SlotRef: s, Sync: syncRevision})
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
	if err := pc.respond(slotAcquireReply{SlotRef: slot, Sync: syncRevision}); err != nil {
		t.mu.Lock()
		t.releaseLocked(j, "origin went away during slot.acquire")
		t.mu.Unlock()
	}
}

// newZstdDecoder bounds what a peer's stream can make us allocate: our
// encoders use windows of at most 8 MiB.
func newZstdDecoder() (*zstd.Decoder, error) {
	return zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(1<<30), zstd.WithDecoderMaxWindow(32<<20))
}

// maxManifest caps a decompressed manifest (a few MB for 30k files).
var maxManifest int64 = 256 << 20

func decodeManifest(r io.Reader, m *proto.Manifest) error {
	lr := &io.LimitedReader{R: r, N: maxManifest + 1}
	err := json.NewDecoder(lr).Decode(m)
	if lr.N <= 0 {
		return fmt.Errorf("manifest larger than %d MiB", maxManifest>>20)
	}
	return err
}

func (t *targetSide) handleSlotDiff(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	var b slotDiffBody
	if len(req.Body) > 0 && string(req.Body) != "null" {
		if err := json.Unmarshal(req.Body, &b); err != nil {
			pc.fail(codeBadRequest, "malformed slot.diff")
			return
		}
	}
	origin := ps.name()
	j, err := t.begin(req.JobID, origin, true)
	if err != nil {
		failPeer(pc, err)
		return
	}
	defer t.end(j, true)
	t.mu.Lock()
	slot := j.Slot
	t.mu.Unlock()
	if slot == nil {
		pc.fail(codeBadRequest, "slot.diff before slot.acquire")
		return
	}
	var base *proto.Manifest
	if b.Format == proto.ManifestSame || b.Format == proto.ManifestDelta {
		if base = t.manifests.get(manifestKey(origin, b.Base)); base == nil {
			pc.fail(codeManifestBase, "%s does not hold manifest %.12s; send all of it", t.d.self, b.Base)
			return
		}
	}
	if err := pc.respond(nil); err != nil {
		return
	}
	m, hash, err := t.readManifest(pc, b, base)
	if err != nil {
		var pe *peerError
		if errors.As(err, &pe) {
			pc.fail(pe.Code, "read manifest: %s", pe.Msg)
		} else {
			pc.fail(codeBadRequest, "read manifest: %v", err)
		}
		return
	}
	if hash != "" {
		t.manifests.put(manifestKey(origin, hash), m)
	}
	ctx, cancel := context.WithTimeout(t.d.lifetime(), t.d.t.PrestartTimeout)
	defer cancel()
	plan, err := t.d.deps.Exec.Diff(ctx, *slot, m)
	if err != nil {
		pc.fail(codeInternal, "diff on %s: %v", t.d.self, err)
		return
	}
	blobs := make(map[string]int64, len(m.Entries))
	for _, e := range m.Entries {
		if e.Kind == proto.EntryFile && e.Hash != "" {
			blobs[e.Hash] = e.Size
		}
	}
	t.mu.Lock()
	j.manifest, j.blobs = m, blobs
	t.mu.Unlock()
	pc.respond(plan)
}

func (t *targetSide) handleBlobPut(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	if req.JobID == "" { // blobs land on disk: only for a live reservation
		pc.fail(codeBadRequest, "blob.put needs a job")
		return
	}
	j, err := t.begin(req.JobID, ps.name(), false)
	if err != nil {
		failPeer(pc, err)
		return
	}
	defer t.end(j, false)
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
			t.mu.Lock()
			size, ok := j.blobs[h.Hash]
			t.mu.Unlock()
			if !ok || h.Size != size { // only the job's files, at their size
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
			t.mu.Lock()
			j.reservedAt = t.d.now()
			t.mu.Unlock()
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

func (t *targetSide) handleSlotCommit(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	j, err := t.begin(req.JobID, ps.name(), true)
	if err != nil {
		failPeer(pc, err)
		return
	}
	defer t.end(j, true)
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

func (t *targetSide) handleSlotRelease(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	t.mu.Lock()
	if j := t.jobs[req.JobID]; j != nil && j.Origin == ps.name() {
		t.cancelLocked(j, "released by origin")
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
	j, err := t.begin(req.JobID, ps.name(), true)
	if err != nil {
		failPeer(pc, err)
		return
	}
	t.mu.Lock()
	slot := j.Slot
	if slot != nil {
		j.starting = make(chan struct{})
	}
	t.mu.Unlock()
	if slot == nil {
		t.end(j, true)
		pc.fail(codeBadRequest, "job.start before slot.commit")
		return
	}
	// Attaches and cancels wait for the outcome (see handleJobCancel).
	startDone := sync.OnceFunc(func() {
		t.mu.Lock()
		close(j.starting)
		j.starting = nil
		t.mu.Unlock()
	})
	defer startDone()
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
		t.end(j, true)
		startDone()
		pc.fail(codeInternal, "start on %s: %v", t.d.self, err)
		return
	}
	j.setProc(proc)
	t.mu.Lock()
	j.busy--
	j.slotOps--
	j.started = true
	j.startedHere = true
	canceled := j.canceled
	t.persistLocked()
	// From here watch owns the slot and the allocation, also for a job the
	// origin canceled while it was starting.
	t.startWatchLocked(j)
	t.mu.Unlock()
	startDone()
	if canceled {
		t.d.logf("target: job %s from %s was canceled while it started; stopping it", j.ID, j.Origin)
		t.cancelProc(j, proc)
		pc.fail(codeUnknownJob, "job %s was canceled on %s", j.ID, t.d.self)
		return
	}
	t.d.logf("target: started job %s from %s", j.ID, j.Origin)
	t.d.publishSelf()
	if err := pc.respond(jobStreamInfo{StdinWindow: t.d.t.StdinQueue}); err != nil {
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
	if j != nil && j.Origin != ps.name() {
		j = nil
		t.mu.Unlock()
		pc.fail(codeUnknownJob, "%s has no job %s", t.d.self, req.JobID)
		return
	}
	if j == nil {
		// Not in memory (e.g. finished long ago or lost on restart without
		// a record): execenv may still know it.
		j = &tjob{ID: req.JobID, Origin: ps.name(), started: true, phaseCh: make(chan struct{}), reservedAt: t.d.now()}
	}
	starting := j.starting
	t.mu.Unlock()
	if starting != nil {
		// An origin that lost the answer to its job.start asks whether the
		// job started: answer that, not "not yet".
		t.waitStart(starting)
	}
	t.mu.Lock()
	started := j.started
	t.mu.Unlock()
	if !started {
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
	if err := pc.respond(jobStreamInfo{StdinWindow: t.d.t.StdinQueue}); err != nil {
		return
	}
	t.streamJob(pc, j, proc, b.Offset)
}

// waitStart waits for an in-flight job.start to finish (bounded like it).
func (t *targetSide) waitStart(starting <-chan struct{}) {
	tm := time.NewTimer(t.d.t.PrestartTimeout + 5*time.Second)
	defer tm.Stop()
	select {
	case <-starting:
	case <-tm.C:
	case <-t.d.lifetime().Done():
	}
}

// handleJobCancel stops a job and tells the origin whether it may run it
// elsewhere: Released only when the job never started here and no
// job.start can start it any more (SPEC §6.1, exactly once).
func (t *targetSide) handleJobCancel(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	t.mu.Lock()
	j := t.jobs[req.JobID]
	if j != nil && j.Origin != ps.name() {
		t.mu.Unlock()
		pc.fail(codeUnknownJob, "%s has no job %s", t.d.self, req.JobID)
		return
	}
	if j == nil {
		t.mu.Unlock()
		// No reservation: job.start fails from now on. Whether one
		// started it earlier (the record lost on a restart) the executor
		// knows.
		if t.d.deps.Exec != nil {
			ctx, cancel := context.WithTimeout(t.d.lifetime(), 10*time.Second)
			proc, err := t.d.deps.Exec.Attach(ctx, req.JobID)
			cancel()
			if err == nil {
				proc.Signal("TERM")
				pc.respond(jobCancelResult{Started: true})
				return
			}
		}
		pc.respond(jobCancelResult{Released: true})
		return
	}
	if !j.started {
		t.cancelLocked(j, "canceled by origin")
		starting := j.starting
		t.mu.Unlock()
		t.d.publishSelf()
		if starting != nil {
			t.waitStart(starting) // handleJobStart stops a job canceled while it started
		}
		t.mu.Lock()
		started, inFlight := j.started, j.starting != nil
		t.mu.Unlock()
		switch {
		case started:
			pc.respond(jobCancelResult{Started: true})
		case inFlight:
			pc.fail(codeInternal, "job %s is still starting on %s", req.JobID, t.d.self)
		default:
			pc.respond(jobCancelResult{Released: true})
		}
		return
	}
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.d.lifetime(), 10*time.Second)
	proc, err := t.ensureProc(ctx, j)
	cancel()
	if err != nil {
		pc.fail(codeUnknownJob, "%v", err)
		return
	}
	if err := t.cancelProc(j, proc); err != nil {
		t.d.logf("target: cancel %s: %v", j.ID, err)
	}
	pc.respond(jobCancelResult{Started: true})
}

func (t *targetSide) handleJobControl(ps *peerSession, pc *peerConn, req rawPeerRequest) {
	if req.Method == proto.PeerJobCancel {
		t.handleJobCancel(ps, pc, req)
		return
	}
	t.mu.Lock()
	j := t.jobs[req.JobID]
	t.mu.Unlock()
	if j == nil || j.Origin != ps.name() {
		pc.fail(codeUnknownJob, "%s has no job %s", t.d.self, req.JobID)
		return
	}
	t.mu.Lock()
	started := j.started
	t.mu.Unlock()
	if !started {
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

	// Phase changes go out from both goroutines below, serialized: output is
	// written only after the phase current at that moment, so the running
	// phase (reported before the command starts) always precedes the
	// command's first byte. The origin decides fallback vs. exit 125 by it.
	var phMu sync.Mutex
	var lastPhase proto.Phase
	sendPhase := func() error {
		phMu.Lock()
		defer phMu.Unlock()
		ph, _ := j.currentPhase()
		if ph.State == "" {
			// Restored after a restart: the phase callback is gone, so ask
			// the job whether its command started meanwhile.
			if r, ok := proc.(interface{ Running() bool }); ok && r.Running() {
				ph = proto.Phase{State: proto.JobRunning}
				j.setPhase(ph)
			}
		}
		if ph == lastPhase || ph.State == "" {
			return nil
		}
		if err := pc.writeJSONFrame(proto.FramePhase, ph); err != nil {
			return err
		}
		lastPhase = ph
		return nil
	}

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
			if err := sendPhase(); err != nil {
				return
			}
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
		for {
			_, ch := j.currentPhase()
			if sendPhase() != nil {
				cancel()
				return
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

	// Input goes to the job's queue, written by its own goroutine: a job
	// that does not read its stdin never blocks heartbeats or signals. The
	// origin learns from the reports how much the job took.
	in := t.inputPump(j, proc)
	go func() {
		if in.sendReports(ctx, pc) != nil {
			cancel()
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
			if !in.push(p) {
				// Only an origin without flow control overruns the queue;
				// never drop input silently: end it here.
				d.logf("target: job %s: its origin sent more than %s of input ahead of the job; closing the job's input", j.ID, humanBytes(d.t.StdinQueue))
				in.pushEOF()
			}
		case proto.FrameStdinEOF:
			in.pushEOF()
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
