package daemon

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Stdin flow control for remote jobs (-i).
//
// Input offsets count bytes from the start of the job's input, the same on
// kran run, origin and target. The target holds a bounded queue per job and
// reports with pfStdinCredit what it received and what the job consumed;
// the origin passes the consumed offset on to kran run as a credit
// (FrameStdinCredit, limit = consumed + window), and kran run reads its own
// stdin only below the limit, keeping what was not consumed yet. Nothing is
// ever dropped: when a stream to the target breaks, the new stream says
// what the target holds and kran run sends again from there (Resume).
// Signals and resizes never wait behind input (outbox priority, separate
// supervisor connection).

// pfStdinCredit is the target's report on a job stream (JSON stdinReport).
const pfStdinCredit proto.FrameType = 105

type stdinReport struct {
	Received int64 `json:"received"`
	Consumed int64 `json:"consumed"`
	EOF      bool  `json:"eof"` // the job's input was closed
	// Known is false when this daemon did not see the input so far (it
	// restarted): the offsets start from zero and say nothing.
	Known bool `json:"known"`
}

// jobStreamInfo answers job.start and job.attach (nil from targets without
// flow control).
type jobStreamInfo struct {
	StdinWindow int64 `json:"stdin_window,omitempty"`
}

// --- origin side ---

// inFlow is the origin's view of a remote job's input.
type inFlow struct {
	on          bool // the job forwards input (spec.Stdin)
	mu          sync.Mutex
	window      int64 // credit beyond the consumed offset (0: no flow control yet)
	limitless   bool  // the target has no flow control
	consumed    int64
	next        int64 // offset of the next byte expected from kran run
	eof         bool  // kran run's EOF was forwarded
	eofDone     bool  // the target closed the job's input
	awaitResume bool  // drop input from kran run until it answers a Resume
	resumeAt    int64 // -1: not asked yet (restored daemon)
	lastLimit   int64
	closed      bool // input was closed after a loss
}

