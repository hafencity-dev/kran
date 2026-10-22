package cli

import (
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/hafencity-dev/kran/internal/proto"
)

// outputCapture puts kran between a local child and the terminal so the
// output can also go to the job history, without changing what the child
// or the terminal see:
//
//   - stdout/stderr that are terminals are replaced by one pseudo-terminal
//     with the real terminal's settings and size (colors, progress bars and
//     isatty checks behave as before). When stdin is that terminal too, it is
//     put in raw mode and keystrokes go to the pty, like ssh -t or script(1):
//     Ctrl-C, line editing and full-screen programs work inside the job.
//   - other outputs (pipes, files) become pipes; one shared pipe when stdout
//     and stderr are the same file, so their interleaving stays exact.
//
// Every read is written to the real destination at once (byte-exact, no
// buffering) before it is offered to the history, which never blocks.
type outputCapture struct {
	push func(proto.FrameType, []byte)

	// For the child.
	stdin, stdout, stderr *os.File
	ctty                  int  // child fd of the controlling terminal; -1 = none
	foreground            bool // stdin is the real terminal: make the child its foreground group

	childEnds []*os.File // closed in the parent after start
	readers   []*os.File // our read ends
	copiers   sync.WaitGroup
	copied    chan struct{} // closed when every copier ended

	in       *os.File // the real stdin (raw mode: forwarded to the pty)
	ptm      *os.File // pty master (nil without a pty)
	term     *os.File // the real terminal the pty stands in for
	rawFd    int
	rawState *term.State
	pipeSig  chan os.Signal // SIGPIPE is noticed, not fatal, while copying
	copyTo   []copyPlan
	finished sync.Once
}

type copyPlan struct {
	from *os.File
	to   io.Writer
	t    proto.FrameType
}

// newCapture prepares the child's stdio for st. nothing is started yet.
func newCapture(st localStdio, push func(proto.FrameType, []byte)) (*outputCapture, error) {
	c := &outputCapture{push: push, ctty: -1, rawFd: -1, copied: make(chan struct{})}
	outTTY := st.Out != nil && isTerminal(st.Out)
	errTTY := st.Err != nil && isTerminal(st.Err)
	inTTY := st.In != nil && isTerminal(st.In)
	c.stdin = st.In
	fail := func(err error) (*outputCapture, error) {
		c.abort()
		return nil, err
	}

	if outTTY || errTTY {
		ptm, pts, err := pty.Open()
		if err != nil {
			return fail(err)
		}
		c.ptm = ptm
		c.childEnds = append(c.childEnds, pts)
		c.term = st.Out
		if !outTTY {
			c.term = st.Err
		}
		raw := inTTY && sameFile(st.In, c.term)
		if err := copyTerminal(c.term, pts, raw); err != nil {
			return fail(err)
		}
		ft := proto.FrameStdout
		if !outTTY {
			ft = proto.FrameStderr
		}
		c.copyTo = append(c.copyTo, copyPlan{ptm, c.term, ft})
		if raw {
			c.stdin, c.in, c.rawFd = pts, st.In, int(st.In.Fd())
		}
		if outTTY {
			c.stdout = pts
		}
		if errTTY && sameFile(st.Err, c.term) {
			c.stderr = pts
		}
		switch pts {
		case c.stdin:
			c.ctty = 0
		case c.stdout:
			c.ctty = 1
		default:
			c.ctty = 2
		}
	} else if inTTY {
		c.foreground = true
	}

	// Outputs not covered by the pty get pipes.
	if c.stdout == nil && st.Out != nil {
		r, w, err := os.Pipe()
		if err != nil {
			return fail(err)
		}
		c.stdout = w
		c.childEnds = append(c.childEnds, w)
		c.readers = append(c.readers, r)
		c.copyTo = append(c.copyTo, copyPlan{r, st.Out, proto.FrameStdout})
		if c.stderr == nil && st.Err != nil && !isTerminal(st.Err) && sameFile(st.Out, st.Err) {
			c.stderr = w // 2>&1: one pipe keeps the order exact
		}
	}
	if c.stderr == nil && st.Err != nil {
		if isTerminal(st.Err) {
			c.stderr = st.Err // a second, different terminal: leave it alone
		} else {
			r, w, err := os.Pipe()
			if err != nil {
				return fail(err)
			}
			c.stderr = w
			c.childEnds = append(c.childEnds, w)
			c.readers = append(c.readers, r)
			c.copyTo = append(c.copyTo, copyPlan{r, st.Err, proto.FrameStderr})
		}
	}
	if c.ptm != nil {
		c.readers = append(c.readers, c.ptm)
	}
	return c, nil
}

