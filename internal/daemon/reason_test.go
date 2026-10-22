package daemon

import (
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// A finished job's record says why it ran where it ran. Sync and setup
// phases are transient: they must not stay behind as its reason.
func TestFinishedJobKeepsPlacementReason(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	b.exec.setup = 50 * time.Millisecond // reports "running setup: fake"
	b.start()
	a.start()
	c.waitOnline("a", "b")
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(20 * time.Second)
	defer cancel()

	for _, script := range []string{"cat f", "exit 3"} {
		res, err := runCLI(ctx, a.sock(), shSpec(ws, script), cliOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if res.exit == nil || res.exit.Node != "b" {
			t.Fatalf("%s: exit %+v", script, res.exit)
		}
		j := storedJob(t, a, res.jobID)
		if j.Reason != "remote test" || j.PlaceReason != "remote test" {
			t.Errorf("%s: %s job recorded reason %q, placement %q; want the placement reason", script, j.State, j.Reason, j.PlaceReason)
		}
		for _, h := range a.daemon().State().Jobs {
			if h.Spec.ID == res.jobID && h.Reason != "remote test" {
				t.Errorf("%s: state shows reason %q", script, h.Reason)
			}
		}
	}

	// A local job too.
	spec := shSpec(ws, "true")
	spec.Req.Prefer = proto.PreferLocal
	res, err := runCLI(ctx, a.sock(), spec, cliOpts{})
	if err != nil || res.exit == nil || res.exit.Code != 0 {
		t.Fatalf("local: %v %+v", err, res.exit)
	}
	if j := storedJob(t, a, res.jobID); j.Reason != "local" || j.PlaceReason != "local" {
		t.Errorf("local job: reason %q, placement %q", j.Reason, j.PlaceReason)
	}
}
