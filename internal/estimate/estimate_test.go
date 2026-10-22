package estimate

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/store"
)

func TestFamily(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"go", "test", "./..."}, "go-test"},
		{[]string{"/usr/local/go/bin/go", "test", "-race", "-count=1", "-run", "^TestScheduler(Fair|Starve)$", "./internal/scheduler/..."}, "go-test"},
		{[]string{"go", "-C", "sub", "test", "./..."}, "go-test"},
		{[]string{"gotestsum", "--format", "dots", "--", "-race", "./..."}, "go-test"},
		{[]string{"go", "vet", "./..."}, "go-build"},
		{[]string{"go", "build", "-o", "bin/kran", "./cmd/kran"}, "go-build"},
		{[]string{"go", "mod", "tidy"}, "other"},
		{[]string{"golangci-lint", "run"}, "go-build"},
		{[]string{"pnpm", "vitest", "run", "--reporter=dot"}, "vitest"},
		{[]string{"pnpm", "exec", "vitest", "run", "src/foo.test.ts"}, "vitest"},
		{[]string{"npx", "vitest", "--run"}, "vitest"},
		{[]string{"npx", "--yes", "jest", "--ci"}, "jest"},
		{[]string{"pnpm", "test"}, "js-test"},
		{[]string{"npm", "run", "test:unit"}, "js-test"},
		{[]string{"yarn", "build"}, "js-build"},
		{[]string{"pnpm", "--filter", "web", "build"}, "js-build"},
		{[]string{"pnpm", "install", "--frozen-lockfile"}, "other"},
		{[]string{"pnpm", "run", "typecheck"}, "tsc"},
		{[]string{"next", "build"}, "next-build"},
		{[]string{"pnpm", "next", "build"}, "next-build"},
		{[]string{"npx", "next", "build", "--no-lint"}, "next-build"},
		{[]string{"next", "dev"}, "other"},
		{[]string{"tsc", "--noEmit", "-p", "."}, "tsc"},
		{[]string{"npx", "tsc", "-b"}, "tsc"},
		{[]string{"docker", "build", "-t", "kran-dev/app:latest", "."}, "docker-build"},
		{[]string{"docker", "buildx", "build", "--platform", "linux/arm64", "."}, "docker-build"},
		{[]string{"docker", "compose", "build", "api"}, "docker-build"},
		{[]string{"docker", "compose", "up"}, "other"},
		{[]string{"docker", "run", "--rm", "alpine"}, "other"},
		{[]string{"pytest", "-x", "-k", "test_sched and not slow", "tests/"}, "pytest"},
		{[]string{"python3", "-m", "pytest", "-n", "auto"}, "pytest"},
		{[]string{"uv", "run", "pytest", "tests/unit"}, "pytest"},
		{[]string{"poetry", "run", "pytest"}, "pytest"},
		{[]string{"cargo", "test", "--workspace"}, "cargo"},
		{[]string{"cargo", "+nightly", "build", "--release"}, "cargo"},
		{[]string{"cargo", "nextest", "run"}, "cargo"},
		{[]string{"cargo", "fmt"}, "other"},
		{[]string{"make", "-j8", "test"}, "make"},
		{[]string{"env", "CGO_ENABLED=0", "GOOS=darwin", "go", "build", "./..."}, "go-build"},
		{[]string{"CI=1", "pnpm", "test"}, "js-test"},
		{[]string{"time", "nice", "-n", "10", "go", "test", "./..."}, "go-test"},
		{[]string{"timeout", "-s", "KILL", "10m", "go", "test", "./..."}, "go-test"},
		{[]string{"mise", "exec", "node@24", "--", "pnpm", "build"}, "js-build"},
		{[]string{"bash", "-c", "cd web && pnpm install && pnpm build"}, "js-build"},
		{[]string{"/bin/zsh", "-c", "go vet ./... && go test -race ./..."}, "go-test"},
		{[]string{"sh", "-c", "echo 'go test ./...'"}, "other"},
		{[]string{"vllm", "serve", "Qwen/Qwen3-8B", "--gpu-memory-utilization", "0.5"}, "vllm"},
		{[]string{"python", "train.py"}, "other"},
		{[]string{"ls"}, "other"},
		{nil, "other"},
	}
	for _, c := range cases {
		if got := family(c.argv); got != c.want {
			t.Errorf("family(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestFingerprintNormalization(t *testing.T) {
	base := proto.JobSpec{
		Argv:      []string{"go", "test", "-count=1", "-run", "^TestA$", "./..."},
		Cwd:       "/home/u/src/kran",
		Workspace: proto.Workspace{Root: "/home/u/src/kran", RepoID: "repo1"},
	}
	fp := fingerprint(&base)
	if len(fp) != 32 {
		t.Fatalf("fingerprint length %d", len(fp))
	}
	same := func(mut func(*proto.JobSpec)) string {
		s := base
		s.Argv = append([]string(nil), base.Argv...)
		mut(&s)
		return fingerprint(&s)
	}
	equal := map[string]func(*proto.JobSpec){
		"abs go path": func(s *proto.JobSpec) { s.Argv[0] = "/usr/local/go/bin/go" },
		"other worktree same repo": func(s *proto.JobSpec) {
			s.Workspace.Root = "/home/u/src/kran-wt/a"
			s.Cwd = "/home/u/src/kran-wt/a"
		},
	}
	for name, mut := range equal {
		if got := same(mut); got != fp {
			t.Errorf("%s: fingerprint changed", name)
		}
	}
	differ := map[string]func(*proto.JobSpec){
		"-run regex":   func(s *proto.JobSpec) { s.Argv[4] = "^TestB$" },
		"-count":       func(s *proto.JobSpec) { s.Argv[2] = "-count=5" },
		"package":      func(s *proto.JobSpec) { s.Argv[5] = "./internal/..." },
		"other repo":   func(s *proto.JobSpec) { s.Workspace.RepoID = "repo2" },
		"subdirectory": func(s *proto.JobSpec) { s.Workspace.Rel = "internal" },
	}
	for name, mut := range differ {
		if got := same(mut); got == fp {
			t.Errorf("%s: fingerprint should change", name)
		}
	}

	// Values that vary per run are normalized away.
	pairs := [][2][]string{
		{{"go", "test", "-coverprofile=/tmp/cover123/c.out", "./..."}, {"go", "test", "-coverprofile=/tmp/go-build998/c.out", "./..."}},
		{{"pytest", "--basetemp", "/var/folders/xy/abc/T/pytest-77"}, {"pytest", "--basetemp", "/var/folders/qq/zzz/T/pytest-78"}},
		{{"node", "bench.js", "--seed", "1729587345123"}, {"node", "bench.js", "--seed", "1729587399999"}},
		{{"docker", "build", "-t", "app:3f2a9c1e5b7d4a60", "."}, {"docker", "build", "-t", "app:9be0c41d22aa7f13", "."}},
		{{"run.sh", "--id", "6f1c2b8e-4a5d-4e3f-9b1a-2c3d4e5f6a7b"}, {"run.sh", "--id", "00000000-1111-4222-8333-444455556666"}},
		{{"go", "test", "./...", "-args", "-dir=/home/u/src/kran/testdata"}, {"go", "test", "./...", "-args", "-dir=/home/u/src/kran/testdata"}},
	}
	for _, p := range pairs {
		a := base
		a.Argv = p[0]
		b := base
		b.Argv = p[1]
		if fingerprint(&a) != fingerprint(&b) {
			t.Errorf("%q vs %q: should share a fingerprint", p[0], p[1])
		}
	}
	// Short numbers are meaningful (-p 4, -count=3).
	a, b := base, base
	a.Argv = []string{"go", "test", "-p", "4"}
	b.Argv = []string{"go", "test", "-p", "8"}
	if fingerprint(&a) == fingerprint(&b) {
		t.Error("-p 4 vs -p 8 must differ")
	}
	// --sh strings ignore the user's login shell and whitespace.
	a, b = base, base
	a.Shell, a.Argv = "go vet ./...  &&  go test ./...", []string{"/bin/zsh", "-c", "go vet ./...  &&  go test ./..."}
	b.Shell, b.Argv = "go vet ./... && go test ./...", []string{"/bin/bash", "-c", "go vet ./... && go test ./..."}
	if fingerprint(&a) != fingerprint(&b) {
		t.Error("shell strings should share a fingerprint")
	}
}

func TestFamilyDefaultsScaledToNode(t *testing.T) {
	e := NewWithOptions(nil, Options{Self: "gx10"})
	cases := []struct {
		argv     []string
		nodeCPUs int
		cpus     float64
		mem      int64
		source   string
	}{
		{[]string{"go", "test", "./..."}, 20, 8, 4 * gib, "family:go-test"},
		{[]string{"go", "test", "./..."}, 4, 4, 4 * gib, "family:go-test"},
		{[]string{"go", "test", "-race", "./..."}, 20, 8, 6 * gib, "family:go-test"},
		{[]string{"go", "vet", "./..."}, 20, 4, 2 * gib, "family:go-build"},
		{[]string{"next", "build"}, 28, 4, 6 * gib, "family:next-build"},
		{[]string{"pnpm", "vitest", "run"}, 28, 4, 4 * gib, "family:vitest"},
		{[]string{"tsc", "--noEmit"}, 28, 2, 3 * gib, "family:tsc"},
		{[]string{"pytest"}, 28, 2, 2 * gib, "family:pytest"},
		{[]string{"docker", "build", "."}, 28, 4, 4 * gib, "family:docker-build"},
		{[]string{"cargo", "build"}, 6, 6, 4 * gib, "family:cargo"},
		{[]string{"make", "-j", "12"}, 28, 12, 2 * gib, "family:make"},
		{[]string{"make", "-j64"}, 20, 20, 2 * gib, "family:make"},
		{[]string{"./script.sh"}, 28, 1, 1 * gib, "family:other"},
	}
	for _, c := range cases {
		spec := &proto.JobSpec{Argv: c.argv, Workspace: proto.Workspace{RepoID: "r"}}
		est := e.Estimate(spec, "", c.nodeCPUs)
		if est.CPUs != c.cpus || est.Mem != c.mem || est.Source != c.source || est.Samples != 0 {
			t.Errorf("%q on %d cpus: got %+v, want cpus=%v mem=%d %s", c.argv, c.nodeCPUs, est, c.cpus, c.mem, c.source)
		}
	}
	gpu := &proto.JobSpec{Argv: []string{"python", "infer.py"}, Req: proto.Requirements{GPU: true}}
	if est := e.Estimate(gpu, "", 20); est.Mem != 8*gib || est.CPUs != 2 {
		t.Errorf("gpu other: %+v", est)
	}
	short := e.Estimate(&proto.JobSpec{Argv: []string{"ls"}}, "", 20)
	if short.Duration >= 5 {
		t.Errorf("unknown commands must default below the 5 s offload threshold: %v", short.Duration)
	}
}

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kran.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func finished(spec proto.JobSpec, fp, node string, state proto.JobState, wall, cpuSec float64, peak int64, at time.Time) *proto.Job {
	return &proto.Job{
		Spec: spec, State: state, Node: node, Origin: "gx10", Local: node == "gx10",
		Fingerprint: fp, EndedAt: at,
		Usage: proto.Usage{WallSeconds: wall, CPUSeconds: cpuSec, PeakMem: peak},
	}
}

func TestLearnedEstimate(t *testing.T) {
	st := newTestStore(t)
	e := NewWithOptions(st, Options{Self: "gx10"})
	spec := proto.JobSpec{Argv: []string{"go", "test", "./..."}, Workspace: proto.Workspace{RepoID: "kran"}}
	fp := e.Fingerprint(&spec)

	// 10 runs: ~40 s wall, 6 cores busy, peaks 1..10 GiB.
	for i := 1; i <= 10; i++ {
		e.Observe(finished(spec, fp, "gx10", proto.JobSucceeded, 40, 240, int64(i)*gib, t0.Add(time.Duration(i)*time.Minute)))
	}
	est := e.Estimate(&spec, fp, 20)
	if est.Source != "learned" || est.Samples != 10 {
		t.Fatalf("source %+v", est)
	}
	if math.Abs(est.Duration-40) > 0.01 || math.Abs(est.CPUs-6) > 0.01 {
		t.Errorf("duration/cpu: %+v", est)
	}
	if est.Mem != 9*gib { // p90 of 1..10 (nearest rank)
		t.Errorf("p90 mem = %d GiB", est.Mem/gib)
	}

	// A fresh estimator over the same store sees the same history.
	e2 := NewWithOptions(st, Options{Self: "gx10"})
	if est2 := e2.Estimate(&spec, fp, 20); est2 != est {
		t.Errorf("reload: %+v vs %+v", est2, est)
	}

	// Explicit flags beat learned values; a rule label changes the source.
	spec.Req.Mem = 12 * gib
	if est := e.Estimate(&spec, fp, 20); est.Mem != 12*gib || est.Source != "flag" || est.CPUs != 6 {
		t.Errorf("flag: %+v", est)
	}
	spec.Labels = map[string]string{LabelReqSource: "rule"}
	spec.Req.CPUs = 3
	if est := e.Estimate(&spec, fp, 20); est.CPUs != 3 || est.Source != "rule" {
		t.Errorf("rule: %+v", est)
	}
}

func TestEWMAFollowsTrend(t *testing.T) {
	e := NewWithOptions(nil, Options{Self: "gx10"})
	spec := proto.JobSpec{Argv: []string{"next", "build"}, Workspace: proto.Workspace{RepoID: "web"}}
	fp := e.Fingerprint(&spec)
	for i := 0; i < 10; i++ {
		e.Observe(finished(spec, fp, "gx10", proto.JobSucceeded, 100, 400, 5*gib, t0.Add(time.Duration(i)*time.Minute)))
	}
	for i := 10; i < 15; i++ {
		e.Observe(finished(spec, fp, "gx10", proto.JobSucceeded, 50, 200, 5*gib, t0.Add(time.Duration(i)*time.Minute)))
	}
	d := e.Estimate(&spec, fp, 20).Duration
	if d < 50 || d > 70 {
		t.Errorf("EWMA after a speedup should be near 50 s, got %.1f", d)
	}
}

func TestOOMRaisesMemory(t *testing.T) {
	e := NewWithOptions(nil, Options{Self: "gx10"})
	spec := proto.JobSpec{Argv: []string{"pnpm", "vitest", "run"}, Workspace: proto.Workspace{RepoID: "web"}}
	fp := e.Fingerprint(&spec)
	before := e.Estimate(&spec, fp, 20)
	// OOM-killed at its 8 GiB cap after 5 s: memory must be learned even
	// though the duration is not representative.
	e.Observe(finished(spec, fp, "devmac", proto.JobFailed, 5, 10, 8*gib, t0))
	after := e.Estimate(&spec, fp, 20)
	if after.Mem != 8*gib {
		t.Errorf("mem after OOM = %d, want 8 GiB", after.Mem)
	}
	if after.Duration != before.Duration {
		t.Errorf("early failure must not teach duration: %v -> %v", before.Duration, after.Duration)
	}
	// Canceled jobs: memory only. Jobs without usage: ignored.
	e.Observe(finished(spec, fp, "gx10", proto.JobCanceled, 1, 1, 9*gib, t0.Add(time.Minute)))
	e.Observe(&proto.Job{Spec: spec, Fingerprint: fp, State: proto.JobLost, Origin: "gx10"})
	if est := e.Estimate(&spec, fp, 20); est.Mem != 9*gib || est.Samples != 2 {
		t.Errorf("after cancel: %+v", est)
	}
	// Non-terminal jobs are ignored.
	e.Observe(finished(spec, fp, "gx10", proto.JobRunning, 100, 100, 50*gib, t0))
	if est := e.Estimate(&spec, fp, 20); est.Mem != 9*gib {
		t.Errorf("running job observed: %+v", est)
	}
}

func TestFailedFullRunTeachesDuration(t *testing.T) {
	e := NewWithOptions(nil, Options{Self: "gx10"})
	spec := proto.JobSpec{Argv: []string{"go", "test", "./..."}, Workspace: proto.Workspace{RepoID: "kran"}}
	fp := e.Fingerprint(&spec)
	e.Observe(finished(spec, fp, "gx10", proto.JobSucceeded, 60, 300, 2*gib, t0))
	// One failing test in a full run: still representative.
	e.Observe(finished(spec, fp, "gx10", proto.JobFailed, 62, 310, 2*gib, t0.Add(time.Minute)))
	est := e.Estimate(&spec, fp, 20)
	if est.Duration < 60 || est.Duration > 62 {
		t.Errorf("duration %v", est.Duration)
	}
}

func TestSpeedPriorsAndLearning(t *testing.T) {
	scores := map[string]float64{"gx10": 1000, "devmac": 1250}
	e := NewWithOptions(nil, Options{Self: "gx10", Score: func(n string) float64 { return scores[n] }})
	spec := proto.JobSpec{Argv: []string{"go", "test", "./..."}, Workspace: proto.Workspace{RepoID: "kran"}}
	fp := e.Fingerprint(&spec)

	if s := e.Speed(fp, "gx10"); s != 1 {
		t.Errorf("self speed %v", s)
	}
	if s := e.Speed(fp, "devmac"); math.Abs(s-0.8) > 1e-9 {
		t.Errorf("prior from single-core score: %v, want 0.8", s)
	}
	if s := e.Speed(fp, "unknown"); s != 1 {
		t.Errorf("unknown node: %v", s)
	}

	// Measured: devmac ran this job in 30 s before any gx10 run existed
	// (no ratio yet → prior), then gx10 in 60 s → ratio 0.5 blended with the
	// 0.8 prior (one run is worth 1/(1+2)), converging with more runs.
	e.Observe(finished(spec, fp, "devmac", proto.JobSucceeded, 30, 300, 2*gib, t0))
	if s := e.Speed(fp, "devmac"); math.Abs(s-0.8) > 1e-9 {
		t.Errorf("no self run yet: %v", s)
	}
	e.Observe(finished(spec, fp, "gx10", proto.JobSucceeded, 60, 300, 2*gib, t0.Add(time.Minute)))
	s1 := e.Speed(fp, "devmac")
	want := math.Exp(math.Log(0.5)/3 + 2*math.Log(0.8)/3)
	if math.Abs(s1-want) > 1e-9 {
		t.Errorf("after one run: %v, want %v", s1, want)
	}
	for i := 2; i < 12; i++ {
		e.Observe(finished(spec, fp, "devmac", proto.JobSucceeded, 30, 300, 2*gib, t0.Add(time.Duration(i)*time.Minute)))
	}
	s2 := e.Speed(fp, "devmac")
	if !(s2 < s1 && s2 >= 0.5 && s2 < 0.6) {
		t.Errorf("converging: %v (after 1 run %v)", s2, s1)
	}
	// Duration estimates are origin-equivalent: devmac's 30 s runs count as 60 s here.
	if d := e.Estimate(&spec, fp, 20).Duration; d < 50 || d > 65 {
		t.Errorf("origin-equivalent duration %v", d)
	}

	// A new fingerprint of the same family inherits the family-wide ratio.
	spec2 := proto.JobSpec{Argv: []string{"go", "test", "./internal/..."}, Workspace: proto.Workspace{RepoID: "kran"}}
	fp2 := e.Fingerprint(&spec2)
	e.Estimate(&spec2, fp2, 20) // remembers the family of fp2
	if s := e.Speed(fp2, "devmac"); !(s > 0.45 && s < 0.6) {
		t.Errorf("family prior for new fingerprint: %v", s)
	}
}

func TestFamilySpeedPersists(t *testing.T) {
	st := newTestStore(t)
	e := NewWithOptions(st, Options{Self: "gx10"})
	spec := proto.JobSpec{Argv: []string{"cargo", "build"}, Workspace: proto.Workspace{RepoID: "r"}}
	fp := e.Fingerprint(&spec)
	e.Observe(finished(spec, fp, "gx10", proto.JobSucceeded, 100, 800, gib, t0))
	e.Observe(finished(spec, fp, "devmac", proto.JobSucceeded, 70, 800, gib, t0.Add(time.Minute)))

	e2 := NewWithOptions(st, Options{Self: "gx10"})
	other := proto.JobSpec{Argv: []string{"cargo", "test"}, Workspace: proto.Workspace{RepoID: "r"}}
	ofp := e2.Fingerprint(&other)
	e2.Estimate(&other, ofp, 20)
	if s := e2.Speed(ofp, "devmac"); math.Abs(s-0.7) > 1e-9 {
		t.Errorf("persisted family speed: %v", s)
	}
}

func TestSelfInferredFromOrigin(t *testing.T) {
	e := NewWithOptions(nil, Options{})
	spec := proto.JobSpec{Argv: []string{"go", "test", "./..."}}
	fp := e.Fingerprint(&spec)
	e.Observe(finished(spec, fp, "gx10", proto.JobSucceeded, 60, 60, gib, t0))
	e.Observe(finished(spec, fp, "devmac", proto.JobSucceeded, 120, 60, gib, t0.Add(time.Minute)))
	if s := e.Speed(fp, "gx10"); s != 1 {
		t.Errorf("self: %v", s)
	}
	if s := e.Speed(fp, "devmac"); !(s > 1.3 && s <= 2) {
		t.Errorf("devmac slower: %v", s)
	}
}

func TestCacheEviction(t *testing.T) {
	e := NewWithOptions(nil, Options{Self: "gx10"}).(*estimator)
	for i := 0; i < cacheSize+50; i++ {
		e.Speed(fmt.Sprintf("fp%d", i), "devmac")
	}
	if len(e.cache) > cacheSize {
		t.Errorf("cache grew to %d", len(e.cache))
	}
}

func TestP90(t *testing.T) {
	if p90(nil) != 0 || p90([]int64{5}) != 5 {
		t.Error("small inputs")
	}
	v := make([]int64, 20)
	for i := range v {
		v[i] = int64(20 - i)
	}
	if got := p90(v); got != 18 {
		t.Errorf("p90 of 1..20 = %d, want 18", got)
	}
}
