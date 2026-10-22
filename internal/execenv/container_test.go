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

const testRunnerDockerfile = "FROM alpine:3.20\nRUN apk add --no-cache coreutils\nLABEL kran.runner=\"test\"\n"

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
	if out, err := dk.run(ctx, nil, "ps", "--all", "--quiet", "--filter", "label=kran.ns=kran-test"); err == nil {
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
func noHeartbeat() jobOpt { return func(r *StartRequest) { r.Spec.Labels = map[string]string{"test.noheartbeat": "1"} } }

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
