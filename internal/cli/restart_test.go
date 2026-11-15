package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/daemon"
	"github.com/hafencity-dev/kran/internal/proto"
)

// TestRestartWindowsLeaveNoGapInvariant: kran run keeps dialing a lost
// daemon for as long as a restart may take plus the time the new daemon
// keeps a restored job for it, so a job is never run outside the daemon's
// books because the two sides gave up on each other at different times.
func TestRestartWindowsLeaveNoGapInvariant(t *testing.T) {
	dw := daemon.DefaultTiming().CLIReattachWait
	if dw != proto.ReattachWait {
		t.Errorf("the daemon waits %s for a restored job's kran run, the shared bound is %s", dw, proto.ReattachWait)
	}
	if c := defaultTiming().Reconnect; c < proto.DaemonStartBound+dw {
		t.Errorf("kran run redials %s, less than a restart (%s) plus the daemon's wait (%s)", c, proto.DaemonStartBound, dw)
	}
	if proto.DaemonStartBound < time.Minute {
		t.Errorf("a restart bound of %s leaves no margin on a loaded machine", proto.DaemonStartBound)
	}
}

// stepClock is a clock the test advances: each After is a step the test
// releases, moving the time by its duration.
type stepClock struct {
	mu    sync.Mutex
	now   time.Time
	steps chan stepWait
}

type stepWait struct {
	d  time.Duration
	ch chan time.Time
}

func newStepClock() *stepClock {
	return &stepClock{now: time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC), steps: make(chan stepWait)}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	go func() { c.steps <- stepWait{d, ch} }()
	return ch
}

// release lets the next pending wait elapse; false when none came.
func (c *stepClock) release(t *testing.T) (time.Duration, bool) {
	t.Helper()
	return c.releaseWithin(5 * time.Second)
}

func (c *stepClock) releaseWithin(limit time.Duration) (time.Duration, bool) {
	select {
	case w := <-c.steps:
		c.mu.Lock()
		c.now = c.now.Add(w.d)
		now := c.now
		c.mu.Unlock()
		w.ch <- now
		return w.d, true
	case <-time.After(limit):
		return 0, false
	}
}

// TestQueuedJobOutlastsASlowRestart: the daemon goes away while the job is
// queued and serves again only 60 s later, well past the old 20 s. kran
// run is still dialing, reattaches by the job's ID and runs the job once,
// never on its own next to the queue.
func TestQueuedJobOutlastsASlowRestart(t *testing.T) {
	s1, s2 := newFakeSession(), newFakeSession()
	cl := &fakeClient{sessions: []*fakeSession{s1}}
	mark := filepath.Join(t.TempDir(), "ran")
	h := newHarness(t, cl, "sh", "-c", "echo x >> "+mark+"; exit 7")
	h.r.tm.Reconnect = defaultTiming().Reconnect
	h.r.tm.RetryEvery = time.Second
	clock := newStepClock()
	h.r.reconnectClock = clock
	go func() {
		waitFor(t, func() bool { return s1.count(proto.FrameSubmit) == 1 })
		// The daemon's first notice names the job (no reason yet).
		s1.push(proto.FrameNotice, proto.Notice{Kind: "queued", JobID: "jq"})
		time.Sleep(20 * time.Millisecond)
		s1.drop()
		elapsed := time.Duration(0)
		for elapsed < 60*time.Second { // the restart takes 60 s: nothing serves
			d, ok := clock.release(t)
			if !ok {
				return // kran run stopped dialing (the test fails below)
			}
			elapsed += d
		}
		cl.mu.Lock()
		cl.sessions = append(cl.sessions, s2)
		cl.mu.Unlock()
		// The next dial finds the new daemon (a pending redial may already
		// have been let go above).
		for s2.count(proto.FrameReattach) == 0 {
			clock.releaseWithin(50 * time.Millisecond)
		}
		s2.push(proto.FrameDecision, proto.Decision{JobID: "jq", Mode: "local", Envelope: &proto.LocalEnvelope{}})
		waitFor(t, func() bool { return s2.count(proto.FrameExit) == 1 })
		s2.tryPush(proto.FrameExit, proto.Exit{JobID: "jq", Code: 7, State: proto.JobFailed})
	}()
	if code := h.run(t); code != 7 {
		t.Fatalf("code %d, stderr %q", code, h.stderr.String())
	}
	if strings.Contains(h.stderr.String(), "running locally") {
		t.Fatalf("ran the queued job outside the daemon's books: %q", h.stderr.String())
	}
	f, _ := s2.find(proto.FrameReattach)
	var ra proto.Reattach
	json.Unmarshal(f.payload, &ra)
	if ra.JobID != "jq" {
		t.Fatalf("reattach %+v", ra)
	}
	if b, _ := os.ReadFile(mark); string(b) != "x\n" {
		t.Fatalf("command ran %q", b)
	}
}

