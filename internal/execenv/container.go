package execenv

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/dockercli"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/version"
)

//go:embed runner.Dockerfile
var runnerDockerfile string

// Fixed paths inside job and helper containers.
const (
	ctrSlots = "/kran/slots"
	ctrCache = "/kran/cache"
	ctrTools = "/kran/tools"
	ctrHome  = "/kran/cache/home"
)

// dockerEnv is a reachable Docker daemon.
type dockerEnv struct {
	cli    string // docker CLI
	host   string // DOCKER_HOST used for every CLI call
	sock   string // unix socket path of the API ("" when not a unix socket)
	api    *dockerAPI
	ver    dockerVersion
	info   dockerInfo
	cliEnv []string

	mu      sync.Mutex
	ready   bool              // volumes and image verified
	sockGID int               // group of /var/run/docker.sock inside the Docker host
	syncBin string            // kran binary path inside the sync container
	syncAt  time.Time         // when the sync helper was last seen running with syncBin
	volDirs map[string]string // volume name → its directory on the Docker host
}

// syncRecheck: how long a verified sync helper is trusted without asking
// Docker again. Each check is a docker CLI round trip (100 ms or more on a
// Mac); a helper that vanished meanwhile makes the next run fail without
// side effects, which re-checks and retries (volumeRun).
const syncRecheck = time.Minute

// sameDocker reports whether two probes found the same Docker daemon, so
// what was verified on the first still holds.
func sameDocker(a, b *dockerEnv) bool {
	return a != nil && b != nil && a.cli == b.cli && a.host == b.host &&
		a.info.ID == b.info.ID && a.info.ServerVersion == b.info.ServerVersion
}

// inherit carries the verified state of an earlier probe of the same
// Docker daemon over to a fresh one (Refresh probes every few minutes;
// without this every refresh cost the next container sync a volume setup
// container and a helper check).
func (dk *dockerEnv) inherit(old *dockerEnv) {
	if !sameDocker(old, dk) {
		return
	}
	old.mu.Lock()
	ready, gid, bin, at, dirs := old.ready, old.sockGID, old.syncBin, old.syncAt, old.volDirs
	old.mu.Unlock()
	dk.mu.Lock()
	dk.ready, dk.sockGID, dk.syncBin, dk.syncAt, dk.volDirs = ready, gid, bin, at, dirs
	dk.mu.Unlock()
}

func (m *manager) containerSetting() string {
	switch strings.ToLower(strings.TrimSpace(m.opts.Container)) {
	case "false", "off", "no", "0", "disabled":
		return "off"
	case "true", "on", "yes", "1", "enabled":
		return "on"
	}
	return "auto"
}

// findDockerCLI looks on the login PATH, then where Docker Desktop, Colima,
// OrbStack, Rancher Desktop and Homebrew put it.
func findDockerCLI(path, home string) string { return dockercli.Find(path, home) }

func (m *manager) probeDocker(ctx context.Context) (*dockerEnv, string) {
	if m.containerSetting() == "off" {
		return nil, "container runtime disabled in config (runtime.container.enabled)"
	}
	if !m.tun.probeDocker {
		return nil, "docker probing disabled"
	}
	he := m.hostEnv()
	cli := findDockerCLI(he.path, he.home)
	if cli == "" {
		return nil, "Docker is not installed on " + m.opts.NodeName
	}
	base := []string{"PATH=" + he.path, "HOME=" + he.home, "DOCKER_CLI_HINTS=false"}
	if v := os.Getenv("DOCKER_CONFIG"); v != "" {
		base = append(base, "DOCKER_CONFIG="+v)
	}
	host := os.Getenv("DOCKER_HOST")
	ctxHost := ""
	if host == "" {
		ctxHost = dockercli.ContextHost(ctx, cli, base)
	}
	host = dockercli.Endpoint(host, ctxHost, he.home, os.Getenv("XDG_RUNTIME_DIR"), nil)
	dk := &dockerEnv{cli: cli, host: host, cliEnv: append(base, "DOCKER_HOST="+host)}
	sock, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		return nil, fmt.Sprintf("Docker endpoint %s is not a local socket; kran needs a local Docker", host)
	}
	dk.sock = sock
	dk.api = newDockerAPI(sock)
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := dk.api.get(actx, "/version", &dk.ver); err != nil {
		return nil, fmt.Sprintf("Docker is not reachable on %s (%v); start Docker (or Colima) there", m.opts.NodeName, err)
	}
	if err := dk.api.get(actx, "/info", &dk.info); err != nil {
		return nil, fmt.Sprintf("Docker is not reachable on %s (%v)", m.opts.NodeName, err)
	}
	return dk, ""
}

