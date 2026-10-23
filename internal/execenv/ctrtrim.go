package execenv

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// The daemon's side of the cache trim (ctrcache.go, SPEC §6.2): the
// reaper starts one in a container about once an hour, never while a
// container job of this node runs or starts; a job that starts meanwhile
// stops it first (a trim is safe to interrupt).

// cacheTrimEvery spaces trims; each walks both volumes.
const cacheTrimEvery = time.Hour

// A node that always runs a container job never trims its caches. When a
// trim is overdue by cacheStarveAfter while the Docker disk keeps less free
// than its reserve, new container jobs pause (the runtime reports itself
// unavailable, so jobs queue or go elsewhere) until the running ones end
// and the trim ran, at most cachePauseMax.
const (
	cacheStarveAfter = 15 * time.Minute
	cachePauseMax    = 15 * time.Minute
)

// cachePauseReason is the container runtime's reason while it pauses.
const cachePauseReason = "kran is trimming its caches on the Docker disk; container jobs start again in a few minutes"

type cacheTrim struct {
	mu       sync.Mutex
	holds    int                // container jobs being started
	running  context.CancelFunc // non-nil while a trim runs
	pause    time.Time          // new container jobs wait for a starved trim since then (zero: none)
	done     chan struct{}
	last     time.Time
	report   *cacheReport
	reportAt time.Time

	// The Docker disk as last measured (trim or slot check).
	diskFree, diskTotal int64
	diskAt              time.Time
	slotCheck           time.Time // last volume slot check (slotreclaim.go)
}

func (m *manager) trimName() string { return m.ns + "-cache-trim" }

// holdCaches keeps the cache trim away while a container job starts (the
// started job is then active, which keeps it away until it ends); a trim
// already running is stopped first.
func (m *manager) holdCaches(ctx context.Context) (release func()) {
	m.ct.mu.Lock()
	m.ct.holds++
	cancel, done := m.ct.running, m.ct.done
	m.ct.mu.Unlock()
	if cancel != nil {
		m.logf("stopping the cache trim: a container job starts")
		cancel()
		if dk, _ := m.docker(); dk != nil {
			rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
			dk.run(rctx, nil, "rm", "--force", m.trimName())
			rcancel()
		}
		select {
		case <-done:
		case <-ctx.Done():
		case <-time.After(20 * time.Second):
		}
	}
	return func() {
		m.ct.mu.Lock()
		m.ct.holds--
		m.ct.mu.Unlock()
	}
}

// containerJobsActive reports container jobs this manager supervises.
func (m *manager) containerJobsActive() bool {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	for _, js := range m.jobs {
		if js.spec.Docker != nil && !js.isDone() {
			return true
		}
	}
	return false
}

// maybeTrimCaches starts a cache trim when one is due and nothing uses
// the caches. It returns at once; the trim runs in the background.
func (m *manager) maybeTrimCaches(ctx context.Context) {
	if m.tun.cacheTrimEvery <= 0 {
		return // tests
	}
	dk, _ := m.docker()
	if dk == nil {
		return
	}
	m.ct.mu.Lock()
	defer m.ct.mu.Unlock()
	now := m.now()
	if m.ct.running != nil || m.ct.holds > 0 {
		return
	}
	if m.ct.pause.IsZero() && now.Sub(m.ct.last) < m.tun.cacheTrimEvery {
		return
	}
	if m.containerJobsActive() {
		switch {
		case m.ct.pause.IsZero() && m.trimStarvedLocked(now):
			m.ct.pause = now
			m.logf("container caches: the Docker disk has %s free, less than kran's reserve, and the caches could not be trimmed for %s (container jobs kept running): new container jobs wait until the running ones end",
				fmtGiB(m.ct.diskFree), now.Sub(m.ct.last).Round(time.Minute))
		case !m.ct.pause.IsZero() && now.Sub(m.ct.pause) > cachePauseMax:
			m.ct.pause, m.ct.last = time.Time{}, now
			m.logf("container caches: the running container jobs did not end within %s; container jobs start again and the trim waits for the next chance", cachePauseMax)
		}
		return // the next pass tries again
	}
	// Checked at most once per period: most nodes never run a container
	// job, and these ask Docker.
	m.ct.last = m.now()
	if !m.imageBuilt(ctx, dk) || !m.volumeExists(ctx, dk, m.volume("cache")) {
		m.ct.pause = time.Time{} // nothing to trim: never hold jobs back for it
		return                   // no container job ran here yet
	}
	tctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	done := make(chan struct{})
	m.ct.running, m.ct.done = cancel, done
	go func() {
		defer close(done)
		defer cancel()
		rep, err := m.runCacheTrim(tctx, dk)
		m.ct.mu.Lock()
		m.ct.running, m.ct.done = nil, nil
		if err == nil || tctx.Err() == nil {
			m.ct.pause = time.Time{} // done, or failed for good: jobs go on either way
		}
		if err == nil && rep.Total > 0 { // measured (a trim skipped for a running job was not)
			m.ct.report, m.ct.reportAt = &rep, m.now()
			m.ct.diskFree, m.ct.diskTotal, m.ct.diskAt = rep.Free, rep.Total, m.now()
		}
		m.ct.mu.Unlock()
		if err == nil && rep.Total > 0 && rep.Free < rep.Reserve {
			// The caches alone did not free enough: idle slots next.
			m.reclaimVolumeSlots(tctx, true)
		}
		switch {
		case err != nil && tctx.Err() == nil:
			m.logf("container caches: trim failed: %v", err)
		case err != nil:
		case rep.Skipped != "":
			m.logf("container caches: trim skipped: %s", rep.Skipped)
		case rep.Removed > 0:
			m.logf("container caches: removed %d least recently used entries, reclaimed %s (%s → %s, budget %s; Docker disk %s free)",
				rep.Removed, fmtGiB(rep.Reclaimed), fmtGiB(rep.Used), fmtGiB(rep.After), fmtGiB(rep.Budget), fmtGiB(rep.Free))
		}
	}()
}

