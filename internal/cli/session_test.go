package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// ---- fakes ----------------------------------------------------------------

type sentFrame struct {
	t       proto.FrameType
	payload []byte
}

type fakeSession struct {
	toClient chan sentFrame
	mu       sync.Mutex
	sent     []sentFrame
	closed   bool
	once     sync.Once
}

func newFakeSession() *fakeSession { return &fakeSession{toClient: make(chan sentFrame, 64)} }

func (s *fakeSession) Send(t proto.FrameType, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	s.sent = append(s.sent, sentFrame{t, append([]byte(nil), p...)})
	return nil
}
func (s *fakeSession) SendJSON(t proto.FrameType, v any) error {
	b, _ := json.Marshal(v)
	return s.Send(t, b)
}
func (s *fakeSession) Recv() (proto.FrameType, []byte, error) {
	f, ok := <-s.toClient
	if !ok {
		return 0, nil, io.EOF
	}
	return f.t, f.payload, nil
}
func (s *fakeSession) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.toClient)
	})
	return nil
}

// push delivers a frame from the "daemon".
func (s *fakeSession) push(t proto.FrameType, v any) {
	var b []byte
	switch x := v.(type) {
	case []byte:
		b = x
	case string:
		b = []byte(x)
	case nil:
	default:
		b, _ = json.Marshal(v)
	}
	s.toClient <- sentFrame{t, b}
}

func (s *fakeSession) drop() { s.once.Do(func() { close(s.toClient) }) }

// tryPush is push for a session the CLI may have closed already.
func (s *fakeSession) tryPush(t proto.FrameType, v any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	b, _ := v.([]byte)
	if v != nil && b == nil {
		b, _ = json.Marshal(v)
	}
	s.toClient <- sentFrame{t, b}
	return true
}

func (s *fakeSession) sentTypes() []proto.FrameType {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []proto.FrameType
	for _, f := range s.sent {
		out = append(out, f.t)
	}
	return out
}

func (s *fakeSession) find(t proto.FrameType) (sentFrame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.sent {
		if f.t == t {
			return f, true
		}
	}
	return sentFrame{}, false
}

func (s *fakeSession) count(t proto.FrameType) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.sent {
		if f.t == t {
			n++
		}
	}
	return n
}

type fakeClient struct {
	sessions []*fakeSession
	mu       sync.Mutex
	runCalls int
	version  proto.VersionInfo
	verErr   error
}

func (c *fakeClient) next() (*fakeSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runCalls >= len(c.sessions) {
		return nil, false
	}
	s := c.sessions[c.runCalls]
	c.runCalls++
	return s, true
}

func (c *fakeClient) Version(context.Context) (proto.VersionInfo, error) { return c.version, c.verErr }
func (c *fakeClient) State(context.Context) (*proto.State, error)        { return nil, errors.New("n/a") }
func (c *fakeClient) Events(context.Context) (<-chan proto.Event, error) {
	return nil, errors.New("n/a")
}
func (c *fakeClient) Jobs(context.Context, int) ([]proto.Job, error)     { return nil, nil }
func (c *fakeClient) Job(context.Context, string) (*proto.Job, error)    { return nil, nil }
func (c *fakeClient) Log(context.Context, string, bool, io.Writer) error { return nil }
func (c *fakeClient) Cancel(context.Context, string) error               { return nil }
func (c *fakeClient) SetPriority(context.Context, string, proto.Priority) error {
	return nil
}
func (c *fakeClient) Drain(context.Context, string) error  { return nil }
func (c *fakeClient) Resume(context.Context, string) error { return nil }
func (c *fakeClient) Close() error                         { return nil }
func (c *fakeClient) Run(context.Context) (client.RunSession, error) {
	s, ok := c.next()
	if !ok {
		return nil, errors.New("no more sessions")
	}
	return s, nil
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	r      *runner
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	sigs   chan os.Signal
	cl     *fakeClient
	stdin  *os.File
	stdinW *os.File
}

func newHarness(t *testing.T, cl *fakeClient, argv ...string) *harness {
	t.Helper()
	h := &harness{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, sigs: make(chan os.Signal, 8), cl: cl}
	if cl != nil && cl.version.API == "" {
		cl.version.API = proto.APIVersion
	}
	outF, errF := tmpFile(t, "lout"), tmpFile(t, "lerr")
	h.r = &runner{
		spec:   proto.JobSpec{Argv: argv, Cwd: t.TempDir()},
		env:    os.Environ(),
		stdout: h.stdout, stderr: &lineWriter{w: h.stderr},
		stdio: localStdio{Out: outF, Err: errF}, signals: h.sigs,
		tm:    timing{NoticeDelay: 80 * time.Millisecond, Heartbeat: 20 * time.Millisecond, Reconnect: 600 * time.Millisecond, DoubleInt: time.Second, RetryEvery: 10 * time.Millisecond},
		start: startLocal, probe: func(int64) bool { return false },
	}
	h.r.dial = func(ctx context.Context) (client.Client, error) {
		if h.cl == nil {
			return nil, client.ErrNoDaemon
		}
		return h.cl, nil
	}
	return h
}

