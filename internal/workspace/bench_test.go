package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
