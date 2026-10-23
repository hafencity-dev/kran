package execenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// slotLoc is where a slot's files live.
type slotLoc string

const (
	locHost   slotLoc = "host"   // a directory under slots/ on this machine
	locVolume slotLoc = "volume" // a subpath of the kran-slots Docker volume
)

const maxSlotsPerRepo = 4

type slotMeta struct {
	Hint string `json:"hint"` // worktree that used the slot last
	// LastUsed is the last acquisition or release. Zero in metadata an
	// older kran wrote at acquisition: see lastUsed.
	LastUsed time.Time `json:"last_used"`
}

type slot struct {
	repo    string
	index   int
	loc     slotLoc
	busy    bool
	job     string    // job running in it, if any
	touched time.Time // last acquire/diff/commit/start (detects abandoned acquisitions)
	meta    slotMeta
	// manifest is the last manifest committed into the slot (memory only):
	// it keys the setup step without reading files from the slot.
	manifest *proto.Manifest
	// synced is the manifest Diff already committed for the current
	// holder, so its Commit has nothing left to do.
	synced *proto.Manifest
}

// slotPool hands out slots per repo and location (SPEC §6.7).
type slotPool struct {
	l    layout
	node string
	now  func() time.Time
	max  int

	mu     sync.Mutex
	repos  map[string][]*slot // key: repo + "|" + loc
	loaded map[string]bool
	free   chan struct{} // closed and replaced whenever a slot is released
}

func newSlotPool(l layout, node string, now func() time.Time) *slotPool {
	return &slotPool{l: l, node: node, now: now, max: maxSlotsPerRepo,
		repos: map[string][]*slot{}, loaded: map[string]bool{}, free: make(chan struct{})}
}

func poolKey(repo string, loc slotLoc) string { return repo + "|" + string(loc) }

func (p *slotPool) metaPath(s *slot) string {
	name := strconv.Itoa(s.index)
	if s.loc == locVolume {
		name = "v" + name
	}
	return filepath.Join(p.l.repoSlots(s.repo), name+".json")
}

// setupKeyPath records the setup key that last succeeded in the slot.
func (p *slotPool) setupKeyPath(s *slot) string {
	return strings.TrimSuffix(p.metaPath(s), ".json") + ".setup"
}

func (p *slotPool) hostPath(s *slot) string {
	return filepath.Join(p.l.repoSlots(s.repo), strconv.Itoa(s.index))
}

func (p *slotPool) ref(s *slot) proto.SlotRef {
	r := proto.SlotRef{Node: p.node, RepoID: s.repo, Index: s.index}
	if s.loc == locHost {
		r.Path = p.hostPath(s)
	} else {
		r.Path = s.repo + "/" + strconv.Itoa(s.index)
	}
	return r
}

// load reads the persisted slots of a repo (caller holds mu).
func (p *slotPool) load(repo string) {
	if p.loaded[repo] {
		return
	}
	p.loaded[repo] = true
	ents, err := os.ReadDir(p.l.repoSlots(repo))
	if err != nil {
		return
	}
	seen := map[string]bool{}
	add := func(idx int, loc slotLoc) {
		k := fmt.Sprintf("%s%d", loc, idx)
		if seen[k] || idx < 0 || idx >= p.max {
			return
		}
		seen[k] = true
		s := &slot{repo: repo, index: idx, loc: loc}
		readJSON(p.metaPath(s), &s.meta)
		key := poolKey(repo, loc)
		p.repos[key] = append(p.repos[key], s)
	}
	for _, e := range ents {
		name := e.Name()
		if base, ok := strings.CutSuffix(name, ".json"); ok {
			if v, ok := strings.CutPrefix(base, "v"); ok {
				if n, err := strconv.Atoi(v); err == nil {
					add(n, locVolume)
				}
			} else if n, err := strconv.Atoi(base); err == nil {
				add(n, locHost)
			}
		} else if e.IsDir() {
			if n, err := strconv.Atoi(name); err == nil {
				add(n, locHost)
			}
		}
	}
	for _, loc := range []slotLoc{locHost, locVolume} {
		ss := p.repos[poolKey(repo, loc)]
		sort.Slice(ss, func(i, j int) bool { return ss[i].index < ss[j].index })
	}
}

