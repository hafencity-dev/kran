package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// testWatcher runs this test binary as `kran internal watch-parent`.
func testWatcher(pgid int) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestWatchParentHelper$")
	cmd.Env = append(os.Environ(), "KRAN_TEST_WATCH_PGID="+strconv.Itoa(pgid))
	return cmd
}

func TestWatchParentHelper(t *testing.T) {
	v := os.Getenv("KRAN_TEST_WATCH_PGID")
	if v == "" {
		t.Skip("helper process only")
	}
	pgid, _ := strconv.Atoi(v)
	os.Exit(watchParent(os.Stdin, pgid, 500*time.Millisecond))
}

// TestStandaloneRunHelper is a `kran run` without a daemon (the test
// SIGKILLs it).
func TestStandaloneRunHelper(t *testing.T) {
	pidFile := os.Getenv("KRAN_TEST_ORPHAN_PIDFILE")
	if pidFile == "" {
		t.Skip("helper process only")
	}
	r := &runner{
		spec:   proto.JobSpec{Argv: []string{"sh", "-c", "echo $$ > " + pidFile + "; sleep 60 & sleep 60"}, Cwd: os.TempDir()},
		env:    os.Environ(),
		stdout: os.Stdout, stderr: &lineWriter{w: os.Stderr},
		stdio:   localStdio{Out: os.Stdout, Err: os.Stderr},
		signals: make(chan os.Signal), tm: defaultTiming(),
		start: startLocal, probe: func(int64) bool { return false },
		watcher: testWatcher,
		dial:    func(context.Context) (client.Client, error) { return nil, client.ErrNoDaemon },
	}
	if os.Getenv("KRAN_TEST_NO_WATCHER") != "" {
		r.watcher = func(int) *exec.Cmd { return nil }
	}
	os.Exit(r.Run(context.Background()))
}

// kran run without a daemon must not leave its job running when it is
// killed with SIGKILL (an agent's timeout, the OOM killer): nobody else
// would ever stop it.
func TestStandaloneJobDiesWithKranRun(t *testing.T) {
	pgid, kill := startStandaloneHelper(t)
	kill()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(-pgid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the job's process group outlived its SIGKILLed kran run")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startStandaloneHelper starts a standalone kran run of a job with a
// background child and returns the job's process group and a SIGKILL of
// kran run.
func startStandaloneHelper(t *testing.T, env ...string) (int, func()) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStandaloneRunHelper$")
	cmd.Env = append(append(os.Environ(), "KRAN_TEST_ORPHAN_PIDFILE="+pidFile), env...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pgid int
	deadline := time.Now().Add(10 * time.Second)
	for pgid == 0 {
		if b, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(b), "\n") {
			pgid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("job did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { syscall.Kill(-pgid, syscall.SIGKILL) })
	time.Sleep(200 * time.Millisecond) // the background sleep exists too
	return pgid, func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

// A job that ends normally keeps its background children, as it would
// without kran, and its watcher goes away.
func TestLifelineReleasedOnNormalEnd(t *testing.T) {
	dir := t.TempDir()
	out, errf := tmpFile(t, "out"), tmpFile(t, "err")
	p, lerr := startLocal(localJob{JobID: "t1", Argv: []string{"sh", "-c", "sleep 30 & echo $! > bg"}, Dir: dir,
		Env: os.Environ(), Stdio: localStdio{Out: out, Err: errf}, Watcher: testWatcher})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if p.life == nil {
		t.Fatal("no watcher started")
	}
	<-p.Done()
	b, _ := os.ReadFile(filepath.Join(dir, "bg"))
	bg, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if bg <= 0 {
		t.Fatal("no background pid")
	}
	defer syscall.Kill(bg, syscall.SIGKILL)
	select {
	case <-p.life.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not exit after a normal end")
	}
	time.Sleep(700 * time.Millisecond) // longer than the watcher's grace
	if syscall.Kill(bg, 0) != nil {
		t.Fatal("the watcher killed a background child of a job that ended normally")
	}
}
