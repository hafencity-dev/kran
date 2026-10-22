package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
	"github.com/hafencity-dev/kran/internal/workspace"
)

// synthRepo writes a git repository of n files, about a dozen per
// directory three levels deep (sizes from a few bytes to ~8 KiB), and
// commits it.
func synthRepo(tb testing.TB, root string, n int) {
	tb.Helper()
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	for i := 0; i < n; i++ {
		d := i / 12
		rel := fmt.Sprintf("src/pkg%02d/mod%03d/file%05d.txt", d%25, d/25, i)
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			tb.Fatal(err)
		}
		body := make([]byte, 0, 8<<10)
		body = append(body, strconv.Itoa(i)...)
		for j := 0; j < (i*7919)%150; j++ {
			body = append(body, line...)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "synthetic"}} {
		cmd := exec.Command("git", append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			tb.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

// BenchmarkSyncUnchanged30k measures what every remote job of an unchanged
// 30k-file repository pays before its command starts: scan, slot
// acquisition, manifest exchange, diff and commit, through two real daemons
// (real execenv with native slots, real scanner, in-memory transport).
// sync-ms is the job's recorded sync time; ns/op adds the job itself.
//
//	nice go test -run '^$' -bench SyncUnchanged -benchtime 20x ./internal/e2e/
func BenchmarkSyncUnchanged30k(b *testing.B) {
	n := 30000
	if v, err := strconv.Atoi(os.Getenv("KRAN_BENCH_FILES")); err == nil && v > 0 {
		n = v
	}
	base, err := os.MkdirTemp("/tmp", "kran-bench-sync-")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(base)
	b.Setenv("KRAN_CACHE_DIR", filepath.Join(base, "cache"))
	log := &logSink{}
	defer func() {
		log.mu.Lock()
		log.closed = true
		log.mu.Unlock()
	}()
	t := &testing.T{}
	net := transport.NewMemNetwork(transport.SessionConfig{})
	tgt := startNode(t, net, log, "b", "a", base)
	org := startNode(t, net, log, "a", "b", base)
	defer org.stop()
	defer tgt.stop()

	root := filepath.Join(base, "repo")
	synthRepo(b, root, n)
	time.Sleep(2100 * time.Millisecond) // past the hash cache's racy window
	ctx := context.Background()
	ws, err := workspace.Detect(ctx, root)
	if err != nil {
		b.Fatal(err)
	}
	spec := proto.JobSpec{
		Argv: []string{"true"}, Cwd: root, Workspace: ws,
		Owner:    proto.Owner{Kind: proto.OwnerHuman, ID: "bench", Name: "bench"},
		Req:      proto.Requirements{OS: proto.OSSame, Prefer: proto.PreferRemote, Node: "b", CPUs: 1, Mem: 256 << 20},
		Priority: proto.PriorityNormal,
	}
	sock := filepath.Join(org.dirs.Runtime, "kran.sock")
	c, err := client.DialSocket(ctx, sock)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	run := func() float64 {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		res, err := runJob(rctx, sock, spec)
		if err != nil || res.exit == nil || res.exit.Code != 0 || res.exit.Node != "b" {
			return -1
		}
		j, err := c.Job(rctx, res.exit.JobID)
		if err != nil {
			b.Fatal(err)
		}
		return j.Usage.SyncSeconds
	}
	// The first run waits for b to show up and fills the slot (cold).
	deadline := time.Now().Add(60 * time.Second)
	cold := run()
	for cold < 0 {
		if time.Now().After(deadline) {
			b.Fatal("first remote run did not reach b")
		}
		time.Sleep(300 * time.Millisecond)
		cold = run()
	}
	var total float64
	b.ResetTimer()
	for b.Loop() {
		s := run()
		if s < 0 {
			b.Fatal("remote run failed")
		}
		total += s
	}
	b.ReportMetric(total*1000/float64(b.N), "sync-ms")
	b.ReportMetric(float64(n), "files")
	b.ReportMetric(cold*1000, "cold-sync-ms")

}
