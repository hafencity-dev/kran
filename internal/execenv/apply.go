package execenv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/workspace"
)

// The apply engine materializes a manifest into a slot directory. It runs
// in-process for slots on the host filesystem and inside the kran-sync
// helper container (`kran internal apply`) for slots in a Docker volume.
//
// Safety: every file operation goes through directory file descriptors
// opened component by component with O_NOFOLLOW, so neither a malicious
// manifest nor a symlink left behind by a job can make kran write outside
// the slot. Manifest paths with "..", absolute paths and symlink targets
// that escape the root are rejected up front.

const tmpPrefix = ".kran-tmp-"

// existing describes what is currently in the slot at a path.
type existing struct {
	kind   proto.EntryKind
	mode   uint32
	size   int64
	mtime  int64
	target string
}

type plan struct {
	m       *proto.Manifest
	writes  []int    // file entries needing content
	chmods  []int    // file entries whose content is current but mode differs
	links   []int    // symlinks to (re)create or re-time
	dirs    []int    // explicit empty-dir entries to ensure
	deletes []string // extras, deepest first
	rmTrees []string // dirs standing where a file or link must go
	needed  []string // unique hashes of writes, in first-use order
}

func (p *plan) changed() int { return len(p.writes) + len(p.chmods) + len(p.links) + len(p.dirs) }

// validRel checks a manifest path: relative, slash separated, no empty,
// "." or ".." components, no NUL.
func validRel(p string) bool {
	if p == "" || strings.IndexByte(p, 0) >= 0 {
		return false
	}
	for start := 0; start <= len(p); {
		end := strings.IndexByte(p[start:], '/')
		if end < 0 {
			end = len(p)
		} else {
			end += start
		}
		if c := p[start:end]; c == "" || c == "." || c == ".." {
			return false
		}
		start = end + 1
	}
	return true
}

// manifestIndex is what the plan needs to know about a manifest's paths.
type manifestIndex struct {
	kind    map[string]proto.EntryKind // every entry
	parents map[string]bool            // directories that hold entries
}

// validateManifest rejects anything the apply engine must never do.
func validateManifest(m *proto.Manifest, caseInsensitive bool) error {
	_, err := checkManifest(m, caseInsensitive)
	return err
}

// checkManifest validates m and indexes its paths.
func checkManifest(m *proto.Manifest, caseInsensitive bool) (*manifestIndex, error) {
	ix := &manifestIndex{kind: make(map[string]proto.EntryKind, len(m.Entries)), parents: map[string]bool{}}
	var folded map[string]string
	if caseInsensitive {
		folded = make(map[string]string, len(m.Entries))
	}
	for _, e := range m.Entries {
		if !validRel(e.Path) {
			return nil, fmt.Errorf("%w: unsafe path %q", ErrBadManifest, e.Path)
		}
		if strings.HasPrefix(e.Path[strings.LastIndexByte(e.Path, '/')+1:], tmpPrefix) {
			return nil, fmt.Errorf("%w: reserved name %q", ErrBadManifest, e.Path)
		}
		if _, dup := ix.kind[e.Path]; dup {
			return nil, fmt.Errorf("%w: duplicate path %q", ErrBadManifest, e.Path)
		}
		ix.kind[e.Path] = e.Kind
		switch e.Kind {
		case proto.EntryFile:
			if err := validHash(e.Hash); err != nil {
				return nil, fmt.Errorf("%w: %s: %v", ErrBadManifest, e.Path, err)
			}
			if e.Size < 0 {
				return nil, fmt.Errorf("%w: %s: negative size", ErrBadManifest, e.Path)
			}
		case proto.EntrySymlink:
			if !workspace.LinkStaysInside(e.Path, e.Target) {
				return nil, fmt.Errorf("%w: symlink %q points outside the workspace (%q)", ErrBadManifest, e.Path, e.Target)
			}
		case proto.EntryDir:
		default:
			return nil, fmt.Errorf("%w: %s: unknown entry kind %d", ErrBadManifest, e.Path, e.Kind)
		}
		if folded != nil {
			k := strings.ToLower(e.Path)
			if other, ok := folded[k]; ok {
				return nil, fmt.Errorf("%w: %q and %q differ only in case, which this node's filesystem cannot hold; run the job on a Linux node or rename one of them", ErrBadManifest, other, e.Path)
			}
			folded[k] = e.Path
		}
		for d := e.Path; ; {
			i := strings.LastIndexByte(d, '/')
			if i < 0 {
				break
			}
			d = d[:i]
			if ix.parents[d] {
				break
			}
			ix.parents[d] = true
		}
	}
	// A path used as a directory by another entry must be a directory.
	for d := range ix.parents {
		if k, ok := ix.kind[d]; ok && k != proto.EntryDir {
			return nil, fmt.Errorf("%w: %q is both a file and a directory", ErrBadManifest, d)
		}
	}
	return ix, nil
}