func (h *harness) run(t *testing.T) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- h.r.Run(context.Background()) }()
	select {
	case c := <-done:
		return c
	case <-time.After(15 * time.Second):
		t.Fatal("run did not finish")
		return -1
	}
}

func localOut(t *testing.T, h *harness) string {
	b, _ := os.ReadFile(h.r.stdio.Out.Name())
	return string(b)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

// ---- tests -----------------------------------------------------------------

func TestRemoteRunPassthroughAndNotices(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "go", "test")
	go func() {
		waitFor(t, func() bool { return s.count(proto.FrameSubmit) == 1 })
		s.push(proto.FrameNotice, proto.Notice{Kind: "queued", Message: "waiting: needs 6 GiB, devmac has 3.1 GiB free"})
		time.Sleep(250 * time.Millisecond) // longer than NoticeDelay: must be printed
		s.push(proto.FrameDecision, proto.Decision{JobID: "j1", Mode: "remote", Node: "devmac", Runtime: proto.RuntimeContainer, Reason: "22 cores free"})
		s.push(proto.FramePhase, proto.Phase{State: proto.JobSyncing, Message: "sending 3 files"})
		s.push(proto.FramePhase, proto.Phase{State: proto.JobRunning})
		s.push(proto.FrameStdout, []byte("he\x00llo\r\n"))
		s.push(proto.FrameStderr, []byte("warn no newline"))
		s.push(proto.FrameStdout, []byte{0xff, 0xfe})
		s.push(proto.FrameExit, proto.Exit{JobID: "j1", Code: 3, State: proto.JobFailed, Node: "devmac"})
	}()
	code := h.run(t)
	if code != 3 {
		t.Fatalf("code %d", code)
	}
	if h.stdout.String() != "he\x00llo\r\n\xff\xfe" {
		t.Fatalf("stdout not byte-exact: %q", h.stdout.String())
	}
	se := h.stderr.String()
	if !strings.Contains(se, "kran: waiting: needs 6 GiB") || !strings.Contains(se, "kran: running on devmac (container) — 22 cores free") {
		t.Fatalf("stderr: %q", se)
	}
	if strings.Contains(se, "syncing") {
		t.Fatalf("short phases must stay silent: %q", se)
	}
	if !strings.Contains(se, "warn no newline") {
		t.Fatal("program stderr lost")
	}
}

// A wait whose only change is a live number ("3.1 GiB free" -> "3.0 GiB free")
// is one reason, so it is said once; a different reason is said again.
func TestWaitingLineIsNotRepeatedForChangingNumbers(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "true")
	go func() {
		waitFor(t, func() bool { return s.count(proto.FrameSubmit) == 1 })
		s.push(proto.FrameNotice, proto.Notice{Kind: "queued", Message: "waiting: needs 6 GiB, devmac has 3.1 GiB free"})
		time.Sleep(250 * time.Millisecond)
		s.push(proto.FrameNotice, proto.Notice{Kind: "queued", Message: "waiting: needs 6 GiB, devmac has 3.0 GiB free"})
		time.Sleep(50 * time.Millisecond)
		s.push(proto.FrameNotice, proto.Notice{Kind: "queued", Message: "waiting: devmac is draining"})
		time.Sleep(50 * time.Millisecond)
		s.push(proto.FrameExit, proto.Exit{JobID: "j1", Code: 0, State: proto.JobSucceeded})
	}()
	if code := h.run(t); code != 0 {
		t.Fatal(code)
	}
	se := h.stderr.String()
	if n := strings.Count(se, "needs 6 GiB"); n != 1 || !strings.Contains(se, "kran: waiting: devmac is draining") {
		t.Fatalf("stderr:\n%s", se)
	}
}

func TestQuickQueueStaysSilent(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "x")
	go func() {
		s.push(proto.FrameNotice, proto.Notice{Kind: "queued", Message: "waiting briefly"})
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n"})
		s.push(proto.FrameExit, proto.Exit{JobID: "j", Code: 0, State: proto.JobSucceeded})
	}()
	if code := h.run(t); code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(h.stderr.String(), "waiting briefly") {
		t.Fatalf("%q", h.stderr.String())
	}
}

