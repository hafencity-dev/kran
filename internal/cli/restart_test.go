package cli

import (
	"encoding/json"
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
// while a job is queued, as they wait while one is starting.
func TestRestartWaitsForQueuedJobs(t *testing.T) {
	for _, c := range []struct {
		states []proto.JobState
		idle   bool
		why    string
	}{
		{nil, true, ""},
		{[]proto.JobState{proto.JobRunning, proto.JobSucceeded}, true, ""},
		{[]proto.JobState{proto.JobQueued}, false, "1 jobs are queued"},
		{[]proto.JobState{proto.JobSyncing, proto.JobQueued, proto.JobQueued}, false, "1 jobs are starting, 2 jobs are queued"},
	} {
		st := &proto.State{}
		for _, s := range c.states {
			st.Jobs = append(st.Jobs, proto.Job{State: s})
		}
		idle, why, _ := idleReason(st)
		if idle != c.idle || why != c.why {
			t.Errorf("%v: idle %v %q, want %v %q", c.states, idle, why, c.idle, c.why)
		}
	}
}
