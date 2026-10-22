package cli

import (
	"encoding/json"
	"fmt"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Input forwarding to a remote job (-i), with the daemon's flow control
// (see internal/daemon/stdin.go). Offsets count input bytes from the start.
// kran run reads its stdin only while the remote command runs (before, a
// fallback to running here must find the input untouched) and only below
// the credit's limit, and keeps what the job has not consumed so a Resume
// after a lost connection can be served.
type inputState struct {
	on      bool  // forwarding (remote job with -i, or the TTY end-of-input)
	base    int64 // offset of buf[0]
	buf     []byte
	sent    int64 // next offset to send
	limit   int64 // read below this; -1: no limit (daemon without flow control)
	eof     bool  // our stdin ended
	eofSent bool
	reading bool // a read is outstanding
	gone    bool // the daemon could not get lost input back; stop forwarding

	reqCh chan int    // to the reader: read up to n bytes
	data  chan []byte // from the reader; nil = end of input
}

func (r *runner) end() int64 { return r.in.base + int64(len(r.in.buf)) }

// startInput begins forwarding after a remote decision.
func (r *runner) startInput(d proto.Decision) {
	in := &r.in
	in.limit = d.StdinLimit
	if in.limit == 0 {
		in.limit = -1
	}
	switch {
	case in.on:
		// A second remote decision (another node): same input.
	case r.flags.Stdin && r.stdin != nil:
		in.on = true
		in.reqCh = make(chan int, 1)
		in.data = make(chan []byte)
		r.stdinCh = in.data
		go readInput(r.stdin.Read, in.reqCh, in.data, r.quit)
	case r.spec.TTY != nil && !r.flags.Stdin:
		// The job reads its remote terminal, where nothing typed here
		// arrives without -i: end its input instead of letting a prompt
		// wait forever.
		in.on, in.eof = true, true
	}
	r.pumpInput()
}

// readInput reads stdin on request, so nothing is read beyond the credit.
func readInput(read func([]byte) (int, error), req <-chan int, out chan<- []byte, quit <-chan struct{}) {
	buf := make([]byte, 32<<10)
	for {
		var n int
		select {
		case n = <-req:
		case <-quit:
			return
		}
		k, err := read(buf[:min(n, len(buf))])
		if k > 0 {
			select {
			case out <- append([]byte(nil), buf[:k]...):
			case <-quit:
				return
			}
		}
		if err != nil {
			select {
			case out <- nil:
			case <-quit:
			}
			return
		}
	}
}

// onInput takes what the reader read.
func (r *runner) onInput(b []byte) {
	in := &r.in
	in.reading = false
	if b == nil {
		in.eof = true
		r.stdinCh = nil
	} else {
		in.buf = append(in.buf, b...)
	}
	r.pumpInput()
}

// onCredit applies the daemon's credit and serves a Resume.
func (r *runner) onCredit(c proto.StdinCredit) {
	in := &r.in
	if !in.on {
		return
	}
	if drop := c.Consumed - in.base; drop > 0 {
		drop = min(drop, int64(len(in.buf)))
		in.buf = in.buf[drop:]
		in.base += drop
	}
	in.limit = c.Limit
	if c.Resume != nil {
		at := *c.Resume
		if at < in.base || at > r.end() {
			// The input the job is missing is gone from here too. The
			// daemon closes the job's input and says so.
			in.gone = true
			r.sendJSON(proto.FrameStdinResume, proto.StdinResume{Offset: in.base, Lost: true})
			return
		}
		in.sent, in.eofSent = at, false
		r.sendJSON(proto.FrameStdinResume, proto.StdinResume{Offset: at})
	}
	r.pumpInput()
}

// pumpInput sends what is due and asks the reader for more when allowed.
func (r *runner) pumpInput() {
	in := &r.in
	if !in.on || in.gone || r.sess == nil {
		return
	}
	for in.sent < r.end() {
		from := in.sent - in.base
		chunk := in.buf[from:min(int64(len(in.buf)), from+32<<10)]
		if r.sess.Send(proto.FrameStdin, chunk) != nil {
			return // the session is gone; a reattach resumes
		}
		in.sent += int64(len(chunk))
	}
	if in.limit < 0 {
		// No flow control: nothing can be asked for again, keep nothing.
		in.base, in.buf = in.sent, in.buf[:0]
	}
	if in.eof && !in.eofSent {
		if r.sess.Send(proto.FrameStdinEOF, nil) == nil {
			in.eofSent = true
		}
	}
	// Read on only while the command runs, within the credit.
	if in.reqCh != nil && r.started && !in.eof && !in.reading && (in.limit < 0 || r.end() < in.limit) {
		n := int64(32 << 10)
		if in.limit >= 0 {
			n = min(n, in.limit-r.end())
		}
		in.reading = true
		in.reqCh <- int(n)
	}
}

func decodeCredit(p []byte) (proto.StdinCredit, error) {
	var c proto.StdinCredit
	if err := json.Unmarshal(p, &c); err != nil {
		return c, fmt.Errorf("unreadable stdin credit: %w", err)
	}
	return c, nil
}
