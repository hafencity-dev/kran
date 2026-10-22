package execenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestCacheUnitDepth(t *testing.T) {
	for _, tc := range []struct {
		tools bool
		path  string
		want  string // the unit, "" = never deleted
	}{
		{false, "go-build/ab/abcdef-d", "go-build/ab/abcdef-d"},
		{false, "go-build/trim.txt", ""},
		{false, "gomod/github.com/x/y@v1.2.3/go.mod", "gomod/github.com/x/y@v1.2.3"},
		{false, "gomod/golang.org/x/sys@v0.1.0", "gomod/golang.org/x/sys@v0.1.0"},
		{false, "gomod/cache/download/github.com/x/y/@v/v1.2.3.zip", "gomod/cache/download/github.com/x/y/@v/v1.2.3.zip"},
		{false, "gomod/cache/download/github.com/x/y/@v/list", ""},
		{false, "gomod/cache/download/github.com/x/y/@v/v1.2.3.lock", ""},
		{false, "gomod/cache/vcs/0123abcd/HEAD", "gomod/cache/vcs/0123abcd"},
		{false, "pnpm-store/v10/files/00/abc", "pnpm-store/v10"},
		{false, "npm/_cacache/index-v5/x", "npm/_cacache"},
		{false, "uv/archive-v0/x", "uv"},
		{false, "pip/http/x", "pip"},
		{false, "mise/node/x.tar.gz", "mise/node"},
		{false, "cargo/bin/cargo-nextest", ""},
		{false, "cargo/registry/cache/index.crates.io-1/serde-1.0.0.crate", "cargo/registry/cache/index.crates.io-1/serde-1.0.0.crate"},
		{false, "cargo/registry/src/index.crates.io-1/serde-1.0.0/src/lib.rs", "cargo/registry/src/index.crates.io-1/serde-1.0.0"},
		{false, "cargo/git/checkouts/foo-123/abc/file", "cargo/git/checkouts/foo-123"},
		{false, "home/.cache/ms-playwright/chromium-1/chrome", "home/.cache/ms-playwright"},
		{false, "home/.npm/_cacache/x", "home/.npm/_cacache"},
		{false, "home/.gitconfig", ""},
		{false, "home/.ssh/known_hosts", ""},
		{false, "home/project/notes.txt", ""},
		{false, "mise-state/x", ""},
		{false, ".kran-lock", ""},
		{false, "somethingelse/x", ""},
		{true, "mise/installs/node/24.8.0/bin/node", "mise/installs/node/24.8.0"},
		{true, "mise/installs/node/24", "mise/installs/node/24"},
		{true, "mise/downloads/go/1.25.1/go.tar.gz", "mise/downloads/go/1.25.1"},
		{true, "mise/shims/node", ""},
		{true, "bin/kran-sync", ""},
	} {
		parts := strings.Split(tc.path, "/")
		d := cacheUnitDepth(tc.tools, parts)
		got := ""
		if d > 0 {
			got = strings.Join(parts[:d], "/")
		}
		if got != tc.want {
			t.Errorf("%s (tools=%v): unit %q, want %q", tc.path, tc.tools, got, tc.want)
		}
	}
}

