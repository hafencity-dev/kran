package estimate

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Explicit --cpus/--mem set only cpu and memory; the duration must still
// come from the job's history, also when every run so far failed.
func TestFlagsKeepLearnedDuration(t *testing.T) {
	e := NewWithOptions(nil, Options{Self: "gx10"})
	spec := proto.JobSpec{
		Argv:      []string{"bash", "ci/job.sh", "walls"},
		Workspace: proto.Workspace{RepoID: "app"},
		Req:       proto.Requirements{CPUs: 8, Mem: 12 * gib},
	}
	fp := e.Fingerprint(&spec)
	at := t0
	run := func(state proto.JobState, wall float64, peak int64) {
		at = at.Add(10 * time.Minute)
		j := finished(spec, fp, "devmac", state, wall, wall*2, peak, at)
		j.Runtime = proto.RuntimeContainer
		j.Alloc = proto.Resources{CPUs: 8, Mem: 12 * gib}
		e.Observe(j)
	}
	run(proto.JobFailed, 5.5, 50<<20)
	run(proto.JobCanceled, 1428, 9*gib)
	run(proto.JobFailed, 308, 4700<<20)
	run(proto.JobFailed, 12, 70<<20)
	run(proto.JobFailed, 336, 3*gib)

	est := e.Estimate(&spec, fp, 20)
	if est.CPUs != 8 || est.Mem != 12*gib || est.Source != "flag" {
		t.Errorf("flags must set cpu and memory: %+v", est)
	}
	if est.Samples == 0 || est.DurationSource != "learned" {
		t.Errorf("duration should be learned: %+v", est)
	}
	// Two full runs of ~5 min, two early failures, a 24 min cancel: the
	// estimate is minutes, not the 3 s default of an unknown command.
	if est.Duration < 120 || est.Duration > 400 {
		t.Errorf("duration %.1f s, want minutes", est.Duration)
	}

	// --cpus alone: memory still comes from history.
	spec.Req = proto.Requirements{CPUs: 2}
	est = e.Estimate(&spec, fp, 20)
	if est.CPUs != 2 || est.Mem < 4*gib || est.Mem > 12*gib || est.DurationSource != "learned" {
		t.Errorf("--cpus only: %+v", est)
	}
}

// goTestRuns are measured go test runs of one repo: different packages
// and -run filters each time (no fingerprint repeats).
var goTestRuns = []struct {
	wall, cores float64
	peakMB      int64
}{
	{7.8, 4.07, 625}, {67.1, 1.87, 1389}, {48.7, 0.06, 171}, {46.9, 5.0, 1500},
	{3.4, 0.73, 801}, {4.6, 3.61, 920}, {6.5, 3.54, 706}, {12, 2.5, 650},
	{9, 1.2, 400}, {21, 2.2, 980}, {5.2, 0.9, 350}, {30, 3.1, 1100},
}

func observeGoTests(e Estimator, repo string, n int, from time.Time) {
	for i := 0; i < n; i++ {
		r := goTestRuns[i%len(goTestRuns)]
		spec := proto.JobSpec{Argv: []string{"go", "test", "-count=1", fmt.Sprintf("./internal/pkg%d/", i)},
			Workspace: proto.Workspace{RepoID: repo}}
		fp := e.Fingerprint(&spec)
		e.Observe(finished(spec, fp, "gx10", proto.JobSucceeded, r.wall, r.wall*r.cores, r.peakMB<<20,
			from.Add(time.Duration(i)*time.Minute)))
	}
}

