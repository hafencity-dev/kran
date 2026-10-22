package execenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Container runtime tests need a Docker daemon and create only resources
// named kran-test-* (removed at the end). Run them with
//
//	KRAN_TEST_DOCKER=1 go test -run Container ./internal/execenv/
//
// On Linux they run twice: slots on the host (bind mount, the Linux
// default) and slots in the volume via the sync helper (the macOS path).

const testRunnerDockerfile = "FROM alpine:3.20\nRUN apk add --no-cache coreutils setpriv flock\nCOPY kran-init /usr/local/lib/kran/init\nCOPY kran /usr/local/lib/kran/kran\nLABEL kran.runner=\"test\"\n"

func dockerTests(t *testing.T) {
	t.Helper()
	if os.Getenv("KRAN_TEST_DOCKER") != "1" {
		t.Skip("set KRAN_TEST_DOCKER=1 to run container runtime tests")
	}
}

var applyBin struct {
	sync.Once
	path string
	err  error
}

// linuxApplyBinary builds the static stand-in for the Linux kran binary for
// the Docker host's architecture.
func linuxApplyBinary(t *testing.T, arch string) string {
	t.Helper()
	applyBin.Do(func() {
		dir, err := os.MkdirTemp("", "kran-test-applycmd-")
		if err != nil {
			applyBin.err = err
			return
		}
		applyBin.path = filepath.Join(dir, "kran-linux")
		cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", applyBin.path, "./testdata/applycmd")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			applyBin.err = fmt.Errorf("%v: %s", err, out)
		}
	})
	if applyBin.err != nil {
		t.Fatal(applyBin.err)
	}
	return applyBin.path
}

func dockerArch(t *testing.T) string {
	out, err := exec.Command(dockerCLIForTests(), "info", "--format", "{{.Architecture}}").Output()
	if err != nil {
		t.Skipf("docker not reachable: %v", err)
	}
	return normArch(strings.TrimSpace(string(out)))
}

func dockerCLIForTests() string {
	home, _ := os.UserHomeDir()
	if p := findDockerCLI(os.Getenv("PATH"), home); p != "" {
		return p
	}
	return "docker"
}

func newContainerEnv(t *testing.T, volume bool, opts ...mgrOpt) *testEnv {
	t.Helper()
	dockerTests(t)
	bin := linuxApplyBinary(t, dockerArch(t))
	all := append([]mgrOpt{func(o *Options, tun *tunables) {
		tun.probeDocker = true
		tun.runnerDockerfile = testRunnerDockerfile
		tun.forceVolume = volume
		o.LinuxBinary = bin
	}}, opts...)
	te := newTestEnv(t, all...)
	rts := te.m.Runtimes(context.Background())
	if len(rts) < 2 || rts[1].Kind != proto.RuntimeContainer || !rts[1].Available {
		t.Fatalf("container runtime not available: %+v", rts)
	}
	t.Cleanup(func() { cleanupDocker(te) })
	return te
}

// cleanupDocker removes what the tests created (kran-test-* only).
func cleanupDocker(te *testEnv) {
	dk, _ := te.m.docker()
	if dk == nil {
		return
	}
	ctx := context.Background()
	if out, err := dk.run(ctx, nil, "ps", "--all", "--quiet", "--filter", "label=kran.ns="+te.m.ns); err == nil {
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			dk.run(ctx, nil, append([]string{"rm", "--force", "--volumes"}, ids...)...)
		}
	}
	for _, v := range []string{"slots", "cache", "tools"} {
		dk.run(ctx, nil, "volume", "rm", "--force", te.m.volume(v))
	}
}

const ctrRoot = "/work/kran-test/proj"

// noHeartbeat lets the job's lease expire.
func noHeartbeat() jobOpt {
	return func(r *StartRequest) { r.Spec.Labels = map[string]string{"test.noheartbeat": "1"} }
}