// TestLongQuietQueueKeepsWaiting: the daemon's heartbeats never reach the
// runner (client.RunSession swallows them), so a long wait without other
// frames must not look like a dead daemon.
func TestLongQuietQueueKeepsWaiting(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "sh", "-c", "printf %s \"$KRAN_JOB_ID\"")
	h.r.flags.Quiet = true
	go func() {
		s.push(proto.FrameNotice, proto.Notice{Kind: "queued", Message: "waiting: a is at its limit of 1 jobs"})
		time.Sleep(1500 * time.Millisecond) // many heartbeat ticks without a frame
		s.push(proto.FrameDecision, proto.Decision{JobID: "jq", Mode: "local", Envelope: &proto.LocalEnvelope{Env: map[string]string{"KRAN_JOB_ID": "jq"}}})
	}()
	if code := h.run(t); code != 0 {
		t.Fatal(code)
	}
	if localOut(t, h) != "jq" {
		t.Fatalf("ran without the daemon's decision: %q", localOut(t, h))
	}
}

func TestQuietSuppressesNotices(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "x")
	h.r.flags.Quiet = true
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n", Reason: "r"})
		s.push(proto.FrameExit, proto.Exit{JobID: "j", Code: 0, State: proto.JobSucceeded})
	}()
	h.run(t)
	if h.stderr.Len() != 0 {
		t.Fatalf("%q", h.stderr.String())
	}
}

func TestExitReasonAndInfraCode(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "x")
	h.r.flags.Quiet = true
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n"})
		s.push(proto.FrameExit, proto.Exit{JobID: "j", Code: 0, State: proto.JobLost, Reason: "lost contact with devmac"})
	}()
	if code := h.run(t); code != 125 {
		t.Fatal(code)
	}
	if !strings.Contains(h.stderr.String(), "kran: lost contact with devmac") {
		t.Fatalf("reasons print even with --quiet: %q", h.stderr.String())
	}
}

func TestLocalDecisionRunsCommand(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "sh", "-c", "printf %s \"$KRAN_JOB_ID\"; exit 4")
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "jobl", Mode: "local", Envelope: &proto.LocalEnvelope{Env: map[string]string{"KRAN_JOB_ID": "jobl"}}})
	}()
	if code := h.run(t); code != 4 {
		t.Fatal(code)
	}
	if localOut(t, h) != "jobl" {
		t.Fatal(localOut(t, h))
	}
	if _, ok := s.find(proto.FrameStarted); !ok {
		t.Fatal("started frame not sent")
	}
	f, ok := s.find(proto.FrameExit)
	var ex proto.Exit
	json.Unmarshal(f.payload, &ex)
	if !ok || ex.Code != 4 || ex.JobID != "jobl" || ex.State != proto.JobFailed {
		t.Fatalf("%+v", ex)
	}
}

func TestNoDaemonFallsBackLocally(t *testing.T) {
	h := newHarness(t, nil, "sh", "-c", "exit 5")
	if code := h.run(t); code != 5 {
		t.Fatal(code)
	}
	if !strings.Contains(h.stderr.String(), "kran: daemon not running — running locally (start it: kran service start)") {
		t.Fatalf("%q", h.stderr.String())
	}
}

func TestNoDaemonNoFallback(t *testing.T) {
	h := newHarness(t, nil, "sh", "-c", "exit 5")
	h.r.spec.NoFallback = true
	if code := h.run(t); code != 125 {
		t.Fatal(code)
	}
	if want := "kran: daemon not running; --no-fallback forbids running here (start it: kran service start)\n"; h.stderr.String() != want {
		t.Fatalf("%q, want %q", h.stderr.String(), want)
	}
}

func TestNoDaemonJSONEventsSaysItOnce(t *testing.T) {
	h := newHarness(t, nil, "true")
	h.r.flags.JSONEvents = true
	if code := h.run(t); code != 0 {
		t.Fatal(code)
	}
	if n := strings.Count(h.stderr.String(), "daemon not running"); n != 1 {
		t.Fatalf("the fallback reason appears %d times:\n%s", n, h.stderr.String())
	}
}

func TestIncompatibleDaemonFallsBack(t *testing.T) {
	h := newHarness(t, &fakeClient{version: proto.VersionInfo{API: "v9", Version: "9.0"}}, "true")
	h.r.flags.JSONEvents = true
	if code := h.run(t); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(h.stderr.String(), `"kran":"fallback"`) {
		t.Fatalf("%q", h.stderr.String())
	}
}