func TestRepoFamilyTierLearnsFromOtherCommands(t *testing.T) {
	st := newTestStore(t)
	e := NewWithOptions(st, Options{Self: "gx10"})
	observeGoTests(e, "app", len(goTestRuns), t0)

	fresh := proto.JobSpec{Argv: []string{"go", "test", "-run", "TestNew", "./internal/new/"},
		Workspace: proto.Workspace{RepoID: "app"}}
	est := e.Estimate(&fresh, "", 20)
	if est.Source != "repo:go-test" || est.Samples != 0 || est.RepoSamples != len(goTestRuns) || est.DurationSource != "repo:go-test" {
		t.Fatalf("source: %+v", est)
	}
	// Measured 0.06-5 cores: far below the static 8, above the typical use.
	if est.CPUs < 3 || est.CPUs > 6 {
		t.Errorf("cpus %.1f", est.CPUs)
	}
	// Peaks up to 1.5 GB: well below the static 4 GiB, never below the
	// largest seen (with headroom it is above it).
	if est.Mem < 1500<<20 || est.Mem > 3*gib {
		t.Errorf("mem %d MiB", est.Mem>>20)
	}
	if est.Duration < 10 || est.Duration > 40 {
		t.Errorf("duration %.1f", est.Duration)
	}

	// Another repo, another family, the -race variant: not affected.
	other := fresh
	other.Workspace.RepoID = "other"
	if est := e.Estimate(&other, "", 20); est.Source != "family:go-test" || est.CPUs != 8 || est.Mem != 4*gib {
		t.Errorf("other repo: %+v", est)
	}
	race := fresh
	race.Argv = []string{"go", "test", "-race", "./internal/new/"}
	if est := e.Estimate(&race, "", 20); est.Source != "family:go-test" || est.Mem != 6*gib {
		t.Errorf("-race variant: %+v", est)
	}
	vet := fresh
	vet.Argv = []string{"go", "vet", "./..."}
	if est := e.Estimate(&vet, "", 20); est.Source != "family:go-build" {
		t.Errorf("go vet: %+v", est)
	}

	// A fresh estimator over the same store knows the tier.
	e2 := NewWithOptions(st, Options{Self: "gx10"})
	if est2 := e2.Estimate(&fresh, "", 20); est2 != est {
		t.Errorf("reload: %+v vs %+v", est2, est)
	}

	// The fingerprint's own history wins once it has runs.
	fp := e.Fingerprint(&fresh)
	e.Observe(finished(fresh, fp, "gx10", proto.JobSucceeded, 2, 1, 100<<20, t0.Add(time.Hour)))
	if est := e.Estimate(&fresh, fp, 20); est.Source != "learned" || est.Duration != 2 || est.DurationSource != "learned" {
		t.Errorf("fingerprint history: %+v", est)
	}
}

func TestRepoFamilyTierNeedsAFewRuns(t *testing.T) {
	e := NewWithOptions(nil, Options{Self: "gx10"})
	observeGoTests(e, "app", groupMinRuns-1, t0)
	spec := proto.JobSpec{Argv: []string{"go", "test", "./x/"}, Workspace: proto.Workspace{RepoID: "app"}}
	if est := e.Estimate(&spec, "", 20); est.Source != "family:go-test" {
		t.Errorf("after %d runs: %+v", groupMinRuns-1, est)
	}
	observeGoTests(e, "app", 1, t0.Add(time.Hour))
	if est := e.Estimate(&spec, "", 20); est.Source != "repo:go-test" {
		t.Errorf("after %d runs: %+v", groupMinRuns, est)
	}
}

