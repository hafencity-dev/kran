package execenv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Runner image lifecycle: built lazily by the first container job, one
// build at a time, and kept building when that job gives up (the next job
// finds it ready). Images of other kran versions are removed by the reaper.

// imageBuildTimeout bounds one build (a slow network pulling the base
// image and mise); it is not tied to the job that started the build.
const imageBuildTimeout = 15 * time.Minute

// imagePruneEvery spaces out the reaper's image listing.
const imagePruneEvery = time.Hour

// firstUseMessage is the setup phase shown while the image builds.
const firstUseMessage = "preparing the Linux runtime (first use, ~30 s)"

type runnerImage struct {
	mu         sync.Mutex
	ready      bool          // the current tag exists with the current Dockerfile
	building   chan struct{} // closed when the running build ends; nil when idle
	err        error         // result of the last build
	lastPruned time.Time
}

// runnerInit is the job containers' entrypoint, built into the image as
// /usr/local/lib/kran/init (SPEC §6.8).
//
//go:embed runner-init.sh
var runnerInit string

// dockerfileLabel identifies what the image was built from: the
// Dockerfile, kran-init and the kran binary in it.
func (m *manager) dockerfileLabel() string {
	sum, err := m.linuxBinaryHash()
	if err != nil {
		sum = "none"
	}
	return hashString(m.dockerfile() + "\x00" + runnerInit + "\x00" + sum)[:16]
}

// runnerBinary is the Linux kran binary for the image: the one `kran link`
// placed for the Docker VM (macOS), else this binary itself (Linux).
func (m *manager) runnerBinary() string {
	if m.opts.LinuxBinary != "" || runtime.GOOS != "linux" {
		return m.opts.LinuxBinary
	}
	return m.opts.Self
}

// noBinary stands in for a missing kran binary in the image: kran-init
// then gives jobs Docker's socket directly, and no cache trim runs.
const noBinary = "#!/bin/sh\necho 'kran: this runner image has no kran binary' >&2\nexit 1\n"

