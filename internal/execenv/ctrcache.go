package execenv

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The container runtime's shared caches (SPEC §6.8): the kran-cache volume
// (GOCACHE, GOMODCACHE, the pnpm store, npm, uv, pip, cargo and mise
// caches, the persistent home) and the kran-tools volume (mise installs).
// Jobs only add to them; `kran internal cache-trim` keeps them within a
// budget. It runs in a container of the runner image with both volumes,
// as the node's user, never while a job uses them: every job container
// holds a shared lock on cacheLockFile (kran-init), the trim takes it
// exclusively or skips. It deletes whole entries, least recently used
// first, and only kinds it knows to be caches: an entry is first renamed
// into a trash directory and then removed, so an interrupted trim never
// leaves a half-deleted entry where a tool would find it.

const (
	cacheLockFile = ".kran-lock"
	cacheTrashDir = ".kran-trash"
)

// cacheReport is what one trim did; the daemon logs it and shows it in
// the container runtime's status (kran doctor).
type cacheReport struct {
	Used      int64  `json:"used"`      // bytes in both volumes before
	After     int64  `json:"after"`     // and after
	Budget    int64  `json:"budget"`    // what the caches may use
	Free      int64  `json:"free"`      // free bytes on the Docker disk (after)
	Total     int64  `json:"total"`     // size of the Docker disk
	Floor     int64  `json:"floor"`     // the disk floor below which it trims harder
	Reserve   int64  `json:"reserve"`   // free space the trim keeps on the disk
	Removed   int    `json:"removed"`   // entries deleted
	Reclaimed int64  `json:"reclaimed"` // bytes freed
	Skipped   string `json:"skipped,omitempty"`
}

// cacheUnit is one deletable cache entry.
type cacheUnit struct {
	path string
	root string // the volume root it is in
	size int64
	last time.Time // latest access or change of a file in it
	mise bool      // a mise install: its version symlinks go with it
}

// cacheUnitDepth says whether rel (slash-separated, relative to the cache
// or tools root) lies inside a deletable entry, and how many leading
// components name that entry. Everything else (cargo's installed
// binaries, the home directory outside its caches, mise's state) is
// counted but never deleted.
func cacheUnitDepth(tools bool, parts []string) int {
	n := len(parts)
	at := func(i int, s string) bool { return n > i && parts[i] == s }
	if n == 0 || parts[0] == cacheTrashDir || parts[0] == cacheLockFile {
		return 0
	}
	if tools {
		// mise installs and downloads: one tool version each.
		if at(0, "mise") && (at(1, "installs") || at(1, "downloads")) && n >= 4 {
			return 4
		}
		return 0
	}
	switch parts[0] {
	case "go-build": // GOCACHE: single files, as Go's own trim deletes them
		if n == 3 {
			return 3
		}
	case "gomod": // GOMODCACHE
		if at(1, "cache") {
			switch {
			case at(2, "download"):
				// Files of a module version (.zip, .mod, .info, …).
				if n >= 5 && parts[n-2] == "@v" && !strings.HasSuffix(parts[n-1], ".lock") && parts[n-1] != "list" {
					return n
				}
			case at(2, "vcs") && n >= 4:
				return 4
			}
			return 0
		}
		// An extracted module: the first component naming a version.
		for i := 1; i < n; i++ {
			if strings.Contains(parts[i], "@") {
				return i + 1
			}
		}
	case "pnpm-store": // a whole store version (see trimCaches)
		if n >= 2 {
			return 2
		}
	case "npm", "mise": // npm's cache and npx installs; mise's downloads
		if n >= 2 {
			return 2
		}
	case "uv", "pip": // entries reference each other: all or nothing
		if n >= 1 {
			return 1
		}
	case "cargo":
		switch {
		case at(1, "registry") && (at(2, "cache") || at(2, "src")) && n >= 5:
			return 5
		case at(1, "registry") && at(2, "index") && n >= 4:
			return 4
		case at(1, "git") && (at(2, "checkouts") || at(2, "db")) && n >= 4:
			return 4
		}
	case "home": // the persistent home: only its caches
		switch {
		case at(1, ".cache") && n >= 3:
			return 3
		case at(1, ".npm") && at(2, "_cacache"):
			return 3
		}
	}
	return 0
}