// acquire blocks until a slot is free. Preference: the slot last used by
// the same worktree (warm build caches, identical files), then the most
// recently used free slot, then a new slot while under the per-repo cap.
func (p *slotPool) acquire(ctx context.Context, repo, hint string, loc slotLoc) (*slot, error) {
	if err := validRepoID(repo); err != nil {
		return nil, err
	}
	for {
		p.mu.Lock()
		p.load(repo)
		key := poolKey(repo, loc)
		var pick *slot
		for _, s := range p.repos[key] {
			if s.busy {
				continue
			}
			if hint != "" && s.meta.Hint == hint {
				pick = s
				break
			}
			if pick == nil || s.meta.LastUsed.After(pick.meta.LastUsed) {
				pick = s
			}
		}
		if pick == nil && len(p.repos[key]) < p.max {
			used := map[int]bool{}
			for _, s := range p.repos[key] {
				used[s.index] = true
			}
			idx := 0
			for used[idx] {
				idx++
			}
			pick = &slot{repo: repo, index: idx, loc: loc}
			p.repos[key] = append(p.repos[key], pick)
		}
		if pick != nil {
			pick.busy = true
			pick.job = ""
			pick.synced = nil
			pick.touched = p.now()
			// Stamped now, not only at release: a slot whose holder never
			// releases it (daemon restart, crash) must not look unused
			// since year one to the next reaper.
			pick.meta.LastUsed = pick.touched
			if hint != "" {
				pick.meta.Hint = hint
			}
			meta := pick.meta
			p.mu.Unlock()
			if err := os.MkdirAll(p.l.repoSlots(repo), 0o700); err != nil {
				p.release(pick)
				return nil, fmt.Errorf("slot dir: %w", err)
			}
			writeJSONAtomic(p.metaPath(pick), meta)
			return pick, nil
		}
		wait := p.free
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for a free slot (all %d slots of this repo are busy): %w", p.max, ctx.Err())
		case <-wait:
		}
	}
}

func (p *slotPool) release(s *slot) {
	p.mu.Lock()
	if !s.busy {
		p.mu.Unlock()
		return
	}
	s.busy = false
	s.job = ""
	s.synced = nil
	s.meta.LastUsed = p.now()
	meta := s.meta
	close(p.free)
	p.free = make(chan struct{})
	p.mu.Unlock()
	writeJSONAtomic(p.metaPath(s), meta)
}

// lookup finds the busy slot behind a ref handed out by acquire.
func (p *slotPool) lookup(ref proto.SlotRef) (*slot, error) {
	loc := locVolume
	if filepath.IsAbs(ref.Path) {
		loc = locHost
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load(ref.RepoID)
	for _, s := range p.repos[poolKey(ref.RepoID, loc)] {
		if s.index == ref.Index {
			if !s.busy {
				return nil, fmt.Errorf("slot %s/%d is not acquired; acquire it first", short(ref.RepoID), ref.Index)
			}
			s.touched = p.now()
			return s, nil
		}
	}
	return nil, fmt.Errorf("unknown slot %s/%d", short(ref.RepoID), ref.Index)
}

// markBusy re-establishes a slot held by a job that survived a daemon
// restart.
func (p *slotPool) markBusy(ref proto.SlotRef, job string) *slot {
	if validRepoID(ref.RepoID) != nil {
		return nil
	}
	loc := locVolume
	if filepath.IsAbs(ref.Path) {
		loc = locHost
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load(ref.RepoID)
	key := poolKey(ref.RepoID, loc)
	for _, s := range p.repos[key] {
		if s.index == ref.Index {
			s.busy, s.job, s.touched = true, job, p.now()
			return s
		}
	}
	s := &slot{repo: ref.RepoID, index: ref.Index, loc: loc, busy: true, job: job, touched: p.now()}
	p.repos[key] = append(p.repos[key], s)
	return s
}

func (p *slotPool) setJob(s *slot, job string) {
	p.mu.Lock()
	s.job = job
	s.touched = p.now()
	p.mu.Unlock()
}

func (p *slotPool) jobOf(s *slot) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return s.job
}

func (p *slotPool) setManifest(s *slot, m *proto.Manifest) {
	p.mu.Lock()
	s.manifest = m
	p.mu.Unlock()
}

// setSynced records (m) or clears (nil) the manifest Diff committed.
func (p *slotPool) setSynced(s *slot, m *proto.Manifest) {
	p.mu.Lock()
	s.synced = m
	if m != nil {
		s.manifest = m
	}
	p.mu.Unlock()
}

func (p *slotPool) syncedTo(s *slot) *proto.Manifest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return s.synced
}

