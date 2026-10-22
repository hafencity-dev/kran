//go:build unix

package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/selfexe"
)

// Autostart: a peer reaching this node over SSH while no daemon runs (e.g.
// a headless Mac where launchd user agents cannot run without a GUI login)
// starts one. The daemon is spawned detached (own session, stdio to a log
// file) so it outlives the SSH session; a lock file makes concurrent
// `kran serve --stdio` processes spawn at most one.

// AutostartWait bounds how long serve waits for a spawned daemon's socket.
var AutostartWait = 5 * time.Second

// serviceGrace is how long we give a daemon that may be (re)starting under
// its service manager before spawning our own.
var serviceGrace = 1500 * time.Millisecond

type spawnFunc func() (wait func() error, err error)

// ensureDaemon makes sure a daemon answers on sock, spawning one if needed.
func ensureDaemon(ctx context.Context, sock, lockPath string, spawn spawnFunc) error {
	if socketAlive(sock) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", lockPath, err)
	}
	defer f.Close()
	// Whoever holds the lock is starting a daemon: wait for it, then check.
	deadline := time.Now().Add(AutostartWait + serviceGrace + 5*time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return fmt.Errorf("lock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for another kran to start the daemon")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	if waitSocket(ctx, sock, serviceGrace) {
		return nil
	}
	wait, err := spawn()
	if err != nil {
		return fmt.Errorf("start the kran daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- wait() }()
	deadline = time.Now().Add(AutostartWait)
	for time.Now().Before(deadline) {
		if socketAlive(sock) {
			return nil
		}
		select {
		case err := <-exited:
			if err == nil {
				err = errors.New("exited")
			}
			return fmt.Errorf("the kran daemon stopped right after starting (%v); see its log in %s", err, paths.StateDir())
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("the kran daemon did not open %s within %s", sock, AutostartWait)
}

func socketAlive(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func waitSocket(ctx context.Context, sock string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if socketAlive(sock) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// spawnDaemon starts `<this binary> daemon` detached, in its own session,
// with stdin from /dev/null and stdout/stderr appended to
// <state>/daemon.out.log, the same crash-only file launchd uses (the daemon
// logs to daemon.log itself and keeps this one small). The environment
// (KRAN_*_DIR overrides) is inherited.
func spawnDaemon() (func() error, error) {
	// Never a test binary: `<test> daemon` would run the package's suite
	// again instead of a daemon, and recurse (see internal/selfexe).
	exe, err := selfexe.Path()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(paths.StateDir(), 0o700); err != nil {
		return nil, err
	}
	logPath := filepath.Join(paths.StateDir(), "daemon.out.log")
	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, out, out
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Wait, nil
}

func autostart(ctx context.Context, sock string) error {
	if os.Getenv("KRAN_NO_AUTOSTART") != "" {
		return nil
	}
	return ensureDaemon(ctx, sock, filepath.Join(paths.RuntimeDir(), "daemon-start.lock"), spawnDaemon)
}

// HoldStartLock takes the lock autostart takes before spawning a daemon, so
// `kran serve --stdio` waits instead of starting an on-demand daemon while
// `kran service` hands the socket over to the managed one. The returned
// function releases it.
func HoldStartLock(ctx context.Context) (func(), error) {
	lockPath := filepath.Join(paths.RuntimeDir(), "daemon-start.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", lockPath, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", lockPath, err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