// cacheScan walks root: total bytes (hard links once) and its units.
func cacheScan(root string, tools bool, seen map[[2]uint64]bool) (int64, map[string]*cacheUnit, error) {
	var total int64
	units := map[string]*cacheUnit{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // vanished or unreadable: not ours to judge
		}
		var st unix.Stat_t
		if unix.Lstat(p, &st) != nil {
			return nil
		}
		size := st.Blocks * 512
		if st.Nlink > 1 && !d.IsDir() {
			k := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
			if seen[k] {
				size = 0
			}
			seen[k] = true
		}
		total += size
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		depth := cacheUnitDepth(tools, parts)
		if depth == 0 {
			return nil
		}
		key := filepath.Join(root, filepath.Join(parts[:depth]...))
		if len(parts) == depth && d.Type()&fs.ModeSymlink != 0 {
			return nil // mise's version aliases: removed with their target
		}
		u := units[key]
		if u == nil {
			u = &cacheUnit{path: key, root: root, mise: tools}
			units[key] = u
		}
		u.size += size
		if d.IsDir() {
			// A directory's times say nothing about use: listing it (this
			// walk included) updates its atime.
			return nil
		}
		t := time.Unix(st.Atim.Unix())
		if mt := time.Unix(st.Mtim.Unix()); mt.After(t) {
			t = mt
		}
		if t.After(u.last) {
			u.last = t
		}
		return nil
	})
	return total, units, err
}

// cacheTrimOpts configure one trim.
type cacheTrimOpts struct {
	Cache, Tools string
	Budget       int64         // 0: min(20 GiB, a quarter of the disk)
	Reserve      int64         // free space to keep; 0: diskReserve
	Floor        int64         // 0: max(10 GiB, 5% of the disk)
	Recent       time.Duration // entries used this recently stay unless the disk is short
	Now          func() time.Time
	Prune        func(ctx context.Context, cache, tools string) // the tools' own pruning
	DiskSpace    func(string) (free, total int64, err error)
}

// trimCaches is the trim itself (the caller holds the lock).
func trimCaches(ctx context.Context, o cacheTrimOpts) (cacheReport, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.DiskSpace == nil {
		o.DiskSpace = diskSpace
	}
	var rep cacheReport
	// Leftovers of an interrupted trim go first.
	for _, r := range []string{o.Cache, o.Tools} {
		if r != "" {
			removeAllForce(filepath.Join(r, cacheTrashDir))
		}
	}
	free, total, err := o.DiskSpace(o.Cache)
	if err != nil {
		return rep, err
	}
	rep.Total, rep.Free = total, free
	rep.Budget = o.Budget
	if rep.Budget <= 0 {
		rep.Budget = min(20*gib, total/4)
	}
	rep.Floor = o.Floor
	if rep.Floor <= 0 {
		rep.Floor = diskFloor(total)
	}
	rep.Reserve = o.Reserve
	if rep.Reserve <= 0 {
		rep.Reserve = diskReserve(total)
	}
	rep.Reserve = max(rep.Reserve, rep.Floor)
	used, units, err := scanCaches(o)
	if err != nil {
		return rep, err
	}
	rep.Used, rep.After = used, used
	// short: below the floor, where even entries used just now go;
	// tight: below the reserve the disk should keep free.
	short, tight := free < rep.Floor, free < rep.Reserve
	if used <= rep.Budget && !tight {
		return rep, nil
	}
	// Over budget: first what the tools drop themselves (entries nothing
	// refers to any more), then whole entries, least recently used first.
	if o.Prune != nil {
		o.Prune(ctx, o.Cache, o.Tools)
		if used, units, err = scanCaches(o); err != nil {
			return rep, err
		}
		if free, _, err = o.DiskSpace(o.Cache); err == nil {
			short, tight = free < rep.Floor, free < rep.Reserve
		}
	}
	target := used
	if used > rep.Budget {
		target = rep.Budget * 8 / 10 // some headroom, so the next job does not trigger a trim at once
	}
	if tight {
		// The disk keeps less free than its reserve: free what it lacks
		// as well (the product images and slots next to the caches count).
		target = max(min(target, used-(rep.Reserve-free)), 0)
	}
	list := make([]*cacheUnit, 0, len(units))
	var deletable int64
	for _, u := range units {
		list = append(list, u)
		deletable += u.size
	}
	if kept := used - deletable; kept >= target && !short {
		// Even without every cache entry the volumes stay over budget
		// (files in the home, installed binaries): emptying the caches
		// each hour would only slow jobs down.
		rep.Skipped = fmt.Sprintf("%s of the caches are not cache entries kran may delete (the persistent home, installed binaries)", fmtGiB(kept))
		return rep, nil
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].last.Equal(list[j].last) {
			return list[i].last.Before(list[j].last)
		}
		return list[i].path < list[j].path
	})
	cur := used
	for _, u := range list {
		if cur <= target || ctx.Err() != nil {
			break
		}
		if !short && o.Now().Sub(u.last) < o.Recent {
			break // everything from here on was used just now
		}
		if err := removeCacheUnit(u); err != nil {
			continue
		}
		cur -= u.size
		rep.Removed++
	}
	rep.After = max(cur, 0)
	rep.Reclaimed = used - rep.After
	if f, _, err := o.DiskSpace(o.Cache); err == nil {
		rep.Free = f
	}
	return rep, nil
}