func (p *slotPool) manifestOf(s *slot) *proto.Manifest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return s.manifest
}

// abandoned returns busy slots without a job whose last use is older than
// idle: acquisitions whose origin vanished between sync and start.
func (p *slotPool) abandoned(idle time.Duration) []*slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*slot
	cut := p.now().Add(-idle)
	for _, ss := range p.repos {
		for _, s := range ss {
			if s.busy && s.job == "" && s.touched.Before(cut) {
				out = append(out, s)
			}
		}
	}
	return out
}

// stale removes and returns free slots unused for longer than idle (slot
// GC); the caller deletes their files.
func (p *slotPool) stale(idle time.Duration) []*slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Make sure repos never touched by this daemon are considered too.
	if ents, err := os.ReadDir(p.l.slotsDir()); err == nil {
		for _, e := range ents {
			if e.IsDir() && validRepoID(e.Name()) == nil {
				p.load(e.Name())
			}
		}
	}
	var out []*slot
	cut := p.now().Add(-idle)
	for key, ss := range p.repos {
		kept := ss[:0]
		for _, s := range ss {
			if !s.busy && p.lastUsed(s).Before(cut) {
				out = append(out, s)
				continue
			}
			kept = append(kept, s)
		}
		p.repos[key] = kept
	}
	return out
}

// evictLRU takes the idle slot at loc that was used least recently out of
// the pool, for deletion (nil when every slot there is in use, or, with a
// non-zero usedBefore, when none was last used before it). A slot in use,
// or acquired for a job about to start, is never taken.
func (p *slotPool) evictLRU(loc slotLoc, usedBefore time.Time) *slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ents, err := os.ReadDir(p.l.slotsDir()); err == nil {
		for _, e := range ents {
			if e.IsDir() && validRepoID(e.Name()) == nil {
				p.load(e.Name())
			}
		}
	}
	var best *slot
	var bestKey string
	var bestAt time.Time
	for key, ss := range p.repos {
		for _, s := range ss {
			if s.busy || s.loc != loc {
				continue
			}
			if at := p.lastUsed(s); best == nil || at.Before(bestAt) {
				best, bestKey, bestAt = s, key, at
			}
		}
	}
	if best == nil || (!usedBefore.IsZero() && !bestAt.Before(usedBefore)) {
		return nil
	}
	kept := p.repos[bestKey][:0]
	for _, s := range p.repos[bestKey] {
		if s != best {
			kept = append(kept, s)
		}
	}
	p.repos[bestKey] = kept
	return best
}

// lastUsed is the slot's last use (caller holds mu). Metadata without one
// (written by an older kran, or missing) counts from the latest change of
// the slot's files we can see, else from now: an unknown age never makes
// a slot look abandoned.
func (p *slotPool) lastUsed(s *slot) time.Time {
	if !s.meta.LastUsed.IsZero() {
		return s.meta.LastUsed
	}
	var t time.Time
	files := []string{p.metaPath(s), p.setupKeyPath(s)}
	if s.loc == locHost {
		files = append(files, p.hostPath(s))
	}
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(t) {
			t = fi.ModTime()
		}
	}
	if t.IsZero() {
		t = p.now()
	}
	s.meta.LastUsed = t
	return t
}

// forget deletes a GC'd slot's bookkeeping files.
func (p *slotPool) forget(s *slot) error {
	var errs []error
	for _, f := range []string{p.metaPath(s), p.setupKeyPath(s)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	os.Remove(p.l.repoSlots(s.repo)) // only succeeds when empty
	return errors.Join(errs...)
}

// busySlots describes every busy slot as "<repo prefix>/<index> job <id>"
// (job "" = acquired but not started yet).
func (p *slotPool) busySlots() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, ss := range p.repos {
		for _, s := range ss {
			if s.busy {
				out = append(out, fmt.Sprintf("%.12s/%d job %q", s.repo, s.index, s.job))
			}
		}
	}
	sort.Strings(out)
	return out
}

// BusySlots lists the slots in use (diagnostics: the daemon's debug
// endpoint; outside any job every slot must be free).
func (m *manager) BusySlots() []string { return m.pool.busySlots() }
