package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
)

// teeSession records what the tee sends; gate (when set) blocks every Send
// until closed, like a daemon that stopped reading.
type teeSession struct {
	*fakeSession
	gate chan struct{}
	fail bool
}

func (s *teeSession) Send(t proto.FrameType, p []byte) error {
	if s.gate != nil {
		<-s.gate
	}
	if s.fail {
		return io.ErrClosedPipe
	}
	return s.fakeSession.Send(t, p)
}

func (s *teeSession) history() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, f := range s.sent {
		if f.t == proto.FrameStdout || f.t == proto.FrameStderr {
			b.Write(f.payload)
		}
	}
	return b.String()
}

func TestHistoryTeeDeliversInOrder(t *testing.T) {
	s := &teeSession{fakeSession: newFakeSession()}
	tee := newHistoryTee(1 << 20)
	defer tee.stop()
	tee.setSession(s)
	tee.push(proto.FrameStdout, []byte("one "))
	tee.push(proto.FrameStderr, []byte("two "))
	tee.push(proto.FrameStdout, bytes.Repeat([]byte("x"), 3*teeChunkMax+5))
	if !tee.flush(10 * time.Second) {
		t.Fatal("flush timed out")
	}
	want := "one two " + strings.Repeat("x", 3*teeChunkMax+5)
	if got := s.history(); got != want {
		t.Fatalf("history has %d bytes, want %d", len(got), len(want))
	}
	types := s.sentTypes()
	if types[0] != proto.FrameStdout || types[1] != proto.FrameStderr || len(types) != 2+4 {
		t.Fatalf("frames %v", types)
	}
}

// A daemon that stops reading never slows the producer: pushes return at
// once, the excess is dropped, and the history says so.
func TestHistoryTeeNeverBlocksAndMarksDrops(t *testing.T) {
	s := &teeSession{fakeSession: newFakeSession(), gate: make(chan struct{})}
	tee := newHistoryTee(64 << 10)
	defer tee.stop()
	tee.setSession(s)
	chunk := bytes.Repeat([]byte("y"), 1000)
	start := time.Now()
	for i := 0; i < 10000; i++ { // 10 MB into a 64 KiB queue
		tee.push(proto.FrameStdout, chunk)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("pushing took %s: the producer was slowed down", el)
	}
	close(s.gate)
	if !tee.flush(10 * time.Second) {
		t.Fatal("flush timed out")
	}
	h := s.history()
	if !strings.HasSuffix(h, "\n"+historyTruncated+"\n") || len(h) > 70<<10 {
		t.Fatalf("history (%d bytes) ends %q", len(h), h[max(0, len(h)-60):])
	}
}

// Without a session (reconnecting) output waits, bounded, and goes out
// once a session is back; a failed send is marked.
func TestHistoryTeeWaitsForASessionAndMarksLostSends(t *testing.T) {
	tee := newHistoryTee(1 << 20)
	defer tee.stop()
	tee.push(proto.FrameStdout, []byte("early\n"))
	if tee.flush(50 * time.Millisecond) {
		t.Fatal("flush without a session cannot succeed")
	}
	broken := &teeSession{fakeSession: newFakeSession(), fail: true}
	tee.setSession(broken)
	tee.flush(time.Second) // the chunk is lost on the broken session
	good := &teeSession{fakeSession: newFakeSession()}
	tee.setSession(good)
	tee.push(proto.FrameStdout, []byte("late\n"))
	if !tee.flush(10 * time.Second) {
		t.Fatal("flush timed out")
	}
	if got := good.history(); got != historyTruncated+"\nlate\n" {
		t.Fatalf("history %q", got)
	}
}

func TestHistoryTeeStopReportsLoss(t *testing.T) {
	s := &teeSession{fakeSession: newFakeSession(), gate: make(chan struct{})}
	defer close(s.gate)
	tee := newHistoryTee(1 << 20)
	tee.setSession(s)
	tee.push(proto.FrameStdout, []byte("a"))
	tee.push(proto.FrameStdout, []byte("b"))
	if !tee.stop() {
		t.Fatal("stop with queued output must report an incomplete history")
	}
	tee.push(proto.FrameStdout, []byte("c")) // after stop: ignored, no panic
}