func normArch(a string) string {
	switch a {
	case "aarch64", "arm64":
		return "arm64"
	case "x86_64", "amd64":
		return "amd64"
	}
	return a
}

// containerRuntime describes the container runtime for Runtimes().
func (m *manager) useVolume() bool { return runtime.GOOS != "linux" || m.tun.forceVolume }

func (m *manager) containerRuntime(ctx context.Context, dk *dockerEnv, reason string) (proto.Runtime, bool) {
	rt := proto.Runtime{Kind: proto.RuntimeContainer, OS: "linux", Arch: runtime.GOARCH}
	if dk == nil {
		// Worth showing on macOS (that is where Linux jobs need it) or when
		// the user enabled it explicitly; noise elsewhere.
		if runtime.GOOS == "darwin" || m.containerSetting() == "on" {
			rt.Reason = reason
			return rt, true
		}
		return rt, false
	}
	rt.CPUs, rt.Mem = dk.info.NCPU, dk.info.MemTotal
	rt.Arch = normArch(dk.info.Architecture)
	rt.Docker = true
	rt.Toolchains = map[string]string{"mise": "auto"}
	switch {
	case dk.info.OSType != "" && dk.info.OSType != "linux":
		rt.Reason = "Docker runs " + dk.info.OSType + " containers; kran needs Linux containers"
	case m.useVolume() && !apiVersionAtLeast(dk.ver.APIVersion, 1, 45):
		rt.Reason = fmt.Sprintf("Docker %s is too old: same-path workspace mounts need Docker 26 or newer", dk.ver.Version)
	case m.useVolume() && m.opts.LinuxBinary == "":
		rt.Reason = "no Linux kran binary for the sync helper; run `kran upgrade` to fetch it"
	case m.useVolume() && checkFile(m.opts.LinuxBinary) != nil:
		rt.Reason = fmt.Sprintf("Linux kran binary %s is missing; run `kran upgrade`", m.opts.LinuxBinary)
	default:
		rt.Available = true
	}
	return rt, true
}

func checkFile(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", p)
	}
	return nil
}

func (m *manager) volume(kind string) string { return m.ns + "-" + kind }
func (m *manager) syncName() string          { return m.ns + "-sync" }

func (m *manager) dockerfile() string {
	if m.tun.runnerDockerfile != "" {
		return m.tun.runnerDockerfile
	}
	return runnerDockerfile
}

func (m *manager) imageTag() string {
	v := strings.NewReplacer("+", "-", "/", "-").Replace(version.Version)
	return m.ns + "-runner:" + v
}

// dockerCmd runs a docker CLI command and returns stdout; errors include
// docker's stderr.
func (dk *dockerEnv) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, dk.cli, args...)
	cmd.Env = dk.cliEnv
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 600 {
			msg = "…" + msg[len(msg)-600:]
		}
		return out, fmt.Errorf("docker %s: %v: %s", args[0], err, msg)
	}
	return out, nil
}

// ensureContainerReady makes sure volumes, the runner image and (macOS) the
// sync helper exist. The image is built once per kran version, serialized
// across processes by a lock file.
func (m *manager) ensureContainerReady(ctx context.Context, onPhase func(proto.Phase)) error {
	dk, reason := m.docker()
	if dk == nil {
		return fmt.Errorf("%w: %s", ErrRuntimeUnavailable, reason)
	}
	for _, rt := range m.Runtimes(ctx) {
		if rt.Kind == proto.RuntimeContainer && !rt.Available {
			return fmt.Errorf("%w: %s", ErrRuntimeUnavailable, rt.Reason)
		}
	}
	dk.mu.Lock()
	defer dk.mu.Unlock()
	if !dk.ready {
		if err := m.ensureImage(ctx, dk, onPhase); err != nil {
			return err
		}
		if err := m.prepareVolumes(ctx, dk); err != nil {
			return err
		}
		dk.ready = true
	}
	if m.useVolume() {
		if err := m.ensureSync(ctx, dk); err != nil {
			return err
		}
	}
	return nil
}

