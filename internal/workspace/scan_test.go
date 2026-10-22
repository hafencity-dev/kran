package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func entryMap(m *proto.Manifest) map[string]proto.ManifestEntry {
	out := map[string]proto.ManifestEntry{}
	for _, e := range m.Entries {
		out[e.Path] = e
	}
	return out
}

func manifestPaths(m *proto.Manifest) []string {
	var out []string
	for _, e := range m.Entries {
		out = append(out, e.Path)
	}
	return out
}

// testScanner returns a scanner whose clock is far in the future so every
// file counts as old (cacheable) unless a test says otherwise.
func testScanner(t *testing.T) *scanner {
	t.Helper()
	s, err := newScanner(filepath.Join(realTempDir(t), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	sc := s.(*scanner)
	sc.now = func() time.Time { return time.Now().Add(time.Hour) }
	return sc
}

func TestScanGitRepo(t *testing.T) {
	ctx := context.Background()
	root := newRepo(t)
	writeFile(t, root, ".gitignore", "build/\n*.log\n")
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "pkg/a.go", "package pkg")
	writeFile(t, root, "pkg/deleted.go", "package pkg")
	writeFile(t, root, "build/out.bin", "ignored")
	writeFile(t, root, "debug.log", "ignored")
	writeFile(t, root, "node_modules/x/index.js", "dep")
	writeFile(t, root, ".env", "SECRET=1")
	writeFile(t, root, ".env.example", "SECRET=")
	writeFile(t, root, "certs/dev.pem", "key")
	writeFile(t, root, "vendor-bin/tool", "#!/bin/sh")
	writeFile(t, root, "testdata/huge/blob", "big")
	writeFile(t, root, "scripts/run.sh", "#!/bin/sh\necho hi\n")
	os.Chmod(filepath.Join(root, "scripts/run.sh"), 0o755)
	os.Symlink("pkg/a.go", filepath.Join(root, "inside-link"))
	os.Symlink("/etc/passwd", filepath.Join(root, "outside-abs"))
	os.Symlink("../../outside", filepath.Join(root, "pkg", "outside-rel"))
	os.Symlink(filepath.Join(root, "main.go"), filepath.Join(root, "abs-inside"))
	runGit(t, root, "add", ".gitignore", "main.go", "pkg/a.go", "pkg/deleted.go", "scripts/run.sh", "testdata")
	runGit(t, root, "commit", "-qm", "init")
	os.Remove(filepath.Join(root, "pkg/deleted.go"))
	writeFile(t, root, "untracked.txt", "new")
	// vendor-bin is ignored by git but requested via sync.include
	writeFile(t, root, ".gitignore", "build/\n*.log\nvendor-bin/\n")

	ws, err := Detect(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s := testScanner(t)
	m, err := s.Scan(ctx, ws, []string{"vendor-bin/"}, []string{"testdata/huge/"})
	if err != nil {
		t.Fatal(err)
	}
	got := manifestPaths(m)
	want := []string{".env.example", ".gitignore", "abs-inside", "inside-link", "main.go", "pkg/a.go", "scripts/run.sh", "untracked.txt", "vendor-bin/tool"}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("entries not sorted: %v", got)
	}
	if len(got) != len(want) {
		t.Fatalf("paths = %v\nwant    %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paths = %v\nwant    %v", got, want)
		}
	}
	em := entryMap(m)
	if e := em["main.go"]; e.Hash != sha("package main") || e.Size != 12 || e.Kind != proto.EntryFile || e.Mode != 0o644 {
		t.Fatalf("main.go entry %+v", e)
	}
	if e := em["scripts/run.sh"]; e.Mode != 0o755 {
		t.Fatalf("mode not preserved: %o", e.Mode)
	}
	if e := em["inside-link"]; e.Kind != proto.EntrySymlink || e.Target != "pkg/a.go" || e.Hash != "" {
		t.Fatalf("symlink entry %+v", e)
	}
	if e := em["abs-inside"]; e.Target != "main.go" {
		t.Fatalf("absolute in-root symlink should become relative: %+v", e)
	}
	fi, _ := os.Stat(filepath.Join(root, "main.go"))
	if em["main.go"].MTime != fi.ModTime().UnixNano() {
		t.Fatalf("mtime not nanosecond exact")
	}
	if m.RepoID != ws.RepoID || m.Root != root || len(m.Keep) == 0 {
		t.Fatalf("manifest header %+v", m)
	}

	// Open serves blob content by hash.
	rc, size, err := s.Open(sha("package main"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "package main" || size != 12 {
		t.Fatalf("Open = %q, %d", b, size)
	}
	if _, _, err := s.Open(sha("nope")); err == nil {
		t.Fatal("Open of unknown hash should fail")
	}
	// A file changed after the scan is refused rather than served wrong.
	writeFile(t, root, "main.go", "package main // changed")
	if _, _, err := s.Open(sha("package main")); err == nil {
		t.Fatal("Open of a changed file should fail")
	}
}

func TestScanHashCache(t *testing.T) {
	ctx := context.Background()
	root := newRepo(t)
	for i := 0; i < 50; i++ {
		writeFile(t, root, filepath.Join("d", string(rune('a'+i%26)), "f"+string(rune('a'+i/26))+".txt"), "content")
	}
	writeFile(t, root, "x.txt", "one")
	ws, _ := Detect(ctx, root)
	s := testScanner(t)
	if _, err := s.Scan(ctx, ws, nil, nil); err != nil {
		t.Fatal(err)
	}
	cache := loadCache(s.cacheDir, root)
	if len(cache) != 51 {
		t.Fatalf("cache has %d entries, want 51", len(cache))
	}

	// Poison the cache entry for an unchanged file: a cache hit must be
	// served from the cache (proving no rehash happened).
	ce := cache["x.txt"]
	copy(ce.hash[:], make([]byte, 32))
	cache["x.txt"] = ce
	if err := saveCache(s.cacheDir, root, cache); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Scan(ctx, ws, nil, nil)
	if h := entryMap(m)["x.txt"].Hash; h != hex.EncodeToString(make([]byte, 32)) {
		t.Fatalf("expected cached hash to be used, got %s", h)
	}

	// Changing content (and therefore mtime/ctime/size) forces a rehash.
	time.Sleep(10 * time.Millisecond)
	writeFile(t, root, "x.txt", "two!")
	m, _ = s.Scan(ctx, ws, nil, nil)
	if h := entryMap(m)["x.txt"].Hash; h != sha("two!") {
		t.Fatalf("changed file not rehashed: %s", h)
	}
	// Deleted files leave the cache.
	os.Remove(filepath.Join(root, "x.txt"))
	s.Scan(ctx, ws, nil, nil)
	if _, ok := loadCache(s.cacheDir, root)["x.txt"]; ok {
		t.Fatal("deleted file still cached")
	}
}

func TestScanRacyFilesNotCached(t *testing.T) {
	ctx := context.Background()
	root := newRepo(t)
	writeFile(t, root, "fresh.txt", "x")
	ws, _ := Detect(ctx, root)
	s := testScanner(t)
	s.now = time.Now // the file was just written: racy
	m, err := s.Scan(ctx, ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if entryMap(m)["fresh.txt"].Hash != sha("x") {
		t.Fatal("racy file must still be hashed")
	}
	if _, ok := loadCache(s.cacheDir, root)["fresh.txt"]; ok {
		t.Fatal("racy file must not be cached")
	}
}

func TestScanCorruptCacheIgnored(t *testing.T) {
	ctx := context.Background()
	root := newRepo(t)
	writeFile(t, root, "a.txt", "a")
	ws, _ := Detect(ctx, root)
	s := testScanner(t)
	s.Scan(ctx, ws, nil, nil)
	f := cacheFile(s.cacheDir, root)
	b, _ := os.ReadFile(f)
	b[len(b)/2] ^= 0xff
	os.WriteFile(f, b, 0o600)
	m, err := s.Scan(ctx, ws, nil, nil)
	if err != nil || entryMap(m)["a.txt"].Hash != sha("a") {
		t.Fatalf("corrupt cache should be ignored: %v", err)
	}
}

func TestScanSubmoduleAndNestedRepo(t *testing.T) {
	ctx := context.Background()
	lib := newRepo(t)
	writeFile(t, lib, "lib.go", "package lib")
	writeFile(t, lib, ".env", "x")
	runGit(t, lib, "add", ".")
	runGit(t, lib, "commit", "-qm", "lib")

	root := filepath.Join(filepath.Dir(lib), "super")
	os.MkdirAll(root, 0o755)
	runGit(t, root, "init", "-q")
	writeFile(t, root, "main.go", "package main")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "super")
	runGit(t, root, "submodule", "add", "-q", lib, "third_party/lib")
	runGit(t, root, "commit", "-qm", "sub")
	// An untracked nested repository is listed by git as "nested/".
	nested := filepath.Join(root, "nested")
	os.MkdirAll(nested, 0o755)
	runGit(t, nested, "init", "-q")
	writeFile(t, nested, "n.txt", "n")

	ws, _ := Detect(ctx, root)
	m, err := testScanner(t).Scan(ctx, ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	em := entryMap(m)
	for _, p := range []string{"main.go", ".gitmodules", "third_party/lib/lib.go", "nested/n.txt"} {
		if _, ok := em[p]; !ok {
			t.Errorf("missing %s in %v", p, manifestPaths(m))
		}
	}
	for _, p := range []string{"third_party/lib/.git", "third_party/lib/.env", "third_party/lib"} {
		if _, ok := em[p]; ok {
			t.Errorf("%s must not be synced", p)
		}
	}
}

func TestScanPlainDir(t *testing.T) {
	ctx := context.Background()
	root := realTempDir(t)
	writeFile(t, root, ".gitignore", "*.tmp\n/out/\n")
	writeFile(t, root, "a.txt", "a")
	writeFile(t, root, "x.tmp", "t")
	writeFile(t, root, "out/o", "o")
	writeFile(t, root, "sub/.gitignore", "!keep.tmp\nlocal/\n")
	writeFile(t, root, "sub/keep.tmp", "k")
	writeFile(t, root, "sub/local/z", "z")
	writeFile(t, root, "sub/out/o", "kept: /out/ is anchored to root")
	writeFile(t, root, "node_modules/m/i.js", "m")
	ws, _ := Detect(ctx, root)
	if ws.Git {
		t.Skip("temp dir is inside a git repository")
	}
	m, err := testScanner(t).Scan(ctx, ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := manifestPaths(m)
	want := []string{".gitignore", "a.txt", "sub/.gitignore", "sub/keep.tmp", "sub/out/o"}
	if len(got) != len(want) {
		t.Fatalf("paths = %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paths = %v want %v", got, want)
		}
	}
}

func TestScanIncludeGlobs(t *testing.T) {
	ctx := context.Background()
	root := newRepo(t)
	writeFile(t, root, ".gitignore", "gen/\n")
	writeFile(t, root, "gen/a/x.pb.go", "x")
	writeFile(t, root, "gen/a/x.txt", "x")
	writeFile(t, root, "gen/b/y.pb.go", "y")
	ws, _ := Detect(ctx, root)
	m, err := testScanner(t).Scan(ctx, ws, []string{"gen/**/*.pb.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	em := entryMap(m)
	if _, ok := em["gen/a/x.pb.go"]; !ok {
		t.Fatalf("include glob missed file: %v", manifestPaths(m))
	}
	if _, ok := em["gen/a/x.txt"]; ok {
		t.Fatal("include glob too broad")
	}
	if _, err := testScanner(t).Scan(ctx, ws, []string{"../etc"}, nil); err == nil {
		t.Fatal("include outside workspace must fail")
	}
}

func TestScanConcurrentScanners(t *testing.T) {
	ctx := context.Background()
	root := newRepo(t)
	for i := 0; i < 200; i++ {
		writeFile(t, root, filepath.Join("f", hex.EncodeToString([]byte{byte(i)})), "v")
	}
	ws, _ := Detect(ctx, root)
	dir := filepath.Join(realTempDir(t), "cache")
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			s, err := newScanner(dir)
			if err != nil {
				done <- err
				return
			}
			for j := 0; j < 5; j++ {
				m, err := s.Scan(ctx, ws, nil, nil)
				if err != nil {
					done <- err
					return
				}
				if len(m.Entries) != 200 {
					t.Errorf("got %d entries", len(m.Entries))
				}
			}
			done <- nil
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// sync.include comes from the repo's .kran.toml (untrusted): a symlinked
// directory in the repo must not let it pull files from outside, such as
// ~/.aws/credentials (SPEC §6.7: symlinks are never followed out).
func TestIncludeDoesNotFollowSymlinkedDirs(t *testing.T) {
	ctx := context.Background()
	outside := realTempDir(t)
	writeFile(t, outside, "credentials", "secret")
	writeFile(t, outside, "sub/more", "secret")
	root := newRepo(t)
	writeFile(t, root, "a.txt", "a")
	if err := os.Symlink(outside, filepath.Join(root, "cfg")); err != nil {
		t.Fatal(err)
	}
	ws, _ := Detect(ctx, root)
	for _, inc := range [][]string{{"cfg/credentials"}, {"cfg/*"}, {"cfg/sub/**"}, {"cfg/sub"}} {
		m, err := testScanner(t).Scan(ctx, ws, inc, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range m.Entries {
			if strings.HasPrefix(e.Path, "cfg/") {
				t.Fatalf("include %v synced %s from outside the workspace", inc, e.Path)
			}
		}
	}
}
