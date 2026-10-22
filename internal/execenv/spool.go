package execenv

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// The spool jobs/<id>/out is append-only and framed exactly like run-session
// frames (type u8 | len u32be | data) with type FrameStdout or FrameStderr.
// One process (the supervisor) writes it; any number of readers tail it.
// Offsets used for reattach count payload bytes only, so the origin can
// re-chunk frames freely and still resume exactly.

const spoolChunk = 32 << 10

type spoolWriter struct {
	mu  sync.Mutex
	f   *os.File
	buf []byte
	n   int64 // payload bytes written
}

func openSpool(path string) (*spoolWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &spoolWriter{f: f, buf: make([]byte, 5+spoolChunk)}, nil
}

// write appends data as frames of type t, each with a single write call so
// a reader never sees a header without its data for long.
func (s *spoolWriter) write(t proto.FrameType, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(data) > 0 {
		n := min(len(data), spoolChunk)
		s.buf[0] = byte(t)
		binary.BigEndian.PutUint32(s.buf[1:5], uint32(n))
		copy(s.buf[5:], data[:n])
		if _, err := s.f.Write(s.buf[:5+n]); err != nil {
			return err
		}
		s.n += int64(n)
		data = data[n:]
	}
	return nil
}

func (s *spoolWriter) Close() error { return s.f.Close() }

// streamWriter adapts one stream of the spool to io.Writer.
type streamWriter struct {
	s *spoolWriter
	t proto.FrameType
}

func (w streamWriter) Write(p []byte) (int, error) {
	if err := w.s.write(w.t, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// spoolReader tails a spool from a payload offset. Each Read returns one
// whole frame (split if p is small). It blocks while the job runs and
// returns io.EOF once the job ended and everything was delivered.
type spoolReader struct {
	path    string
	mu      sync.Mutex // guards f against Close during Read
	f       *os.File
	pos     int64 // file position of the next frame header
	skip    int64 // payload bytes still to skip (reattach offset)
	cur     proto.FrameType
	curLeft []byte // remaining payload of the current frame
	ended   func() bool
	closed  chan struct{}
	once    sync.Once
	hdr     [5]byte
	data    []byte
	minPoll time.Duration
	maxPoll time.Duration
	eof     bool
	pending []byte // encoded bytes not yet returned
	// beforeFrame runs before a frame is handed out (outside mu).
	beforeFrame func()
}

var errReaderClosed = errors.New("output reader closed")

func newSpoolReader(path string, offset int64, ended func() bool) *spoolReader {
	return &spoolReader{
		path:    path,
		skip:    max(offset, 0),
		ended:   ended,
		closed:  make(chan struct{}),
		minPoll: 10 * time.Millisecond,
		maxPoll: 100 * time.Millisecond,
	}
}

func (r *spoolReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	return nil
}

func (r *spoolReader) isClosed() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
}

// nextFrame loads the next complete frame into cur/curLeft. It returns
// false when none is available yet. Caller holds mu.
func (r *spoolReader) nextFrame() (bool, error) {
	if r.f == nil {
		f, err := os.Open(r.path)
		if err != nil {
			return false, nil // supervisor has not created it yet
		}
		r.f = f
	}
	for {
		if _, err := r.f.ReadAt(r.hdr[:], r.pos); err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		n := int64(binary.BigEndian.Uint32(r.hdr[1:]))
		if n > proto.MaxFramePayload {
			return false, errors.New("output spool corrupt")
		}
		if int64(cap(r.data)) < n {
			r.data = make([]byte, n)
		}
		buf := r.data[:n]
		if _, err := r.f.ReadAt(buf, r.pos+5); err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil // data not fully written yet
			}
			return false, err
		}
		r.pos += 5 + n
		if r.skip >= n {
			r.skip -= n
			continue
		}
		buf = buf[r.skip:]
		r.skip = 0
		r.cur = proto.FrameType(r.hdr[0])
		r.curLeft = buf
		return true, nil
	}
}

// Read returns the output as a byte stream of encoded frames. A buffer of
// at least 5+32 KiB always receives whole frames (one per Read); smaller
// buffers get the stream in pieces, which proto.ReadFrame handles.
func (r *spoolReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	poll := r.minPoll
	for {
		if len(r.pending) > 0 {
			n := copy(p, r.pending)
			r.pending = r.pending[n:]
			return n, nil
		}
		if r.eof {
			return 0, io.EOF
		}
		if r.isClosed() {
			return 0, errReaderClosed
		}
		r.mu.Lock()
		// Check "ended" before looking for data: the supervisor writes the
		// status only after the last output, so seeing ended and then no
		// frame means everything was delivered.
		ended := r.ended()
		ok, err := r.nextFrame()
		r.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if ok {
			if r.beforeFrame != nil {
				r.beforeFrame()
			}
			r.pending = r.encodeCurrent()
			if len(p) >= len(r.pending) {
				n := copy(p, r.pending)
				r.pending = r.pending[n:]
				return n, nil
			}
			continue
		}
		if ended {
			r.eof = true
			r.Close()
			return 0, io.EOF
		}
		select {
		case <-r.closed:
		case <-time.After(poll):
		}
		poll = min(poll*2, r.maxPoll)
	}
}

func (r *spoolReader) encodeCurrent() []byte {
	b := make([]byte, 5+len(r.curLeft))
	b[0] = byte(r.cur)
	binary.BigEndian.PutUint32(b[1:5], uint32(len(r.curLeft)))
	copy(b[5:], r.curLeft)
	r.curLeft = nil
	return b
}