func flockCtx(ctx context.Context, f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// prepareVolumes creates kran's volumes and hands their top dirs to the
// node user (jobs run as that user, never root), and learns the group of
// the Docker socket inside the Docker host so jobs can use it.
func (m *manager) prepareVolumes(ctx context.Context, dk *dockerEnv) error {
	vols := []string{"cache", "tools"}
	if m.useVolume() {
		vols = append(vols, "slots")
	}
	args := []string{"run", "--rm", "--user", "0", "--label", "kran.ns=" + m.ns,
		"-v", "/var/run/docker.sock:/var/run/docker.sock"}
	var dirs []string
	for _, v := range vols {
		if _, err := dk.run(ctx, nil, "volume", "create", "--label", "kran.ns="+m.ns, m.volume(v)); err != nil {
			return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
		}
		args = append(args, "--mount", "type=volume,src="+m.volume(v)+",dst=/kran/"+v)
		dirs = append(dirs, "/kran/"+v)
	}
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	// The cache lock: jobs hold it shared, the cache trim exclusively.
	lock := ctrCache + "/" + cacheLockFile
	script := fmt.Sprintf("mkdir -p %s/bin %s/home %s/mise && touch %s && chown %s %s %s/bin %s/home %s/mise %s && stat -c %%g /var/run/docker.sock 2>/dev/null || echo -1",
		ctrTools, ctrCache, ctrCache, lock, owner, strings.Join(dirs, " "), ctrTools, ctrCache, ctrCache, lock)
	args = append(args, "--entrypoint", "/bin/sh", m.imageTag(), "-c", script)
	out, err := dk.run(ctx, nil, args...)
	if err != nil {
		return fmt.Errorf("%w: preparing volumes: %v", ErrRuntimeUnavailable, err)
	}
	f := strings.Fields(string(out))
	if len(f) > 0 {
		dk.sockGID, _ = strconv.Atoi(f[len(f)-1])
	}
	dk.volDirs = nil // (re)created volumes may live elsewhere
	return nil
}

// ensureSync keeps the long-lived helper container that writes slots into
// the kran-slots volume (macOS: the volume lives inside the Docker VM). The
// helper binary lives in that container's own root-owned file system, not
// in a volume: job containers mount the volumes, and a job that replaced the
// helper would write every repo's slots. (A job given the Docker socket can
// still reach it; SPEC §9.)
func (m *manager) ensureSync(ctx context.Context, dk *dockerEnv) error {
	name := m.syncName()
	sum, err := m.linuxBinaryHash()
	if err != nil {
		return fmt.Errorf("%w: Linux kran binary: %v", ErrRuntimeUnavailable, err)
	}
	bin := "/usr/local/bin/kran-sync-" + sum[:16]
	if dk.syncBin == bin && m.now().Sub(dk.syncAt) < syncRecheck {
		return nil
	}
	out, err := dk.run(ctx, nil, "inspect", "--format", `{{.State.Running}} {{.Config.Image}}`, name)
	running := err == nil && strings.HasPrefix(string(out), "true ") && strings.TrimSpace(strings.TrimPrefix(string(out), "true ")) == m.imageTag()
	if !running {
		// The volumes may be gone with it (docker volume prune, a
		// reset Docker VM): set them up again first.
		if err := m.prepareVolumes(ctx, dk); err != nil {
			return err
		}
		dk.run(ctx, nil, "rm", "--force", name)
		_, err := dk.run(ctx, nil, "run", "--detach", "--name", name, "--label", "kran.ns="+m.ns, "--label", "kran.role=sync",
			"--restart", "unless-stopped", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"--network", "none",
			"--mount", "type=volume,src="+m.volume("slots")+",dst="+ctrSlots,
			"--entrypoint", "/bin/sh", m.imageTag(), "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done")
		if err != nil {
			return fmt.Errorf("%w: starting the sync helper: %v", ErrRuntimeUnavailable, err)
		}
		dk.syncBin = ""
	}
	if dk.syncBin == bin {
		return nil
	}
	if _, err := dk.run(ctx, nil, "exec", name, "test", "-x", bin); err != nil {
		if _, err := dk.run(ctx, nil, "cp", m.opts.LinuxBinary, name+":"+bin); err != nil {
			return fmt.Errorf("%w: copying the Linux kran binary: %v", ErrRuntimeUnavailable, err)
		}
		if _, err := dk.run(ctx, nil, "exec", "--user", "0", name, "chown", "0:0", bin); err != nil {
			return fmt.Errorf("%w: installing the Linux kran binary: %v", ErrRuntimeUnavailable, err)
		}
		dk.run(ctx, nil, "exec", "--user", "0", name, "chmod", "0755", bin)
	}
	dk.syncBin = bin
	dk.syncAt = m.now()
	return nil
}

// linuxBinaryHash hashes the Linux kran binary (runnerBinary), once per
// version of the file (it is tens of MB; every container sync needs its
// name).
func (m *manager) linuxBinaryHash() (string, error) {
	p := m.runnerBinary()
	if p == "" {
		return "", errors.New("no Linux kran binary")
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	m.binMu.Lock()
	defer m.binMu.Unlock()
	if m.binKey.path == p && m.binKey.size == fi.Size() && m.binKey.mtime.Equal(fi.ModTime()) {
		return m.binSum, nil
	}
	sum, err := fileHash(p)
	if err != nil {
		return "", err
	}
	m.binKey.path, m.binKey.size, m.binKey.mtime, m.binSum = p, fi.Size(), fi.ModTime(), sum
	return sum, nil
}

func fileHash(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return hashString(string(b)), nil
}

// volumeApply runs one apply request against a slot in the volume.
func (m *manager) volumeApply(ctx context.Context, req applyReq, open blobOpener) (applyNeed, error) {
	return m.volumeRun(ctx, req, open, nil)
}

// volumeSync runs op sync: one scan of the slot in the volume, then a
// commit in the same helper run when proceed approves the need list (the
// blob store holds everything). The caller holds blobs.trimMu (shared).
func (m *manager) volumeSync(ctx context.Context, slot string, mf *proto.Manifest, proceed func(applyNeed) bool) (applyNeed, bool, error) {
	committed := false
	need, err := m.volumeRun(ctx, applyReq{Op: "sync", Slot: slot, Manifest: mf}, m.openBlob, func(n applyNeed) bool {
		committed = proceed(n)
		return committed
	})
	if err != nil {
		return need, false, err
	}
	return need, committed, nil
}

// volumeRun runs one helper process for req. The manifest goes in the
// compact encoding; a helper that answers nothing (its container was
// gone) is retried once after the helper is checked and restarted.
func (m *manager) volumeRun(ctx context.Context, req applyReq, open blobOpener, proceed func(applyNeed) bool) (applyNeed, error) {
	req.Compact = req.Manifest != nil && proto.SortedByPath(req.Manifest.Entries)
	for attempt := 0; ; attempt++ {
		need, err := m.volumeExec(ctx, req, open, proceed)
		switch {
		case err == nil:
			return need, nil
		case attempt == 0 && errors.Is(err, errNoReply) && ctx.Err() == nil:
			m.logf("sync helper did not answer (%v); checking it and retrying", err)
			if dk, _ := m.docker(); dk != nil {
				dk.mu.Lock()
				dk.syncAt = time.Time{}
				dk.mu.Unlock()
			}
		case attempt == 0 && req.Compact && strings.Contains(err.Error(), "request without manifest"):
			req.Compact = false // a helper from an older kran: JSON it is
		default:
			return need, err
		}
	}
}

func (m *manager) volumeExec(ctx context.Context, req applyReq, open blobOpener, proceed func(applyNeed) bool) (applyNeed, error) {
	if err := m.ensureContainerReady(ctx, nil); err != nil {
		return applyNeed{}, err
	}
	dk, _ := m.docker()
	dk.mu.Lock()
	bin := dk.syncBin
	dk.mu.Unlock()
	cmd := exec.CommandContext(ctx, dk.cli, "exec", "--interactive", m.syncName(), bin, "internal", "apply", "--base", ctrSlots)
	cmd.Env = dk.cliEnv
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return applyNeed{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return applyNeed{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return applyNeed{}, fmt.Errorf("sync helper: %w", err)
	}
	need, _, derr := driveApply(ctx, stdin, stdout, req, open, proceed)
	stdin.Close()
	werr := cmd.Wait()
	if derr != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			derr = fmt.Errorf("%w (%s)", derr, msg)
		}
		return need, derr
	}
	if werr != nil {
		return need, fmt.Errorf("sync helper: %v: %s", werr, strings.TrimSpace(stderr.String()))
	}
	return need, nil
}

// reservedMount reports container paths a workspace must not shadow.
func reservedMount(root string) bool {
	// docker reads --mount as CSV, later keys winning: ",src=/" would add one.
	if root == "/" || !path.IsAbs(root) || strings.ContainsAny(root, ",\"\n\r\x00") {
		return true
	}
	for _, p := range []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/sbin", "/sys", "/usr", "/kran", "/var/run", "/run"} {
		if root == p || strings.HasPrefix(root, p+"/") {
			return true
		}
	}
	return false
}

