//go:build unix

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// killLocal stops a local job's process tree after its CLI vanished: the
// systemd scope when there is one (cgroup kill catches everything), else
// the process group, else the pid. TERM first, KILL after grace.
func killLocal(pid, pgid int, scope string, grace time.Duration, logf func(string, ...any)) {
	if scope != "" && validScope(scope) {
		if stopScope(scope, grace) {
			return
		}
	}
	target := 0
	switch {
	case pgid > 1 && pgid != syscall.Getpgrp():
		target = -pgid
	case pid > 1 && pid != os.Getpid():
		target = pid
	default:
		return
	}
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil {
		return // already gone
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if syscall.Kill(target, 0) != nil {
			return
		}
	}
	if err := syscall.Kill(target, syscall.SIGKILL); err == nil {
		logf("killed process group of a local job whose kran run went away (pgid %d)", pgid)
	}
}

// validScope accepts only kran's own transient scopes.
func validScope(s string) bool {
	if !strings.HasPrefix(s, "kran") || !strings.HasSuffix(s, ".scope") || len(s) > 200 {
		return false
	}
	return !strings.ContainsAny(s, "/ \t\n")
}

func systemctl(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func stopScope(scope string, grace time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), grace+10*time.Second)
	defer cancel()
	if _, err := systemctl(ctx, "kill", "--signal=SIGTERM", scope); err != nil {
		return false
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		if st, _ := systemctl(ctx, "is-active", scope); st != "active" && st != "deactivating" {
			return true
		}
	}
	systemctl(ctx, "kill", "--signal=SIGKILL", scope)
	systemctl(ctx, "stop", scope)
	return true
}

// processAlive reports whether pid exists (a zombie counts as alive until
// reaped; a CLI is never our child).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
