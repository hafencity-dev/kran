package execenv

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Under disk pressure idle slots go, least recently used first, until the
// reserve is back; a slot in use stays.
func TestReclaimSlotsLRU(t *testing.T) {
	te := newTestEnv(t)
	m := te.m
	ctx := context.Background()
	clock := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	m.pool.now = func() time.Time { return clock }
	var refs []proto.SlotRef
	for i := 0; i < 4; i++ {
		ref, err := m.AcquireSlot(ctx, testRepo, "wt"+string(rune('a'+i)), proto.RuntimeNative)
		if err != nil {
			t.Fatal(err)
		}
		te.commit(ref, testRepo, map[string]string{"f": "x"})
		refs = append(refs, ref)
	}
	// Released at different times: slot 2 oldest, then 0; 1 newest; 3 stays busy.
	for _, i := range []int{2, 0, 1} {
		clock = clock.Add(time.Hour)
		m.ReleaseSlot(refs[i])
	}
	m.opts.DiskReserve = 100
	// Each removal frees 40 bytes: from 20 free, two reach the reserve.
	free := int64(20)
	m.reclaimSlots(ctx, locHost, "the disk", func() (int64, int64, error) {
		f := free
		free += 40
		return f, 1000, nil
	})
	var removed []string
	for _, r := range refs {
		if _, err := os.Stat(r.Path); err != nil {
			removed = append(removed, r.Path[len(r.Path)-1:])
		}
	}
	if strings.Join(removed, ",") != "0,2" {
		t.Fatalf("removed slots %v, want the two least recently used (2, then 0)", removed)
	}
	log := te.log.String()
	if !strings.Contains(log, "removing idle slot") {
		t.Errorf("not logged:\n%s", log)
	}
	// Still short: everything idle goes, the busy slot never.
	free = -1000
	m.reclaimSlots(ctx, locHost, "the disk", func() (int64, int64, error) { return free, 1000, nil })
	if _, err := os.Stat(refs[1].Path); err == nil {
		t.Error("idle slot 1 kept under pressure")
	}
	if _, err := os.Stat(refs[3].Path); err != nil {
		t.Errorf("busy slot removed: %v", err)
	}
	if !strings.Contains(te.log.String(), "no idle slot is left") {
		t.Errorf("not logged:\n%s", te.log.String())
	}
}

// Above the floor a slot used within slotReclaimMinIdle stays: the next
// job would sync the whole workspace again. Below the floor it goes.
func TestReclaimSlotsKeepsRecentAboveFloor(t *testing.T) {
	te := newTestEnv(t)
	m := te.m
	ctx := context.Background()
	clock := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	m.pool.now = func() time.Time { return clock }
	var refs []proto.SlotRef
	for i := 0; i < 2; i++ {
		ref, err := m.AcquireSlot(ctx, testRepo, "wt"+string(rune('a'+i)), proto.RuntimeNative)
		if err != nil {
			t.Fatal(err)
		}
		te.commit(ref, testRepo, map[string]string{"f": "x"})
		refs = append(refs, ref)
	}
	m.ReleaseSlot(refs[0])
	clock = clock.Add(time.Hour)
	m.ReleaseSlot(refs[1]) // used just now
	clock = clock.Add(5 * time.Minute)
	m.opts.DiskReserve, m.opts.DiskFloor = 100, 10
	space := func(free int64) func() (int64, int64, error) {
		return func() (int64, int64, error) { return free, 1000, nil }
	}
	m.reclaimSlots(ctx, locHost, "the disk", space(50))
	if _, err := os.Stat(refs[0].Path); err == nil {
		t.Error("slot idle for an hour kept under pressure")
	}
	if _, err := os.Stat(refs[1].Path); err != nil {
		t.Fatalf("slot used 5 minutes ago removed above the floor: %v", err)
	}
	m.reclaimSlots(ctx, locHost, "the disk", space(5))
	if _, err := os.Stat(refs[1].Path); err == nil {
		t.Error("recent slot kept below the floor")
	}
}

func TestParseDF(t *testing.T) {
	free, total, err := parseDF("     Avail        1B-blocks\n10276630528 103865303040\n")
	if err != nil || free != 10276630528 || total != 103865303040 {
		t.Fatalf("%d %d %v", free, total, err)
	}
	if _, _, err := parseDF("garbage"); err == nil {
		t.Error("garbage parsed")
	}
}