// collect is a Tee that keeps everything per stream.
type collect struct {
	mu  sync.Mutex
	out map[proto.FrameType]*bytes.Buffer
	all bytes.Buffer
}

func newCollect() *collect {
	return &collect{out: map[proto.FrameType]*bytes.Buffer{proto.FrameStdout: {}, proto.FrameStderr: {}}}
}

func (c *collect) push(t proto.FrameType, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out[t].Write(b)
	c.all.Write(b)
}

func (c *collect) get(t proto.FrameType) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out[t].String()
}

func runCaptured(t *testing.T, st localStdio, tee func(proto.FrameType, []byte), argv ...string) proto.Exit {
	t.Helper()
	p, lerr := startLocal(localJob{JobID: "t1", Argv: argv, Dir: t.TempDir(), Env: os.Environ(), Stdio: st, Tee: tee})
	if lerr != nil {
		t.Fatal(lerr)
	}
	select {
	case ex := <-p.Done():
		return ex
	case <-time.After(30 * time.Second):
		t.Fatal("job did not end")
		return proto.Exit{}
	}
}

func TestCaptureToFilesIsByteExact(t *testing.T) {
	out, errf := tmpFile(t, "out"), tmpFile(t, "err")
	c := newCollect()
	ex := runCaptured(t, localStdio{Out: out, Err: errf}, c.push, "sh", "-c", `printf 'a\0b\r\n'; printf 'warn' >&2; printf '\377'; exit 3`)
	if ex.Code != 3 {
		t.Fatalf("exit %+v", ex)
	}
	o, _ := os.ReadFile(out.Name())
	e, _ := os.ReadFile(errf.Name())
	if string(o) != "a\x00b\r\n\xff" || string(e) != "warn" {
		t.Fatalf("files: out %q err %q", o, e)
	}
	if c.get(proto.FrameStdout) != string(o) || c.get(proto.FrameStderr) != string(e) {
		t.Fatalf("history: out %q err %q", c.get(proto.FrameStdout), c.get(proto.FrameStderr))
	}
}

// 2>&1 into one file: the interleaving must stay exactly as the child wrote.
func TestCaptureKeepsOrderWhenStdoutAndStderrAreOneFile(t *testing.T) {
	f := tmpFile(t, "both")
	c := newCollect()
	runCaptured(t, localStdio{Out: f, Err: f}, c.push, "sh", "-c", `for i in 1 2 3 4 5; do printf "o$i "; printf "e$i " >&2; done`)
	b, _ := os.ReadFile(f.Name())
	want := "o1 e1 o2 e2 o3 e3 o4 e4 o5 e5 "
	if string(b) != want || c.all.String() != want {
		t.Fatalf("file %q history %q", b, c.all.String())
	}
}

// A grandchild that keeps the pipe open must not keep kran run waiting.
func TestCaptureDoesNotWaitForBackgroundHolders(t *testing.T) {
	out := tmpFile(t, "out")
	start := time.Now()
	runCaptured(t, localStdio{Out: out, Err: out}, func(proto.FrameType, []byte) {}, "sh", "-c", "echo hi; sleep 30 &")
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("waited %s for a background process", el)
	}
	b, _ := os.ReadFile(out.Name())
	if string(b) != "hi\n" {
		t.Fatalf("out %q", b)
	}
}