// stdinLimit is the first credit, sent with a remote decision.
func (e *jobEntry) stdinLimit() int64 {
	f := &e.in
	if !f.on {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.window == 0 {
		f.window = e.d.t.StdinWindow
	}
	f.lastLimit = f.consumed + f.window
	return f.lastLimit
}

func (f *inFlow) creditLocked(resume *int64) proto.StdinCredit {
	c := proto.StdinCredit{Consumed: f.consumed, Limit: f.consumed + f.window, Resume: resume}
	if f.limitless {
		c.Limit = -1
	}
	if f.closed {
		c.Limit = f.next // nothing more is taken
	}
	f.lastLimit = c.Limit
	return c
}

// cliInput takes stdin or stdin_eof from kran run. It reports false when the
// outbox is full (only without flow control): the caller waits.
func (e *jobEntry) cliInput(t proto.FrameType, p []byte) bool {
	f := &e.in
	if !f.on {
		// No -i: only the end of input of a terminal job comes here.
		return e.outbox.put(t, p)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.awaitResume || f.closed {
		return true // superseded by a Resume (or input is closed)
	}
	if !e.outbox.put(t, p) {
		return false
	}
	if t == proto.FrameStdin {
		f.next += int64(len(p))
	} else {
		f.eof = true
	}
	return true
}

// cliResume handles kran run's answer to a Resume.
func (e *jobEntry) cliResume(r proto.StdinResume) {
	f := &e.in
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.awaitResume {
		return
	}
	if r.Lost {
		// kran run no longer has the input the target is missing.
		f.awaitResume = false
		e.closeInputLocked("kran run could not send the job's input again after a lost connection")
		return
	}
	if r.Offset == f.resumeAt {
		f.awaitResume = false
	}
	// Otherwise it answers an older request; the answer to the latest
	// one follows.
}

// closeInputLocked ends the job's input after a loss, loudly.
func (e *jobEntry) closeInputLocked(why string) {
	f := &e.in
	if f.closed {
		return
	}
	f.closed = true
	e.outbox.put(proto.FrameStdinEOF, nil)
	e.notice("warn", why+"; the job's input was closed early and may be incomplete")
	e.d.logf("job %s: %s; closed its input", e.id, why)
	e.sendFrame(proto.FrameStdinCredit, f.creditLocked(nil))
}

// cliReattached asks a reattaching kran run to send its input again from
// what this daemon expects next: frames in flight on the lost session are
// gone. A restored daemon learns that offset from the target first.
func (e *jobEntry) cliReattached() {
	f := &e.in
	if !f.on {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.window == 0 || f.limitless || f.closed || f.resumeAt < 0 {
		return
	}
	f.awaitResume = true
	f.resumeAt = f.next
	f.eof = false
	at := f.next
	e.sendFrame(proto.FrameStdinCredit, f.creditLocked(&at))
}

// targetStream starts input on a new stream to the target. window is what
// the target announced (0: no flow control).
func (e *jobEntry) targetStream(window int64) (flow bool) {
	f := &e.in
	f.mu.Lock()
	defer f.mu.Unlock()
	if window <= 0 {
		if !f.limitless {
			f.limitless = true
			if f.on {
				e.sendFrame(proto.FrameStdinCredit, f.creditLocked(nil))
			}
		}
		return false
	}
	if f.window == 0 || f.window > window {
		f.window = min(e.d.t.StdinWindow, window)
	}
	return true
}

// targetReport applies a pfStdinCredit. first marks the report that opens
// a stream: input queued for the stream before it is dropped, and kran run
// resends what the target does not have.
func (e *jobEntry) targetReport(r stdinReport, first bool, node string) {
	f := &e.in
	if !f.on {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	if !r.Known && f.next == 0 {
		// Nothing was forwarded yet (the job restarted on its target before
		// it read any input, e.g. during setup): nothing can repeat, so
		// the input starts from zero as if the target had said so.
		r = stdinReport{Known: true}
	}
	if !r.Known {
		if first {
			e.outbox.dropInput()
			if !f.eofDone {
				// The target restarted and cannot say how much input the
				// job read: sending again could repeat input.
				e.closeInputLocked("the kran daemon on " + node + " restarted while forwarding the job's input")
			}
		}
		return
	}
	f.consumed = max(f.consumed, r.Consumed)
	f.eofDone = f.eofDone || r.EOF
	if first {
		e.outbox.dropInput()
		if f.awaitResume || r.Received != f.next {
			f.awaitResume = true
			f.resumeAt = r.Received
			f.next = r.Received
			f.eof = false
			at := r.Received
			e.sendFrame(proto.FrameStdinCredit, f.creditLocked(&at))
			return
		}
		if f.eof && !r.EOF {
			e.outbox.put(proto.FrameStdinEOF, nil) // it was dropped with the queue
		}
	}
	if f.awaitResume {
		return
	}
	limit := f.consumed + f.window
	if limit != f.lastLimit && (limit-f.lastLimit >= f.window/4 || f.consumed == f.next || first) {
		e.sendFrame(proto.FrameStdinCredit, f.creditLocked(nil))
	}
}

// --- target side ---

// stdinPump holds a job's input on the target and writes it to the job. It
// outlives job streams, so a reattaching origin continues where it left.
type stdinPump struct {
	mu       sync.Mutex
	chunks   [][]byte
	size     int64
	max      int64
	received int64
	consumed int64
	eof      bool // EOF received
	eofDone  bool // the job's stdin was closed
	closed   bool
	known    bool // false: created after a restart, offsets unknown
	notify   chan struct{}
	changed  chan struct{} // closed and replaced when consumed or eofDone change
}

func newStdinPump(max int64, known bool) *stdinPump {
	return &stdinPump{max: max, known: known, notify: make(chan struct{}, 1), changed: make(chan struct{})}
}

func (q *stdinPump) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *stdinPump) changedLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// push queues input. It fails when the queue is full, which an origin
// with flow control never causes; the caller then closes the input.
func (q *stdinPump) push(b []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.eof || q.closed {
		return true
	}
	if q.size+int64(len(b)) > q.max {
		return false
	}
	q.chunks = append(q.chunks, append([]byte(nil), b...))
	q.size += int64(len(b))
	q.received += int64(len(b))
	q.wake()
	return true
}

func (q *stdinPump) pushEOF() {
	q.mu.Lock()
	q.eof = true
	q.mu.Unlock()
	q.wake()
}

func (q *stdinPump) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wake()
}

func (q *stdinPump) report() (stdinReport, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return stdinReport{Received: q.received, Consumed: q.consumed, EOF: q.eofDone, Known: q.known}, q.changed
}

// run writes the input to the job until EOF, the job's end or ctx.
func (q *stdinPump) run(ctx context.Context, w io.WriteCloser) {
	for {
		q.mu.Lock()
		switch {
		case q.closed:
			q.mu.Unlock()
			return
		case len(q.chunks) > 0:
			b := q.chunks[0]
			q.chunks = q.chunks[1:]
			q.mu.Unlock()
			// Blocks while the job does not read: that is the backpressure
			// the credits pass on to kran run.
			if _, err := w.Write(b); err != nil {
				q.close()
				return
			}
			q.mu.Lock()
			q.size -= int64(len(b))
			q.consumed += int64(len(b))
			q.changedLocked()
			q.mu.Unlock()
			continue
		case q.eof:
			q.eofDone = true
			q.closed = true
			q.changedLocked()
			q.mu.Unlock()
			w.Close()
			return
		}
		q.mu.Unlock()
		select {
		case <-q.notify:
		case <-ctx.Done():
			return
		}
	}
}

// sendReports keeps the origin informed on one stream: at once, then
// whenever the job consumed input (at most every 50 ms).
func (q *stdinPump) sendReports(ctx context.Context, pc *peerConn) error {
	var last stdinReport
	for first := true; ; first = false {
		r, ch := q.report()
		if first || r != last {
			b, _ := json.Marshal(r)
			if err := pc.writeFrame(pfStdinCredit, b); err != nil {
				return err
			}
			last = r
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}
