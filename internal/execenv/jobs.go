package execenv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
)

// jobState is the manager's view of one supervised job. Several Process
// handles (Start, later Attach calls) share it.
type jobState struct {
	m    *manager
	id   string
	dir  string
	spec superSpec
	done chan struct{}
	exit proto.Exit

	evMu             sync.Mutex // serializes readEvents and its callbacks
	mu               sync.Mutex
	onPhase          func(proto.Phase)
	evOff            int64
	ready            bool
	running          bool
	runningDelivered bool // OnPhase(running) has returned (readEvents)
	// Two connections to the supervisor: stdin may block for as long as
	// the job does not read it, signals and resizes must never wait
	// behind it.
	ctl       ctlConn
	in        ctlConn
	slot      *slot
	finishing bool
	endedAt   time.Time
}

func (js *jobState) statusPath() string { return filepath.Join(js.dir, fileStatus) }

func (js *jobState) isDone() bool {
	select {
	case <-js.done:
		return true
	default:
		return false
	}
}

// watch follows the supervisor's events and status until the job ends.
func (js *jobState) watch() {
	every := js.m.tun.watchEvery
	lastLiveness := time.Now()
	for {
		js.readEvents()
		if exit, ok := js.readStatus(); ok {
			js.finish(exit)
			return
		}
		js.mu.Lock()
		ready := js.ready
		js.mu.Unlock()
		if ready && time.Since(lastLiveness) >= js.m.tun.livenessEvery {
			lastLiveness = time.Now()
			if !supervisorAlive(js.dir) {
				// It may have written the status just before exiting.
				if exit, ok := js.readStatus(); ok {
					js.finish(exit)
					return
				}
				exit := proto.Exit{JobID: js.id, Node: js.m.opts.NodeName, State: proto.JobInfraErr, Code: proto.ExitInfra,
					Reason: fmt.Sprintf("the job's supervisor on %s ended unexpectedly; output may be incomplete", js.m.opts.NodeName)}
				js.mu.Lock()
				exit.BeforeStart = !js.running
				js.mu.Unlock()
				writeJSONAtomic(js.statusPath(), exit)
				js.finish(exit)
				return
			}
		}
		time.Sleep(every)
	}
}

func (js *jobState) readStatus() (proto.Exit, bool) {
	var exit proto.Exit
	if err := readJSON(js.statusPath(), &exit); err != nil {
		return exit, false
	}
	return exit, true
}

// readEvents delivers new events; evMu serializes callers including the
// phase callbacks, so when readEvents returns every event already in the
// file has been reported.
func (js *jobState) readEvents() {
	js.evMu.Lock()
	defer js.evMu.Unlock()
	f, err := os.Open(filepath.Join(js.dir, fileEvents))
	if err != nil {
		return
	}
	defer f.Close()
	js.mu.Lock()
	off := js.evOff
	js.mu.Unlock()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReader(f)
	var phases []proto.Phase
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial last line is read again next time
		}
		off += int64(len(line))
		var ev event
		if json.Unmarshal(bytes.TrimSpace(line), &ev) != nil {
			continue
		}
		js.mu.Lock()
		switch ev.Ev {
		case "ready":
			js.ready = true
		case "phase":
			if ev.State == proto.JobRunning {
				js.running = true
			}
			phases = append(phases, proto.Phase{State: ev.State, Message: ev.Message})
		}
		js.mu.Unlock()
	}
	js.mu.Lock()
	js.evOff = off
	cb := js.onPhase
	js.mu.Unlock()
	if cb != nil {
		for _, p := range phases {
			cb(p)
		}
	}
	// Only now may output be handed out (Output's beforeFrame): a reader
	// that saw running set by a concurrent readEvents would otherwise pass
	// the first byte on before OnPhase(running) has been delivered.
	js.mu.Lock()
	if js.running {
		js.runningDelivered = true
	}
	js.mu.Unlock()
}

func (js *jobState) finish(exit proto.Exit) {
	js.mu.Lock()
	if js.isDone() || js.finishing {
		js.mu.Unlock()
		return
	}
	js.finishing = true
	js.exit = exit
	js.endedAt = time.Now()
	js.ctl.close()
	js.in.close() // unblocks a stdin write the job never read
	sl := js.slot
	js.mu.Unlock()
	// Free the slot for the next commit before announcing the end: a
	// caller woken by Wait may reuse it right away.
	if sl != nil {
		js.m.pool.setJob(sl, "")
	}
	js.mu.Lock()
	close(js.done)
	js.mu.Unlock()
	js.m.logf("job %s ended: %s (exit %d)", js.id, exit.State, exit.Code)
}