// containerSpec fills spec for the container runtime (SPEC §6.8).
func (m *manager) containerSpec(ctx context.Context, spec *superSpec, req *StartRequest, s *slot) error {
	if err := m.ensureContainerReady(ctx, req.OnPhase); err != nil {
		return err
	}
	dk, _ := m.docker()
	root := path.Clean(req.Spec.Workspace.Root)
	if reservedMount(root) {
		return fmt.Errorf("%w: workspace %s cannot be mounted at the same path in a container", ErrRuntimeUnavailable, root)
	}
	cwd := path.Clean(req.Spec.Cwd)
	if cwd != root && !strings.HasPrefix(cwd, root+"/") {
		cwd = path.Join(root, req.Spec.Workspace.Rel)
	}
	var slotMount string
	if s.loc == locVolume {
		slotMount = fmt.Sprintf("type=volume,src=%s,dst=%s,volume-subpath=%s", m.volume("slots"), root, m.pool.ref(s).Path)
	} else {
		slotMount = fmt.Sprintf("type=bind,src=%s,dst=%s", m.pool.hostPath(s), root)
	}

	// Toolchains come from mise: the origin's versions unless the
	// workspace pins its own, each installed on first use into the shared
	// tools volume (SPEC §6.8).
	oe := req.Spec.OriginEnv
	if oe == nil {
		oe = &proto.OriginEnv{}
	}
	tools := containerToolchains(oe.Toolchains, req.Spec.Req.Toolchains)
	wrap := func(argv []string) []string {
		if len(tools.eager) == 0 {
			return argv
		}
		return append(append(append([]string{"mise", "exec"}, tools.eager...), "--"), argv...)
	}
	home, homeMount := ctrHomeFor(root, oe.Home, apiVersionAtLeast(dk.ver.APIVersion, 1, 45))
	user := ctrUser(oe, m.hostEnv().user)
	initTools, initShims := tools.initEnv()
	var groups []string
	if !req.Spec.Req.NoDocker && dk.sockGID > 0 {
		groups = append(groups, strconv.Itoa(dk.sockGID))
	}

	fixed := map[string]string{
		"HOME": home, "USER": user, "LOGNAME": user, "SHELL": "/bin/bash", "PATH": ctrPath(home), "TMPDIR": "/tmp",
		"GOCACHE": ctrCache + "/go-build", "GOMODCACHE": ctrCache + "/gomod",
		"npm_config_cache": ctrCache + "/npm", "npm_config_store_dir": ctrCache + "/pnpm-store",
		"UV_CACHE_DIR": ctrCache + "/uv", "PIP_CACHE_DIR": ctrCache + "/pip", "CARGO_HOME": ctrCache + "/cargo",
		"MISE_DATA_DIR": ctrTools + "/mise", "MISE_CACHE_DIR": ctrCache + "/mise", "MISE_STATE_DIR": ctrCache + "/mise-state", "MISE_YES": "1",
		"MISE_GLOBAL_CONFIG_FILE": ctrMiseConf, "MISE_TRUSTED_CONFIG_PATHS": root,
		"KRAN_JOB_ID": req.Spec.ID, "KRAN_NODE": m.opts.NodeName, "KRAN_ORIGIN": req.Origin, "KRAN_ORIGIN_ROOT": root,
		// Read by kran-init, which unsets them before the job starts.
		"KRAN_INIT_UID": strconv.Itoa(os.Getuid()), "KRAN_INIT_GID": strconv.Itoa(os.Getgid()),
		"KRAN_INIT_GROUPS": strings.Join(groups, ","), "KRAN_INIT_USER": user, "KRAN_INIT_GROUP": nodeGroupName(),
		"KRAN_INIT_TOOLS": initTools, "KRAN_INIT_SHIMS": initShims, "KRAN_INIT_TZ": oe.TZ,
	}
	if !req.Spec.Req.NoDocker {
		fixed["KRAN_INIT_DOCKER_MAP"] = encodeDockerMaps(m.dockerMaps(ctx, dk, s, root, home, homeMount))
	}
	if req.Spec.TTY != nil && req.Spec.Env["TERM"] == "" {
		fixed["TERM"] = "xterm-256color"
	}
	// User variables travel by name through the docker CLI's environment
	// (`-e NAME`), so their values never show up in a process listing.
	cliEnv := append([]string(nil), dk.cliEnv...)
	cliKeys := map[string]bool{}
	for _, kv := range cliEnv {
		k, _, _ := strings.Cut(kv, "=")
		cliKeys[k] = true
	}
	var envArgs []string
	userKeys := make([]string, 0, len(req.Spec.Env))
	for k := range req.Spec.Env {
		userKeys = append(userKeys, k)
	}
	sort.Strings(userKeys)
	for _, k := range userKeys {
		v := req.Spec.Env[k]
		if k == "" || strings.ContainsAny(k, "=\x00") || hostSpecificEnv(k) || ctrDropEnv(k, v) {
			continue
		}
		if _, ok := fixed[k]; ok {
			continue
		}
		if cliKeys[k] || strings.HasPrefix(k, "DOCKER_") {
			envArgs = append(envArgs, "-e", k+"="+v)
			continue
		}
		cliEnv = append(cliEnv, k+"="+v)
		envArgs = append(envArgs, "-e", k)
	}
	fixedKeys := make([]string, 0, len(fixed))
	for k := range fixed {
		fixedKeys = append(fixedKeys, k)
	}
	sort.Strings(fixedKeys)
	for _, k := range fixedKeys {
		envArgs = append(envArgs, "-e", k+"="+fixed[k])
	}

	common := func(name string) []string {
		// kran-init starts as root with only the capabilities it needs to
		// write the user entry and drop to the job's uid (KILL: tini
		// forwards signals to it); the job never gains privileges.
		a := []string{"run", "--name", name, "--label", "kran.job=" + req.Spec.ID, "--label", "kran.ns=" + m.ns,
			"--init", "--network", "host",
			"--user", "0:0", "--entrypoint", ctrInit,
			"--cap-drop", "ALL", "--cap-add", "SETUID", "--cap-add", "SETGID", "--cap-add", "KILL",
			"--security-opt", "no-new-privileges",
			"--shm-size", strconv.FormatInt(ctrShmSize(spec.MemMax), 10),
			"--mount", "type=volume,src=" + m.volume("cache") + ",dst=" + ctrCache,
			"--mount", "type=volume,src=" + m.volume("tools") + ",dst=" + ctrTools,
		}
		if homeMount {
			// nocopy: Docker would otherwise hand the home directory to
			// root when it creates the workdir's mount points in it.
			a = append(a, "--mount", "type=volume,src="+m.volume("cache")+",dst="+home+",volume-subpath=home,volume-nocopy=true")
		}
		a = append(a, "--mount", slotMount)
		if !dk.info.rootless() {
			// Docker's default soft limit of 1024 open files breaks file
			// watchers and big test runs; the hard limit stays Docker's.
			a = append(a, "--ulimit", "nofile=65536:524288")
		}
		// The Docker socket lets testcontainers and docker compose work in
		// jobs; it is root-equivalent on rootful Docker (SPEC §9), so a
		// project can opt out. kran-init puts the bind-mount proxy in
		// front of it (dockerproxy.go).
		if !req.Spec.Req.NoDocker {
			a = append(a, "-v", "/var/run/docker.sock:"+ctrDockerHost)
		}
		if spec.MemMax > 0 {
			a = append(a, "--memory", strconv.FormatInt(spec.MemMax, 10), "--memory-swap", strconv.FormatInt(spec.MemMax, 10))
		}
		if spec.CPUs > 0 {
			a = append(a, "--cpus", strconv.FormatFloat(spec.CPUs, 'f', 2, 64))
		}
		if spec.Nice > 0 {
			a = append(a, "--cpu-shares", "256") // niced relative to the owner's own containers (1024)
		}
		return append(a, envArgs...)
	}
	name := m.ns + "-job-" + req.Spec.ID
	run := common(name)
	if req.Spec.TTY != nil {
		run = append(run, "--interactive", "--tty")
	} else if req.Spec.Stdin {
		run = append(run, "--interactive")
	}
	argv := containerArgv(req.Spec.Argv)
	run = append(run, "--workdir", cwd, m.imageTag())
	run = append(run, wrap(argv)...)

	step := &dockerStep{CLI: dk.cli, Env: cliEnv, Name: name, RunArgs: run, API: dk.sock}
	if st := m.setupFor(req, s, hostRootOf(m, s)); st != nil {
		step.SetupName = name + "-setup"
		sa := common(step.SetupName)
		sa = append(sa, "--workdir", root, m.imageTag())
		sa = append(sa, wrap([]string{"/bin/sh", "-c", st.Run})...)
		step.SetupArgs = sa
		spec.Setup = st
	}
	spec.Docker = step
	spec.Argv = argv
	return nil
}

