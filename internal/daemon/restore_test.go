package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestRestoreReleasesSlotOfJobThatEndedMeanwhile: a job that ends between
// the executor's re-index (which marks its slot busy again) and the
// daemon's restore is no longer active, but its slot must still be freed.
// A slot that an active job holds stays busy.
func TestRestoreReleasesSlotOfJobThatEndedMeanwhile(t *testing.T) {
	c := newCluster(t, "b")
	b := c.nodes["b"]
	ended := proto.SlotRef{Node: "b", RepoID: "r1", Index: 0, Path: "/slots/r1/0"}
	b.exec.mu.Lock()
	b.exec.slots["r1/0"] = true
	b.exec.mu.Unlock()
	raw, _ := json.Marshal(map[string]tjobRecord{"0endedjob1": {Origin: "a", Slot: &ended}})
	b.store.Set(context.Background(), targetJobsKey, string(raw))
	b.start()
	waitFor(t, 5*time.Second, "slot of the ended job released", func() bool { return b.exec.heldSlots() == 0 })
}
