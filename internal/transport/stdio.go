package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/hafencity-dev/kran/internal/paths"
)

func serveStdio(ctx context.Context) int { return serveStdioAs(ctx, "") }

func serveStdioAs(ctx context.Context, peer string) int {
	sock := paths.PeerSocket()
	if err := autostart(ctx, sock); err != nil {
		fmt.Fprintf(os.Stderr, "kran: the kran daemon on this machine is not running and could not be started: %v (start it: kran service start)\n", err)
		return ExitDaemonDown
	}
	return BridgeAs(ctx, os.Stdin, os.Stdout, os.Stderr, sock, peer)
}

// Bridge connects in/out (the SSH channel of the forced command) to the
// daemon's peer socket and copies bytes both ways until either side closes.
// It returns the exit code for `kran serve --stdio`: 0 after a normal
// close, ExitDaemonDown when no daemon listens, ExitSocketDenied when the
// socket cannot be opened.
func Bridge(ctx context.Context, in io.Reader, out io.Writer, errOut io.Writer, socket string) int {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		code, msg := bridgeDialError(err, socket)
		fmt.Fprintln(errOut, msg)
		return code
	}
	defer conn.Close()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			conn.Close()
			if c, ok := out.(io.Closer); ok {
				c.Close()
			}
			if c, ok := in.(io.Closer); ok {
				c.Close()
			}
		})
	}
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(conn, in)
		stop()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(out, conn)
		stop()
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		stop()
	}
	return 0
}

func bridgeDialError(err error, socket string) (int, string) {
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return ExitDaemonDown, "kran: the kran daemon on this machine is not running (start it: kran service start)"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return ExitSocketDenied, fmt.Sprintf("kran: permission denied on %s; the daemon runs as another user (check: kran doctor)", socket)
	}
	return ExitDaemonDown, fmt.Sprintf("kran: cannot reach the kran daemon on this machine: %v (check: kran doctor)", err)
}

// ListenUnix listens on a unix socket at path with mode 0600 inside a 0700
// directory. A stale socket file (nothing answering) is replaced; a live one
// yields ErrAlreadyRunning.
func ListenUnix(path string) (*net.UnixListener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	if st, err := os.Stat(dir); err == nil && st.Mode().Perm()&0o077 != 0 {
		// Only tighten a directory we own; the runtime dir is ours.
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("restrict %s: %w", dir, err)
		}
	}
	if _, err := os.Lstat(path); err == nil {
		c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
		if err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: %w", path, ErrAlreadyRunning)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("restrict %s: %w", path, err)
	}
	return l, nil
}