func (te *testEnv) startCtr(ref proto.SlotRef, argv []string, opts ...jobOpt) Process {
	te.t.Helper()
	req := StartRequest{
		Spec: proto.JobSpec{ID: newJobID(), Argv: argv, Cwd: ctrRoot + "/sub",
			Workspace: proto.Workspace{Root: ctrRoot, RepoID: ref.RepoID, Rel: "sub"}},
		Slot: ref, Runtime: proto.RuntimeContainer, Origin: "originnode",
	}
	for _, o := range opts {
		o(&req)
	}
	p, err := te.m.Start(context.Background(), req)
	if err != nil {
		te.t.Fatal(err)
	}
	if req.Spec.Labels["test.noheartbeat"] == "" {
		heartbeat(te.t, p)
	}
	return p
}

// heartbeat renews the job's lease like the origin daemon would.
func heartbeat(t *testing.T, p Process) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
				if err := p.Renew(); err != nil {
					return
				}
			}
		}
	}()
}

func TestContainerRuntime(t *testing.T) {
	variants := []bool{runtime.GOOS != "linux"}
	if runtime.GOOS == "linux" {
		variants = append(variants, true)
	}
	for _, volume := range variants {
		name := "hostslots"
		if volume {
			name = "volumeslots"
		}
		t.Run(name, func(t *testing.T) { testContainerRuntime(t, volume) })
	}
}