// keepMatcher compiles Manifest.Keep (gitignore-style; "node_modules/"
// matches at any depth).
// Unlike .gitignore, entries match at any depth even when they contain a
// slash (".next/cache/" also keeps apps/web/.next/cache in a monorepo);
// a leading "/" anchors an entry at the slot root.
func keepMatcher(m *proto.Manifest) *workspace.Matcher {
	pats := make([]string, 0, len(m.Keep))
	for _, k := range m.Keep {
		k = strings.TrimSpace(k)
		switch {
		case k == "" || strings.HasPrefix(k, "#") || strings.HasPrefix(k, "!"):
			continue
		case strings.HasPrefix(k, "/") || strings.HasPrefix(k, "**/"):
			pats = append(pats, k)
		default:
			pats = append(pats, "**/"+k)
		}
	}
	return workspace.NewMatcher(pats)
}

// scanSlot lists what is in the slot, skipping kept dirs. Only paths for
// which want returns true are stat'ed (kind, mode, size, mtime, link
// target); for the others, extras to delete, the kind from the directory
// listing is enough. Directories are listed in parallel: on a loaded
// machine or APFS the per-file system calls dominate an unchanged sync.
func scanSlot(root string, keep *workspace.Matcher, want func(rel string) bool) (map[string]existing, error) {
	sc := &slotScan{root: root, keep: keep, want: want, state: map[string]existing{},
		sem: make(chan struct{}, min(16, max(2, runtime.GOMAXPROCS(0))))}
	sc.wg.Add(1)
	sc.dir("")
	sc.wg.Wait()
	if sc.err != nil {
		if errors.Is(sc.err, fs.ErrNotExist) || errors.Is(sc.err, unix.ENOTDIR) {
			return map[string]existing{}, nil
		}
		return nil, fmt.Errorf("scan slot: %w", sc.err)
	}
	return sc.state, nil
}

type slotScan struct {
	root string
	keep *workspace.Matcher
	want func(string) bool
	sem  chan struct{}
	wg   sync.WaitGroup

	mu    sync.Mutex
	state map[string]existing
	err   error // listing the root failed
}

type scanned struct {
	rel string
	ex  existing
}

func (sc *slotScan) dir(rel string) {
	defer sc.wg.Done()
	abs := sc.root
	if rel != "" {
		abs = filepath.Join(sc.root, filepath.FromSlash(rel))
	}
	// Never through a symlink: a job may have swapped a directory for one.
	fd, err := unix.Open(abs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if rel == "" {
			sc.err = &fs.PathError{Op: "open", Path: abs, Err: err}
		}
		return
	}
	f := os.NewFile(uintptr(fd), abs)
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil && rel == "" {
		sc.err = err
		return
	}
	out := make([]scanned, 0, len(ents))
	for _, d := range ents {
		r := d.Name()
		if rel != "" {
			r = rel + "/" + r
		}
		isDir := d.IsDir()
		if sc.keep.Match(r, isDir) {
			continue
		}
		var ex existing
		if sc.want(r) {
			fi, err := d.Info()
			if err != nil {
				continue // vanished meanwhile
			}
			ex = existing{kind: kindOf(fi.Mode()), mode: uint32(fi.Mode().Perm()), size: fi.Size(), mtime: fi.ModTime().UnixNano()}
			if ex.kind == proto.EntrySymlink {
				ex.target, _ = os.Readlink(filepath.Join(abs, d.Name()))
			}
			isDir = ex.kind == proto.EntryDir
		} else {
			ex.kind = kindOf(d.Type())
		}
		out = append(out, scanned{r, ex})
		if isDir {
			sc.wg.Add(1)
			select {
			case sc.sem <- struct{}{}:
				go func(r string) {
					defer func() { <-sc.sem }()
					sc.dir(r)
				}(r)
			default:
				sc.dir(r)
			}
		}
	}
	sc.mu.Lock()
	for _, e := range out {
		sc.state[e.rel] = e.ex
	}
	sc.mu.Unlock()
}

func kindOf(m fs.FileMode) proto.EntryKind {
	switch {
	case m.IsDir():
		return proto.EntryDir
	case m&fs.ModeSymlink != 0:
		return proto.EntrySymlink
	case m.IsRegular():
		return proto.EntryFile
	}
	return 255 // fifo/socket/device: always an extra
}

