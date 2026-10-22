package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/scheduler"
)

// preferInOrder places on the first online, not yet rejected node of order,
// else locally.
func preferInOrder(order ...string) placeFunc {
	return func(job *proto.Job, nodes map[string]proto.NodeState, rejected map[string]bool) (scheduler.Placement, string, bool) {
		for _, n := range order {
			if st, ok := nodes[n]; ok && st.Status == proto.NodeOnline && !rejected[n] {
				return scheduler.Placement{Node: n, Runtime: proto.RuntimeNative, Reason: "test order"}, "", true
			}
		}
		return scheduler.Placement{Reason: "fallback", Fallback: len(rejected) > 0}, "", true
	}
}

// A pre-start failure on one peer moves the job to the next peer within the
// same run session; the failed peer keeps nothing.
func TestPrestartFailureMovesToAnotherPeer(t *testing.T) {
	c := newCluster(t, "a", "b", "c")
	a, b, cn := c.nodes["a"], c.nodes["b"], c.nodes["c"]
	a.place = preferInOrder("b", "c")
	b.exec.mu.Lock()
	b.exec.startErr = errors.New("runtime broken")
	b.exec.mu.Unlock()
	b.start()
	cn.start()
	a.start()
	c.waitOnline("a", "b")
	c.waitOnline("a", "c")
	ws := workspaceDir(t, map[string]string{"in": "data\n"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "cat in; exit 3"), cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == nil || res.exit.Code != 3 || res.exit.Node != "c" || res.out() != "data\n" {
		t.Fatalf("exit %+v out %q", res.exit, res.out())
	}
	if len(res.decisions) != 2 || res.decisions[0].Node != "b" || res.decisions[1].Node != "c" || res.decisions[1].Mode != "remote" {
		t.Fatalf("decisions %+v", res.decisions)
	}
	found := false
	for _, n := range res.notices {
		if n.Kind == "fallback" && strings.Contains(n.Message, "trying c") {
			found = true
		}
	}
	if !found {
		t.Fatalf("notices %+v", res.notices)
	}
	waitFor(t, 3*time.Second, "b and c clean", func() bool {
		return b.exec.heldSlots() == 0 && cn.exec.heldSlots() == 0 &&
			len(b.daemon().target.status().Allocs) == 0 && len(cn.daemon().target.status().Allocs) == 0
	})
	waitFor(t, time.Second, "origin allocation released", func() bool { return a.scheduler().allocCount() == 0 })
	j := storedJob(t, a, res.jobID)
	if j.Node != "c" || j.State != proto.JobFailed || j.Fallback {
		t.Fatalf("stored %+v", j)
	}
}

// Each peer's clock offset is measured over the live session: on one
// machine both clocks are the same, so it must come out near zero (the age
// of the last metrics sample would not).
func TestPeerClockOffsetMeasured(t *testing.T) {
	_, a, _ := twoNodes(t)
	var link proto.LinkInfo
	waitFor(t, 5*time.Second, "clock measured", func() bool {
		for _, n := range a.daemon().State().Nodes {
			if n.Info.Name == "b" {
				link = n.Link
			}
		}
		return link.ClockRTTMillis > 0
	})
	if link.ClockOffsetMillis < -100 || link.ClockOffsetMillis > 100 || link.ClockRTTMillis > 1000 {
		t.Fatalf("offset %.1f ms, rtt %.1f ms", link.ClockOffsetMillis, link.ClockRTTMillis)
	}
}