// supervisorAlive reports whether a supervisor holds the job's lock.
func supervisorAlive(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, fileLock), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
	}
	unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false
}

// sendCtl delivers one control frame to the supervisor, reconnecting once.
// ctlConn is one connection to the supervisor's control socket.
type ctlConn struct {
	wmu sync.Mutex // one writer at a time (held during a write)
	mu  sync.Mutex // guards c; never held while writing
	c   net.Conn
}

func (cc *ctlConn) get() net.Conn {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.c
}

func (cc *ctlConn) set(c net.Conn) {
	cc.mu.Lock()
	cc.c = c
	cc.mu.Unlock()
}

func (cc *ctlConn) close() {
	cc.mu.Lock()
	if cc.c != nil {
		cc.c.Close()
		cc.c = nil
	}
	cc.mu.Unlock()
}

func (js *jobState) dialCtl() (net.Conn, error) {
	c, err := net.DialTimeout("unix", js.spec.CtlPath, 2*time.Second)
	if err != nil {
		if js.isDone() {
			return nil, fmt.Errorf("job %s has ended", js.id)
		}
		return nil, fmt.Errorf("job %s control: %w", js.id, err)
	}
	return c, nil
}

// sendCtl delivers a signal or resize. A write that fails (even half way)
// discards the connection, so the next frame starts clean on a new one.
func (js *jobState) sendCtl(t proto.FrameType, payload []byte) error {
	if js.isDone() {
		return fmt.Errorf("job %s has ended", js.id)
	}
	cc := &js.ctl
	cc.wmu.Lock()
	defer cc.wmu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		c := cc.get()
		if c == nil {
			var err error
			if c, err = js.dialCtl(); err != nil {
				return err
			}
			cc.set(c)
		}
		c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		err := proto.WriteFrame(c, t, payload)
		if err == nil {
			return nil
		}
		cc.close()
		if attempt == 1 || js.isDone() {
			return fmt.Errorf("job %s control: %w", js.id, err)
		}
	}
	return nil
}

// sendStdin delivers input on its own connection. It blocks while the job
// does not read (backpressure, no deadline) and never retries: frames on a
// second connection could overtake ones still queued on the first.
func (js *jobState) sendStdin(t proto.FrameType, payload []byte) error {
	if js.isDone() {
		return fmt.Errorf("job %s has ended", js.id)
	}
	cc := &js.in
	cc.wmu.Lock()
	defer cc.wmu.Unlock()
	c := cc.get()
	if c == nil {
		if js.isDone() {
			return fmt.Errorf("job %s has ended", js.id)
		}
		var err error
		if c, err = js.dialCtl(); err != nil {
			return err
		}
		cc.set(c)
	}
	if err := proto.WriteFrame(c, t, payload); err != nil {
		cc.close()
		return fmt.Errorf("job %s stdin: %w", js.id, err)
	}
	return nil
}

// process is a Process handle.
type process struct{ js *jobState }

func (p *process) JobID() string { return p.js.id }

func (p *process) Output(offset int64) io.ReadCloser {
	status := p.js.statusPath()
	r := newSpoolReader(filepath.Join(p.js.dir, fileOut), offset, func() bool {
		_, err := os.Stat(status)
		return err == nil
	})
	// The supervisor writes the running event before any command output,
	// so catching up on events before handing out a frame guarantees
	// OnPhase(running) precedes the command's first byte.
	r.beforeFrame = func() {
		p.js.mu.Lock()
		delivered := p.js.runningDelivered
		p.js.mu.Unlock()
		if !delivered {
			// Reads the running event itself, or waits (evMu) for a
			// concurrent reader to finish delivering it.
			p.js.readEvents()
		}
	}
	return r
}

func (p *process) Stdin() io.WriteCloser { return &stdinWriter{js: p.js} }

// Running reports whether the user's command has started. A daemon that
// re-attached after a restart has no OnPhase callback and asks this.
func (p *process) Running() bool {
	p.js.mu.Lock()
	defer p.js.mu.Unlock()
	return p.js.running
}

func (p *process) Resize(t proto.TTY) error {
	b, _ := json.Marshal(t)
	return p.js.sendCtl(proto.FrameResize, b)
}

func (p *process) Signal(name string) error {
	if _, ok := signalByName(name); !ok {
		return fmt.Errorf("unknown signal %q", name)
	}
	b, _ := json.Marshal(proto.Signal{Name: strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), "SIG"))})
	return p.js.sendCtl(proto.FrameSignal, b)
}

