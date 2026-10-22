package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/hafencity-dev/kran/internal/proto"
)

// spool is the origin-side, append-only record of everything a job sends to
// the CLI: notices, decision, phases, stdout/stderr and the exit, framed
// exactly like the run session (proto.WriteFrame). Run sessions tail it, so
// the job's driver never blocks on a slow CLI, a reattaching CLI resumes at
// an output offset, and `kran logs` reads the same file. Output offsets
// count only stdout/stderr payload bytes (proto.Reattach).
type spool struct {
	path string

	mu       sync.Mutex
	f        *os.File
	size     int64 // bytes in the file
	out      int64 // output payload bytes
	finished bool
	notify   chan struct{}
	err      error // first write error (disk full…)
	// After a write error the spool continues in memory: the file holds
	// the first memBase bytes, mem the frames after it. The run session
	// must keep working on a full disk (never worse than local).
	memBase int64
	mem     []byte
}

// spoolMemMax bounds the in-memory tail; beyond it output frames are
// dropped (control frames, e.g. the exit, are always kept).
const spoolMemMax = 64 << 20

func isOutput(t proto.FrameType) bool { return t == proto.FrameStdout || t == proto.FrameStderr }

// openSpool opens or creates the spool at path. Reopening (after a daemon
// restart) recounts the output and drops a torn final frame.
func openSpool(path string) (*spool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	s := &spool{path: path, f: f, notify: make(chan struct{})}
	if err := s.recover(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func (s *spool) recover() error {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReader(s.f)
	var pos int64
	var buf []byte
	for {
		t, p, err := proto.ReadFrame(br, buf)
		if err != nil {
			break
		}
		buf = p[:0]
		pos += 5 + int64(len(p))
		if isOutput(t) {
			s.out += int64(len(p))
		}
		if t == proto.FrameExit {
			s.finished = true
		}
	}
	if err := s.f.Truncate(pos); err != nil {
		return err
	}
	if _, err := s.f.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	s.size = pos
	return nil
}

func (s *spool) append(t proto.FrameType, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return errors.New("spool finished")
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	hdr[1] = byte(len(payload) >> 24)
	hdr[2] = byte(len(payload) >> 16)
	hdr[3] = byte(len(payload) >> 8)
	hdr[4] = byte(len(payload))
	frame := append(hdr[:], payload...)
	if s.err == nil {
		if _, err := s.f.Write(frame); err != nil {
			// A torn frame would corrupt the stream; cut back to the last
			// complete one and continue in memory.
			s.f.Truncate(s.size)
			s.f.Seek(s.size, io.SeekStart)
			s.err = fmt.Errorf("write output spool: %w", err)
			s.memBase = s.size
		}
	}
	if s.err != nil {
		if isOutput(t) && len(s.mem)+len(frame) > spoolMemMax {
			return s.err
		}
		s.mem = append(s.mem, frame...)
	}
	s.size += 5 + int64(len(payload))
	if isOutput(t) {
		s.out += int64(len(payload))
	}
	if t == proto.FrameExit {
		s.finished = true
	}
	close(s.notify)
	s.notify = make(chan struct{})
	return nil
}

// appendData splits output into frames below the frame limit.
func (s *spool) appendData(t proto.FrameType, b []byte) error {
	for len(b) > 0 {
		n := len(b)
		if n > proto.MaxFramePayload {
			n = proto.MaxFramePayload
		}
		if err := s.append(t, b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// finish marks the spool complete without an exit frame (job abandoned).
func (s *spool) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.finished {
		s.finished = true
		close(s.notify)
		s.notify = make(chan struct{})
	}
}

func (s *spool) outputBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out
}

func (s *spool) state() (size int64, finished bool, notify <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size, s.finished, s.notify
}

// memFrame returns the in-memory frame at position pos, if pos is past the
// file part.
func (s *spool) memFrame(pos int64) (proto.FrameType, []byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil || pos < s.memBase {
		return 0, nil, false
	}
	i := pos - s.memBase
	if i+5 > int64(len(s.mem)) {
		return 0, nil, false
	}
	n := int64(s.mem[i+1])<<24 | int64(s.mem[i+2])<<16 | int64(s.mem[i+3])<<8 | int64(s.mem[i+4])
	return proto.FrameType(s.mem[i]), append([]byte(nil), s.mem[i+5:i+5+n]...), true
}

func (s *spool) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

// spoolReader tails a spool.
type spoolReader struct {
	s      *spool
	f      *os.File
	br     *bufio.Reader
	pos    int64 // file position of the next frame
	outPos int64 // output payload bytes before pos
	offset int64 // deliver output from here
	// skipCtlBefore: control frames before this file position were already
	// seen by a reattaching client and are skipped (exit is never skipped).
	skipCtlBefore int64
	buf           []byte
	// hideLocalOutput: run sessions do not get a local job's output back
	// (its CLI sent it for the history; the terminal already has it).
	// Output after a decision{mode:"local"} is skipped but still counted.
	hideLocalOutput bool
	localMode       bool
}

// reader returns a reader delivering output from offset. With reattach,
// control frames already in the spool are skipped.
func (s *spool) reader(offset int64, reattach bool) (*spoolReader, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	r := &spoolReader{s: s, f: f, br: bufio.NewReaderSize(f, 64<<10), offset: offset}
	if reattach {
		r.skipCtlBefore, _, _ = s.state()
	}
	return r, nil
}

func (r *spoolReader) Close() error { return r.f.Close() }

// Next returns the next frame to deliver, blocking for new frames. It
// returns io.EOF once the spool is finished and fully read.
func (r *spoolReader) Next(ctx context.Context) (proto.FrameType, []byte, error) {
	return r.next(ctx, true)
}

// NextNoWait is Next without blocking: io.EOF when nothing more is written
// yet.
func (r *spoolReader) NextNoWait() (proto.FrameType, []byte, error) {
	return r.next(context.Background(), false)
}

func (r *spoolReader) next(ctx context.Context, wait bool) (proto.FrameType, []byte, error) {
	for {
		size, finished, notify := r.s.state()
		if r.pos >= size {
			if finished || !wait {
				return 0, nil, io.EOF
			}
			select {
			case <-notify:
				continue
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			}
		}
		t, p, inMem := r.s.memFrame(r.pos)
		if !inMem {
			var err error
			t, p, err = proto.ReadFrame(r.br, r.buf)
			if err != nil {
				return 0, nil, fmt.Errorf("read spool: %w", err)
			}
			r.buf = p[:0]
		}
		framePos := r.pos
		r.pos += 5 + int64(len(p))
		if t == proto.FrameDecision && r.hideLocalOutput {
			var dc proto.Decision
			if json.Unmarshal(p, &dc) == nil {
				r.localMode = dc.Mode == "local"
			}
		}
		if isOutput(t) {
			start := r.outPos
			r.outPos += int64(len(p))
			if r.outPos <= r.offset || r.localMode {
				continue
			}
			if start < r.offset {
				p = p[r.offset-start:]
			}
			return t, p, nil
		}
		if t != proto.FrameExit && framePos < r.skipCtlBefore {
			continue
		}
		return t, p, nil
	}
}
