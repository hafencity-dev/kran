package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// racyWindow: files modified this recently are hashed but not cached, so a
// write landing in the same timestamp granularity as our stat can never
// leave a stale hash behind (git's "racy clean" problem).
const racyWindow = 2 * time.Second

type scanner struct {
	cacheDir string
	now      func() time.Time

	mu    sync.Mutex
	blobs map[string]map[string]blobRef // root → hash → file
}

type blobRef struct {
	root, rel string // joined only when opened
	size      int64
	mtime     int64
}

func newScanner(cacheDir string) (Scanner, error) {
	if cacheDir == "" {
		return nil, errors.New("workspace: scanner needs a cache dir")
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("hash cache dir %s: %w", cacheDir, err)
	}
	return &scanner{cacheDir: cacheDir, now: time.Now, blobs: map[string]map[string]blobRef{}}, nil
}

func (s *scanner) Close() error { return nil }

// candidate is a path that passed the filters, with its lstat result.
type candidate struct {
	rel string
	fi  os.FileInfo
}

func (s *scanner) Scan(ctx context.Context, ws proto.Workspace, include, exclude []string) (*proto.Manifest, error) {
	root := ws.Root
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("scan: workspace root %q is not absolute", root)
	}
	start := s.now()
	excl := NewMatcher(exclude)
	keep := func(rel string) bool { return !HardExcluded(rel, false) && !excl.Match(rel, false) }

	var names []string
	var err error
	if ws.Git {
		names, err = gitList(ctx, root, "")
	} else {
		names, err = plainWalk(ctx, root)
	}
	if err != nil {
		return nil, err
	}
	inc, err := includeList(ctx, root, include)
	if err != nil {
		return nil, err
	}
	names = append(names, inc...)

	cands, err := s.collect(ctx, root, ws.Git, names, keep)
	if err != nil {
		return nil, err
	}

	cache := loadCache(s.cacheDir, root)
	entries := make([]proto.ManifestEntry, 0, len(cands))
	newCache := make(map[string]cacheEntry, len(cands))
	var toHash []int // indexes into entries
	keys := make([]statKey, 0, len(cands))
	racyBefore := start.Add(-racyWindow).UnixNano()
	dirty := false

	for _, c := range cands {
		mode := c.fi.Mode()
		switch {
		case mode.IsRegular():
			k := statKeyOf(c.fi)
			e := proto.ManifestEntry{Path: c.rel, Kind: proto.EntryFile, Mode: uint32(mode.Perm()), Size: k.size, MTime: k.mtime}
			if ce, ok := cache[c.rel]; ok && ce.key == k {
				e.Hash = hex.EncodeToString(ce.hash[:])
				newCache[c.rel] = ce
			} else {
				toHash = append(toHash, len(entries))
				dirty = true
			}
			entries = append(entries, e)
			keys = append(keys, k)
		case mode&os.ModeSymlink != 0:
			target, ok := safeLinkTarget(root, c.rel)
			if !ok {
				continue
			}
			entries = append(entries, proto.ManifestEntry{
				Path: c.rel, Kind: proto.EntrySymlink, Mode: uint32(mode.Perm()),
				MTime: c.fi.ModTime().UnixNano(), Target: target,
			})
			keys = append(keys, statKey{})
		default:
			// sockets, fifos, devices: not content, never synced
		}
	}
	if len(newCache) != len(cache) {
		dirty = true
	}

	if err := hashFiles(ctx, root, entries, toHash); err != nil {
		return nil, err
	}
	for _, i := range toHash {
		k := keys[i]
		if k.mtime >= racyBefore || k.ctime >= racyBefore {
			continue
		}
		var ce cacheEntry
		ce.key = k
		if _, err := hex.Decode(ce.hash[:], []byte(entries[i].Hash)); err == nil {
			newCache[entries[i].Path] = ce
		}
	}
	if dirty {
		// A failed cache write only costs rehashing next time.
		_ = saveCache(s.cacheDir, root, newCache)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	s.remember(root, entries)
	return &proto.Manifest{
		RepoID:  ws.RepoID,
		Root:    root,
		Entries: entries,
		Keep:    append([]string(nil), DefaultKeep...),
	}, nil
}

