package daemon

// Peer protocol details (SPEC §8.3; the envelope is proto.PeerRequest /
// proto.PeerResponse). Every yamux stream starts with one JSON request line
// and one JSON response line. Afterwards, depending on the method:
//
//   hello              nothing; both sides exchange proto.Hello
//   metrics.subscribe  target writes one peerStatus JSON line every 2 s
//   admit              nothing (reserve resources for job_id, 30 s TTL)
//   slot.acquire       nothing (response body: proto.SlotRef plus "sync": 1
//                      from a 1.1 target, see syncproto.go)
//   slot.diff          request body {format, hash, base} (1.1; absent =
//                      JSON); origin sends the manifest zstd-compressed in
//                      pfData frames ending with pfEnd (nothing for format
//                      "same"); then the response line (body:
//                      proto.SyncPlan) follows
//   blob.put           origin sends, per blob, pfBlob{hash,size} + pfData
//                      (zstd) frames + pfEnd; target acks each blob with a
//                      pfAck frame; pfDone ends the stream
//   slot.commit        nothing (commits the manifest of the last diff;
//                      skipped when the plan said committed)
//   slot.release       nothing
//   job.start          after the OK the stream becomes the job stream
//   job.attach         job stream from body.offset
//   job.heartbeat/job.signal/job.resize/job.cancel  one-shot
//   mesh.peers         body: []string
//
// The job stream carries run-session frames (proto.WriteFrame): target →
// origin FramePhase, FrameStdout/FrameStderr, FrameHeartbeat, FrameExit;
// origin → target FrameHeartbeat (renews the lease), FrameStdin,
// FrameStdinEOF, FrameResize, FrameSignal.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

// Frame types private to peer streams (outside the run-session range).
const (
	pfData proto.FrameType = 100 + iota
	pfEnd
	pfBlob
	pfAck
	pfDone
)

// Peer error codes (PeerResponse.Code).
const (
	codeVersion      = "version"
	codeHelloFirst   = "hello_first"
	codeNotAccepting = "not_accepting"
	codeDraining     = "draining"
	codeBusy         = "busy"
	codeMaxJobs      = "max_jobs"
	codeUnknownJob   = "unknown_job"
	codeJobStarted   = "job_started"
	codeBadRequest   = "bad_request"
	codeInternal     = "internal"
	codeNoSlot       = "slot"
	codeDisk         = "disk_floor"
	codeNotLinked    = "not_linked"
)

type peerError struct {
	Method string
	Code   string
	Msg    string
}

func (e *peerError) Error() string {
	if e.Msg == "" {
		return e.Method + ": " + e.Code
	}
	return e.Msg
}