func TestErrorBeforeStartFallsBackAfterStartFails(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "sh", "-c", "exit 0")
	go func() { s.push(proto.FrameError, proto.APIError{Error: "scheduler crashed", Hint: "kran doctor"}) }()
	if code := h.run(t); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(h.stderr.String(), "scheduler crashed (kran doctor) — running locally") {
		t.Fatalf("%q", h.stderr.String())
	}

	s2 := newFakeSession()
	h2 := newHarness(t, &fakeClient{sessions: []*fakeSession{s2}}, "x")
	go func() {
		s2.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n"})
		s2.push(proto.FramePhase, proto.Phase{State: proto.JobRunning})
		s2.push(proto.FrameError, proto.APIError{Error: "peer died"})
	}()
	if code := h2.run(t); code != 125 {
		t.Fatal(code)
	}
	if !strings.Contains(h2.stderr.String(), "kran: peer died") {
		t.Fatalf("%q", h2.stderr.String())
	}
}

func TestRemoteFallbackAfterRemoteDecision(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "sh", "-c", "exit 2")
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "devmac"})
		s.push(proto.FrameNotice, proto.Notice{Kind: "fallback", Message: "devmac unreachable — running locally"})
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "local", Envelope: &proto.LocalEnvelope{}})
	}()
	if code := h.run(t); code != 2 {
		t.Fatal(code)
	}
	f, _ := s.find(proto.FrameExit)
	var ex proto.Exit
	json.Unmarshal(f.payload, &ex)
	if !ex.Fallback {
		t.Fatalf("exit must be marked fallback: %+v", ex)
	}
	if !strings.Contains(h.stderr.String(), "devmac unreachable") {
		t.Fatal(h.stderr.String())
	}
}

func TestDoubleCtrlCEscalates(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "x")
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n"})
		s.push(proto.FramePhase, proto.Phase{State: proto.JobRunning})
		time.Sleep(100 * time.Millisecond)
		h.sigs <- syscall.SIGINT
		waitFor(t, func() bool { return s.count(proto.FrameSignal) == 1 })
		h.sigs <- syscall.SIGINT
		waitFor(t, func() bool { return s.count(proto.FrameSignal) == 2 })
		h.sigs <- syscall.SIGTERM
		waitFor(t, func() bool { return s.count(proto.FrameSignal) == 3 })
		s.push(proto.FrameExit, proto.Exit{JobID: "j", Code: 137, Signal: "KILL", State: proto.JobFailed})
	}()
	if code := h.run(t); code != 137 {
		t.Fatal(code)
	}
	var names []string
	s.mu.Lock()
	for _, f := range s.sent {
		if f.t == proto.FrameSignal {
			var sg proto.Signal
			json.Unmarshal(f.payload, &sg)
			names = append(names, sg.Name)
		}
	}
	s.mu.Unlock()
	if strings.Join(names, ",") != "INT,KILL,TERM" {
		t.Fatalf("%v", names)
	}
}

func TestCtrlCWhileQueuedCancels(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "x")
	go func() {
		waitFor(t, func() bool { return s.count(proto.FrameSubmit) == 1 })
		s.push(proto.FrameNotice, proto.Notice{Kind: "queued", Message: "waiting"})
		time.Sleep(20 * time.Millisecond)
		h.sigs <- syscall.SIGINT
	}()
	if code := h.run(t); code != 130 {
		t.Fatal(code)
	}
}

func TestLocalSignalForwarded(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "sh", "-c", "trap 'exit 9' INT; echo ready; while :; do sleep 0.05; done")
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "local", Envelope: &proto.LocalEnvelope{}})
		waitFor(t, func() bool { return strings.Contains(localOut(t, h), "ready") })
		h.sigs <- syscall.SIGINT
	}()
	if code := h.run(t); code != 9 {
		t.Fatal(code)
	}
}

func TestStdinForwarding(t *testing.T) {
	pr, pw, _ := os.Pipe()
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "cat")
	h.r.flags.Stdin = true
	h.r.stdin = pr
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n"})
		s.push(proto.FramePhase, proto.Phase{State: proto.JobRunning}) // input is read once the command runs
		pw.Write([]byte("hello"))
		waitFor(t, func() bool { return s.count(proto.FrameStdin) >= 1 })
		pw.Close()
		waitFor(t, func() bool { return s.count(proto.FrameStdinEOF) == 1 })
		s.push(proto.FrameExit, proto.Exit{JobID: "j", Code: 0, State: proto.JobSucceeded})
	}()
	if code := h.run(t); code != 0 {
		t.Fatal(code)
	}
	f, _ := s.find(proto.FrameStdin)
	if string(f.payload) != "hello" {
		t.Fatalf("%q", f.payload)
	}
}