// collect lstats names in parallel, drops filtered paths and recurses into
// directories that git reported as single entries (submodules and nested
// repositories).
func (s *scanner) collect(ctx context.Context, root string, gitMode bool, names []string, keep func(string) bool) ([]candidate, error) {
	seen := make(map[string]bool, len(names))
	realDir := map[string]bool{} // dir rel → a real directory, not a symlink
	var out []candidate
	pending := names
	for depth := 0; len(pending) > 0; depth++ {
		if depth > 8 {
			break // nested submodules deeper than this are not real-world
		}
		var batch []string
		for _, n := range pending {
			n = strings.TrimSuffix(n, "/")
			if n == "" || seen[n] || !keep(n) {
				continue
			}
			seen[n] = true
			batch = append(batch, n)
		}
		if err := statDirs(ctx, root, batch, realDir); err != nil {
			return nil, err
		}
		kept := batch[:0]
		for _, n := range batch {
			if !throughSymlink(root, n, realDir) {
				kept = append(kept, n)
			}
		}
		batch = kept
		fis, err := lstatAll(ctx, root, batch)
		if err != nil {
			return nil, err
		}
		pending = nil
		for i, fi := range fis {
			if fi == nil {
				continue // listed but deleted from the work tree
			}
			if fi.IsDir() {
				if !gitMode {
					continue
				}
				sub := batch[i]
				if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(sub), ".git")); err != nil {
					continue // uninitialized submodule: nothing to send
				}
				more, err := gitList(ctx, root, sub)
				if err != nil {
					return nil, err
				}
				pending = append(pending, more...)
				continue
			}
			out = append(out, candidate{rel: batch[i], fi: fi})
		}
	}
	return out, nil
}

// throughSymlink reports whether a parent directory of rel is a symlink (or
// not a directory): Lstat resolves every component but the last, so such a
// path reads files from wherever the link points (sync.include "cfg/x" with
// cfg -> ~/.aws). known caches verdicts per directory.
func throughSymlink(root, rel string, known map[string]bool) bool {
	for i := 0; i < len(rel); i++ {
		if rel[i] != '/' {
			continue
		}
		dir := rel[:i]
		ok, seen := known[dir]
		if !seen {
			fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir)))
			ok = err == nil && fi.IsDir()
			known[dir] = ok
		}
		if !ok {
			return true
		}
	}
	return false
}

// statDirs fills known with the verdict for every parent directory of
// rels not seen yet, with the lstats in parallel (a repository has
// thousands of directories; one by one they cost tens of milliseconds).
func statDirs(ctx context.Context, root string, rels []string, known map[string]bool) error {
	var dirs []string
	for _, rel := range rels {
		for i := len(rel) - 1; i >= 0; i-- {
			if rel[i] != '/' {
				continue
			}
			d := rel[:i]
			if _, ok := known[d]; ok {
				break // and so are its parents
			}
			known[d] = false // placeholder, set below
			dirs = append(dirs, d)
		}
	}
	fis, err := lstatAll(ctx, root, dirs)
	if err != nil {
		return err
	}
	for i, d := range dirs {
		known[d] = fis[i] != nil && fis[i].IsDir()
	}
	return nil
}

// lstatAll stats rel paths in parallel; missing files yield nil.
func lstatAll(ctx context.Context, root string, rels []string) ([]os.FileInfo, error) {
	out := make([]os.FileInfo, len(rels))
	workers := runtime.GOMAXPROCS(0)
	if workers > 16 {
		workers = 16
	}
	const chunk = 256
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for lo := range next {
				hi := min(lo+chunk, len(rels))
				for i := lo; i < hi; i++ {
					fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rels[i])))
					if err == nil {
						out[i] = fi
					}
				}
			}
		}()
	}
	var err error
feed:
	for lo := 0; lo < len(rels); lo += chunk {
		select {
		case next <- lo:
		case <-ctx.Done():
			err = ctx.Err()
			break feed
		}
	}
	close(next)
	wg.Wait()
	return out, err
}

