//go:build unix

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hafencity-dev/kran/internal/transport"
)

// acquireInstance makes this the only daemon on these dirs, before it
// restores jobs, dials peers or writes its log: a second daemon (one
// started by hand while the service runs, or the service while an
// on-demand daemon runs) must not touch either. The lock is a flock on
// <runtime>/daemon.lock holding the owner's pid, released when the process
// exits however it exits. An older daemon holds no lock but answers on the
// API socket, so that counts as held too.
//
// Started by hand, a second daemon fails at once. Under a service manager
// it waits for the first to stop (once noting why), instead of exiting and
// being respawned by launchd or systemd every few seconds.
func acquireInstance(ctx context.Context, runtimeDir string, standby bool, note func(string, ...any)) (func(), error) {
	path := filepath.Join(runtimeDir, "daemon.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	sock := filepath.Join(runtimeDir, "kran.sock")
	noted := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		holder := ""
		if err != nil {
			holder = lockHolder(path)
		} else if socketAnswers(sock) {
			holder = "an older version"
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		} else {
			f.Truncate(0)
			f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !standby {
			f.Close()
			return nil, fmt.Errorf("%w (%s, socket %s); stop it first: kran service stop", transport.ErrAlreadyRunning, holder, sock)
		}
		if !noted && note != nil {
			note("kran: another kran daemon is running (%s, socket %s); waiting to take over when it stops", holder, sock)
			noted = true
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func lockHolder(path string) string {
	b, err := os.ReadFile(path)
	if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && perr == nil && pid > 0 {
		return fmt.Sprintf("pid %d", pid)
	}
	return "pid unknown"
}

func socketAnswers(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// capCrashLog keeps the file that receives the daemon's stdout/stderr
// (launchd's StandardErrorPath, the on-demand start's log) small: the
// daemon logs to daemon.log, so only fatal errors and crash traces land
// there, but they accumulate across restarts. f must be that file (path);
// when it is larger than max, it is cut down to its last keep bytes.
func capCrashLog(f *os.File, path string, max, keep int64) {
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() <= max {
		return
	}
	pst, err := os.Stat(path)
	if err != nil || !os.SameFile(st, pst) {
		return
	}
	r, err := os.Open(path)
	if err != nil {
		return
	}
	tail := make([]byte, keep)
	n, _ := r.ReadAt(tail, st.Size()-keep)
	r.Close()
	tail = tail[:n]
	if i := strings.IndexByte(string(tail), '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:] // start at a line
	}
	if f.Truncate(0) != nil {
		return
	}
	f.Seek(0, io.SeekStart)
	f.Write([]byte("[kran: older output removed]\n"))
	f.Write(tail)
}
