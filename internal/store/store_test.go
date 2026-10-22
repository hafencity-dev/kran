package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func openTest(t *testing.T) (Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "kran.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func job(id string, state proto.JobState, owner, node string, queued time.Time) *proto.Job {
	j := &proto.Job{
		Spec: proto.JobSpec{
			ID:    id,
			Argv:  []string{"go", "test", "./..."},
			Owner: proto.Owner{Kind: proto.OwnerClaude, ID: owner, Name: "claude · " + owner},
		},
		State:       state,
		Node:        node,
		Origin:      "gx10",
		Fingerprint: "fp-" + id,
		QueuedAt:    queued,
	}
	if state.Terminal() {
		j.EndedAt = queued.Add(time.Minute)
		code := 0
		j.ExitCode = &code
	}
	return j
}

func TestOpenCreatesPrivateFileAndWAL(t *testing.T) {
	st, path := openTest(t)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("db mode = %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
	}
	var mode string
	if err := st.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var timeout int
	if err := st.DB().QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout < 1000 {
		t.Errorf("busy_timeout = %d", timeout)
	}
}

func TestReopenKeepsDataAndSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kran.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.PutJob(ctx, job("a1", proto.JobRunning, "s1", "devmac", t0)); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	j, err := st.GetJob(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != proto.JobRunning || j.Node != "devmac" {
		t.Errorf("got %+v", j)
	}
	var v int
	st.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != len(migrations) {
		t.Errorf("version = %d, want %d", v, len(migrations))
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kran.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.DB().Exec(`UPDATE schema_version SET version = 999`)
	st.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("expected error for newer schema")
	}
}

func TestJobRoundTripAndUpsert(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	j := job("j1", proto.JobQueued, "s1", "", t0)
	j.Estimate = proto.Estimate{CPUs: 8, Mem: 4 << 30, Duration: 60, Source: "family:go-test"}
	if err := st.PutJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	j.State = proto.JobSucceeded
	j.Node = "devmac"
	j.EndedAt = t0.Add(2 * time.Minute)
	j.Usage = proto.Usage{CPUSeconds: 300, PeakMem: 3 << 30, WallSeconds: 55}
	if err := st.PutJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetJob(ctx, "j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.JobSucceeded || got.Node != "devmac" || got.Usage.PeakMem != 3<<30 ||
		got.Estimate.Source != "family:go-test" || !got.QueuedAt.Equal(t0) {
		t.Errorf("round trip mismatch: %+v", got)
	}
	active, err := st.ActiveJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Errorf("active = %d, want 0", len(active))
	}
	if _, err := st.GetJob(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetJob(nope) err = %v, want ErrNotFound", err)
	}
	if err := st.PutJob(ctx, &proto.Job{}); err == nil {
		t.Error("PutJob without ID should fail")
	}
}