// buildContext is the image's build context: the Dockerfile, kran-init
// and the kran binary (kranBin; "" for noBinary).
func buildContext(dockerfile, kranBin string) ([]byte, error) {
	kran := []byte(noBinary)
	if kranBin != "" {
		b, err := os.ReadFile(kranBin)
		if err != nil {
			return nil, err
		}
		kran = b
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct {
		name, body string
		mode       int64
	}{{"Dockerfile", dockerfile, 0o644}, {"kran-init", runnerInit, 0o755}, {"kran", string(kran), 0o755}} {
		hdr := &tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(tw, f.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// imageExists asks Docker whether the current tag was built from the
// current Dockerfile.
func (m *manager) imageExists(ctx context.Context, dk *dockerEnv) bool {
	out, err := dk.run(ctx, nil, "image", "inspect", "--format", `{{index .Config.Labels "kran.dockerfile"}}`, m.imageTag())
	return err == nil && strings.TrimSpace(string(out)) == m.dockerfileLabel()
}

// imageBuilt reports (and remembers) whether the runner image is ready.
func (m *manager) imageBuilt(ctx context.Context, dk *dockerEnv) bool {
	m.img.mu.Lock()
	ready := m.img.ready
	m.img.mu.Unlock()
	if ready || dk == nil {
		return ready
	}
	if !m.imageExists(ctx, dk) {
		return false
	}
	m.img.mu.Lock()
	m.img.ready = true
	m.img.mu.Unlock()
	return true
}

// ensureImage returns once the runner image exists, building it if needed.
// The build runs detached from ctx: a job that stops waiting (its origin
// timed out and ran it locally) does not throw away a half-done build.
func (m *manager) ensureImage(ctx context.Context, dk *dockerEnv, onPhase func(proto.Phase)) error {
	if m.imageBuilt(ctx, dk) {
		return nil
	}
	m.img.mu.Lock()
	done := m.img.building
	if done == nil {
		done = make(chan struct{})
		m.img.building = done
		go m.buildImage(dk, done)
	}
	m.img.mu.Unlock()
	if onPhase != nil {
		onPhase(proto.Phase{State: proto.JobSetup, Message: firstUseMessage})
	}
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("%w: the Linux runtime on %s is still being prepared (first use); the next job will use it", ErrRuntimeUnavailable, m.opts.NodeName)
	}
	m.img.mu.Lock()
	defer m.img.mu.Unlock()
	if m.img.ready {
		return nil
	}
	return m.img.err
}

func (m *manager) buildImage(dk *dockerEnv, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), imageBuildTimeout)
	defer cancel()
	err := m.buildImageLocked(ctx, dk)
	m.img.mu.Lock()
	m.img.err = err
	m.img.ready = err == nil
	m.img.building = nil
	m.img.mu.Unlock()
	close(done)
	if err != nil {
		m.logf("container runtime: %v", err)
		return
	}
	m.rtMu.Lock()
	for i := range m.runtimes {
		if m.runtimes[i].Kind == proto.RuntimeContainer {
			m.runtimes[i].FirstUse = false
		}
	}
	m.rtMu.Unlock()
}

// buildImageLocked builds under a lock file so that two daemons (or a
// daemon and the tests) sharing one Docker never build the same tag twice.
func (m *manager) buildImageLocked(ctx context.Context, dk *dockerEnv) error {
	tag := m.imageTag()
	if m.imageExists(ctx, dk) {
		return nil
	}
	lock, err := os.OpenFile(filepath.Join(m.l.state, "runner-image.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("image lock: %w", err)
	}
	defer lock.Close()
	if err := flockCtx(ctx, lock); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if m.imageExists(ctx, dk) {
		return nil
	}
	m.logf("building %s (first container job on this node)", tag)
	start := time.Now()
	bin := m.runnerBinary()
	if bin != "" && checkFile(bin) != nil {
		bin = ""
	}
	bctx, err := buildContext(m.dockerfile(), bin)
	if err != nil {
		return fmt.Errorf("runner image build context: %w", err)
	}
	_, err = dk.run(ctx, bctx, "build", "--quiet", "--tag", tag,
		"--label", "kran.dockerfile="+m.dockerfileLabel(), "--label", "kran.ns="+m.ns, "-")
	if err != nil {
		return fmt.Errorf("%w: building the runner image failed: %v", ErrRuntimeUnavailable, err)
	}
	m.logf("built %s in %s", tag, time.Since(start).Round(time.Second))
	return nil
}

// pruneImages removes runner images of other kran versions in this
// namespace (left behind by upgrades). Images still used by a container
// are kept by Docker; the next pass tries again.
func (m *manager) pruneImages(ctx context.Context) error {
	dk, _ := m.docker()
	if dk == nil {
		return nil
	}
	m.img.mu.Lock()
	due := m.now().Sub(m.img.lastPruned) >= imagePruneEvery
	if due {
		m.img.lastPruned = m.now()
	}
	m.img.mu.Unlock()
	if !due {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := dk.run(ctx, nil, "image", "ls", "--filter", "label=kran.ns="+m.ns,
		"--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return fmt.Errorf("list runner images: %w", err)
	}
	stale := staleRunnerImages(string(out), m.ns+"-runner", m.imageTag())
	m.pruneStoppedContainers(ctx, dk)
	kept := false
	cur := m.imageID(ctx, dk, m.imageTag())
	for _, ref := range stale {
		// Another tag of the current image only goes as a tag: the
		// containers on it are the current ones.
		if id := m.imageID(ctx, dk, ref); id != "" && id != cur {
			if busy := m.releaseImage(ctx, dk, ref); busy != "" {
				m.logf("reaper: keeping %s while %s runs on it", ref, busy)
				kept = true
				continue
			}
		}
		if _, err := dk.run(ctx, nil, "image", "rm", ref); err != nil {
			m.logf("reaper: keeping %s: %v", ref, err)
			kept = true
			continue
		}
		m.logf("reaper: removed old runner image %s", ref)
	}
	if kept {
		// A job of the old version still runs: try again soon.
		m.img.mu.Lock()
		m.img.lastPruned = m.now().Add(imageRetryAfter - imagePruneEvery)
		m.img.mu.Unlock()
	}
	// Rebuilds of the same tag (a new kran binary, a changed Dockerfile)
	// leave the old image dangling.
	if out, err := dk.run(ctx, nil, "image", "prune", "--force", "--filter", "label=kran.ns="+m.ns); err != nil {
		m.logf("reaper: pruning dangling runner images: %v", err)
	} else if r := pruneSummary(string(out)); r != "" {
		m.logf("reaper: removed dangling runner images: %s", r)
	}
	return nil
}

// imageID is the ID of image ref ("" when unknown).
func (m *manager) imageID(ctx context.Context, dk *dockerEnv, ref string) string {
	if dk.api == nil {
		return ""
	}
	var img struct{ Id string }
	if dk.api.get(ctx, "/images/"+url.PathEscape(ref)+"/json", &img) != nil {
		return ""
	}
	return img.Id
}

// imageRetryAfter: how soon the reaper tries again to remove an old
// runner image a job still used.
const imageRetryAfter = 5 * time.Minute

// releaseImage removes this namespace's containers that keep an old
// runner image (ref) alive without doing work: the sync helper of the old
// version (ensureSync starts a new one from the current image when a sync
// needs it; it never uses one of another image) and stopped containers.
// It returns what still runs on ref (a job of the old version), "" when
// nothing does.
func (m *manager) releaseImage(ctx context.Context, dk *dockerEnv, ref string) string {
	if dk.api == nil {
		return ""
	}
	cs, err := dk.api.containersWhere(ctx, map[string][]string{"ancestor": {ref}})
	if err != nil {
		return ""
	}
	for _, c := range cs {
		if c.Labels["kran.ns"] != m.ns {
			if _, job := c.Labels["kran.job"]; !job {
				continue // not ours: Docker keeps the image for it
			}
		}
		running := c.State == "running" || c.State == "restarting" || c.State == "paused"
		jobID, job := c.Labels["kran.job"]
		if job && (running || m.jobSupervised(jobID)) {
			// Its supervisor still owns it (and reads its exit status).
			return "job container " + strings.TrimPrefix(firstName(c.Names), "/")
		}
		if running && c.Labels["kran.role"] != "sync" && c.Labels["kran.role"] != "trim" {
			return "container " + strings.TrimPrefix(firstName(c.Names), "/")
		}
		if _, err := dk.run(ctx, nil, "rm", "--force", "--volumes", c.ID); err != nil {
			return "container " + strings.TrimPrefix(firstName(c.Names), "/")
		}
		m.logf("reaper: removed %s container %s of the old runner image %s", orDefault(c.Labels["kran.role"], "stopped"), strings.TrimPrefix(firstName(c.Names), "/"), ref)
		if c.Labels["kran.role"] == "sync" {
			dk.mu.Lock()
			dk.syncBin, dk.syncAt = "", time.Time{}
			dk.mu.Unlock()
		}
	}
	return ""
}

// jobSupervised: a supervisor of this state dir still owns job id (its
// status is not written yet and the supervisor lives).
func (m *manager) jobSupervised(id string) bool {
	if validJobID(id) != nil {
		return false
	}
	dir := m.l.job(id)
	if _, err := os.Stat(filepath.Join(dir, fileSpec)); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, fileStatus)); err == nil {
		return false
	}
	return supervisorAlive(dir)
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// pruneStoppedContainers removes stopped containers of this namespace that
// are not a job's (those are the supervisor's and reapContainers'
// business): a sync helper or trim left behind by a crash or a Docker
// restart. A running one is never touched.
func (m *manager) pruneStoppedContainers(ctx context.Context, dk *dockerEnv) {
	if dk.api == nil {
		return
	}
	cs, err := dk.api.containers(ctx, "kran.ns="+m.ns)
	if err != nil {
		return
	}
	var ids []string
	for _, c := range cs {
		if _, job := c.Labels["kran.job"]; job || c.Labels["kran.ns"] != m.ns {
			continue
		}
		switch c.State {
		case "exited", "created", "dead":
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if _, err := dk.run(ctx, nil, append([]string{"rm", "--volumes"}, ids...)...); err != nil {
		m.logf("reaper: removing stopped containers: %v", err)
		return
	}
	m.logf("reaper: removed %d stopped kran containers", len(ids))
}

// pruneSummary sums up docker's prune output: images deleted and the
// space reclaimed ("" when nothing was deleted).
func pruneSummary(out string) string {
	n, space := 0, ""
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "deleted:") {
			n++
		}
		if v, ok := strings.CutPrefix(ln, "Total reclaimed space:"); ok {
			space = strings.TrimSpace(v)
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d layers or images, %s reclaimed", n, space)
}

// staleRunnerImages picks repo:tag lines of repo other than current.
func staleRunnerImages(list, repo, current string) []string {
	var out []string
	for _, ln := range strings.Split(list, "\n") {
		ref := strings.TrimSpace(ln)
		r, tag, ok := strings.Cut(ref, ":")
		if !ok || r != repo || tag == "" || tag == "<none>" || ref == current {
			continue
		}
		out = append(out, ref)
	}
	return out
}