// TestStdinKeptForFallback: a remote attempt that fails before the command
// runs must leave stdin untouched for the local run that follows.
func TestStdinKeptForFallback(t *testing.T) {
	pr, pw, _ := os.Pipe()
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "sh", "-c", "cat")
	h.r.flags.Stdin = true
	h.r.stdin = pr
	h.r.stdio.In = pr
	pw.Write([]byte("all of it"))
	pw.Close()
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n"})
		s.push(proto.FramePhase, proto.Phase{State: proto.JobSetup})
		time.Sleep(100 * time.Millisecond)
		s.push(proto.FrameNotice, proto.Notice{Kind: "fallback", Message: "n failed before the job started"})
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "local", Envelope: &proto.LocalEnvelope{}})
	}()
	if code := h.run(t); code != 0 {
		t.Fatal(code)
	}
	if out := localOut(t, h); out != "all of it" {
		t.Fatalf("local fallback got %q of stdin", out)
	}
	if s.count(proto.FrameStdin) != 0 {
		t.Fatal("stdin was forwarded to a job that never ran")
	}
}

func TestStdinNotConsumedForLocalPlacement(t *testing.T) {
	pr, pw, _ := os.Pipe()
	defer pw.Close()
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "true")
	h.r.flags.Stdin = true
	h.r.stdin = pr
	go s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "local", Envelope: &proto.LocalEnvelope{}})
	h.run(t)
	if s.count(proto.FrameStdin)+s.count(proto.FrameStdinEOF) != 0 {
		t.Fatal("stdin must stay with the local child")
	}
}

func TestReattachAfterDrop(t *testing.T) {
	s1, s2 := newFakeSession(), newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s1, s2}}, "x")
	go func() {
		s1.push(proto.FrameDecision, proto.Decision{JobID: "jr", Mode: "remote", Node: "n"})
		s1.push(proto.FramePhase, proto.Phase{State: proto.JobRunning})
		s1.push(proto.FrameStdout, []byte("12345"))
		s1.push(proto.FrameStderr, []byte("67"))
		time.Sleep(100 * time.Millisecond) // let the loop consume the frames
		s1.drop()
		waitFor(t, func() bool { return s2.count(proto.FrameReattach) == 1 })
		s2.push(proto.FrameStdout, []byte("89"))
		s2.push(proto.FrameExit, proto.Exit{JobID: "jr", Code: 0, State: proto.JobSucceeded})
	}()
	if code := h.run(t); code != 0 {
		t.Fatal(code, h.stderr.String())
	}
	f, _ := s2.find(proto.FrameReattach)
	var ra proto.Reattach
	json.Unmarshal(f.payload, &ra)
	if ra.JobID != "jr" || ra.Offset != 7 {
		t.Fatalf("%+v", ra)
	}
	if h.stdout.String() != "1234589" {
		t.Fatal(h.stdout.String())
	}
}

// TestLocalExitReportedAfterReconnect: a local job that ends while the
// daemon restarts has its exit reported once the daemon is back.
func TestLocalExitReportedAfterReconnect(t *testing.T) {
	s1, s2 := newFakeSession(), newFakeSession()
	cl := &fakeClient{sessions: []*fakeSession{s1}}
	h := newHarness(t, cl, "sh", "-c", "sleep 0.2; exit 3")
	go func() {
		s1.push(proto.FrameDecision, proto.Decision{JobID: "jl", Mode: "local", Envelope: &proto.LocalEnvelope{}})
		waitFor(t, func() bool { return s1.count(proto.FrameStarted) == 1 })
		s1.drop()                          // the daemon goes away
		time.Sleep(400 * time.Millisecond) // the job ends meanwhile
		cl.mu.Lock()
		cl.sessions = append(cl.sessions, s2) // and the daemon is back
		cl.mu.Unlock()
	}()
	if code := h.run(t); code != 3 {
		t.Fatal(code)
	}
	types := s2.sentTypes()
	if len(types) < 2 || types[0] != proto.FrameReattach || types[len(types)-1] != proto.FrameExit {
		t.Fatalf("frames after reconnect: %v", types)
	}
	f, _ := s2.find(proto.FrameExit)
	var ex proto.Exit
	json.Unmarshal(f.payload, &ex)
	if ex.Code != 3 || ex.JobID != "jl" {
		t.Fatalf("%+v", ex)
	}
}

