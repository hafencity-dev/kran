package execenv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// dockerfileLabel identifies what the image was built from.
func (m *manager) dockerfileLabel() string {
	return hashString(m.dockerfile() + "\x00" + runnerInit)[:16]
}

// buildContext is the image's build context: the Dockerfile and kran-init.
func buildContext(dockerfile string) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct {
		name, body string
		mode       int64
	}{{"Dockerfile", dockerfile, 0o644}, {"kran-init", runnerInit, 0o755}} {
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
	bctx, err := buildContext(m.dockerfile())
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
	for _, ref := range stale {
		if _, err := dk.run(ctx, nil, "image", "rm", ref); err != nil {
			m.logf("reaper: keeping %s: %v", ref, err)
			continue
		}
		m.logf("reaper: removed old runner image %s", ref)
	}
	return nil
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
