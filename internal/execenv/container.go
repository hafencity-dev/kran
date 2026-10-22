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
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

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
	ready   bool   // volumes and image verified
	sockGID int    // group of /var/run/docker.sock inside the Docker host
	syncBin string // kran binary path inside the sync container
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
func findDockerCLI(path, home string) string {
	if p, err := lookPathEnv("docker", path, "/"); err == nil {
		return p
	}
	cands := []string{"/usr/local/bin/docker", "/opt/homebrew/bin/docker", "/usr/bin/docker",
		"/Applications/Docker.app/Contents/Resources/bin/docker"}
	if home != "" {
		cands = append(cands, filepath.Join(home, ".orbstack", "bin", "docker"), filepath.Join(home, ".rd", "bin", "docker"),
			filepath.Join(home, ".docker", "bin", "docker"))
	}
	for _, c := range cands {
		if checkExecutable(c) == nil {
			return c
		}
	}
	return ""
}

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
	if host == "" {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(cctx, cli, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		cmd.Env = base
		out, err := cmd.Output()
		cancel()
		if err == nil {
			host = strings.TrimSpace(string(out))
		}
	}
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
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

func (m *manager) ensureImage(ctx context.Context, dk *dockerEnv, onPhase func(proto.Phase)) error {
	tag := m.imageTag()
	want := hashString(m.dockerfile())[:16]
	have := func() bool {
		out, err := dk.run(ctx, nil, "image", "inspect", "--format", `{{index .Config.Labels "kran.dockerfile"}}`, tag)
		return err == nil && strings.TrimSpace(string(out)) == want
	}
	if have() {
		return nil
	}
	// One build at a time per node, also across daemon restarts and tests.
	lock, err := os.OpenFile(filepath.Join(m.l.state, "runner-image.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("image lock: %w", err)
	}
	defer lock.Close()
	if err := flockCtx(ctx, lock); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if have() {
		return nil
	}
	if onPhase != nil {
		onPhase(proto.Phase{State: proto.JobSetup, Message: fmt.Sprintf("building the %s image on %s (first use only, takes a few minutes)", tag, m.opts.NodeName)})
	}
	m.logf("building %s", tag)
	_, err = dk.run(ctx, []byte(m.dockerfile()), "build", "--quiet", "--tag", tag,
		"--label", "kran.dockerfile="+want, "--label", "kran.ns="+m.ns, "-")
	if err != nil {
		return fmt.Errorf("%w: building the runner image failed: %v", ErrRuntimeUnavailable, err)
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
	script := fmt.Sprintf("mkdir -p %s/bin %s/home %s/mise && chown %s %s %s/bin %s/home %s/mise && stat -c %%g /var/run/docker.sock 2>/dev/null || echo -1",
		ctrTools, ctrCache, ctrCache, owner, strings.Join(dirs, " "), ctrTools, ctrCache, ctrCache)
	args = append(args, "--entrypoint", "/bin/sh", m.imageTag(), "-c", script)
	out, err := dk.run(ctx, nil, args...)
	if err != nil {
		return fmt.Errorf("%w: preparing volumes: %v", ErrRuntimeUnavailable, err)
	}
	f := strings.Fields(string(out))
	if len(f) > 0 {
		dk.sockGID, _ = strconv.Atoi(f[len(f)-1])
	}
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
	sum, err := fileHash(m.opts.LinuxBinary)
	if err != nil {
		return fmt.Errorf("%w: Linux kran binary: %v", ErrRuntimeUnavailable, err)
	}
	bin := "/usr/local/bin/kran-sync-" + sum[:16]
	out, err := dk.run(ctx, nil, "inspect", "--format", `{{.State.Running}} {{.Config.Image}}`, name)
	running := err == nil && strings.HasPrefix(string(out), "true ") && strings.TrimSpace(strings.TrimPrefix(string(out), "true ")) == m.imageTag()
	if !running {
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
	return nil
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
	need, _, derr := driveApply(ctx, stdin, stdout, req, open)
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

	// Toolchains always come from mise in the container (installed once
	// into the shared tools volume).
	var miseTools []string
	keys := make([]string, 0, len(req.Spec.Req.Toolchains))
	for k := range req.Spec.Req.Toolchains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "mise" {
			continue
		}
		miseTools = append(miseTools, k+"@"+miseVersion(req.Spec.Req.Toolchains[k]))
	}
	wrap := func(argv []string) []string {
		if len(miseTools) == 0 {
			return argv
		}
		return append(append(append([]string{"mise", "exec"}, miseTools...), "--"), argv...)
	}

	fixed := map[string]string{
		"HOME": ctrHome, "TMPDIR": "/tmp",
		"GOCACHE": ctrCache + "/go-build", "GOMODCACHE": ctrCache + "/gomod",
		"npm_config_cache": ctrCache + "/npm", "npm_config_store_dir": ctrCache + "/pnpm-store",
		"UV_CACHE_DIR": ctrCache + "/uv", "PIP_CACHE_DIR": ctrCache + "/pip", "CARGO_HOME": ctrCache + "/cargo",
		"MISE_DATA_DIR": ctrTools + "/mise", "MISE_CACHE_DIR": ctrCache + "/mise", "MISE_YES": "1",
		"MISE_TRUSTED_CONFIG_PATHS": root,
		"KRAN_JOB_ID":               req.Spec.ID, "KRAN_NODE": m.opts.NodeName, "KRAN_ORIGIN": req.Origin, "KRAN_ORIGIN_ROOT": root,
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
		if k == "" || strings.ContainsAny(k, "=\x00") || hostSpecificEnv(k) {
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
		a := []string{"run", "--name", name, "--label", "kran.job=" + req.Spec.ID, "--label", "kran.ns=" + m.ns,
			"--init", "--network", "host",
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"-v", "/var/run/docker.sock:/var/run/docker.sock",
			"--mount", slotMount,
			"--mount", "type=volume,src=" + m.volume("cache") + ",dst=" + ctrCache,
			"--mount", "type=volume,src=" + m.volume("tools") + ",dst=" + ctrTools,
		}
		if dk.sockGID > 0 {
			a = append(a, "--group-add", strconv.Itoa(dk.sockGID))
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
