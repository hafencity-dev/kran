package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// BenchmarkRescan measures a re-scan of an unchanged repository with a warm
// hash cache (SPEC §6.7 target: < 300 ms for 30k files). The repository is
// only read. Set KRAN_BENCH_REPO to pick one; defaults to this repository.
//
//	KRAN_BENCH_REPO=~/src/big nice go test -run '^$' -bench Rescan ./internal/workspace/
func BenchmarkRescan(b *testing.B) {
	repo := os.Getenv("KRAN_BENCH_REPO")
	if repo == "" {
		repo = "../.."
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		b.Skipf("no repository at %s", repo)
	}
	ctx := context.Background()
	ws, err := Detect(ctx, repo)
	if err != nil {
		b.Fatal(err)
	}
	s, err := NewScanner(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	m, err := s.Scan(ctx, ws, nil, nil) // cold: fills the cache
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(len(m.Entries)), "files")
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.Scan(ctx, ws, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDetect(b *testing.B) {
	repo := os.Getenv("KRAN_BENCH_REPO")
	if repo == "" {
		repo = "../.."
	}
	if _, err := os.Stat(repo); err != nil {
		b.Skip("no repository")
	}
	for b.Loop() {
		if _, err := Detect(context.Background(), repo); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRescanSynthetic30k: the re-scan of an unchanged 30k-file
// repository (about a dozen files per directory, three levels deep) with
// a warm hash cache, independent of the checkout it runs in. Set
// KRAN_BENCH_FILES for another size.
func BenchmarkRescanSynthetic30k(b *testing.B) {
	n := 30000
	if v, err := strconv.Atoi(os.Getenv("KRAN_BENCH_FILES")); err == nil && v > 0 {
		n = v
	}
	base, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	root := filepath.Join(base, "repo")
	for i := 0; i < n; i++ {
		d := i / 12
		p := filepath.Join(root, fmt.Sprintf("src/pkg%02d/mod%03d/file%05d.txt", d%25, d/25, i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat(fmt.Sprintf("line %d\n", i), 1+i%200)), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "synthetic"}} {
		cmd := exec.Command("git", append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	time.Sleep(racyWindow + 100*time.Millisecond) // older than the racy window: cacheable
	ctx := context.Background()
	ws, err := Detect(ctx, root)
	if err != nil {
		b.Fatal(err)
	}
	s, err := NewScanner(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	m, err := s.Scan(ctx, ws, nil, nil)
	if err != nil || len(m.Entries) != n {
		b.Fatalf("cold scan: %v (%d entries)", err, len(m.Entries))
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.Scan(ctx, ws, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(n), "files")
}
