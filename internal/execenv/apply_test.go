package execenv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// fixture is a blob store plus helpers to build manifests from contents.
type fixture struct {
	t     *testing.T
	l     layout
	bs    *blobStore
	slot  string
	mtime int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	l := layout{state: dir}
	bs, err := newBlobStore(l, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, l: l, bs: bs, slot: filepath.Join(dir, "slot"), mtime: time.Date(2024, 5, 6, 7, 8, 9, 123456789, time.UTC).UnixNano()}
}

func (f *fixture) file(p, content string, mode uint32) proto.ManifestEntry {
	h := hashOf(content)
	if err := f.bs.put(context.Background(), h, int64(len(content)), strings.NewReader(content)); err != nil {
		f.t.Fatal(err)
	}
	f.mtime += 1001
	return proto.ManifestEntry{Path: p, Kind: proto.EntryFile, Mode: mode, Size: int64(len(content)), MTime: f.mtime, Hash: h}
}

func (f *fixture) link(p, target string) proto.ManifestEntry {
	f.mtime += 7
	return proto.ManifestEntry{Path: p, Kind: proto.EntrySymlink, Mode: 0o777, MTime: f.mtime, Target: target}
}

func (f *fixture) commit(m *proto.Manifest) *plan {
	f.t.Helper()
	p, err := commitLocal(f.slot, m, f.bs, 4, false)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// verifySlot checks that the slot matches the manifest exactly (content,
// mode, nanosecond mtime, links).
func verifySlot(t *testing.T, root string, m *proto.Manifest) {
	t.Helper()
	for _, e := range m.Entries {
		p := filepath.Join(root, filepath.FromSlash(e.Path))
		fi, err := os.Lstat(p)
		if err != nil {
			t.Errorf("%s: %v", e.Path, err)
			continue
		}
		switch e.Kind {
		case proto.EntryFile:
			b, _ := os.ReadFile(p)
			if hashOf(string(b)) != e.Hash {
				t.Errorf("%s: content mismatch", e.Path)
			}
			if uint32(fi.Mode().Perm()) != e.Mode {
				t.Errorf("%s: mode %o want %o", e.Path, fi.Mode().Perm(), e.Mode)
			}
			if fi.ModTime().UnixNano() != e.MTime {
				t.Errorf("%s: mtime %d want %d", e.Path, fi.ModTime().UnixNano(), e.MTime)
			}
		case proto.EntrySymlink:
			tg, _ := os.Readlink(p)
			if fi.Mode()&os.ModeSymlink == 0 || tg != e.Target {
				t.Errorf("%s: link %q want %q", e.Path, tg, e.Target)
			}
			if fi.ModTime().UnixNano() != e.MTime {
				t.Errorf("%s: link mtime %d want %d", e.Path, fi.ModTime().UnixNano(), e.MTime)
			}
		case proto.EntryDir:
			if !fi.IsDir() {
				t.Errorf("%s: not a dir", e.Path)
			}
		}
	}
}

func TestCommitLocalLifecycle(t *testing.T) {
	f := newFixture(t)
	m := &proto.Manifest{RepoID: "r", Keep: []string{"node_modules/", ".next/cache/"}, Entries: []proto.ManifestEntry{
		f.file("README.md", "hello", 0o644),
		f.file("bin/run.sh", "#!/bin/sh\necho hi\n", 0o755),
		f.file("pkg/a/a.go", "package a", 0o644),
		f.file("pkg/a/dup.go", "package a", 0o600), // same content, other path
		f.link("pkg/link", "a/a.go"),
		{Path: "empty/dir", Kind: proto.EntryDir, Mode: 0o750},
		f.file("old/file.txt", "old", 0o644),
		f.file("typechange", "was a file", 0o644),
	}}
	p := f.commit(m)
	if len(p.writes) != 6 || len(p.needed) != 5 || len(p.links) != 1 || len(p.dirs) != 1 {
		t.Fatalf("first plan: writes=%d needed=%d links=%d dirs=%d", len(p.writes), len(p.needed), len(p.links), len(p.dirs))
	}
	verifySlot(t, f.slot, m)

	// Unchanged manifest: nothing to do.
	p = f.commit(m)
	if p.changed() != 0 || len(p.deletes) != 0 {
		t.Fatalf("idempotent commit changed %d deleted %v", p.changed(), p.deletes)
	}

	// Things a job leaves behind: deps (kept), build output (deleted).
	os.MkdirAll(filepath.Join(f.slot, "node_modules", "dep"), 0o755)
	os.WriteFile(filepath.Join(f.slot, "node_modules", "dep", "i.js"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(f.slot, "web", ".next", "cache"), 0o755)
	os.WriteFile(filepath.Join(f.slot, "web", ".next", "cache", "c"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(f.slot, "web", ".next", "build-manifest.json"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(f.slot, "stray.o"), []byte("x"), 0o644)
	// A job modified a tracked file: must be restored.
	os.WriteFile(filepath.Join(f.slot, "README.md"), []byte("tampered"), 0o644)

	m2 := &proto.Manifest{RepoID: "r", Keep: m.Keep, Entries: []proto.ManifestEntry{
		m.Entries[0],
		m.Entries[1],
		f.file("pkg/a/a.go", "package a // v2", 0o644),
		m.Entries[3],
		f.link("pkg/link", "a/dup.go"),
		{Path: "empty/dir", Kind: proto.EntryDir, Mode: 0o755},
		f.file("typechange/now/a/dir.txt", "x", 0o644),
	}}
	m2.Entries[1].Mode = 0o700 // mode-only change
	p = f.commit(m2)
	if len(p.chmods) != 1 {
		t.Fatalf("expected one chmod, got %d", len(p.chmods))
	}
	verifySlot(t, f.slot, m2)
	for _, gone := range []string{"old/file.txt", "old", "stray.o", "web/.next/build-manifest.json"} {
		if _, err := os.Lstat(filepath.Join(f.slot, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted", gone)
		}
	}
	for _, kept := range []string{"node_modules/dep/i.js", "web/.next/cache/c"} {
		if _, err := os.Lstat(filepath.Join(f.slot, kept)); err != nil {
			t.Errorf("%s should be kept: %v", kept, err)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(f.slot, "*", tmpPrefix+"*"))
	if len(leftovers) > 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}

	// dir -> file and link -> file
	m3 := &proto.Manifest{RepoID: "r", Entries: []proto.ManifestEntry{
		f.file("typechange", "file again", 0o644),
		f.file("pkg/link", "now a file", 0o644),
	}}
	f.commit(m3)
	verifySlot(t, f.slot, m3)
	// Keep comes from each manifest: without it node_modules is an extra.
	if _, err := os.Lstat(filepath.Join(f.slot, "node_modules")); !os.IsNotExist(err) {
		t.Fatal("node_modules not in Keep must be deleted")
	}
}

func TestCommitRejectsUnsafeManifests(t *testing.T) {
	f := newFixture(t)
	good := f.file("ok", "x", 0o644)
	bad := []proto.ManifestEntry{
		{Path: "../escape", Kind: proto.EntryFile, Hash: good.Hash},
		{Path: "/abs", Kind: proto.EntryFile, Hash: good.Hash},
		{Path: "a/../../b", Kind: proto.EntryFile, Hash: good.Hash},
		{Path: "a//b", Kind: proto.EntryFile, Hash: good.Hash},
		{Path: "./a", Kind: proto.EntryFile, Hash: good.Hash},
		{Path: "", Kind: proto.EntryFile, Hash: good.Hash},
		{Path: "l", Kind: proto.EntrySymlink, Target: "/etc"},
		{Path: "a/l", Kind: proto.EntrySymlink, Target: "../../etc"},
		{Path: "h", Kind: proto.EntryFile, Hash: "nothex"},
		{Path: "x/" + tmpPrefix + "1", Kind: proto.EntryFile, Hash: good.Hash},
	}
	for _, e := range bad {
		m := &proto.Manifest{Entries: []proto.ManifestEntry{e}}
		if _, err := commitLocal(f.slot, m, f.bs, 1, false); !errors.Is(err, ErrBadManifest) {
			t.Errorf("%+v: err = %v, want ErrBadManifest", e, err)
		}
	}
	dup := &proto.Manifest{Entries: []proto.ManifestEntry{good, good}}
	if _, err := commitLocal(f.slot, dup, f.bs, 1, false); !errors.Is(err, ErrBadManifest) {
		t.Error("duplicate paths must be rejected")
	}
	conflict := &proto.Manifest{Entries: []proto.ManifestEntry{good, f.file("ok/child", "y", 0o644)}}
	if _, err := commitLocal(f.slot, conflict, f.bs, 1, false); !errors.Is(err, ErrBadManifest) {
		t.Error("file/dir conflict must be rejected")
	}
	ci := &proto.Manifest{Entries: []proto.ManifestEntry{f.file("Readme", "a", 0o644), f.file("README", "b", 0o644)}}
	if _, err := commitLocal(f.slot, ci, f.bs, 1, true); !errors.Is(err, ErrBadManifest) {
		t.Error("case collisions must be rejected on case-insensitive targets")
	}
	if _, err := commitLocal(f.slot, ci, f.bs, 1, false); err != nil {
		t.Errorf("case collisions are fine on case-sensitive targets: %v", err)
	}
}

func TestCommitNeverFollowsSymlinks(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "victim"), []byte("precious"), 0o644)

	// A job (or an earlier manifest) left symlinks pointing outside.
	os.MkdirAll(f.slot, 0o755)
	os.Symlink(outside, filepath.Join(f.slot, "evil"))
	os.Symlink(filepath.Join(outside, "victim"), filepath.Join(f.slot, "victimlink"))
	// A kept dir that is really a symlink out of the slot.
	os.Symlink(outside, filepath.Join(f.slot, "node_modules"))

	m := &proto.Manifest{Keep: []string{"node_modules/"}, Entries: []proto.ManifestEntry{
		f.file("evil/victim", "pwned", 0o644),
		f.file("victimlink", "pwned", 0o644),
		f.file("node_modules/victim", "pwned", 0o644), // origin never sends this, a hostile one might
	}}
	f.commit(m)
	b, _ := os.ReadFile(filepath.Join(outside, "victim"))
	if string(b) != "precious" {
		t.Fatalf("wrote outside the slot: %q", b)
	}
	verifySlot(t, f.slot, m)
	fi, _ := os.Lstat(filepath.Join(f.slot, "evil"))
	if !fi.IsDir() {
		t.Fatal("symlinked parent must be replaced by a real directory")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Fatalf("outside dir changed: %v", ents)
	}
}

func TestCommitMissingAndTornBlobs(t *testing.T) {
	f := newFixture(t)
	e := f.file("a", "content", 0o644)
	missing := proto.ManifestEntry{Path: "b", Kind: proto.EntryFile, Mode: 0o644, Size: 3, Hash: hashOf("zzz")}
	if _, err := commitLocal(f.slot, &proto.Manifest{Entries: []proto.ManifestEntry{e, missing}}, f.bs, 1, false); !errors.Is(err, ErrMissingBlob) {
		t.Fatalf("err = %v, want ErrMissingBlob", err)
	}
	// Torn blob: right name, wrong size.
	p := f.bs.path(e.Hash)
	os.Chmod(p, 0o600)
	os.WriteFile(p, []byte("con"), 0o600)
	if _, err := commitLocal(f.slot, &proto.Manifest{Entries: []proto.ManifestEntry{e}}, f.bs, 1, false); !errors.Is(err, ErrMissingBlob) {
		t.Fatalf("err = %v, want ErrMissingBlob for torn blob", err)
	}
	if f.bs.has(e.Hash) {
		t.Fatal("torn blob should be dropped")
	}
}

func TestPutBlob(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := hashOf("data")
	if err := f.bs.put(ctx, h, 4, strings.NewReader("datatrailing")); err != nil {
		t.Fatal(err)
	}
	// Existing blob: still consumes exactly size bytes.
	r := strings.NewReader("dataNEXT")
	if err := f.bs.put(ctx, h, 4, r); err != nil {
		t.Fatal(err)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "NEXT" {
		t.Fatalf("stream out of sync: %q", rest)
	}
	if err := f.bs.put(ctx, hashOf("other"), 5, strings.NewReader("wrong")); !errors.Is(err, ErrBlobMismatch) {
		t.Fatalf("err = %v, want mismatch", err)
	}
	if err := f.bs.put(ctx, hashOf("short"), 5, strings.NewReader("sho")); err == nil {
		t.Fatal("short read must fail")
	}
	if err := f.bs.put(ctx, "../../etc", 1, strings.NewReader("x")); err == nil {
		t.Fatal("bad hash must fail")
	}
	if ents, _ := os.ReadDir(f.l.blobTmp()); len(ents) != 0 {
		t.Fatalf("partial blobs left behind: %d", len(ents))
	}
	if f.bs.has(hashOf("other")) || f.bs.has(hashOf("short")) {
		t.Fatal("rejected blobs must not be stored")
	}
}

func TestBlobTrimLRU(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var hashes []string
	for i := 0; i < 10; i++ {
		c := strings.Repeat(string(rune('a'+i)), 100)
		h := hashOf(c)
		f.bs.put(ctx, h, 100, strings.NewReader(c))
		old := time.Now().Add(time.Duration(i-20) * time.Hour)
		os.Chtimes(f.bs.path(h), old, old)
		hashes = append(hashes, h)
	}
	f.bs.touch(hashes[0]) // most recently used now
	n, err := f.bs.trim(500)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 { // down to 90% of 500 = 450 → 4 blobs remain
		t.Fatalf("trimmed %d, want 6", n)
	}
	if !f.bs.has(hashes[0]) || !f.bs.has(hashes[9]) || f.bs.has(hashes[1]) {
		t.Fatal("LRU order not respected")
	}
	if n, _ := f.bs.trim(10_000); n != 0 {
		t.Fatal("no trim below cap")
	}
}

func TestStreamApplyMatchesLocal(t *testing.T) {
	f := newFixture(t)
	m := &proto.Manifest{Keep: []string{"node_modules/"}, Entries: []proto.ManifestEntry{
		f.file("a.txt", "alpha", 0o644),
		f.file("dir/b.txt", "alpha", 0o755), // dedup within the stream
		f.file("dir/big.bin", strings.Repeat("0123456789", 100_000), 0o644),
		f.link("dir/l", "../a.txt"),
		{Path: "e", Kind: proto.EntryDir, Mode: 0o700},
	}}
	base := filepath.Join(t.TempDir(), "slots")
	repo := strings.Repeat("ab", 32)
	open := func(h string) (io.ReadCloser, int64, error) {
		fh, err := os.Open(f.bs.path(h))
		if err != nil {
			return nil, 0, err
		}
		fi, _ := fh.Stat()
		return fh, fi.Size(), nil
	}
	runReq := func(req applyReq, proceed func(applyNeed) bool) (applyNeed, applyResult, error) {
		toApply, hostW := io.Pipe()
		hostR, fromApply := io.Pipe()
		done := make(chan error, 1)
		go func() {
			err := serveApply(base, toApply, fromApply)
			fromApply.Close()
			toApply.CloseWithError(io.ErrClosedPipe)
			done <- err
		}()
		need, res, err := driveApply(context.Background(), hostW, hostR, req, open, proceed)
		hostW.Close()
		if serr := <-done; serr != nil && err == nil {
			err = serr
		}
		return need, res, err
	}
	run := func(op string, m *proto.Manifest) (applyNeed, applyResult, error) {
		return runReq(applyReq{Op: op, Slot: repo + "/0", Manifest: m}, nil)
	}
	need, _, err := run("check", m)
	if err != nil || len(need.Needed) != 2 {
		t.Fatalf("check: %v need=%v", err, need.Needed)
	}
	if _, err := os.Stat(filepath.Join(base, repo, "0", "a.txt")); err == nil {
		t.Fatal("check must not write")
	}
	if _, res, err := run("commit", m); err != nil || res.Changed != 5 {
		t.Fatalf("commit: %v %+v", err, res)
	}
	verifySlot(t, filepath.Join(base, repo, "0"), m)
	need, _, _ = run("check", m)
	if len(need.Needed) != 0 || need.Changed != 0 {
		t.Fatalf("after commit check wants %+v", need)
	}
	// Op sync with the compact manifest: stop leaves the slot alone, go
	// repairs it in the same run.
	sorted := *m
	sorted.Entries = append([]proto.ManifestEntry(nil), m.Entries...)
	sort.Slice(sorted.Entries, func(i, j int) bool { return sorted.Entries[i].Path < sorted.Entries[j].Path })
	slotA := filepath.Join(base, repo, "0", "a.txt")
	os.WriteFile(slotA, []byte("tampered"), 0o644)
	syncReq := applyReq{Op: "sync", Slot: repo + "/0", Manifest: &sorted, Compact: true}
	need, _, err = runReq(syncReq, func(applyNeed) bool { return false })
	if err != nil || len(need.Needed) != 1 {
		t.Fatalf("sync stop: %v %+v", err, need)
	}
	if b, _ := os.ReadFile(slotA); string(b) != "tampered" {
		t.Fatal("sync changed the slot although the host said stop")
	}
	if _, res, err := runReq(syncReq, func(applyNeed) bool { return true }); err != nil || res.Changed != 1 {
		t.Fatalf("sync go: %v %+v", err, res)
	}
	verifySlot(t, filepath.Join(base, repo, "0"), m)
	// Host cannot provide a blob: abort with a clear error.
	m2 := &proto.Manifest{Entries: []proto.ManifestEntry{{Path: "x", Kind: proto.EntryFile, Mode: 0o644, Size: 1, Hash: hashOf("q"), MTime: 1}}}
	if _, _, err := run("commit", m2); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("abort: %v", err)
	}
	// Bad slot names never resolve outside base.
	for _, s := range []string{"../x/0", repo + "/../../0", repo + "/01", repo} {
		toApply, hostW := io.Pipe()
		var out bytes.Buffer
		go func() { writeAFJSON(hostW, afReq, applyReq{Op: "remove", Slot: s}); hostW.Close() }()
		serveApply(base, toApply, &out)
		_, b, _ := readAF(&out)
		if !strings.Contains(string(b), `"ok":false`) {
			t.Errorf("slot %q accepted: %s", s, b)
		}
	}
	if _, res, err := run("remove", nil); err != nil || !res.OK {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, repo, "0")); !os.IsNotExist(err) {
		t.Fatal("remove did not remove")
	}
}
