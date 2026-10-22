package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// The daemon stamps QueuedAt with wall time (it is persisted). Queue age
// decides starvation boosts and how long a job may wait for a better node,
// so it must be measured on the monotonic clock: a wall-clock step (NTP,
// a manual change) must neither boost every queued job at once nor delay a
// boost by the size of the step.
func TestQueueAgeUsesMonotonicClock(t *testing.T) {
	s := New(Options{Origin: "gx10", Now: time.Now, Logf: t.Logf}).(*sched)
	s.UpdateNode(self(gx10(time.Now())))
	busy := &proto.Job{Spec: proto.JobSpec{ID: "hog", Argv: []string{"x"}, Owner: proto.Owner{Kind: proto.OwnerHuman, ID: "u"}},
		Estimate: proto.Estimate{CPUs: 1, Mem: 500 * gib, Duration: 60}, QueuedAt: time.Now().UTC().Add(-30 * time.Second)}
	s.Submit(busy)
	s.mu.Lock()
	j := s.jobs["hog"]
	var q time.Time
	if j != nil {
		q = j.queuedAt
	}
	s.mu.Unlock()
	if j == nil {
		t.Fatal("job not queued")
	}
	if !strings.Contains(q.String(), " m=") {
		t.Fatalf("queue time %s has no monotonic reading: age follows wall-clock steps", q)
	}
	if age := time.Since(q); age < 29*time.Second || age > 31*time.Second {
		t.Fatalf("age %s, want about 30s", age)
	}
}

// A QueuedAt in the future (the wall clock went back since) is age zero.
func TestQueueAgeNeverNegative(t *testing.T) {
	clk := &fakeClock{t: t0}
	s := newTestSched(t, clk, self(gx10(t0)))
	s.Submit(&proto.Job{Spec: proto.JobSpec{ID: "j", Argv: []string{"x"}, Owner: proto.Owner{Kind: proto.OwnerHuman, ID: "u"}},
		Estimate: proto.Estimate{CPUs: 1, Mem: 500 * gib, Duration: 60}, QueuedAt: t0.Add(time.Hour)})
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.jobs["j"]; j == nil || j.queuedAt.After(t0) {
		t.Fatalf("queued at %v, now %v", j.queuedAt, t0)
	}
}
