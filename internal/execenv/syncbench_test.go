package execenv

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// synthManifest returns a manifest of n small files, about a dozen per
// directory, with their blobs stored in m's blob store.
func synthManifest(tb testing.TB, m *manager, repo string, n int) *proto.Manifest {
	tb.Helper()
	mf := &proto.Manifest{RepoID: repo, Root: "/work/kran-test/proj", Keep: []string{"node_modules/"}}
	for i := 0; i < n; i++ {
		c := fmt.Sprintf("file %d content %0100d", i, i)
		h := hashOf(c)
		if err := m.blobs.put(context.Background(), h, int64(len(c)), strings.NewReader(c)); err != nil {
			tb.Fatal(err)
		}
		d := i / 12
		mf.Entries = append(mf.Entries, proto.ManifestEntry{
			Path: fmt.Sprintf("src/pkg%02d/mod%03d/file%05d.txt", d%25, d/25, i),
			Mode: 0o644, Size: int64(len(c)), MTime: 1_600_000_000_000_000_000 + int64(i)*1000, Hash: h,
		})
	}
	return mf
}

// TestVolumeSyncCost measures the target side of an unchanged sync into a
// slot in the Docker volume (the macOS container path): acquire, diff,
// commit, release, also right after a runtime refresh (every few minutes
// in a daemon). Needs Docker; set KRAN_SYNC_COST=1 as well.
func TestVolumeSyncCost(t *testing.T) {
	if os.Getenv("KRAN_SYNC_COST") != "1" {
		t.Skip("set KRAN_SYNC_COST=1 and KRAN_TEST_DOCKER=1 to measure")
	}
	te := newContainerEnv(t, true)
	ctx := context.Background()
	repo := "synthrepo"
	mf := synthManifest(t, te.m, repo, 30000)
	once := func(refresh bool) (acq, diff, commit time.Duration) {
		if refresh {
			te.m.Refresh(ctx)
		}
		t0 := time.Now()
		ref, err := te.m.AcquireSlot(ctx, repo, "/w", proto.RuntimeContainer)
		if err != nil {
			t.Fatal(err)
		}
		t1 := time.Now()
		plan, err := te.m.Diff(ctx, ref, mf)
		if err != nil {
			t.Fatal(err)
		}
		t2 := time.Now()
		if err := te.m.Commit(ctx, ref, mf); err != nil {
			t.Fatal(err)
		}
		t3 := time.Now()
		te.m.ReleaseSlot(ref)
		_ = plan
		return t1.Sub(t0), t2.Sub(t1), t3.Sub(t2)
	}
	a, d, c := once(false)
	t.Logf("cold:            acquire %v diff %v commit %v", a, d, c)
	for i := 0; i < 3; i++ {
		a, d, c := once(false)
		t.Logf("unchanged:       acquire %v diff %v commit %v total %v", a, d, c, a+d+c)
	}
	for i := 0; i < 2; i++ {
		a, d, c := once(true)
		t.Logf("after refresh:   acquire %v diff %v commit %v total %v", a, d, c, a+d+c)
	}
}
