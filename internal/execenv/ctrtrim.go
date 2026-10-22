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

type cacheTrim struct {
	mu       sync.Mutex
	holds    int                // container jobs being started
	running  context.CancelFunc // non-nil while a trim runs
	done     chan struct{}
	last     time.Time
	report   *cacheReport
	reportAt time.Time
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
	if m.ct.running != nil || m.ct.holds > 0 || m.now().Sub(m.ct.last) < m.tun.cacheTrimEvery {
		return
	}
	if m.containerJobsActive() {
		return // the next pass tries again
	}
	// Checked at most once per period: most nodes never run a container
	// job, and these ask Docker.
	m.ct.last = m.now()
	if !m.imageBuilt(ctx, dk) || !m.volumeExists(ctx, dk, m.volume("cache")) {
		return // no container job ran here yet
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
		if err == nil && rep.Total > 0 { // measured (a trim skipped for a running job was not)
			m.ct.report, m.ct.reportAt = &rep, m.now()
		}
		m.ct.mu.Unlock()
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
		"--budget", strconv.FormatInt(m.opts.CacheBudget, 10), "--floor", strconv.FormatInt(m.opts.DiskFloor, 10)}
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
	r := m.ct.report
	if r == nil || r.Total <= 0 || m.now().Sub(m.ct.reportAt) > 2*cacheTrimEvery {
		return nil // unknown or stale: do not block jobs on it
	}
	floor := m.opts.DiskFloor
	if floor <= 0 {
		floor = diskFloor(r.Total)
	}
	if r.Free >= floor {
		return nil
	}
	m.ct.last = time.Time{}
	return fmt.Errorf("%w: the Docker disk on %s has %s free, needs at least %s; kran is trimming its caches, or free space with `docker system prune`",
		ErrDiskFloor, m.opts.NodeName, fmtGiB(r.Free), fmtGiB(floor))
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
	return &proto.CacheStatus{Used: r.After, Budget: r.Budget, DiskFree: r.Free, DiskTotal: r.Total,
		Reclaimed: r.Reclaimed, At: m.ct.reportAt.UTC()}
}
