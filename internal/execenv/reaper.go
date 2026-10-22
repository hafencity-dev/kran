package execenv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Reap is one reaper pass (SPEC §6.2): orphan scopes, containers and
// process trees of jobs that are no longer supervised; old job dirs; the
// blob cache; idle slots and abandoned slot acquisitions.
func (m *manager) Reap(ctx context.Context) error {
	var errs []error
	m.pruneJobs()
	for _, s := range m.pool.abandoned(m.tun.slotHoldIdle) {
		m.logf("releasing slot %s/%d: acquired but unused for %s", short(s.repo), s.index, m.tun.slotHoldIdle)
		m.pool.release(s)
	}
	if err := m.reapScopes(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := m.reapContainers(ctx); err != nil {
		errs = append(errs, err)
	}
	m.reapTmpVolumes(ctx)
	m.reapProcesses()
	if err := m.pruneImages(ctx); err != nil {
		errs = append(errs, err)
	}
	m.maybeTrimCaches(ctx)
	if err := m.reapJobDirs(); err != nil {
		errs = append(errs, err)
	}
	m.trimBlobs()
	if err := m.gcSlots(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// pruneJobs forgets finished jobs after a while (their dirs remain and
// Attach re-reads them).
func (m *manager) pruneJobs() {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	for id, js := range m.jobs {
		js.mu.Lock()
		old := js.isDone() && time.Since(js.endedAt) > time.Hour
		js.mu.Unlock()
		if old {
			delete(m.jobs, id)
		}
	}
}

// jobOrphaned: the job has a supervised job dir here and has ended or has
// no live supervisor. Jobs unknown to this state dir are never touched.
func (m *manager) jobOrphaned(id string) bool {
	if validJobID(id) != nil {
		return false
	}
	dir := m.l.job(id)
	if _, err := os.Stat(filepath.Join(dir, fileSpec)); err != nil {
		return false
	}
	// The supervisor cleans up before it writes the status, so anything
	// still around after that is left over, even while the supervisor
	// process itself is still exiting.
	if _, err := os.Stat(filepath.Join(dir, fileStatus)); err == nil {
		return true
	}
	return !supervisorAlive(dir)
}

func (m *manager) reapScopes(ctx context.Context) error {
	if m.systemdRun == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	prefix := m.ns + "-job-"
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "list-units", "--type=scope", "--all",
		"--plain", "--no-legend", "--no-pager", prefix+"*").Output()
	if err != nil {
		return fmt.Errorf("list scopes: %w", err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		unit := f[0]
		id, ok := strings.CutSuffix(strings.TrimPrefix(unit, prefix), ".scope")
		if !ok || !strings.HasPrefix(unit, prefix) || !m.jobOrphaned(id) {
			continue
		}
		m.logf("reaper: stopping orphan scope %s", unit)
		exec.CommandContext(ctx, "systemctl", "--user", "kill", "--signal=SIGKILL", unit).Run()
		exec.CommandContext(ctx, "systemctl", "--user", "stop", unit).Run()
	}
	return nil
}

func (m *manager) reapContainers(ctx context.Context) error {
	dk, _ := m.docker()
	if dk == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cs, err := dk.api.containers(ctx, "kran.job")
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	var ids []string
	for _, c := range cs {
		job := c.Labels["kran.job"]
		// Our own job containers carry our namespace; containers a job
		// started itself carry only kran.job. Either way the job must be
		// one of ours and unsupervised.
		if ns, ok := c.Labels["kran.ns"]; ok && ns != m.ns {
			continue
		}
		if m.jobOrphaned(job) {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	m.logf("reaper: removing %d orphan containers", len(ids))
	_, err = dk.run(ctx, nil, append([]string{"rm", "--force", "--volumes"}, ids...)...)
	return err
}

// reapTmpVolumes removes job /tmp volumes no container uses any more.
// Removing a job's container removes its /tmp with it (--volumes); this
// catches one removed otherwise (by hand, or by a Docker that crashed).
func (m *manager) reapTmpVolumes(ctx context.Context) {
	dk, _ := m.docker()
	if dk == nil || dk.api == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	f, _ := json.Marshal(map[string][]string{"label": {"kran.ns=" + m.ns, "kran.tmp=1"}, "dangling": {"true"}})
	var out struct {
		Volumes []struct {
			Name   string
			Labels map[string]string
		}
	}
	if err := dk.api.get(ctx, "/volumes?filters="+url.QueryEscape(string(f)), &out); err != nil {
		return
	}
	var names []string
	for _, v := range out.Volumes {
		// Docker filtered already; check again, as deleting a volume
		// cannot be undone.
		if v.Labels["kran.ns"] == m.ns && v.Labels["kran.tmp"] == "1" {
			names = append(names, v.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	if _, err := dk.run(ctx, nil, append([]string{"volume", "rm"}, names...)...); err != nil {
		m.logf("reaper: removing left-over job /tmp volumes: %v", err)
		return
	}
	m.logf("reaper: removed %d left-over job /tmp volumes", len(names))
}

// reapProcesses kills processes that still carry the KRAN_JOB_ID of an
// unsupervised job of ours (pgid-mode jobs whose supervisor was killed).
func (m *manager) reapProcesses() {
	procs, err := listProcs()
	if err != nil {
		return
	}
	uid := os.Getuid()
	self := os.Getpid()
	verdict := map[string]bool{}
	for _, p := range procs {
		if p.pid == self || p.pid <= 1 || !procOwnedBy(p.pid, uid) {
			continue
		}
		id := procEnvValue(p.pid, "KRAN_JOB_ID")
		if id == "" {
			continue
		}
		orphan, ok := verdict[id]
		if !ok {
			orphan = m.jobOrphaned(id)
			verdict[id] = orphan
		}
		if orphan {
			m.logf("reaper: killing orphan process %d of job %s", p.pid, id)
			syscall.Kill(p.pid, syscall.SIGKILL)
		}
	}
}

func (m *manager) reapJobDirs() error {
	ents, err := os.ReadDir(m.l.jobsDir())
	if err != nil {
		return nil
	}
	cut := m.now().Add(-m.tun.jobMaxAge)
	var errs []error
	for _, e := range ents {
		if !e.IsDir() || validJobID(e.Name()) != nil {
			continue
		}
		dir := m.l.job(e.Name())
		if _, err := os.Stat(filepath.Join(dir, fileSpec)); err != nil {
			continue // not a supervised job dir (the daemon may keep others here)
		}
		fi, err := os.Stat(filepath.Join(dir, fileStatus))
		if err != nil {
			continue // running, or the reaper above will settle it
		}
		if fi.ModTime().After(cut) || supervisorAlive(dir) {
			continue
		}
		m.jobsMu.Lock()
		delete(m.jobs, e.Name())
		m.jobsMu.Unlock()
		var spec superSpec
		if readJSON(filepath.Join(dir, fileSpec), &spec) == nil && spec.CtlPath != "" && !strings.HasPrefix(spec.CtlPath, dir) {
			os.Remove(spec.CtlPath)
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *manager) blobCap() int64 {
	if m.opts.BlobCap > 0 {
		return m.opts.BlobCap
	}
	_, total, err := diskSpace(m.l.state)
	if err != nil || total == 0 {
		return 20 * gib
	}
	return min(20*gib, total/10)
}

func (m *manager) trimBlobs() {
	n, err := m.blobs.trim(m.blobCap())
	if n > 0 {
		m.logf("blob cache: removed %d least recently used blobs", n)
	}
	if err != nil {
		m.logf("blob cache trim: %v", err)
	}
}

func (m *manager) gcSlots(ctx context.Context) error {
	var errs []error
	for _, s := range m.pool.stale(m.tun.slotMaxIdle) {
		m.logf("removing slot %s/%d unused since %s", short(s.repo), s.index, s.meta.LastUsed.Format(time.DateTime))
		if s.loc == locHost {
			if err := os.RemoveAll(m.pool.hostPath(s)); err != nil {
				errs = append(errs, err)
				continue
			}
		} else {
			if _, err := m.volumeApply(ctx, applyReq{Op: "remove", Slot: m.pool.ref(s).Path}, nil); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := m.pool.forget(s); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