func scanCaches(o cacheTrimOpts) (int64, map[string]*cacheUnit, error) {
	seen := map[[2]uint64]bool{}
	used, units, err := cacheScan(o.Cache, false, seen)
	if err != nil {
		return 0, nil, err
	}
	if o.Tools != "" {
		n, tu, err := cacheScan(o.Tools, true, seen)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, nil, err
		}
		used += n
		for k, v := range tu {
			units[k] = v
		}
	}
	return used, units, nil
}

// removeCacheUnit moves u into its root's trash (atomic for a tool that
// looks for it) and deletes it there.
func removeCacheUnit(u *cacheUnit) error {
	trash := filepath.Join(u.root, cacheTrashDir)
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	// Go's module cache is read-only; a directory needs write permission
	// to move to another parent.
	if fi, err := os.Lstat(u.path); err == nil && fi.IsDir() && fi.Mode().Perm()&0o200 == 0 {
		os.Chmod(u.path, fi.Mode().Perm()|0o700)
	}
	dst := filepath.Join(trash, randHex(8))
	if err := os.Rename(u.path, dst); err != nil {
		return err
	}
	if u.mise {
		removeDanglingLinks(filepath.Dir(u.path))
	}
	return removeAllForce(dst)
}

// removeDanglingLinks drops symlinks in dir whose target is gone (mise's
// "node/24 -> 24.8.0" aliases would otherwise claim an install).
func removeDanglingLinks(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			os.Remove(p)
		}
	}
}

// removeAllForce is os.RemoveAll that also deletes read-only directories
// (Go's module cache).
func removeAllForce(p string) error {
	if err := os.RemoveAll(p); err == nil {
		return nil
	}
	filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(q, 0o700)
		}
		return nil
	})
	return os.RemoveAll(p)
}

// pruneWithTools runs the tools' own pruning where it removes only what
// nothing refers to: `uv cache prune`, with a uv already installed in the
// tools volume (never a download). Go trims its build cache itself (files
// unused for five days); `pnpm store prune` is not used, since node_modules
// in a slot are copies of the store (another mount) and it would see every
// package as unreferenced.
func pruneWithTools(ctx context.Context, cache, tools string) {
	ver := installedVersion(filepath.Join(tools, "mise", "installs", "uv"))
	if ver == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(cache, "uv")); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "mise", "exec", "uv@"+ver, "--", "uv", "cache", "prune", "--cache-dir", filepath.Join(cache, "uv"))
	cmd.Env = append(os.Environ(), "MISE_OFFLINE=1", "HOME=/tmp")
	cmd.Stdout, cmd.Stderr = nil, nil
	cmd.Run()
}

// installedVersion is the newest-named installed version in a mise
// installs dir ("" when none).
func installedVersion(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range ents {
		if !e.IsDir() || !ctrVersionRe.MatchString(e.Name()) {
			continue
		}
		if best == "" || compareVersions(e.Name(), best) > 0 {
			best = e.Name()
		}
	}
	return best
}

// compareVersions orders dotted versions numerically where it can.
func compareVersions(a, b string) int {
	va, _, oka := parseVer(a)
	vb, _, okb := parseVer(b)
	if oka && okb {
		for i := range va {
			if va[i] != vb[i] {
				if va[i] < vb[i] {
					return -1
				}
				return 1
			}
		}
	}
	return strings.Compare(a, b)
}

// CacheTrim implements `kran internal cache-trim` (in the trim container).
// It prints a cacheReport as JSON.
func CacheTrim(args []string) int {
	fs := flag.NewFlagSet("cache-trim", flag.ContinueOnError)
	cache := fs.String("cache", ctrCache, "the cache volume")
	tools := fs.String("tools", ctrTools, "the tools volume")
	budget := fs.Int64("budget", 0, "bytes the caches may use (0: automatic)")
	reserve := fs.Int64("reserve", 0, "free bytes to keep on the disk (0: automatic)")
	floor := fs.Int64("floor", 0, "free bytes below which to trim harder (0: automatic)")
	recent := fs.Duration("recent", time.Hour, "keep entries used this recently")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rep := cacheReport{}
	lock, err := os.OpenFile(filepath.Join(*cache, cacheLockFile), os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kran: cache trim: %v\n", err)
		return 1
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		rep.Skipped = "a job is using the caches"
	} else {
		rep, err = trimCaches(context.Background(), cacheTrimOpts{
			Cache: *cache, Tools: *tools, Budget: *budget, Reserve: *reserve, Floor: *floor, Recent: *recent, Prune: pruneWithTools,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "kran: cache trim: %v\n", err)
			return 1
		}
	}
	json.NewEncoder(os.Stdout).Encode(rep)
	return 0
}