// TestRestartWaitsForQueuedJobs: kran service restart and kran upgrade wait
// while a job is starting, or queued by a kran run that would give up during
// the restart (older than 0.1.12-6); a queued job whose kran run outlasts a
// restart, and a job held for the restart, do not hold it up.
func TestRestartWaitsForQueuedJobs(t *testing.T) {
	newRun := int(proto.ClientReconnect / time.Second)
	job := func(s proto.JobState, reconnect int, held bool) proto.Job {
		j := proto.Job{State: s, Held: held}
		j.Spec.ClientReconnect = reconnect
		return j
	}
	for _, c := range []struct {
		name string
		jobs []proto.Job
		idle bool
		why  string
	}{
		{"nothing", nil, true, ""},
		{"running and done", []proto.Job{job(proto.JobRunning, 0, false), job(proto.JobSucceeded, 0, false)}, true, ""},
		{"queued, new kran run", []proto.Job{job(proto.JobQueued, newRun, false)}, true, ""},
		{"held for the restart", []proto.Job{job(proto.JobQueued, 0, true)}, true, ""},
		{"queued, old kran run", []proto.Job{job(proto.JobQueued, 0, false)}, false, "1 jobs are queued by a kran run older than 0.1.12-6"},
		{"starting and old queued", []proto.Job{job(proto.JobSyncing, newRun, false), job(proto.JobQueued, 0, false), job(proto.JobQueued, 20, false)}, false,
			"1 jobs are starting, 2 jobs are queued by a kran run older than 0.1.12-6"},
	} {
		idle, why, _ := idleReason(&proto.State{Jobs: c.jobs})
		if idle != c.idle || why != c.why {
			t.Errorf("%s: idle %v %q, want %v %q", c.name, idle, why, c.idle, c.why)
		}
	}
}

// TestForceSaysWhatItRisks: --force names, before it restarts, the jobs it
// does not wait for and what happens to them.
func TestForceSaysWhatItRisks(t *testing.T) {
	st := &proto.State{Jobs: []proto.Job{
		{Spec: proto.JobSpec{ID: "qold"}, State: proto.JobQueued},
		{Spec: proto.JobSpec{ID: "qnew", ClientReconnect: int(proto.ClientReconnect / time.Second)}, State: proto.JobQueued},
		{Spec: proto.JobSpec{ID: "sync1"}, State: proto.JobSyncing},
	}}
	got := forceLoss(st)
	for _, want := range []string{"qold", "older than 0.1.12-6", "outside the queue", "sync1 (syncing)", "canceled"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "qnew") {
		t.Errorf("a job that carries over is named: %q", got)
	}
	if got := forceLoss(&proto.State{}); !strings.Contains(got, "no job is at risk") {
		t.Errorf("empty: %q", got)
	}
}

// TestRestartWaitIsBounded: the wait gives up after its bound with the
// reason (stepped clock, no real waits), and ends at once when idle.
func TestRestartWaitIsBounded(t *testing.T) {
	now := time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	start := now
	// A wait that ignores its bound keeps sleeping; the fake clock stops it
	// at an hour so the test fails by itself instead of by go test's timeout.
	sleep := func(_ context.Context, d time.Duration) error {
		if now.Sub(start) > time.Hour {
			return errors.New("waited past an hour on the fake clock")
		}
		now = now.Add(d)
		return nil
	}
	busy := func(context.Context) (bool, string, error) { return false, "1 jobs are starting", nil }
	var said []string
	why, err := waitIdle(context.Background(), busy, clock, sleep, 2*time.Second, restartWait, func(s string) { said = append(said, s) })
	if err == nil || why != "1 jobs are starting" || !strings.Contains(err.Error(), "still busy after 10m0s") || len(said) != 1 {
		t.Fatalf("%q %v %v", why, err, said)
	}
	calls := 0
	drains := func(context.Context) (bool, string, error) {
		calls++
		return calls > 3, "1 jobs are starting", nil
	}
	if _, err := waitIdle(context.Background(), drains, clock, sleep, 2*time.Second, restartWait, func(string) {}); err != nil || calls != 4 {
		t.Fatalf("%v after %d polls", err, calls)
	}
}