// A terminal on stdin/stdout: the child sees a terminal, keystrokes reach
// it, its output reaches the terminal and the history, and the terminal
// settings are restored afterwards.
func TestCaptureOnATerminal(t *testing.T) {
	ptm, tty, err := pty.Open()
	if err != nil {
		t.Skip("no pty: ", err)
	}
	defer ptm.Close()
	defer tty.Close()
	pty.Setsize(ptm, &pty.Winsize{Rows: 33, Cols: 101})
	before, err := unix.IoctlGetTermios(int(tty.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	var screen bytes.Buffer
	var smu sync.Mutex
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptm.Read(buf)
			smu.Lock()
			screen.Write(buf[:n])
			smu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	c := newCollect()
	done := make(chan proto.Exit, 1)
	p, lerr := startLocal(localJob{JobID: "t1", Argv: []string{"sh", "-c",
		`test -t 0 && test -t 1 && test -t 2 && echo tty; stty size; read line; echo "got $line"`},
		Dir: t.TempDir(), Env: os.Environ(), Stdio: localStdio{In: tty, Out: tty, Err: tty}, Tee: c.push})
	if lerr != nil {
		t.Fatal(lerr)
	}
	go func() { done <- <-p.Done() }()
	// Type once the child is waiting for input.
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(c.get(proto.FrameStdout), "33 101") {
		if time.Now().After(deadline) {
			t.Fatalf("child output so far %q", c.get(proto.FrameStdout))
		}
		time.Sleep(10 * time.Millisecond)
	}
	ptm.Write([]byte("hello\r"))
	var ex proto.Exit
	select {
	case ex = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("job did not end")
	}
	if ex.Code != 0 {
		t.Fatalf("exit %+v", ex)
	}
	hist := c.get(proto.FrameStdout)
	if !strings.Contains(hist, "tty\r\n") || !strings.Contains(hist, "got hello") {
		t.Fatalf("history %q", hist)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		smu.Lock()
		s := screen.String()
		smu.Unlock()
		if strings.Contains(s, "got hello") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal shows %q", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, err := unix.IoctlGetTermios(int(tty.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	if *after != *before {
		t.Fatal("terminal settings were not restored")
	}
}

// Output to a terminal without a terminal on stdin: no raw mode, and the
// bytes reaching the terminal are the child's own (the real terminal does
// the \n → \r\n translation, as without kran).
func TestCaptureTerminalOutputWithPipedInput(t *testing.T) {
	ptm, tty, err := pty.Open()
	if err != nil {
		t.Skip("no pty: ", err)
	}
	defer ptm.Close()
	defer tty.Close()
	before, _ := unix.IoctlGetTermios(int(tty.Fd()), ioctlGetTermios)
	c := newCollect()
	ex := runCaptured(t, localStdio{Out: tty, Err: tty}, c.push, "sh", "-c", `test -t 1 && printf 'tty\nline2\n'`)
	if ex.Code != 0 {
		t.Fatalf("exit %+v", ex)
	}
	if got := c.get(proto.FrameStdout); got != "tty\nline2\n" {
		t.Fatalf("history %q (want the child's bytes unchanged)", got)
	}
	ptm.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	var screen []byte
	for !bytes.Contains(screen, []byte("line2\r\n")) {
		n, err := ptm.Read(buf)
		screen = append(screen, buf[:n]...)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				break
			}
			break
		}
	}
	if string(screen) != "tty\r\nline2\r\n" {
		t.Fatalf("terminal got %q", screen)
	}
	after, _ := unix.IoctlGetTermios(int(tty.Fd()), ioctlGetTermios)
	if *after != *before {
		t.Fatal("terminal settings changed")
	}
}

// A local job's output goes to the daemon for the history: after started,
// before exit, and the terminal (here: files) gets it unchanged.
func TestLocalJobOutputGoesToTheDaemon(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "sh", "-c", "echo hello; echo oops >&2; exit 2")
	go s.push(proto.FrameDecision, proto.Decision{JobID: "jl", Mode: "local", Envelope: &proto.LocalEnvelope{}})
	if code := h.run(t); code != 2 {
		t.Fatal(code)
	}
	if localOut(t, h) != "hello\n" {
		t.Fatalf("terminal got %q", localOut(t, h))
	}
	types := s.sentTypes()
	started, exit := -1, -1
	var out, errOut strings.Builder
	s.mu.Lock()
	for i, f := range s.sent {
		switch f.t {
		case proto.FrameStarted:
			started = i
		case proto.FrameExit:
			exit = i
		case proto.FrameStdout:
			if started < 0 || exit >= 0 {
				t.Errorf("output frame at %d outside started..exit: %v", i, types)
			}
			out.Write(f.payload)
		case proto.FrameStderr:
			errOut.Write(f.payload)
		}
	}
	s.mu.Unlock()
	if out.String() != "hello\n" || errOut.String() != "oops\n" || exit != len(types)-1 {
		t.Fatalf("frames %v: stdout %q stderr %q", types, out.String(), errOut.String())
	}
}