// sysProcAttr returns how the child must be started.
func (c *outputCapture) sysProcAttr() *syscall.SysProcAttr {
	switch {
	case c.ctty >= 0:
		// Its own session with the pty as controlling terminal: the pty
		// delivers Ctrl-C and window-size changes to the child.
		return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: c.ctty}
	case c.foreground:
		return &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: int(c.stdin.Fd())}
	}
	return &syscall.SysProcAttr{Setpgid: true}
}

// started begins copying once the child runs.
func (c *outputCapture) started() {
	for _, f := range c.childEnds {
		f.Close() // the child has its copies; EOF/EIO comes when it is done
	}
	c.childEnds = nil
	if c.rawFd >= 0 {
		if st, err := term.MakeRaw(c.rawFd); err == nil {
			c.rawState = st
		}
	}
	c.pipeSig = make(chan os.Signal, 1)
	signal.Notify(c.pipeSig, syscall.SIGPIPE)
	for _, p := range c.copyTo {
		c.copiers.Add(1)
		go c.copy(p)
	}
	go func() {
		c.copiers.Wait()
		close(c.copied)
	}()
	if c.rawFd >= 0 {
		go c.forwardInput(c.in, c.ptm)
	}
}

func (c *outputCapture) copy(p copyPlan) {
	defer c.copiers.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := p.from.Read(buf)
		if n > 0 {
			if _, werr := p.to.Write(buf[:n]); werr != nil {
				// The reader went away (`kran run … | head`): stop reading,
				// so the child gets EPIPE/SIGPIPE on its next write exactly
				// as it would have.
				p.from.Close()
				return
			}
			c.push(p.t, buf[:n])
		}
		if err != nil {
			return // EOF (pipe) or EIO (pty after the child closed it)
		}
	}
}

// forwardInput copies keystrokes to the pty. It ends with the job: the
// next read after that fails to write and returns.
func (c *outputCapture) forwardInput(in *os.File, ptm *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if _, werr := ptm.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// resize gives the pty the terminal's new size (SIGWINCH).
func (c *outputCapture) resize() {
	if c.ptm == nil || c.term == nil {
		return
	}
	if ws, err := unix.IoctlGetWinsize(int(c.term.Fd()), unix.TIOCGWINSZ); err == nil {
		_ = unix.IoctlSetWinsize(int(c.ptm.Fd()), unix.TIOCSWINSZ, ws)
	}
}

// finish runs after the child exited: it lets the copiers drain what the
// child wrote (until EOF, at most grace: a background grandchild holding
// the pipe must not keep kran run alive), then restores the terminal.
func (c *outputCapture) finish(grace time.Duration) {
	c.finished.Do(func() {
		select {
		case <-c.copied:
		case <-time.After(grace):
		}
		for _, r := range c.readers {
			r.Close()
		}
		<-c.copied
		if c.rawState != nil {
			_ = term.Restore(c.rawFd, c.rawState)
		}
		if c.pipeSig != nil {
			signal.Stop(c.pipeSig)
		}
	})
}

// abort releases everything when the child could not be started.
func (c *outputCapture) abort() {
	for _, f := range c.childEnds {
		f.Close()
	}
	for _, r := range c.readers {
		r.Close()
	}
	if c.ptm != nil {
		c.ptm.Close()
	}
	c.childEnds, c.readers = nil, nil
}

// copyTerminal gives the pty the real terminal's settings and size. Without
// raw mode the real terminal still post-processes output (\n → \r\n), so
// the pty must not do it a second time.
func copyTerminal(real, pts *os.File, raw bool) error {
	tio, err := unix.IoctlGetTermios(int(real.Fd()), ioctlGetTermios)
	if err != nil {
		return err
	}
	if !raw {
		tio.Oflag &^= unix.OPOST
	}
	if err := unix.IoctlSetTermios(int(pts.Fd()), ioctlSetTermios, tio); err != nil {
		return err
	}
	if ws, err := unix.IoctlGetWinsize(int(real.Fd()), unix.TIOCGWINSZ); err == nil {
		_ = unix.IoctlSetWinsize(int(pts.Fd()), unix.TIOCSWINSZ, ws)
	}
	return nil
}

// sameFile reports whether a and b are the same open file (2>&1, or one
// terminal on all three descriptors).
func sameFile(a, b *os.File) bool {
	if a == nil || b == nil {
		return false
	}
	var sa, sb unix.Stat_t
	if unix.Fstat(int(a.Fd()), &sa) != nil || unix.Fstat(int(b.Fd()), &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino
}