func testContainerRuntime(t *testing.T, volume bool) {
	te := newContainerEnv(t, volume, withMemMax(64<<20), func(_ *Options, tun *tunables) { tun.leaseTTL = 5 * time.Second })
	files := map[string]string{"sub/f.txt": "inside the container\n", "lock": "v1"}
	ref, err := te.m.AcquireSlot(context.Background(), testRepo, "wt", proto.RuntimeContainer)
	if err != nil {
		t.Fatal(err)
	}
	if volume == filepath.IsAbs(ref.Path) {
		t.Fatalf("slot location: %+v", ref)
	}
	te.commit(ref, testRepo, files)
	// Re-diff after commit: nothing left to send.
	mt := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC).UnixNano()
	mf := &proto.Manifest{RepoID: testRepo}
	for p, c := range files {
		mf.Entries = append(mf.Entries, proto.ManifestEntry{Path: p, Mode: 0o644, Size: int64(len(c)), MTime: mt, Hash: hashOf(c)})
	}
	if plan, err := te.m.Diff(context.Background(), ref, mf); err != nil || plan.Changed != 0 || len(plan.Missing) != 0 {
		t.Fatalf("diff after commit: %+v %v", plan, err)
	}

	t.Run("basics", func(t *testing.T) {
		p := te.startCtr(ref, sh(`pwd; cat f.txt; id -u; echo "job=$KRAN_JOB_ID root=$KRAN_ORIGIN_ROOT home=$HOME"; stat -c %Y.%y f.txt; echo oops >&2; exit 7`),
			withEnv("FOO", "bar"))
		exit := waitExit(t, p, 120*time.Second)
		out, errOut := output(t, p)
		if exit.Code != 7 || exit.State != proto.JobFailed {
			t.Fatalf("exit %+v\n%s\n%s", exit, out, errOut)
		}
		for _, want := range []string{ctrRoot + "/sub\n", "inside the container", fmt.Sprintf("%d\n", os.Getuid()), "job=" + p.JobID(), "root=" + ctrRoot, "1735787045.", "03:04:05.000000006"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		if errOut != "oops\n" {
			t.Errorf("stderr %q", errOut)
		}
		assertNoContainer(t, te, "kran-test-job-"+p.JobID())
	})

	// The job runs as the node's uid with a passwd entry named like the
	// origin's user, the origin's home, and only kran-init's work done as
	// root (SPEC §6.8).
	t.Run("user-and-home", func(t *testing.T) {
		p := te.startCtr(ref, sh(`echo "who=$(id -un) uid=$(id -u) grp=$(id -gn)"; grep "^alice:" /etc/passwd; echo "home=$HOME user=$USER logname=$LOGNAME"; touch "$HOME/.kran-test-w" && echo home-writable; env | grep -c '^KRAN_INIT_' ; ls /usr/local/lib/kran/shims | tr '\n' ' '; echo; grep -c '^node = "24.8.0"' /usr/local/lib/kran/mise.toml; grep NoNewPrivs /proc/self/status; touch /etc/passwd 2>/dev/null || echo etc-readonly`),
			func(r *StartRequest) {
				r.Spec.OriginEnv = &proto.OriginEnv{User: "alice", Home: "/work/kran-test", Toolchains: map[string]string{"node": "24.8.0", "go": "1.25.1"}}
			})
		exit := waitExit(t, p, 120*time.Second)
		out, errOut := output(t, p)
		if exit.Code != 0 {
			t.Fatalf("exit %+v\n%s\n%s", exit, out, errOut)
		}
		for _, want := range []string{fmt.Sprintf("who=alice uid=%d", os.Getuid()), fmt.Sprintf("alice:x:%d:%d:", os.Getuid(), os.Getgid()) + "kran job:/work/kran-test:",
			"home=/work/kran-test user=alice logname=alice", "home-writable", "\n0\n", "corepack go gofmt node npm npx", "\n1\n", "NoNewPrivs:\t1", "etc-readonly"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s\n%s", want, out, errOut)
			}
		}
	})

	// The sync helper writes every repo's slots: a job must not be able to
	// replace it (it used to live in the job-writable tools volume).
	if volume {
		t.Run("sync-helper-out-of-reach", func(t *testing.T) {
			p := te.startCtr(ref, sh(`for f in $(find /kran -name 'kran-*' 2>/dev/null); do rm -f "$f" && printf '#!/bin/sh\nexit 3\n' > "$f" && chmod 755 "$f" && echo "replaced $f"; done; true`))
			waitExit(t, p, 120*time.Second)
			out, _ := output(t, p)
			if strings.Contains(out, "replaced") {
				t.Errorf("job replaced the helper: %s", out)
			}
			te.commit(ref, testRepo, map[string]string{"sub/f.txt": "inside the container\n", "lock": "v2"})
			te.commit(ref, testRepo, files)
		})
	}

	// .kran.toml docker = false / --no-docker: no Docker socket in the job.
	t.Run("no-docker", func(t *testing.T) {
		probe := sh(`if [ -e /var/run/docker.sock ]; then echo sock; else echo nosock; fi`)
		p := te.startCtr(ref, probe)
		waitExit(t, p, 120*time.Second)
		if out, _ := output(t, p); !strings.Contains(out, "sock") || strings.Contains(out, "nosock") {
			t.Fatalf("default job: %q", out)
		}
		p = te.startCtr(ref, probe, func(r *StartRequest) { r.Spec.Req.NoDocker = true })
		waitExit(t, p, 120*time.Second)
		if out, _ := output(t, p); !strings.Contains(out, "nosock") {
			t.Fatalf("NoDocker job: %q", out)
		}
	})

	// The job's Docker socket is kran's proxy (the real-runner test runs
	// docker in a job); the job holds the cache lock through a process
	// outside its own tree and has no stray descriptor for it.
	t.Run("docker-proxy-and-cache-lock", func(t *testing.T) {
		p := te.startCtr(ref, sh(`readlink /var/run/docker.sock; test -S "$(readlink /var/run/docker.sock)" && echo proxy-up
ls /proc/self/fd | tr '\n' ' '; echo
flock -n -x /kran/cache/.kran-lock true 2>/dev/null && echo unlocked || echo locked
env | grep -c '^KRAN_INIT_' || true
echo "tmp=$(stat -c %a /tmp) tmpdir=$TMPDIR"; d=$(mktemp -d) && echo x > "$d/f" && echo tmp-writable`))
		exit := waitExit(t, p, 120*time.Second)
		out, errOut := output(t, p)
		if exit.Code != 0 {
			t.Fatalf("exit %+v\n%s\n%s", exit, out, errOut)
		}
		for _, want := range []string{"/run/kran/docker/docker.sock\n", "proxy-up\n", "\nlocked\n", "\n0\n", "tmp=1777 tmpdir=/tmp\n", "tmp-writable\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s\n%s", want, out, errOut)
			}
		}
		if strings.Contains(out, " 9 ") {
			t.Errorf("the job inherited the lock's descriptor: %s", out)
		}
		if errOut != "" {
			t.Errorf("stderr %q", errOut)
		}
		// The job's /tmp volume went with its container.
		if v := tmpVolumes(t, te); v != "" {
			t.Errorf("job /tmp volumes left: %s", v)
		}
	})

	// A /tmp volume whose container was removed without it: the reaper
	// removes it, and nothing else.
	t.Run("tmp-volume-reaper", func(t *testing.T) {
		dk, _ := te.m.docker()
		ctx := context.Background()
		left, err := dk.run(ctx, nil, "volume", "create", "--label", "kran.ns="+te.m.ns, "--label", "kran.tmp=1")
		if err != nil {
			t.Fatal(err)
		}
		other, err := dk.run(ctx, nil, "volume", "create", "--label", "kran.ns="+te.m.ns)
		if err != nil {
			t.Fatal(err)
		}
		defer dk.run(ctx, nil, "volume", "rm", "--force", strings.TrimSpace(string(other)))
		te.m.reapTmpVolumes(ctx)
		if v := tmpVolumes(t, te); v != "" {
			t.Errorf("left-over /tmp volume %s not removed (%s)", strings.TrimSpace(string(left)), v)
		}
		if _, err := dk.run(ctx, nil, "volume", "inspect", strings.TrimSpace(string(other))); err != nil {
			t.Errorf("another volume of the namespace was removed: %v", err)
		}
	})

	// The cache trim deletes old entries over budget, keeps fresh ones,
	// and never runs while a job holds the caches.
	t.Run("cache-trim", func(t *testing.T) {
		p := te.startCtr(ref, sh(`set -e
mkdir -p /kran/cache/npm/_cacache /kran/cache/go-build/aa
head -c 3000000 /dev/zero > /kran/cache/npm/_cacache/old
touch -d '2026-01-01 00:00' /kran/cache/npm/_cacache/old
head -c 1000 /dev/zero > /kran/cache/go-build/aa/fresh
echo planted; sleep 300`))
		waitFor(t, p, "planted", 120*time.Second)
		dk, _ := te.m.docker()
		te.m.opts.CacheBudget = 1 << 20
		rep, err := te.m.runCacheTrim(context.Background(), dk)
		if err != nil || rep.Skipped == "" {
			t.Fatalf("trim while a job runs: %+v %v", rep, err)
		}
		p.Signal("KILL")
		waitExit(t, p, 60*time.Second)
		rep, err = te.m.runCacheTrim(context.Background(), dk)
		if err != nil || rep.Skipped != "" || rep.Removed < 1 || rep.Reclaimed < 3000000 || rep.Total == 0 {
			t.Fatalf("trim: %+v %v", rep, err)
		}
		p = te.startCtr(ref, sh(`test -e /kran/cache/npm/_cacache && echo old-kept; test -e /kran/cache/go-build/aa/fresh && echo fresh-kept`))
		waitExit(t, p, 60*time.Second)
		if out, _ := output(t, p); strings.Contains(out, "old-kept") || !strings.Contains(out, "fresh-kept") {
			t.Errorf("after trim: %q", out)
		}
		// The reaper's way: in the background, reported in the runtime.
		te.m.opts.CacheBudget = 0
		te.m.tun.cacheTrimEvery = time.Hour
		defer func() { te.m.tun.cacheTrimEvery = 0 }()
		te.m.maybeTrimCaches(context.Background())
		te.m.ct.mu.Lock()
		done := te.m.ct.done
		te.m.ct.mu.Unlock()
		if done == nil {
			t.Fatal("no trim started")
		}
		<-done
		var cs *proto.CacheStatus
		for _, rt := range te.m.Runtimes(context.Background()) {
			if rt.Kind == proto.RuntimeContainer {
				cs = rt.Caches
			}
		}
		if cs == nil || cs.DiskTotal == 0 || cs.Budget == 0 || cs.Used == 0 {
			t.Errorf("runtime caches %+v", cs)
		}
	})

	t.Run("signal-stdin-tty", func(t *testing.T) {
		p := te.startCtr(ref, sh("kill -TERM $$"))
		if exit := waitExit(t, p, 60*time.Second); exit.Code != 143 || exit.Signal != "TERM" {
			t.Errorf("signal exit %+v", exit)
		}
		p = te.startCtr(ref, []string{"cat"}, withStdin())
		w := p.Stdin()
		w.Write([]byte("piped through docker"))
		w.Close()
		waitExit(t, p, 60*time.Second)
		if out, _ := output(t, p); out != "piped through docker" {
			t.Errorf("stdin %q", out)
		}
		p = te.startCtr(ref, sh("test -t 1 && echo is-a-tty; stty size"), withTTY(30, 100))
		waitExit(t, p, 60*time.Second)
		if out, _ := output(t, p); !strings.Contains(out, "is-a-tty") || !strings.Contains(out, "30 100") {
			t.Errorf("tty %q", out)
		}
		// With --init the container starts and tini reports the failed exec
		// like a shell would.
		p = te.startCtr(ref, []string{"definitely-not-a-command"})
		if exit := waitExit(t, p, 60*time.Second); exit.Code != 127 {
			t.Errorf("not found: %+v", exit)
		}
	})

	t.Run("oom", func(t *testing.T) {
		p := te.startCtr(ref, sh("tail /dev/zero"), withAlloc(1<<30))
		exit := waitExit(t, p, 120*time.Second)
		if exit.Code != 137 || !strings.Contains(exit.Reason, "memory cap") {
			t.Fatalf("oom exit %+v", exit)
		}
	})

	t.Run("setup", func(t *testing.T) {
		setup := "echo prepared > setup-out; echo setup-ran"
		p := te.startCtr(ref, sh("cat ../setup-out"), withSetup(setup, "lock"))
		exit := waitExit(t, p, 60*time.Second)
		out, _ := output(t, p)
		if exit.Code != 0 || !strings.Contains(out, "setup-ran") || !strings.Contains(out, "prepared") {
			t.Fatalf("setup: %+v %q", exit, out)
		}
		p = te.startCtr(ref, sh("echo second"), withSetup(setup, "lock"))
		waitExit(t, p, 60*time.Second)
		if out, _ := output(t, p); strings.Contains(out, "setup-ran") {
			t.Fatalf("setup reran with unchanged key: %q", out)
		}
	})

	t.Run("lease-and-job-containers", func(t *testing.T) {
		p := te.startCtr(ref, sh("echo started; sleep 300"), noHeartbeat())
		waitFor(t, p, "started", 60*time.Second)
		// A container the job started itself (as testcontainers would).
		dk, _ := te.m.docker()
		extra := "kran-test-extra-" + p.JobID()
		if _, err := dk.run(context.Background(), nil, "run", "--detach", "--name", extra, "--label", "kran.job="+p.JobID(),
			"--label", "kran.ns=kran-test", te.m.imageTag(), "sleep", "300"); err != nil {
			t.Fatal(err)
		}
		exit := waitExit(t, p, 60*time.Second) // no heartbeats: lease expires
		if exit.State != proto.JobLost {
			t.Fatalf("exit %+v", exit)
		}
		assertNoContainer(t, te, "kran-test-job-"+p.JobID())
		assertNoContainer(t, te, extra)
	})

	t.Run("reaper", func(t *testing.T) {
		p := te.startCtr(ref, sh("true"))
		waitExit(t, p, 60*time.Second)
		dk, _ := te.m.docker()
		orphan := "kran-test-orphan-" + p.JobID()
		if _, err := dk.run(context.Background(), nil, "run", "--detach", "--name", orphan, "--label", "kran.job="+p.JobID(),
			te.m.imageTag(), "sleep", "300"); err != nil {
			t.Fatal(err)
		}
		if err := te.m.Reap(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertNoContainer(t, te, orphan)
	})
}

// tmpVolumes lists job /tmp volumes of te's namespace.
func tmpVolumes(t *testing.T, te *testEnv) string {
	t.Helper()
	dk, _ := te.m.docker()
	out, err := dk.run(context.Background(), nil, "volume", "ls", "--quiet", "--filter", "label=kran.ns="+te.m.ns, "--filter", "label=kran.tmp=1")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func assertNoContainer(t *testing.T, te *testEnv, name string) {
	t.Helper()
	dk, _ := te.m.docker()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := dk.run(context.Background(), nil, "inspect", name); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("container %s still exists", name)
}