func TestRepoFamilyTierIsRobust(t *testing.T) {
	e := NewWithOptions(nil, Options{Self: "gx10"})
	repo := proto.Workspace{RepoID: "web"}
	at := t0
	obs := func(state proto.JobState, args string, wall, cores float64, peak int64) {
		at = at.Add(time.Minute)
		spec := proto.JobSpec{Argv: []string{"pnpm", "vitest", "run", args}, Workspace: repo}
		e.Observe(finished(spec, e.Fingerprint(&spec), "gx10", state, wall, wall*cores, peak, at))
	}
	// Ten typical runs: 4-7 s, 1.3-3.6 cores, 340-600 MB.
	for i := 0; i < 10; i++ {
		obs(proto.JobSucceeded, fmt.Sprintf("src/a%d.test.ts", i), 4+float64(i%4), 1.3+0.25*float64(i%10), int64(340+26*i)<<20)
	}
	spec := proto.JobSpec{Argv: []string{"pnpm", "vitest", "run", "src/new.test.ts"}, Workspace: repo}
	base := e.Estimate(&spec, "", 20)

	// A 25-minute run (hung, then killed by its owner: the daemon reports
	// it as failed with a signal), a canceled one and runs that failed
	// right away barely move it.
	obs(proto.JobFailed, "src/hang.test.ts", 1500, 0.01, 500<<20)
	obs(proto.JobSucceeded, "src/slow.test.ts", 95, 3.6, 604<<20)
	obs(proto.JobCanceled, "src/c.test.ts", 1400, 3, 450<<20)
	for i := 0; i < 3; i++ {
		obs(proto.JobFailed, fmt.Sprintf("src/broken%d.test.ts", i), 0.4, 0.2, 60<<20)
	}
	after := e.Estimate(&spec, "", 20)
	if after.Duration > 2*base.Duration || after.Duration < 0.8*base.Duration {
		t.Errorf("outliers moved duration %.1f -> %.1f", base.Duration, after.Duration)
	}
	if after.CPUs < 0.8*base.CPUs || after.CPUs > 1.3*base.CPUs {
		t.Errorf("outliers moved cpus %.1f -> %.1f", base.CPUs, after.CPUs)
	}
	// Memory never drops below what was seen: a high quantile plus headroom.
	if after.Mem < 600<<20 || after.Mem > 2*gib {
		t.Errorf("mem %d MiB", after.Mem>>20)
	}

	// One huge run raises memory at once: the cap (twice the estimate)
	// must cover it, or the next such job is OOM-killed.
	obs(proto.JobFailed, "src/big.test.ts", 3, 1, 5*gib)
	if est := e.Estimate(&spec, "", 20); 2*est.Mem < 6*gib {
		t.Errorf("after a 5 GiB peak: mem %d MiB", est.Mem>>20)
	}
}

// A container job runs under a --cpus cap equal to its allocation: using
// all of it says it may want more, so the estimate must be able to grow
// instead of locking in a low allocation.
func TestRepoFamilyTierCPUCapDoesNotRatchet(t *testing.T) {
	e := NewWithOptions(nil, Options{Self: "gx10"})
	repo := proto.Workspace{RepoID: "app"}
	newSpec := proto.JobSpec{Argv: []string{"go", "test", "./new/"}, Workspace: repo}
	at := t0
	run := func(i int, local bool, want float64) {
		at = at.Add(time.Minute)
		spec := proto.JobSpec{Argv: []string{"go", "test", fmt.Sprintf("./p%d/", i)}, Workspace: repo}
		alloc := e.Estimate(&newSpec, "", 20).CPUs
		used := want
		node := "gx10"
		j := finished(spec, e.Fingerprint(&spec), node, proto.JobSucceeded, 60, 0, gib, at)
		if !local {
			used = math.Min(want, alloc) // docker --cpus
			j.Node, j.Local, j.Runtime = "devmac", false, proto.RuntimeContainer
		}
		j.Usage.CPUSeconds = 60 * used
		j.Alloc = proto.Resources{CPUs: alloc, Mem: 2 * gib}
		e.Observe(j)
	}
	// Light packages first: about one core.
	for i := 0; i < 8; i++ {
		run(i, true, 1)
	}
	low := e.Estimate(&newSpec, "", 20).CPUs
	if low > 2.5 {
		t.Fatalf("light runs: %.1f cpus", low)
	}
	// Then heavy ones that would use 6 cores, each capped at the estimate.
	for i := 8; i < 20; i++ {
		run(i, false, 6)
	}
	if got := e.Estimate(&newSpec, "", 20).CPUs; got < 4.5 {
		t.Errorf("jobs that use their whole cap should get more CPUs: %.1f -> %.1f", low, got)
	}
}