func (p *process) Renew() error {
	if p.js.isDone() {
		return nil
	}
	now := time.Now()
	if err := os.Chtimes(filepath.Join(p.js.dir, fileLease), now, now); err != nil {
		return fmt.Errorf("renew lease of %s: %w", p.js.id, err)
	}
	return nil
}

func (p *process) Live() proto.Resources {
	if p.js.isDone() {
		return proto.Resources{}
	}
	var s liveSample
	if readJSON(filepath.Join(p.js.dir, fileLive), &s) != nil {
		return proto.Resources{}
	}
	return proto.Resources{CPUs: s.CPUs, Mem: s.Mem}
}

func (p *process) Wait(ctx context.Context) (proto.Exit, error) {
	select {
	case <-p.js.done:
		p.js.mu.Lock()
		defer p.js.mu.Unlock()
		return p.js.exit, nil
	case <-ctx.Done():
		return proto.Exit{}, ctx.Err()
	}
}

type stdinWriter struct {
	js     *jobState
	closed bool
}

func (w *stdinWriter) Write(b []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	n := 0
	for len(b) > 0 {
		c := min(len(b), 32<<10)
		if err := w.js.sendStdin(proto.FrameStdin, b[:c]); err != nil {
			return n, err
		}
		n += c
		b = b[c:]
	}
	return n, nil
}

func (w *stdinWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.js.sendStdin(proto.FrameStdinEOF, nil)
}

// ---------------------------------------------------------------------------
// Launching

// ctlPath picks the control socket path; unix socket paths are limited to
// ~104 bytes, which deep state dirs (and macOS temp dirs) exceed.
func ctlPath(dir, id string) string {
	p := filepath.Join(dir, fileCtlSock)
	if len(p) < 100 {
		return p
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("kran-%d", os.Getuid()), hashString(dir)[:16]+".sock")
}

// launch starts `kran internal supervise <dir>` detached from the daemon
// and waits until it reports ready.
func (m *manager) launch(ctx context.Context, dir string, spec *superSpec) error {
	args := []string{"internal", "supervise", dir}
	var cmd *exec.Cmd
	if spec.Cgroup {
		sargs := []string{"--user", "--scope", "--quiet", "--collect",
			"--unit=" + spec.Scope, "--slice=" + m.slice(),
			"--description=kran job " + spec.JobID,
			"-p", "Delegate=yes", "-p", fmt.Sprintf("TasksMax=%d", proto.JobTasksMax)}
		if spec.MemMax > 0 {
			// Backstop in case the supervisor cannot set up the job cgroup;
			// the precise cap lives on the job's own cgroup.
			sargs = append(sargs, "-p", fmt.Sprintf("MemoryMax=%d", spec.MemMax+128<<20))
		}
		sargs = append(sargs, "--", m.opts.Self)
		cmd = exec.Command(m.systemdRun, append(sargs, args...)...)
	} else {
		cmd = exec.Command(m.opts.Self, args...)
	}
	// Own session: no signal from the daemon's terminal or process group
	// reaches the supervisor.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Dir = "/"
	cmd.Env = os.Environ()
	logf, err := os.OpenFile(filepath.Join(dir, fileLog), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("supervisor log: %w", err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	err = cmd.Start()
	logf.Close()
	if err != nil {
		return fmt.Errorf("start supervisor: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait() // reap; the supervisor outlives this only if the daemon dies
		close(exited)
	}()
	evPath := filepath.Join(dir, fileEvents)
	deadline := time.NewTimer(m.tun.readyTimeout)
	defer deadline.Stop()
	for {
		if b, err := os.ReadFile(evPath); err == nil && bytes.Contains(b, []byte(`"ev":"ready"`)) {
			return nil
		}
		select {
		case <-exited:
			if b, err := os.ReadFile(evPath); err == nil && bytes.Contains(b, []byte(`"ev":"ready"`)) {
				return nil // started and already finished
			}
			return fmt.Errorf("supervisor exited during start: %s", tailFile(filepath.Join(dir, fileLog), 400))
		case <-deadline.C:
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			return fmt.Errorf("supervisor did not start within %s: %s", m.tun.readyTimeout, tailFile(filepath.Join(dir, fileLog), 400))
		case <-ctx.Done():
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func tailFile(path string, n int) string {
	b, _ := os.ReadFile(path)
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	if s == "" {
		return "no output"
	}
	return s
}
