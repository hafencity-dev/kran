package execenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A slot acquired for the first time and not released before the daemon
// restarted (crash, upgrade, origin gone) used to carry a zero last-used
// time, so the reaper at the next start deleted it at once ("unused since
// 0001-01-01") and threw away the warm workspace.
func TestFreshSlotSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	l := layout{state: dir}
	now := time.Now()
	clock := func() time.Time { return now }
	repo := strings.Repeat("ab", 32)

	p := newSlotPool(l, "n", clock)
	s, err := p.acquire(context.Background(), repo, "/w", locHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.hostPath(s), "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The daemon dies here: no release.

	now = now.Add(time.Hour)
	p2 := newSlotPool(l, "n", clock)
	if gone := p2.stale(3 * 24 * time.Hour); len(gone) != 0 {
		t.Fatalf("reaper removed a slot acquired an hour ago: %s/%d", gone[0].repo, gone[0].index)
	}
	now = now.Add(4 * 24 * time.Hour)
	if gone := p2.stale(3 * 24 * time.Hour); len(gone) != 1 {
		t.Fatalf("slot unused for four days kept (%d removed)", len(gone))
	}
}

// Slot metadata without a last-used time (an older kran wrote it at
// acquisition) falls back to the file times instead of counting as
// unused since year one.
func TestSlotWithoutLastUsedUsesFileTimes(t *testing.T) {
	dir := t.TempDir()
	l := layout{state: dir}
	now := time.Now()
	clock := func() time.Time { return now }
	repo := strings.Repeat("cd", 32)
	slots := l.repoSlots(repo)
	for _, idx := range []string{"0", "1"} {
		if err := os.MkdirAll(filepath.Join(slots, idx), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(slots, idx+".json"), []byte(`{"hint":"/w","last_used":"0001-01-01T00:00:00Z"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-5 * 24 * time.Hour)
	for _, f := range []string{filepath.Join(slots, "1"), filepath.Join(slots, "1.json")} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	p := newSlotPool(l, "n", clock)
	gone := p.stale(3 * 24 * time.Hour)
	if len(gone) != 1 || gone[0].index != 1 {
		var idx []int
		for _, s := range gone {
			idx = append(idx, s.index)
		}
		t.Fatalf("removed slots %v, want only the one untouched for five days", idx)
	}
}