// cacheFixture writes a file of size bytes last used at t.
func cacheFixture(tb testing.TB, root, rel string, size int, at time.Time) {
	tb.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		tb.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		tb.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestTrimCaches(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	setup := func(t *testing.T) (string, string) {
		base := t.TempDir()
		cache, tools := filepath.Join(base, "cache"), filepath.Join(base, "tools")
		cacheFixture(t, cache, cacheLockFile, 0, now)
		cacheFixture(t, cache, "go-build/aa/old-d", 1<<20, now.Add(-9*day))
		cacheFixture(t, cache, "go-build/bb/new-d", 1<<20, now.Add(-time.Minute))
		cacheFixture(t, cache, "npm/_cacache/index", 1<<20, now.Add(-5*day))
		cacheFixture(t, cache, "home/.gitconfig", 1<<20, now.Add(-30*day)) // never a cache
		cacheFixture(t, cache, "home/.cache/pw/chrome", 1<<20, now.Add(-3*day))
		cacheFixture(t, cache, "gomod/example.com/m@v1.0.0/go.mod", 1<<20, now.Add(-8*day))
		// Go makes extracted modules read-only.
		os.Chmod(filepath.Join(cache, "gomod/example.com/m@v1.0.0"), 0o555)
		cacheFixture(t, tools, "mise/installs/node/20.1.0/bin/node", 1<<20, now.Add(-7*day))
		cacheFixture(t, tools, "mise/installs/node/24.8.0/bin/node", 1<<20, now.Add(-2*time.Minute))
		os.Symlink("20.1.0", filepath.Join(tools, "mise/installs/node/20"))
		os.Symlink("24.8.0", filepath.Join(tools, "mise/installs/node/24"))
		// A trim interrupted earlier left its trash.
		cacheFixture(t, cache, cacheTrashDir+"/abc/x", 1<<20, now)
		t.Cleanup(func() { removeAllForce(base) })
		return cache, tools
	}
	disk := func(free, total int64) func(string) (int64, int64, error) {
		return func(string) (int64, int64, error) { return free, total, nil }
	}
	opts := func(cache, tools string, budget int64, free int64) cacheTrimOpts {
		return cacheTrimOpts{Cache: cache, Tools: tools, Budget: budget, Floor: 10 * gib, Recent: time.Hour,
			Now: func() time.Time { return now }, DiskSpace: disk(free, 100*gib)}
	}

	t.Run("under-budget", func(t *testing.T) {
		cache, tools := setup(t)
		pruned := false
		o := opts(cache, tools, 100<<20, 50*gib)
		o.Prune = func(context.Context, string, string) { pruned = true }
		rep, err := trimCaches(context.Background(), o)
		if err != nil || rep.Removed != 0 || pruned || rep.Used < 8<<20 {
			t.Fatalf("%+v %v pruned=%v", rep, err, pruned)
		}
		if exists(filepath.Join(cache, cacheTrashDir, "abc")) {
			t.Error("trash left over")
		}
	})

	t.Run("over-budget-lru", func(t *testing.T) {
		cache, tools := setup(t)
		pruned := false
		o := opts(cache, tools, 5<<20, 50*gib) // 8 MiB of files plus directories
		o.Prune = func(context.Context, string, string) { pruned = true }
		rep, err := trimCaches(context.Background(), o)
		if err != nil || !pruned {
			t.Fatalf("%+v %v", rep, err)
		}
		// Down to 80% of the budget, oldest first: go-build/aa (9d), the
		// module (8d), node 20 (7d), npm (5d), …; what was used within
		// the hour stays.
		for _, gone := range []string{"go-build/aa/old-d", "gomod/example.com/m@v1.0.0", "npm/_cacache"} {
			if exists(filepath.Join(cache, gone)) {
				t.Errorf("%s kept", gone)
			}
		}
		if exists(filepath.Join(tools, "mise/installs/node/20.1.0")) || exists(filepath.Join(tools, "mise/installs/node/20")) {
			t.Error("node 20.1.0 or its alias kept")
		}
		for _, kept := range []string{"home/.gitconfig", "go-build/bb/new-d", cacheLockFile} {
			if !exists(filepath.Join(cache, kept)) {
				t.Errorf("%s removed", kept)
			}
		}
		if !exists(filepath.Join(tools, "mise/installs/node/24")) {
			t.Error("node 24 alias removed")
		}
		if rep.Removed < 4 || rep.Reclaimed < 4<<20 || rep.After > rep.Budget || rep.Used-rep.After != rep.Reclaimed {
			t.Errorf("report %+v", rep)
		}
	})

	t.Run("recent-entries-stay", func(t *testing.T) {
		cache, tools := setup(t)
		rep, err := trimCaches(context.Background(), opts(cache, tools, 2<<20, 50*gib))
		if err != nil || rep.Skipped != "" {
			t.Fatalf("%+v %v", rep, err)
		}
		if !exists(filepath.Join(cache, "go-build/bb/new-d")) || !exists(filepath.Join(tools, "mise/installs/node/24.8.0")) {
			t.Error("entries used within the hour were removed")
		}
		if exists(filepath.Join(cache, "home/.cache/pw")) {
			t.Error("3-day-old home cache kept although over budget")
		}
		if !exists(filepath.Join(cache, "home/.gitconfig")) {
			t.Error("home file removed")
		}
		_ = rep
	})

	// What kran may not delete (a file in the home) is over budget by
	// itself: emptying the caches would not help.
	t.Run("not-trimmable", func(t *testing.T) {
		cache, tools := setup(t)
		rep, err := trimCaches(context.Background(), opts(cache, tools, 1<<20, 50*gib))
		if err != nil || rep.Removed != 0 || !strings.Contains(rep.Skipped, "not cache entries") {
			t.Fatalf("%+v %v", rep, err)
		}
		if !exists(filepath.Join(cache, "go-build/aa/old-d")) {
			t.Error("cache entry removed")
		}
	})

	// Within budget, but the disk keeps less free than its reserve (25 GiB
	// of 100): trim what it lacks, keeping what was used just now.
	t.Run("below-reserve", func(t *testing.T) {
		cache, tools := setup(t)
		rep, err := trimCaches(context.Background(), opts(cache, tools, 100*gib, 25*gib-(3<<20)))
		if err != nil || rep.Removed == 0 || rep.Reserve != 25*gib || rep.Reclaimed < 3<<20 {
			t.Fatalf("%+v %v", rep, err)
		}
		if exists(filepath.Join(cache, "go-build/aa/old-d")) || !exists(filepath.Join(cache, "go-build/bb/new-d")) {
			t.Error("not least recently used first, or a recent entry went above the floor")
		}
	})

	t.Run("disk-below-floor", func(t *testing.T) {
		cache, tools := setup(t)
		// Within budget, but the Docker disk has less free than its floor:
		// trim what the floor lacks, recent entries included.
		rep, err := trimCaches(context.Background(), opts(cache, tools, 100*gib, 10*gib-(20<<20)))
		if err != nil || rep.Removed == 0 {
			t.Fatalf("%+v %v", rep, err)
		}
		if exists(filepath.Join(cache, "go-build/bb/new-d")) {
			t.Error("disk short: recent entries should go too")
		}
		if !exists(filepath.Join(cache, "home/.gitconfig")) {
			t.Error("home file removed")
		}
	})
}

// A container job that starts stops a running cache trim first, and no
// trim starts until the job is tracked.
func TestHoldCachesStopsTrim(t *testing.T) {
	te := newTestEnv(t)
	m := te.m
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { <-ctx.Done(); close(done) }()
	m.ct.mu.Lock()
	m.ct.running, m.ct.done = cancel, done
	m.ct.mu.Unlock()
	release := m.holdCaches(context.Background())
	select {
	case <-done:
	default:
		t.Fatal("trim not stopped")
	}
	m.ct.mu.Lock()
	m.ct.running, m.ct.done = nil, nil
	holds := m.ct.holds
	m.ct.mu.Unlock()
	if holds != 1 {
		t.Fatalf("holds %d", holds)
	}
	m.tun.cacheTrimEvery = time.Hour
	m.maybeTrimCaches(context.Background()) // no Docker here: returns at once
	release()
	m.ct.mu.Lock()
	defer m.ct.mu.Unlock()
	if m.ct.holds != 0 || m.ct.running != nil {
		t.Fatalf("after release: holds %d running %v", m.ct.holds, m.ct.running != nil)
	}
}

// Container jobs are refused while the Docker disk (as the last trim
// measured it) is below the floor, and the next reaper pass trims.
func TestDockerDiskFloor(t *testing.T) {
	te := newTestEnv(t, func(o *Options, _ *tunables) { o.DiskFloor = 10 * gib })
	m := te.m
	if err := m.checkDockerDiskFloor(); err != nil {
		t.Fatalf("no measurement yet: %v", err)
	}
	m.ct.mu.Lock()
	m.ct.diskFree, m.ct.diskTotal, m.ct.diskAt, m.ct.last = 4*gib, 100*gib, m.now(), m.now()
	m.ct.mu.Unlock()
	err := m.checkDockerDiskFloor()
	if !errors.Is(err, ErrDiskFloor) || !strings.Contains(err.Error(), "Docker disk") {
		t.Fatalf("low disk: %v", err)
	}
	if _, err := m.AcquireSlot(context.Background(), testRepo, "wt", proto.RuntimeContainer); !errors.Is(err, ErrDiskFloor) {
		t.Fatalf("container slot below the floor: %v", err)
	}
	m.ct.mu.Lock()
	due := m.ct.last.IsZero()
	m.ct.diskFree = 50 * gib
	m.ct.mu.Unlock()
	if !due {
		t.Error("no trim requested")
	}
	if err := m.checkDockerDiskFloor(); err != nil {
		t.Fatalf("enough disk: %v", err)
	}
}