func hostRootOf(m *manager, s *slot) string {
	if s.loc == locHost {
		return m.pool.hostPath(s)
	}
	return ""
}

// containerArgv maps a macOS origin's shell to one the runner image has.
func containerArgv(argv []string) []string {
	out := append([]string(nil), argv...)
	if len(out) > 0 && path.IsAbs(out[0]) && shellNames[path.Base(out[0])] {
		switch path.Base(out[0]) {
		case "zsh":
			out[0] = "/usr/bin/zsh"
		case "bash":
			out[0] = "/bin/bash"
		default:
			out[0] = "/bin/sh"
		}
	}
	return out
}

// volumeDir is where a volume's data lives on the Docker host (cached; a
// volume keeps its place until it is removed, and prepareVolumes then
// forgets them).
func (m *manager) volumeDir(ctx context.Context, dk *dockerEnv, name string) string {
	dk.mu.Lock()
	dir, ok := dk.volDirs[name]
	dk.mu.Unlock()
	if ok {
		return dir
	}
	var v struct{ Mountpoint string }
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := dk.api.get(ctx, "/volumes/"+name, &v); err != nil || !cleanAbs(v.Mountpoint) {
		return ""
	}
	dk.mu.Lock()
	if dk.volDirs == nil {
		dk.volDirs = map[string]string{}
	}
	dk.volDirs[name] = v.Mountpoint
	dk.mu.Unlock()
	return v.Mountpoint
}

// dockerMaps tells the job's Docker proxy where its mounts live on the
// Docker host: the workspace (the slot), the home directory and the
// caches. A mount whose place is unknown is left out (its bind mounts
// then behave as without the proxy).
func (m *manager) dockerMaps(ctx context.Context, dk *dockerEnv, s *slot, root, home string, homeMount bool) []dockerMap {
	var maps []dockerMap
	add := func(ctr, host string) {
		if host != "" && cleanAbs(host) {
			maps = append(maps, dockerMap{Ctr: ctr, Host: host})
		}
	}
	cache, tools := m.volumeDir(ctx, dk, m.volume("cache")), m.volumeDir(ctx, dk, m.volume("tools"))
	if s.loc == locVolume {
		if d := m.volumeDir(ctx, dk, m.volume("slots")); d != "" {
			add(root, d+"/"+m.pool.ref(s).Path)
		}
	} else {
		add(root, m.pool.hostPath(s))
	}
	if homeMount && cache != "" {
		add(home, cache+"/home")
	}
	if cache != "" {
		add(ctrCache, cache)
	}
	if tools != "" {
		add(ctrTools, tools)
	}
	return maps
}