func peerCode(err error) string {
	var pe *peerError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

type rawPeerRequest struct {
	Method string          `json:"method"`
	JobID  string          `json:"job_id,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type rawPeerResponse struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Code  string          `json:"code,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// Request bodies.
type admitBody struct {
	Origin   string            `json:"origin"`
	Alloc    proto.Resources   `json:"alloc"`
	Runtime  proto.RuntimeKind `json:"runtime"`
	Duration float64           `json:"duration_s,omitempty"` // estimate, origin seconds
	RepoID   string            `json:"repo_id,omitempty"`
	Owner    proto.Owner       `json:"owner"`
	Argv     []string          `json:"argv,omitempty"`
}

type slotAcquireBody struct {
	RepoID  string            `json:"repo_id"`
	Hint    string            `json:"hint"`
	Runtime proto.RuntimeKind `json:"runtime"`
}

type slotBody struct {
	Slot proto.SlotRef `json:"slot"`
}

type blobHeader struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

type blobAck struct {
	Hash  string `json:"hash"`
	Error string `json:"error,omitempty"`
}

type jobStartBody struct {
	Spec    proto.JobSpec     `json:"spec"`
	Slot    proto.SlotRef     `json:"slot"`
	Runtime proto.RuntimeKind `json:"runtime"`
	Alloc   proto.Resources   `json:"alloc"`
	Origin  string            `json:"origin"`
}

type jobAttachBody struct {
	Offset int64 `json:"offset"`
}

// jobCancelResult answers job.cancel. Released: the job never started
// here and no job.start can start it any more, so the origin may run it
// elsewhere. Started: it started here and is being stopped. Neither (an
// older target): nothing is certain.
type jobCancelResult struct {
	Released bool `json:"released,omitempty"`
	Started  bool `json:"started,omitempty"`
}

type jobSignalBody struct {
	Signal string `json:"signal"`
}

// peerStatus is what metrics.subscribe streams.
type peerStatus struct {
	Info     proto.NodeInfo    `json:"info"`
	Metrics  proto.NodeMetrics `json:"metrics"`
	Capacity proto.Resources   `json:"capacity"` // after reserve
	Reserve  proto.Resources   `json:"reserve"`
	Accepts  bool              `json:"accepts"`
	Draining bool              `json:"draining"`
	Degraded string            `json:"degraded,omitempty"`
	MaxJobs  int               `json:"max_jobs,omitempty"`
	// The node's own scheduler totals (its local jobs and admitted
	// guests); AllocatedJobs lists what Allocated counts.
	Allocated     proto.Resources `json:"allocated"`
	AllocatedJobs []string        `json:"allocated_jobs"`
	Running       int             `json:"running"`
	Free          proto.Resources `json:"free"`
	Guests        []proto.JobRef  `json:"guests,omitempty"`
	// Allocs are the remote jobs running here (for live usage).
	Allocs []allocInfo `json:"allocs,omitempty"`
}

type allocInfo struct {
	JobID   string           `json:"job_id"`
	Origin  string           `json:"origin"`
	Res     proto.Resources  `json:"res"`
	Running bool             `json:"running"`
	Live    *proto.Resources `json:"live,omitempty"` // current usage of a running job
}

// peerConn wraps a stream with a line reader and a write lock.
type peerConn struct {
	net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
	buf []byte
	// stdinWindow is what the target announced for a job stream (0: no
	// input flow control).
	stdinWindow int64
}

func newPeerConn(c net.Conn) *peerConn {
	return &peerConn{Conn: c, r: bufio.NewReaderSize(c, 64<<10)}
}

const maxLine = 64 << 20 // responses: a sync plan lists every missing hash

// maxRequestLine bounds a request envelope (job.start carries argv and env,
// which the OS caps at a few MiB anyway).
const maxRequestLine = 16 << 20

// maxPeerStreams bounds the streams one peer session has in service.
const maxPeerStreams = 512

func (c *peerConn) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.Conn.Write(append(b, '\n'))
	return err
}

func (c *peerConn) readJSON(v any) error { return c.readJSONLimit(v, maxLine) }

// readJSONLimit reads one JSON line of at most max bytes.
func (c *peerConn) readJSONLimit(v any, max int) error {
	var line []byte
	for {
		chunk, err := c.r.ReadSlice('\n')
		line = append(line, chunk...)
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
		if len(line) > max {
			return errors.New("peer line too long")
		}
	}
	return json.Unmarshal(line, v)
}

func (c *peerConn) writeFrame(t proto.FrameType, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return proto.WriteFrame(c.Conn, t, p)
}

func (c *peerConn) writeJSONFrame(t proto.FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(t, b)
}

// readFrame returns a payload valid until the next readFrame.
func (c *peerConn) readFrame() (proto.FrameType, []byte, error) {
	t, p, err := proto.ReadFrame(c.r, c.buf)
	if err != nil {
		return 0, nil, err
	}
	if cap(p) > cap(c.buf) {
		c.buf = p[:0]
	}
	return t, p, nil
}

func (c *peerConn) respond(body any) error {
	return c.writeJSON(proto.PeerResponse{OK: true, Body: body})
}

func (c *peerConn) fail(code string, format string, args ...any) error {
	return c.writeJSON(proto.PeerResponse{OK: false, Code: code, Error: fmt.Sprintf(format, args...)})
}

// call opens a stream, sends the request and reads the response. On success
// the stream is returned open (streaming methods continue on it); the caller
// closes it. ctx bounds the request/response exchange only.
func peerCall(ctx context.Context, s transport.Session, method, jobID string, body any, out any) (*peerConn, error) {
	c, err := s.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: open stream: %w", method, err)
	}
	pc := newPeerConn(c)
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if err := pc.writeJSON(proto.PeerRequest{Method: method, JobID: jobID, Body: body}); err != nil {
		c.Close()
		return nil, fmt.Errorf("%s: send: %w", method, ctxErr(ctx, err))
	}
	if err := pc.readResponse(method, out); err != nil {
		c.Close()
		return nil, ctxErr(ctx, err)
	}
	if !stop() {
		c.Close()
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	}
	c.SetDeadline(time.Time{})
	return pc, nil
}

func (c *peerConn) readResponse(method string, out any) error {
	var resp rawPeerResponse
	if err := c.readJSON(&resp); err != nil {
		return fmt.Errorf("%s: read answer: %w", method, err)
	}
	if !resp.OK {
		return &peerError{Method: method, Code: resp.Code, Msg: resp.Error}
	}
	if out != nil && len(resp.Body) > 0 && string(resp.Body) != "null" {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return fmt.Errorf("%s: decode answer: %w", method, err)
		}
	}
	return nil
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w (%v)", ctx.Err(), err)
	}
	return err
}

// oneShot performs a request without a stream phase.
func peerOneShot(ctx context.Context, s transport.Session, method, jobID string, body any, out any) error {
	pc, err := peerCall(ctx, s, method, jobID, body, out)
	if err != nil {
		return err
	}
	pc.Close()
	return nil
}

// frameWriter streams bytes as pfData frames.
type frameWriter struct{ c *peerConn }

func (w frameWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := len(p)
		if k > 256<<10 {
			k = 256 << 10
		}
		if err := w.c.writeFrame(pfData, p[:k]); err != nil {
			return n, err
		}
		n += k
		p = p[k:]
	}
	return n, nil
}

// frameReader reads pfData frames until pfEnd.
type frameReader struct {
	c    *peerConn
	cur  []byte
	done bool
}

func (r *frameReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.done {
			return 0, io.EOF
		}
		t, b, err := r.c.readFrame()
		if err != nil {
			return 0, err
		}
		switch t {
		case pfData:
			r.cur = b
		case pfEnd:
			r.done = true
		default:
			return 0, fmt.Errorf("unexpected frame %d in data stream", t)
		}
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// drain consumes the rest of the data frames (after an early failure) so
// the stream stays in sync.
func (r *frameReader) drain() error {
	r.cur = nil
	for !r.done {
		t, _, err := r.c.readFrame()
		if err != nil {
			return err
		}
		switch t {
		case pfEnd:
			r.done = true
		case pfData:
		default:
			return fmt.Errorf("unexpected frame %d in data stream", t)
		}
	}
	return nil
}

func protocolString() string {
	return strconv.Itoa(proto.ProtocolMajor) + "." + strconv.Itoa(proto.ProtocolMinor)
}

// compatibleProtocol checks the major version of a peer's protocol string.
func compatibleProtocol(p string) bool {
	major, _, _ := strings.Cut(p, ".")
	return major == strconv.Itoa(proto.ProtocolMajor)
}