// TestLocalExitConfirmed: kran run waits for the daemon to record a
// local job's exit; a daemon that dies before that gets it again once it
// is back, and a confirming one ends the wait at once.
func TestLocalExitConfirmed(t *testing.T) {
	t.Run("daemon dies before recording", func(t *testing.T) {
		s1, s2 := newFakeSession(), newFakeSession()
		cl := &fakeClient{sessions: []*fakeSession{s1}}
		h := newHarness(t, cl, "sh", "-c", "exit 3")
		h.r.tm.ExitConfirm = 5 * time.Second
		go func() {
			s1.push(proto.FrameDecision, proto.Decision{JobID: "jc", Mode: "local", Envelope: &proto.LocalEnvelope{}})
			waitFor(t, func() bool { return s1.count(proto.FrameExit) == 1 })
			cl.mu.Lock()
			cl.sessions = append(cl.sessions, s2)
			cl.mu.Unlock()
			s1.drop() // the frame was never read
		}()
		if code := h.run(t); code != 3 {
			t.Fatal(code)
		}
		types := s2.sentTypes()
		if len(types) < 2 || types[0] != proto.FrameReattach || types[len(types)-1] != proto.FrameExit {
			t.Fatalf("frames after reconnect: %v", types)
		}
	})
	t.Run("daemon confirms", func(t *testing.T) {
		s := newFakeSession()
		h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "sh", "-c", "exit 4")
		h.r.tm.ExitConfirm = 10 * time.Second
		go func() {
			s.push(proto.FrameDecision, proto.Decision{JobID: "jd", Mode: "local", Envelope: &proto.LocalEnvelope{}})
			waitFor(t, func() bool { return s.count(proto.FrameExit) == 1 })
			s.push(proto.FrameExit, proto.Exit{JobID: "jd", Code: 4, State: proto.JobFailed})
		}()
		start := time.Now()
		if code := h.run(t); code != 4 {
			t.Fatal(code)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("waited %s for a confirmed exit", d)
		}
	})
}

func TestReconnectGivesUpWith125(t *testing.T) {
	s1 := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s1}}, "x")
	go func() {
		s1.push(proto.FrameDecision, proto.Decision{JobID: "jr", Mode: "remote", Node: "devmac"})
		s1.push(proto.FramePhase, proto.Phase{State: proto.JobRunning})
		time.Sleep(20 * time.Millisecond)
		s1.drop()
	}()
	if code := h.run(t); code != 125 {
		t.Fatal(code)
	}
	if !strings.Contains(h.stderr.String(), "job jr may still be running on devmac") {
		t.Fatalf("%q", h.stderr.String())
	}
}

// TestDropDuringRemoteSetupReattaches: the daemon goes away while the job
// is set up on its target (a restart, a graceful stop). The job may still
// start there, so kran run reattaches instead of running it here too.
func TestDropDuringRemoteSetupReattaches(t *testing.T) {
	for _, ph := range []proto.JobState{proto.JobSyncing, proto.JobSetup} {
		mark := filepath.Join(t.TempDir(), "ran-here")
		s1, s2 := newFakeSession(), newFakeSession()
		h := newHarness(t, &fakeClient{sessions: []*fakeSession{s1, s2}}, "sh", "-c", "touch "+mark+"; exit 6")
		go func() {
			s1.push(proto.FrameDecision, proto.Decision{JobID: "js", Mode: "remote", Node: "n"})
			s1.push(proto.FramePhase, proto.Phase{State: ph})
			time.Sleep(50 * time.Millisecond)
			s1.drop()
			waitFor(t, func() bool { return s2.count(proto.FrameReattach) == 1 })
			s2.push(proto.FramePhase, proto.Phase{State: proto.JobRunning})
			s2.push(proto.FrameStdout, []byte("remote"))
			s2.push(proto.FrameExit, proto.Exit{JobID: "js", Code: 5, State: proto.JobFailed})
		}()
		if code := h.run(t); code != 5 || h.stdout.String() != "remote" {
			t.Fatalf("%s: code %d stdout %q stderr %q", ph, code, h.stdout.String(), h.stderr.String())
		}
		if _, err := os.Stat(mark); err == nil {
			t.Fatalf("%s: the command ran here as well", ph)
		}
	}
}

