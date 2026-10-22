//go:build linux

package cli

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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
