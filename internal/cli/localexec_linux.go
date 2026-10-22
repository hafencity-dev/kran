//go:build linux

package cli

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Linux delivers the parent-death signal when the *thread* that forked the
// child exits, and the Go runtime may retire threads. So children that must
// die with kran run are forked by one goroutine locked to its thread for
// the life of the process (it never returns and never unlocks).
var (
	forkOnce sync.Once
	forkReqs chan forkReq
)

type forkReq struct {
	cmd  *exec.Cmd
	done chan error
}

// startWithDeathSignal starts cmd so that it gets SIGKILL when this process
// dies (covers the job's leader even before the watcher runs).
func startWithDeathSignal(cmd *exec.Cmd) error {
	forkOnce.Do(func() {
		forkReqs = make(chan forkReq)
		go func() {
			runtime.LockOSThread()
			for r := range forkReqs {
				r.done <- r.cmd.Start()
			}
		}()
	})
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	done := make(chan error, 1)
	forkReqs <- forkReq{cmd, done}
	return <-done
}

func maxRSSBytes(kb int64) int64 { return kb * 1024 }

func ignoreTTOU() { signal.Ignore(syscall.SIGTTOU) }

// scopeUsable checks that systemd --user can create a scope with a memory cap.
// A real probe is the only reliable test: the memory controller may not be
// delegated to the user manager.
func scopeUsable(memMax int64) bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		return false
	}
	if _, err := os.Stat(rt + "/systemd"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemd-run", "--user", "--scope", "--quiet", "--collect",
		"--slice=kran.slice", "-p", "MemoryMax="+strconv.FormatInt(memMax, 10), "--", "true")
	return cmd.Run() == nil
}

// totalMemory returns physical memory in bytes (0 when unknown).
func totalMemory() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			fs := strings.Fields(v)
			if len(fs) > 0 {
				n, _ := strconv.ParseInt(fs[0], 10, 64)
				return n * 1024
			}
		}
	}
	return 0
}

// scopeOOMKilled reports whether systemd stopped the scope because the
// kernel OOM-killed it (the scope then stays failed; reset it so no
// failed unit is left behind).
func scopeOOMKilled(scope string) bool {
	out, err := exec.Command("systemctl", "--user", "show", "-p", "Result", "--value", scope).Output()
	res := strings.TrimSpace(string(out))
	if err != nil || res == "" || res == "success" {
		return false
	}
	_ = exec.Command("systemctl", "--user", "reset-failed", scope).Run()
	return res == "oom-kill"
}