// TestDropDuringRemoteSetupNoDaemonExits125: when the daemon does not come
// back, kran run cannot know whether the job started on its target, so it
// does not run it here: exit 125 with what to check.
func TestDropDuringRemoteSetupNoDaemonExits125(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "ran-here")
	s1 := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s1}}, "sh", "-c", "touch "+mark+"; exit 6")
	go func() {
		s1.push(proto.FrameDecision, proto.Decision{JobID: "js", Mode: "remote", Node: "n"})
		s1.push(proto.FramePhase, proto.Phase{State: proto.JobSetup})
		time.Sleep(50 * time.Millisecond)
		s1.drop()
	}()
	if code := h.run(t); code != proto.ExitInfra {
		t.Fatalf("code %d, stderr %q", code, h.stderr.String())
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the command ran here although it may run on n")
	}
	if !strings.Contains(h.stderr.String(), "job js may still start on n") {
		t.Fatalf("%q", h.stderr.String())
	}
}

// TestErrorAfterRemoteDecision: after a remote decision kran run runs the
// command itself only when the daemon says nothing ran anywhere.
func TestErrorAfterRemoteDecision(t *testing.T) {
	for _, runLocally := range []bool{false, true} {
		mark := filepath.Join(t.TempDir(), "ran-here")
		s := newFakeSession()
		h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "sh", "-c", "touch "+mark+"; exit 6")
		go func() {
			s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n"})
			s.push(proto.FramePhase, proto.Phase{State: proto.JobSetup})
			s.push(proto.FrameError, proto.APIError{Error: "job j is not running on this daemon", RunLocally: runLocally})
		}()
		code := h.run(t)
		_, err := os.Stat(mark)
		ranHere := err == nil
		if runLocally && (code != 6 || !ranHere) {
			t.Fatalf("run_locally: code %d ran here %v, stderr %q", code, ranHere, h.stderr.String())
		}
		if !runLocally && (code != proto.ExitInfra || ranHere) {
			t.Fatalf("plain error: code %d ran here %v, stderr %q", code, ranHere, h.stderr.String())
		}
	}
}

func TestDropBeforeDecisionFallsBack(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "sh", "-c", "exit 6")
	go func() {
		waitFor(t, func() bool { return s.count(proto.FrameSubmit) == 1 })
		s.drop()
	}()
	if code := h.run(t); code != 6 {
		t.Fatal(code)
	}
}

func TestJSONEvents(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "x")
	h.r.flags.JSONEvents = true
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "devmac", Reason: "fast"})
		s.push(proto.FramePhase, proto.Phase{State: proto.JobSyncing, Message: "2 files"})
		s.push(proto.FrameStderr, []byte("partial"))
		s.push(proto.FrameExit, proto.Exit{JobID: "j", Code: 1, State: proto.JobFailed, Node: "devmac"})
	}()
	h.run(t)
	var kinds []string
	for _, l := range strings.Split(strings.TrimSpace(h.stderr.String()), "\n") {
		if !strings.HasPrefix(l, `{"kran":`) {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad json line %q", l)
		}
		kinds = append(kinds, m["kran"].(string))
	}
	if strings.Join(kinds, ",") != "decision,phase,exit" {
		t.Fatalf("%v\n%s", kinds, h.stderr.String())
	}
	// A kran line must never glue onto program stderr that lacks a newline.
	if !strings.Contains(h.stderr.String(), "partial\n{\"kran\":\"exit\"") {
		t.Fatalf("%q", h.stderr.String())
	}
}

func TestLaunchFailureCodes(t *testing.T) {
	h := newHarness(t, nil, "no-such-command-kran-xyz")
	if code := h.run(t); code != 127 {
		t.Fatal(code)
	}
	if !strings.Contains(h.stderr.String(), "command not found") {
		t.Fatal(h.stderr.String())
	}
}

// slowWriter blocks its first write, like a pager nobody scrolls or a full
// pipe to a slow consumer.
type slowWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	delay time.Duration
	once  sync.Once
}

func (w *slowWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { time.Sleep(w.delay) })
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestSlowStdoutIsNotADeadDaemon(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "x")
	out := &slowWriter{delay: 1500 * time.Millisecond} // longer than DeadAfter
	h.r.stdout = out
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n"})
		s.push(proto.FramePhase, proto.Phase{State: proto.JobRunning})
		s.push(proto.FrameStdout, []byte("first"))
		// The daemon keeps the session alive meanwhile (its heartbeats are
		// swallowed by the client); the job ends a little after the write.
		time.Sleep(1700 * time.Millisecond)
		s.tryPush(proto.FrameStdout, []byte(" second"))
		s.tryPush(proto.FrameExit, proto.Exit{JobID: "j", Code: 0, State: proto.JobSucceeded})
	}()
	if code := h.run(t); code != 0 {
		t.Fatalf("code %d: %s", code, h.stderr.String())
	}
	if got := out.buf.String(); got != "first second" {
		t.Fatalf("stdout %q", got)
	}
	if strings.Contains(h.stderr.String(), "lost the connection") {
		t.Fatalf("a slow stdout reader was taken for a dead daemon: %q", h.stderr.String())
	}
}