// hashFiles fills entries[i].Hash for each i in idx, with parallelism
// bounded by half the CPUs so a scan never saturates the origin.
func hashFiles(ctx context.Context, root string, entries []proto.ManifestEntry, idx []int) error {
	if len(idx) == 0 {
		return nil
	}
	workers := max(1, runtime.NumCPU()/2)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		first   error
	)
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 256<<10)
			for i := range next {
				e := &entries[i]
				sum, size, err := hashFile(filepath.Join(root, filepath.FromSlash(e.Path)), buf)
				if err != nil {
					errOnce.Do(func() { first = fmt.Errorf("hash %s: %w", e.Path, err); cancel() })
					continue
				}
				e.Hash = sum
				e.Size = size
			}
		}()
	}
feed:
	for _, i := range idx {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

func hashFile(p string, buf []byte) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, f, buf)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// safeLinkTarget returns the link target as a relative path that stays
// inside root, or false when the link points outside (never synced: the
// target must not gain a path out of its slot).
func safeLinkTarget(root, rel string) (string, bool) {
	t, err := os.Readlink(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil || t == "" {
		return "", false
	}
	if filepath.IsAbs(t) {
		r, err := filepath.Rel(root, t)
		if err != nil {
			return "", false
		}
		// Re-express relative to the link's own directory.
		r, err = filepath.Rel(filepath.Dir(filepath.Join(root, filepath.FromSlash(rel))), filepath.Join(root, r))
		if err != nil {
			return "", false
		}
		t = r
	}
	t = filepath.ToSlash(t)
	if !LinkStaysInside(rel, t) {
		return "", false
	}
	return t, true
}

// LinkStaysInside reports whether a relative symlink target, resolved from
// the directory of the link at rel, stays within the root. Both are slash
// paths. Used by the origin (what to send) and the target (what to accept).
func LinkStaysInside(rel, target string) bool {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return false
	}
	dir := path.Dir(rel)
	if dir == "." {
		dir = ""
	}
	// Only "../" steps first, then names: the link's own parent dirs are
	// real directories, but a name in the target may itself be a link
	// (d/up -> ..), so a ".." after it (x -> d/up/..) would climb out.
	// Links reached through names are checked by this same rule.
	depth := 0
	if dir != "" {
		depth = strings.Count(dir, "/") + 1
	}
	named := false
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
		case "..":
			if named {
				return false
			}
			depth--
			if depth < 0 {
				return false
			}
		default:
			named = true
		}
	}
	return true
}

func (s *scanner) remember(root string, entries []proto.ManifestEntry) {
	m := make(map[string]blobRef, len(entries))
	for _, e := range entries {
		if e.Kind == proto.EntryFile && e.Hash != "" {
			m[e.Hash] = blobRef{root: root, rel: e.Path, size: e.Size, mtime: e.MTime}
		}
	}
	s.mu.Lock()
	s.blobs[root] = m
	s.mu.Unlock()
}

// ErrBlobChanged reports that the file behind a hash changed after the scan;
// the caller should rescan.
var ErrBlobChanged = errors.New("file changed since scan")

func (s *scanner) Open(hash string) (ReadSeekCloser, int64, error) {
	s.mu.Lock()
	var refs []blobRef
	for _, m := range s.blobs {
		if r, ok := m[hash]; ok {
			refs = append(refs, r)
		}
	}
	s.mu.Unlock()
	if len(refs) == 0 {
		return nil, 0, fmt.Errorf("blob %s: %w", hash, os.ErrNotExist)
	}
	var last error
	for _, r := range refs {
		p := filepath.Join(r.root, filepath.FromSlash(r.rel))
		f, err := os.Open(p)
		if err != nil {
			last = err
			continue
		}
		fi, err := f.Stat()
		if err != nil || fi.Size() != r.size || fi.ModTime().UnixNano() != r.mtime {
			f.Close()
			last = fmt.Errorf("%s: %w", p, ErrBlobChanged)
			continue
		}
		return f, r.size, nil
	}
	return nil, 0, fmt.Errorf("blob %s: %w", hash, last)
}
