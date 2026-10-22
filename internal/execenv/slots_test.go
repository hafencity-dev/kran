package execenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
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

// Diff commits right away when the blob store holds everything the slot
// lacks; with blobs missing it leaves the slot alone until Commit.
func TestDiffCommitsWhenNothingIsMissing(t *testing.T) {
	te := newTestEnv(t)
	ctx := context.Background()
	repo := strings.Repeat("ef", 32)
	ref, err := te.m.AcquireSlot(ctx, repo, "wt", proto.RuntimeNative)
	if err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC).UnixNano()
	mf := &proto.Manifest{RepoID: repo, Entries: []proto.ManifestEntry{
		{Path: "a.txt", Mode: 0o644, Size: 1, MTime: mt, Hash: hashOf("a")},
		{Path: "d/b.txt", Mode: 0o755, Size: 1, MTime: mt, Hash: hashOf("b")},
	}}
	te.m.PutBlob(ctx, hashOf("a"), 1, strings.NewReader("a"))

	plan, err := te.m.Diff(ctx, ref, mf)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Committed || len(plan.Missing) != 1 || plan.Missing[0] != hashOf("b") {
		t.Fatalf("plan with a missing blob: %+v", plan)
	}
	if _, err := os.Stat(filepath.Join(ref.Path, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("Diff wrote into the slot although a blob was missing")
	}
	te.m.PutBlob(ctx, hashOf("b"), 1, strings.NewReader("b"))
	if err := te.m.Commit(ctx, ref, mf); err != nil {
		t.Fatal(err)
	}

	// A job leaves an extra file and edits a synced one: the next Diff of
	// the same manifest repairs both and reports Committed.
	os.WriteFile(filepath.Join(ref.Path, "build.out"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(ref.Path, "a.txt"), []byte("edited"), 0o644)
	plan, err = te.m.Diff(ctx, ref, mf)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Committed || len(plan.Missing) != 0 || plan.Changed != 1 || plan.Deleted != 1 {
		t.Fatalf("plan: %+v", plan)
	}
	if b, _ := os.ReadFile(filepath.Join(ref.Path, "a.txt")); string(b) != "a" {
		t.Fatalf("a.txt = %q after Diff", b)
	}
	if _, err := os.Stat(filepath.Join(ref.Path, "build.out")); !os.IsNotExist(err) {
		t.Fatal("extra file survived")
	}
	// Commit of the same manifest has nothing left to do (an older origin
	// still sends it), even if the slot changed meanwhile.
	os.WriteFile(filepath.Join(ref.Path, "late.txt"), []byte("x"), 0o644)
	if err := te.m.Commit(ctx, ref, mf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ref.Path, "late.txt")); err != nil {
		t.Fatal("Commit after a committing Diff rescanned the slot")
	}
	// Another manifest is committed in full.
	mf2 := &proto.Manifest{RepoID: repo, Entries: mf.Entries[:1]}
	if err := te.m.Commit(ctx, ref, mf2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ref.Path, "d/b.txt")); !os.IsNotExist(err) {
		t.Fatal("Commit of another manifest skipped")
	}
}