func TestRepoFamilyTierIsBounded(t *testing.T) {
	e := NewWithOptions(nil, Options{Self: "gx10"}).(*estimator)
	for r := 0; r < groupMax+20; r++ {
		runs := groupMinRuns
		if r >= 20 {
			runs = groupWindow + 5 // the groups that survive are full
		}
		observeGoTests(e, fmt.Sprintf("repo%d", r), runs, t0.Add(time.Duration(r)*time.Hour))
	}
	if len(e.groups) != groupMax {
		t.Errorf("%d groups kept, want %d", len(e.groups), groupMax)
	}
	for _, g := range e.groups {
		if len(g.Runs) > groupWindow {
			t.Fatalf("%d runs kept", len(g.Runs))
		}
	}
	// The newest repos survive; the oldest were dropped.
	newest := proto.JobSpec{Argv: []string{"go", "test", "./x/"}, Workspace: proto.Workspace{RepoID: fmt.Sprintf("repo%d", groupMax+19)}}
	oldest := proto.JobSpec{Argv: []string{"go", "test", "./x/"}, Workspace: proto.Workspace{RepoID: "repo0"}}
	if e.Estimate(&newest, "", 20).Source != "repo:go-test" || e.Estimate(&oldest, "", 20).Source != "family:go-test" {
		t.Error("eviction should drop the least recently updated groups")
	}
	b, err := json.Marshal(e.groups)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 100<<10 {
		t.Errorf("a full tier stores %d KB", len(b)>>10)
	}
	t.Logf("a full tier stores %d KB", len(b)>>10)
}

func TestRobustMeanAndQuantile(t *testing.T) {
	if quantile(nil, 0.5) != 0 || quantile([]float64{3}, 0.9) != 3 {
		t.Error("small inputs")
	}
	if m := robustMean([]float64{1, 2, 100}); m != 2 {
		t.Errorf("median of few: %v", m)
	}
	v := []float64{10, 10, 10, 10, 10, 10, 10, 10, 10, 10000}
	if m := robustMean(v); m > 20 {
		t.Errorf("one outlier of ten: %v", m)
	}
	if b := blendLog(1, 8, 2, 2); math.Abs(b-math.Sqrt(8)) > 1e-9 {
		t.Errorf("blend: %v", b)
	}
}

// `go test -run` runs learn apart from whole-package runs once a repo has
// a few: agents write a new filter each time, and a filtered run takes a
// fraction of the package.
func TestRunFilteredGoTestsLearnApart(t *testing.T) {
	st := newTestStore(t)
	e := NewWithOptions(st, Options{Self: "gx10"})
	observeGoTests(e, "app", len(goTestRuns), t0) // whole packages: 3-67 s
	for i := 0; i < 6; i++ {
		spec := proto.JobSpec{Argv: []string{"go", "test", "-run", fmt.Sprintf("TestOne%d", i), "./internal/pkg/"},
			Workspace: proto.Workspace{RepoID: "app"}}
		fp := e.Fingerprint(&spec)
		e.Observe(finished(spec, fp, "gx10", proto.JobSucceeded, 1.5, 1.5, 200<<20, t0.Add(time.Hour+time.Duration(i)*time.Minute)))
	}
	fresh := proto.JobSpec{Argv: []string{"go", "test", "-run", "TestNew", "./internal/pkg/"}, Workspace: proto.Workspace{RepoID: "app"}}
	est := e.Estimate(&fresh, "", 20)
	if est.Source != "repo:go-test+run" || est.Duration > 4 {
		t.Errorf("filtered run: %+v", est)
	}
	whole := proto.JobSpec{Argv: []string{"go", "test", "./internal/other/"}, Workspace: proto.Workspace{RepoID: "app"}}
	if est := e.Estimate(&whole, "", 20); est.Source != "repo:go-test" || est.Duration < 10 {
		t.Errorf("whole package: %+v", est)
	}
}