// trimStarvedLocked: a trim is overdue by cacheStarveAfter and the Docker
// disk, as last measured, keeps less free than the reserve (m.ct.mu held).
func (m *manager) trimStarvedLocked(now time.Time) bool {
	if m.ct.last.IsZero() || now.Sub(m.ct.last) < m.tun.cacheTrimEvery+cacheStarveAfter {
		return false
	}
	free, total := m.ct.diskFree, m.ct.diskTotal
	if total <= 0 || now.Sub(m.ct.diskAt) > 2*volumeDiskEvery {
		return false // unknown or stale
	}
	return free < m.reserveFor(total)
}

// cachePaused reports whether new container jobs wait for a trim.
func (m *manager) cachePaused() bool {
	m.ct.mu.Lock()
	defer m.ct.mu.Unlock()
	return !m.ct.pause.IsZero()
}

func (m *manager) volumeExists(ctx context.Context, dk *dockerEnv, name string) bool {
	var v struct{ Name string }
	return dk.api.get(ctx, "/volumes/"+name, &v) == nil && v.Name == name
}

// runCacheTrim runs `kran internal cache-trim` in the runner image with
// both cache volumes, as the node's user, without network.
func (m *manager) runCacheTrim(ctx context.Context, dk *dockerEnv) (cacheReport, error) {
	name := m.trimName()
	dk.run(ctx, nil, "rm", "--force", name)
	args := []string{"run", "--rm", "--name", name, "--label", "kran.ns=" + m.ns, "--label", "kran.role=trim",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--network", "none",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--cpus", "1", "--cpu-shares", "256", "--memory", "1g",
		"--mount", "type=volume,src=" + m.volume("cache") + ",dst=" + ctrCache,
		"--mount", "type=volume,src=" + m.volume("tools") + ",dst=" + ctrTools,
		"--entrypoint", ctrKran, m.imageTag(), "internal", "cache-trim",
		"--cache", ctrCache, "--tools", ctrTools,
		"--budget", strconv.FormatInt(m.opts.CacheBudget, 10), "--floor", strconv.FormatInt(m.opts.DiskFloor, 10),
		"--reserve", strconv.FormatInt(m.opts.DiskReserve, 10)}
	out, err := dk.run(ctx, nil, args...)
	if err != nil {
		return cacheReport{}, err
	}
	var rep cacheReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &rep); err != nil {
		return cacheReport{}, fmt.Errorf("trim report: %v (%q)", err, out)
	}
	return rep, nil
}

// checkDockerDiskFloor applies the disk floor to the Docker disk, where
// container jobs write (slots in the volume on macOS, the caches, each
// job's /tmp), as the last cache trim measured it. Below it, the next
// reaper pass trims at once; until that frees enough, container jobs are
// refused with reason disk.
func (m *manager) checkDockerDiskFloor() error {
	m.ct.mu.Lock()
	defer m.ct.mu.Unlock()
	free, total := m.ct.diskFree, m.ct.diskTotal
	if total <= 0 || m.now().Sub(m.ct.diskAt) > 2*cacheTrimEvery {
		return nil // unknown or stale: do not block jobs on it
	}
	floor := m.opts.DiskFloor
	if floor <= 0 {
		floor = diskFloor(total)
	}
	if free >= floor {
		return nil
	}
	m.ct.last, m.ct.slotCheck = time.Time{}, time.Time{}
	return fmt.Errorf("%w: the Docker disk on %s has %s free, needs at least %s; kran is trimming its caches and idle slots, or free space with `docker system prune`",
		ErrDiskFloor, m.opts.NodeName, fmtGiB(free), fmtGiB(floor))
}

// cacheStatus is the last trim's report for the container runtime's
// status ("" fields when none ran yet).
func (m *manager) cacheStatus() *proto.CacheStatus {
	m.ct.mu.Lock()
	defer m.ct.mu.Unlock()
	r := m.ct.report
	if r == nil {
		return nil
	}
	cs := &proto.CacheStatus{Used: r.After, Budget: r.Budget, DiskFree: r.Free, DiskTotal: r.Total,
		Reserve: r.Reserve, Reclaimed: r.Reclaimed, At: m.ct.reportAt.UTC()}
	if m.ct.diskAt.After(m.ct.reportAt) {
		cs.DiskFree, cs.DiskTotal = m.ct.diskFree, m.ct.diskTotal
	}
	return cs
}