func computePlan(m *proto.Manifest, ix *manifestIndex, state map[string]existing) *plan {
	p := &plan{m: m}
	seenHash := map[string]bool{}
	for i, e := range m.Entries {
		ex, ok := state[e.Path]
		if ok && ex.kind != e.Kind {
			if ex.kind == proto.EntryDir {
				p.rmTrees = append(p.rmTrees, e.Path)
			} else if e.Kind == proto.EntryDir {
				p.deletes = append(p.deletes, e.Path)
			}
			// file <-> symlink is replaced atomically by rename
			ok = false
		}
		switch e.Kind {
		case proto.EntryFile:
			switch {
			case !ok || ex.size != e.Size || ex.mtime != e.MTime:
				p.writes = append(p.writes, i)
				if !seenHash[e.Hash] {
					seenHash[e.Hash] = true
					p.needed = append(p.needed, e.Hash)
				}
			case ex.mode != e.Mode&0o777:
				p.chmods = append(p.chmods, i)
			}
		case proto.EntrySymlink:
			if !ok || ex.target != e.Target || ex.mtime != e.MTime {
				p.links = append(p.links, i)
			}
		case proto.EntryDir:
			if !ok || ex.mode != e.Mode&0o777 {
				p.dirs = append(p.dirs, i)
			}
		}
	}
	for rel, ex := range state {
		if _, ok := ix.kind[rel]; ok {
			continue
		}
		if ix.parents[rel] && ex.kind == proto.EntryDir {
			continue // still needed as a directory
		}
		p.deletes = append(p.deletes, rel)
	}
	// Reverse lexical order visits children before their parents.
	sort.Sort(sort.Reverse(sort.StringSlice(p.deletes)))
	sort.Strings(p.rmTrees)
	return p
}

// planSlot validates m and computes what it takes to turn the slot at
// root into it, with one scan of the slot.
func planSlot(root string, m *proto.Manifest, caseInsensitive bool) (*plan, error) {
	ix, err := checkManifest(m, caseInsensitive)
	if err != nil {
		return nil, err
	}
	state, err := scanSlot(root, keepMatcher(m), func(rel string) bool {
		_, ok := ix.kind[rel]
		return ok
	})
	if err != nil {
		return nil, err
	}
	return computePlan(m, ix, state), nil
}

// slotFS performs fd-relative operations below a slot root.
type slotFS struct {
	root   string
	rootfd int
}

func openSlotFS(root string) (*slotFS, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("slot %s: %w", root, err)
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open slot %s: %w", root, err)
	}
	return &slotFS{root: root, rootfd: fd}, nil
}

func (s *slotFS) Close() error { return unix.Close(s.rootfd) }