// A remote job with a terminal reads from its pty. Without -i nothing ever
// arrives there: the job must see end of input instead of waiting forever
// (a prompt would hang the run).
func TestRemoteTTYWithoutStdinGetsEOF(t *testing.T) {
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "x")
	h.r.spec.TTY = &proto.TTY{Rows: 24, Cols: 80}
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n"})
		waitFor(t, func() bool { return s.count(proto.FrameStdinEOF) == 1 })
		s.push(proto.FrameExit, proto.Exit{JobID: "j", Code: 0, State: proto.JobSucceeded})
	}()
	done := make(chan int, 1)
	go func() { done <- h.r.Run(context.Background()) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal(code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no stdin EOF for a remote terminal job without -i")
	}
}

func TestJobTTYNeedsStdoutAndStderrTerminals(t *testing.T) {
	size := func() *proto.TTY { return &proto.TTY{Rows: 1, Cols: 2} }
	if jobTTY(true, true, size) == nil {
		t.Fatal("both terminals: want a TTY")
	}
	// kran run make 2>err.log: a pty would merge stderr into the terminal.
	if jobTTY(true, false, size) != nil || jobTTY(false, true, size) != nil {
		t.Fatal("a redirected stream must keep its own pipe")
	}
}

// Output before the running phase is setup output: the command has not
// started. When the daemon later says nothing ran (a restarted daemon that
// found the job stopped in its setup), kran run runs it here.
func TestSetupOutputIsNotAStart(t *testing.T) {
	s1, s2 := newFakeSession(), newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s1, s2}}, "sh", "-c", "exit 6")
	go func() {
		s1.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n"})
		s1.push(proto.FramePhase, proto.Phase{State: proto.JobSetup, Message: "running setup: pnpm install"})
		s1.push(proto.FrameStdout, []byte("Packages: +12\n"))
		time.Sleep(50 * time.Millisecond)
		s1.drop()
		waitFor(t, func() bool { return s2.count(proto.FrameReattach) == 1 })
		s2.push(proto.FrameError, proto.APIError{Error: "n: setup failed", RunLocally: true})
	}()
	if code := h.run(t); code != 6 {
		t.Fatalf("code %d: %s", code, h.stderr.String())
	}
}

// After the daemon restarted, it no longer knows the local child's process
// group: the reattaching CLI must report it again, or a later CLI death
// would leave the child running (SPEC §6.1 step 5).
func TestLocalReattachReportsStartedAgain(t *testing.T) {
	s1, s2 := newFakeSession(), newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s1, s2}}, "sh", "-c", "sleep 0.5")
	go func() {
		s1.push(proto.FrameDecision, proto.Decision{JobID: "jl", Mode: "local", Envelope: &proto.LocalEnvelope{}})
		waitFor(t, func() bool { return s1.count(proto.FrameStarted) == 1 })
		s1.drop()
	}()
	if code := h.run(t); code != 0 {
		t.Fatal(code, h.stderr.String())
	}
	types := s2.sentTypes()
	if len(types) < 2 || types[0] != proto.FrameReattach || types[1] != proto.FrameStarted {
		t.Fatalf("after reattach sent %v", types)
	}
	f, _ := s2.find(proto.FrameStarted)
	var st proto.Started
	json.Unmarshal(f.payload, &st)
	if st.PGID == 0 || st.JobID != "jl" {
		t.Fatalf("%+v", st)
	}
}

// .kran.toml env.set applies wherever the job runs (same result as remote).
func TestLocalJobGetsEnvSet(t *testing.T) {
	r := &runner{env: []string{"CI=0", "A=1"}, envSet: map[string]string{"CI": "1", "B": "2"}}
	got := r.localJob(nil).Env
	want := map[string]int{"CI=1": 1, "A=1": 1, "B=2": 1}
	seen := map[string]int{}
	for _, kv := range got {
		seen[kv]++
		if strings.HasPrefix(kv, "CI=") && kv != "CI=1" {
			t.Fatalf("stale %s in %v", kv, got)
		}
	}
	for k, n := range want {
		if seen[k] != n {
			t.Fatalf("env %v", got)
		}
	}
}