func TestListJobsFilters(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		owner := fmt.Sprintf("s%d", i%3)
		node := []string{"gx10", "devmac"}[i%2]
		state := proto.JobSucceeded
		if i%5 == 0 {
			state = proto.JobRunning
		}
		if err := st.PutJob(ctx, job(fmt.Sprintf("j%02d", i), state, owner, node, t0.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	// queued-only job on origin, never placed
	q := job("q1", proto.JobQueued, "s9", "", t0.Add(time.Hour))
	st.PutJob(ctx, q)

	all, _ := st.ListJobs(ctx, JobQuery{})
	if len(all) != 31 || all[0].Spec.ID != "q1" || all[1].Spec.ID != "j29" {
		t.Fatalf("list all: n=%d first=%s", len(all), all[0].Spec.ID)
	}
	lim, _ := st.ListJobs(ctx, JobQuery{Limit: 5})
	if len(lim) != 5 {
		t.Errorf("limit: %d", len(lim))
	}
	byOwner, _ := st.ListJobs(ctx, JobQuery{Owner: "s1"})
	if len(byOwner) != 10 {
		t.Errorf("owner s1: %d", len(byOwner))
	}
	byName, _ := st.ListJobs(ctx, JobQuery{Owner: "claude · s1"})
	if len(byName) != 10 {
		t.Errorf("owner name: %d", len(byName))
	}
	byNode, _ := st.ListJobs(ctx, JobQuery{Node: "devmac"})
	if len(byNode) != 15 {
		t.Errorf("node devmac: %d", len(byNode))
	}
	originNode, _ := st.ListJobs(ctx, JobQuery{Node: "gx10"})
	if len(originNode) != 16 { // 15 placed + the unplaced one from origin gx10
		t.Errorf("node gx10: %d", len(originNode))
	}
	since, _ := st.ListJobs(ctx, JobQuery{Since: t0.Add(25 * time.Second)})
	if len(since) != 6 {
		t.Errorf("since: %d", len(since))
	}
	running, _ := st.ListJobs(ctx, JobQuery{State: proto.JobRunning})
	if len(running) != 6 {
		t.Errorf("running: %d", len(running))
	}
	active, _ := st.ActiveJobs(ctx)
	if len(active) != 7 || active[0].Spec.ID != "j00" {
		t.Errorf("active: %d", len(active))
	}
}

func TestPruneJobsKeepsActiveAndRecent(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	st.PutJob(ctx, job("old-done", proto.JobSucceeded, "s", "gx10", t0.Add(-10*24*time.Hour)))
	st.PutJob(ctx, job("old-active", proto.JobRunning, "s", "gx10", t0.Add(-10*24*time.Hour)))
	st.PutJob(ctx, job("new-done", proto.JobFailed, "s", "gx10", t0))
	n, err := st.PruneJobs(ctx, t0.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if _, err := st.GetJob(ctx, "old-done"); !errors.Is(err, ErrNotFound) {
		t.Error("old-done should be gone")
	}
	for _, id := range []string{"old-active", "new-done"} {
		if _, err := st.GetJob(ctx, id); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
}

func TestSamplesOrderTrimAndPrune(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	for i := 0; i < MaxSamplesPerFingerprint+10; i++ {
		err := st.AddSample(ctx, Sample{
			Fingerprint: "fp", Node: "gx10", Origin: "gx10", Family: "go-test",
			CPUSeconds: float64(i), PeakMem: int64(i) << 20, WallSeconds: 1, At: t0.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	st.AddSample(ctx, Sample{Fingerprint: "other", Node: "devmac", At: t0.Add(-100 * 24 * time.Hour), WallSeconds: 2})
	got, err := st.Samples(ctx, "fp", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 || got[0].CPUSeconds != float64(MaxSamplesPerFingerprint+9) || !got[0].At.Equal(t0.Add(time.Duration(MaxSamplesPerFingerprint+9)*time.Minute)) {
		t.Fatalf("samples: n=%d first=%+v", len(got), got[0])
	}
	all, _ := st.Samples(ctx, "fp", 0)
	if len(all) != MaxSamplesPerFingerprint {
		t.Errorf("trim: %d samples kept, want %d", len(all), MaxSamplesPerFingerprint)
	}
	// Prune everything older than the newest sample: keep the newest 20 of fp,
	// and the single old sample of "other" (it is within its newest 20).
	if _, err := st.PruneJobs(ctx, t0.Add(1000*time.Hour)); err != nil {
		t.Fatal(err)
	}
	all, _ = st.Samples(ctx, "fp", 0)
	if len(all) != keepSamplesOnPrune {
		t.Errorf("after prune fp: %d, want %d", len(all), keepSamplesOnPrune)
	}
	other, _ := st.Samples(ctx, "other", 0)
	if len(other) != 1 {
		t.Errorf("after prune other: %d", len(other))
	}
}

func TestKV(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	if _, ok, err := st.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("missing key: ok=%v err=%v", ok, err)
	}
	st.Set(ctx, "k", "v1")
	st.Set(ctx, "k", "v2")
	v, ok, err := st.Get(ctx, "k")
	if !ok || err != nil || v != "v2" {
		t.Fatalf("get: %q %v %v", v, ok, err)
	}
}

func TestPackageOwnedTables(t *testing.T) {
	st, _ := openTest(t)
	db := st.DB()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS ws_hash (k TEXT PRIMARY KEY, h TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ws_hash VALUES ('a', 'b')`); err != nil {
		t.Fatal(err)
	}
	var h string
	if err := db.QueryRow(`SELECT h FROM ws_hash WHERE k='a'`).Scan(&h); err != nil || h != "b" {
		t.Fatalf("%q %v", h, err)
	}
}

// Concurrent writers and readers from one process must neither fail with
// SQLITE_BUSY nor lose writes, including writes through DB() by another package.
func TestConcurrentUse(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	st.DB().Exec(`CREATE TABLE ext (n INTEGER)`)
	var wg sync.WaitGroup
	errc := make(chan error, 1000)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id := fmt.Sprintf("w%d-%d", w, i)
				if err := st.PutJob(ctx, job(id, proto.JobRunning, "s", "gx10", t0)); err != nil {
					errc <- err
				}
				j := job(id, proto.JobSucceeded, "s", "gx10", t0)
				if err := st.PutJob(ctx, j); err != nil {
					errc <- err
				}
				if err := st.AddSample(ctx, Sample{Fingerprint: fmt.Sprintf("fp%d", i%3), WallSeconds: 1}); err != nil {
					errc <- err
				}
				if _, err := st.DB().Exec(`INSERT INTO ext VALUES (?)`, i); err != nil {
					errc <- err
				}
				if _, err := st.ListJobs(ctx, JobQuery{Limit: 10}); err != nil {
					errc <- err
				}
				if err := st.Set(ctx, fmt.Sprintf("k%d", w), id); err != nil {
					errc <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	all, _ := st.ListJobs(ctx, JobQuery{Limit: 1000})
	if len(all) != 200 {
		t.Errorf("jobs = %d, want 200", len(all))
	}
	var n int
	st.DB().QueryRow(`SELECT count(*) FROM ext`).Scan(&n)
	if n != 200 {
		t.Errorf("ext rows = %d", n)
	}
	active, _ := st.ActiveJobs(ctx)
	if len(active) != 0 {
		t.Errorf("active = %d", len(active))
	}
}

func TestQueryPlansUseIndexes(t *testing.T) {
	st, _ := openTest(t)
	cases := map[string]string{
		`SELECT data FROM jobs WHERE active = 1 ORDER BY queued_at, id`:                                              "jobs_active",
		`SELECT data FROM jobs WHERE (owner_id = 'x' OR owner_name = 'x') ORDER BY queued_at DESC, id DESC LIMIT 10`: "",
		`SELECT fingerprint FROM samples WHERE fingerprint = 'x' ORDER BY at DESC, id DESC LIMIT 20`:                 "samples_fp",
		`SELECT data FROM jobs ORDER BY queued_at DESC, id DESC LIMIT 100`:                                           "jobs_queued",
	}
	for q, idx := range cases {
		if idx == "" {
			continue
		}
		plan := explain(t, st.DB(), q)
		if !strings.Contains(plan, idx) {
			t.Errorf("plan for %q does not use %s:\n%s", q, idx, plan)
		}
	}
}

func explain(t *testing.T, db *sql.DB, q string) string {
	rows, err := db.Query(`EXPLAIN QUERY PLAN ` + q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		rows.Scan(&id, &parent, &notused, &detail)
		out += detail + "\n"
	}
	return out
}