// openDir returns an fd for rel ("" or "." = root; caller closes unless it
// is the root fd). With create, missing components are created and
// non-directories in the way (including symlinks) are replaced: a symlink
// must never redirect a write.
func (s *slotFS) openDir(rel string, create bool) (int, error) {
	if rel == "" || rel == "." {
		return s.rootfd, nil
	}
	fd := s.rootfd
	for _, comp := range strings.Split(rel, "/") {
		next, err := unix.Openat(fd, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil && create {
			switch {
			case errors.Is(err, unix.ENOENT):
				if err = unix.Mkdirat(fd, comp, 0o755); err == nil || errors.Is(err, unix.EEXIST) {
					next, err = unix.Openat(fd, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				}
			case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.EMLINK):
				// A symlink or file where a directory must be.
				if err = unix.Unlinkat(fd, comp, 0); err == nil {
					if err = unix.Mkdirat(fd, comp, 0o755); err == nil {
						next, err = unix.Openat(fd, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
					}
				}
			}
		}
		if fd != s.rootfd {
			unix.Close(fd)
		}
		if err != nil {
			return -1, fmt.Errorf("open %s in slot: %w", rel, err)
		}
		fd = next
	}
	return fd, nil
}

func (s *slotFS) closeDir(fd int) {
	if fd != s.rootfd && fd >= 0 {
		unix.Close(fd)
	}
}

func splitRel(rel string) (dir, name string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

// remove deletes a file, symlink or (empty) directory. Missing paths and
// non-empty dirs (they hold kept content) are not errors.
func (s *slotFS) remove(rel string) error {
	dir, name := splitRel(rel)
	fd, err := s.openDir(dir, false)
	if err != nil {
		return nil // parent already gone or replaced
	}
	defer s.closeDir(fd)
	err = unix.Unlinkat(fd, name, 0)
	if errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EPERM) {
		err = unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
	}
	if err == nil || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
		return nil
	}
	return fmt.Errorf("remove %s: %w", rel, err)
}

// beforeRemoveTree lets a test swap a parent directory mid-removal.
var beforeRemoveTree func()

// removeTree deletes a directory and everything below it, relative to the
// parent's file descriptor: a parent swapped for a symlink after the check
// cannot redirect the removal.
func (s *slotFS) removeTree(rel string) error {
	dir, name := splitRel(rel)
	fd, err := s.openDir(dir, false)
	if err != nil {
		return nil
	}
	defer s.closeDir(fd)
	if beforeRemoveTree != nil {
		beforeRemoveTree()
	}
	if err := removeAllAt(fd, name); err != nil {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	return nil
}

// removeAllAt removes name below dirfd like os.RemoveAll, through file
// descriptors only (never following a symlink), making directories it must
// empty writable first.
func removeAllAt(dirfd int, name string) error {
	err := unix.Unlinkat(dirfd, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
		return err
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	d := os.NewFile(uintptr(fd), name)
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	unix.Fchmod(fd, 0o700) // a read-only dir (0555) could not be emptied
	for _, n := range names {
		if err := removeAllAt(fd, n); err != nil {
			return err
		}
	}
	if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// finish sets mode and mtime on a freshly written temp file and renames it
// into place.
func finishFile(dirfd int, tmp, name string, mode uint32, mtime int64) error {
	if err := chmodAt(dirfd, tmp, mode&0o777); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	if err := setMtimeAt(dirfd, tmp, mtime); err != nil {
		return err
	}
	if err := unix.Renameat(dirfd, tmp, dirfd, name); err != nil {
		unix.Unlinkat(dirfd, tmp, 0)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func setMtimeAt(dirfd int, name string, mtime int64) error {
	ts := unix.NsecToTimespec(mtime)
	if err := unix.UtimesNanoAt(dirfd, name, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("set mtime: %w", err)
	}
	return nil
}

func (s *slotFS) writeLink(e *proto.ManifestEntry) error {
	dir, name := splitRel(e.Path)
	fd, err := s.openDir(dir, true)
	if err != nil {
		return err
	}
	defer s.closeDir(fd)
	tmp := tmpPrefix + randHex(6)
	if err := unix.Symlinkat(e.Target, fd, tmp); err != nil {
		return fmt.Errorf("symlink %s: %w", e.Path, err)
	}
	if err := setMtimeAt(fd, tmp, e.MTime); err != nil {
		unix.Unlinkat(fd, tmp, 0)
		return fmt.Errorf("%s: %w", e.Path, err)
	}
	if err := unix.Renameat(fd, tmp, fd, name); err != nil {
		unix.Unlinkat(fd, tmp, 0)
		return fmt.Errorf("symlink %s: %w", e.Path, err)
	}
	return nil
}

func (s *slotFS) ensureDir(e *proto.ManifestEntry) error {
	fd, err := s.openDir(e.Path, true)
	if err != nil {
		return err
	}
	defer s.closeDir(fd)
	if err := unix.Fchmod(fd, e.Mode&0o777); err != nil {
		return fmt.Errorf("chmod %s: %w", e.Path, err)
	}
	return nil
}

func (s *slotFS) chmod(e *proto.ManifestEntry) error {
	dir, name := splitRel(e.Path)
	fd, err := s.openDir(dir, false)
	if err != nil {
		return err
	}
	defer s.closeDir(fd)
	if err := chmodAt(fd, name, e.Mode&0o777); err != nil {
		return fmt.Errorf("chmod %s: %w", e.Path, err)
	}
	return nil
}

// prepare runs the phases shared by local and streamed applies: deletions,
// type-conflict removal. Writes follow, then finishMeta.
func (s *slotFS) prepare(p *plan) error {
	for _, rel := range p.deletes {
		if err := s.remove(rel); err != nil {
			return err
		}
	}
	for _, rel := range p.rmTrees {
		if err := s.removeTree(rel); err != nil {
			return err
		}
	}
	return nil
}

func (s *slotFS) finishMeta(p *plan) error {
	for _, i := range p.chmods {
		if err := s.chmod(&p.m.Entries[i]); err != nil {
			return err
		}
	}
	for _, i := range p.links {
		if err := s.writeLink(&p.m.Entries[i]); err != nil {
			return err
		}
	}
	for _, i := range p.dirs {
		if err := s.ensureDir(&p.m.Entries[i]); err != nil {
			return err
		}
	}
	return nil
}

// applyLocal materializes p with content cloned from the blob store.
// Files are grouped by directory so each worker opens a directory once.
func (s *slotFS) applyLocal(p *plan, blobPath func(hash string) string, workers int) error {
	if err := s.prepare(p); err != nil {
		return err
	}
	groups := map[string][]int{}
	var order []string
	for _, i := range p.writes {
		dir, _ := splitRel(p.m.Entries[i].Path)
		if _, ok := groups[dir]; !ok {
			order = append(order, dir)
		}
		groups[dir] = append(groups[dir], i)
	}
	// Create directories serially (parents first) so workers never race
	// on mkdir.
	sort.Strings(order)
	for _, dir := range order {
		fd, err := s.openDir(dir, true)
		if err != nil {
			return err
		}
		s.closeDir(fd)
	}
	if workers < 1 {
		workers = 1
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	next := make(chan string)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for dir := range next {
				if err := s.writeGroup(p, dir, groups[dir], blobPath); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}()
	}
	for _, dir := range order {
		next <- dir
	}
	close(next)
	wg.Wait()
	if len(errs) > 0 {
		return errs[0]
	}
	return s.finishMeta(p)
}

func (s *slotFS) writeGroup(p *plan, dir string, idx []int, blobPath func(string) string) error {
	fd, err := s.openDir(dir, true)
	if err != nil {
		return err
	}
	defer s.closeDir(fd)
	for _, i := range idx {
		e := &p.m.Entries[i]
		_, name := splitRel(e.Path)
		src := blobPath(e.Hash)
		fi, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("%s: %w (%s)", e.Path, ErrMissingBlob, short(e.Hash))
		}
		if fi.Size() != e.Size {
			// A torn blob (crash before data reached disk): drop it so the
			// next sync uploads it again.
			os.Remove(src)
			return fmt.Errorf("%s: blob %s is %d bytes, want %d: %w", e.Path, short(e.Hash), fi.Size(), e.Size, ErrMissingBlob)
		}
		tmp := tmpPrefix + randHex(6)
		if err := cloneFileAt(src, fd, tmp); err != nil {
			unix.Unlinkat(fd, tmp, 0)
			return fmt.Errorf("write %s: %w", e.Path, err)
		}
		if err := finishFile(fd, tmp, name, e.Mode, e.MTime); err != nil {
			return fmt.Errorf("write %s: %w", e.Path, err)
		}
	}
	return nil
}

// commitLocal materializes m into the host directory root using blobs from
// bs. It returns the executed plan.
func commitLocal(root string, m *proto.Manifest, bs *blobStore, workers int, caseInsensitive bool) (*plan, error) {
	p, err := planSlot(root, m, caseInsensitive)
	if err != nil {
		return nil, err
	}
	bs.trimMu.RLock()
	defer bs.trimMu.RUnlock()
	return p, applyPlanLocal(root, p, bs, workers)
}

// applyPlanLocal carries out p in root with content from bs. The caller
// holds bs.trimMu (shared) so no blob vanishes meanwhile.
func applyPlanLocal(root string, p *plan, bs *blobStore, workers int) error {
	for _, h := range p.needed {
		if !bs.has(h) {
			return fmt.Errorf("%w: %s; upload it first", ErrMissingBlob, short(h))
		}
	}
	s, err := openSlotFS(root)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.applyLocal(p, bs.path, workers); err != nil {
		return err
	}
	for _, h := range p.needed {
		bs.touch(h)
	}
	return nil
}

// diffLocal computes the plan for root without changing anything.
func diffLocal(root string, m *proto.Manifest, caseInsensitive bool) (*plan, error) {
	return planSlot(root, m, caseInsensitive)
}

// chmodAt sets the mode of name below dirfd without following a symlink
// (a process left from an earlier job could swap the file for one).
// fchmodat2's AT_SYMLINK_NOFOLLOW (Linux 6.6+, macOS) does it in one call;
// otherwise the file is opened O_NOFOLLOW and fchmod'ed. Residual: on older
// Linux kernels a file its owner cannot read or write is checked with
// fstatat and then chmod'ed by name, a window only a racing process of the
// same user can use.
func chmodAt(dirfd int, name string, mode uint32) error {
	err := unix.Fchmodat(dirfd, name, mode, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil || !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	for _, flag := range []int{unix.O_RDONLY, unix.O_WRONLY} {
		fd, err := unix.Openat(dirfd, name, flag|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOCTTY, 0)
		if err == nil {
			err = unix.Fchmod(fd, mode)
			unix.Close(fd)
			return err
		}
		if !errors.Is(err, unix.EACCES) {
			return err // ELOOP: a symlink
		}
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return unix.ELOOP
	}
	return unix.Fchmodat(dirfd, name, mode, 0)
}
